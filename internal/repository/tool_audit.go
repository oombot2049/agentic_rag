package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/audit"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/tools"
)

type ToolAuditStore struct {
	db       DBTX
	withinTx WithinTxFunc
}

func NewToolAuditStore(db DBTX, withinTx WithinTxFunc) (*ToolAuditStore, error) {
	if db == nil || withinTx == nil {
		return nil, fmt.Errorf("%w: tool audit database", ErrInvalidArgument)
	}
	return &ToolAuditStore{db: db, withinTx: withinTx}, nil
}

func (s *ToolAuditStore) Append(ctx context.Context, value audit.Record) error {
	if err := audit.Validate(value); err != nil {
		return err
	}
	return s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		if value.Outcome != audit.OutcomeDenied {
			var marker int
			err := tx.QueryRowContext(ctx, verifyToolAuditCallSQL, value.TenantID.String(), value.ToolCallID,
				value.RunID.String(), value.StepID.String(), value.ToolName, value.ToolVersion, string(value.Effect),
				value.ArgumentsSHA256[:], string(value.Outcome)).Scan(&marker)
			if errors.Is(err, sql.ErrNoRows) {
				return audit.ErrConflict
			}
			if err != nil {
				return fmt.Errorf("verify Tool Call audit source: %w", err)
			}
		}
		if value.Artifact != nil {
			var marker int
			err := tx.QueryRowContext(ctx, verifyToolAuditArtifactSQL, value.TenantID.String(), value.Artifact.ID,
				value.Artifact.SHA256[:], value.Artifact.SizeBytes, value.Artifact.MediaType, value.ExpiresAt).Scan(&marker)
			if errors.Is(err, sql.ErrNoRows) {
				return audit.ErrArtifact
			}
			if err != nil {
				return fmt.Errorf("verify Tool audit Artifact: %w", err)
			}
		}
		_, err := tx.ExecContext(ctx, insertToolAuditSQL, auditHeaderArguments(value)...)
		if err != nil {
			return fmt.Errorf("insert Tool audit: %w", err)
		}
		for _, attempt := range value.Attempts {
			if _, err = tx.ExecContext(ctx, insertToolAuditAttemptSQL, value.TenantID.String(), value.ID,
				attempt.AttemptID.String(), attempt.AttemptNo, attempt.Status.String(), nullableString(attempt.FailureClass),
				attempt.StartedAt.UTC(), attempt.EndedAt.UTC()); err != nil {
				return fmt.Errorf("insert Tool audit Attempt: %w", err)
			}
		}
		return nil
	})
}

func (s *ToolAuditStore) GetByToolCall(ctx context.Context, tenant domain.TenantID, callID string) (audit.Record, error) {
	value, err := scanToolAudit(s.db.QueryRowContext(ctx, selectToolAuditByCallSQL, tenant.String(), callID))
	if errors.Is(err, sql.ErrNoRows) {
		return value, audit.ErrNotFound
	}
	if err != nil {
		return value, fmt.Errorf("select Tool audit: %w", err)
	}
	value.Attempts, err = loadToolAuditAttempts(ctx, s.db, tenant, value.ID)
	if err != nil {
		return audit.Record{}, err
	}
	if err = audit.Validate(value); err != nil {
		return audit.Record{}, fmt.Errorf("hydrate Tool audit: %w", err)
	}
	return value, nil
}

func (s *ToolAuditStore) ListByRun(ctx context.Context, tenant domain.TenantID, run domain.RunID, after *audit.Cursor, limit int) (audit.Page, error) {
	if limit < 1 || limit > 200 {
		return audit.Page{}, audit.ErrInvalid
	}
	hasCursor, cursorTime, cursorID := false, time.Time{}, ""
	if after != nil {
		hasCursor, cursorTime, cursorID = true, after.CreatedAt.UTC(), after.ID
	}
	rows, err := s.db.QueryContext(ctx, listToolAuditsByRunSQL, tenant.String(), run.String(), hasCursor, cursorTime, cursorID, limit+1)
	if err != nil {
		return audit.Page{}, fmt.Errorf("list Tool audits: %w", err)
	}
	defer rows.Close()
	values := make([]audit.Record, 0, limit+1)
	for rows.Next() {
		value, scanErr := scanToolAudit(rows)
		if scanErr != nil {
			return audit.Page{}, scanErr
		}
		values = append(values, value)
	}
	if err = rows.Err(); err != nil {
		return audit.Page{}, err
	}
	more := len(values) > limit
	if more {
		values = values[:limit]
	}
	if err = loadToolAuditPageAttempts(ctx, s.db, tenant, values); err != nil {
		return audit.Page{}, err
	}
	for _, value := range values {
		if err = audit.Validate(value); err != nil {
			return audit.Page{}, fmt.Errorf("hydrate Tool audit page: %w", err)
		}
	}
	page := audit.Page{Items: values}
	if more {
		last := values[len(values)-1]
		page.NextCursor = &audit.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func scanToolAudit(row rowScanner) (audit.Record, error) {
	var value audit.Record
	var tenant, session, turn, run, step, effect, decision, outcome string
	var approval, artifactID, artifactMedia sql.NullString
	var argumentsHash, resultHash, artifactHash []byte
	var artifactSize sql.NullInt64
	var attemptCount int
	err := row.Scan(&tenant, &value.ID, &session, &turn, &run, &step, &value.ToolCallID, &value.CallerSubject,
		&value.ToolName, &value.ToolVersion, &effect, &decision, &approval, &argumentsHash, &outcome, &resultHash,
		&value.ResultSizeBytes, &value.BusinessImpactCode, &artifactID, &artifactHash, &artifactSize, &artifactMedia,
		&attemptCount, &value.StartedAt, &value.EndedAt, &value.CreatedAt, &value.ExpiresAt)
	if err != nil {
		return value, err
	}
	if len(argumentsHash) != sha256.Size || len(resultHash) != 0 && len(resultHash) != sha256.Size {
		return value, audit.ErrInvalid
	}
	var parseErr error
	if value.TenantID, parseErr = domain.ParseTenantID(tenant); parseErr != nil {
		return value, parseErr
	}
	if value.SessionID, parseErr = domain.ParseSessionID(session); parseErr != nil {
		return value, parseErr
	}
	if value.TurnID, parseErr = domain.ParseTurnID(turn); parseErr != nil {
		return value, parseErr
	}
	if value.RunID, parseErr = domain.ParseRunID(run); parseErr != nil {
		return value, parseErr
	}
	if value.StepID, parseErr = domain.ParseStepID(step); parseErr != nil {
		return value, parseErr
	}
	value.Effect, value.PolicyDecision, value.Outcome, value.ApprovalID = tools.Effect(effect), audit.PolicyDecision(decision), audit.Outcome(outcome), approval.String
	copy(value.ArgumentsSHA256[:], argumentsHash)
	copy(value.ResultSHA256[:], resultHash)
	if artifactID.Valid {
		if len(artifactHash) != sha256.Size || !artifactSize.Valid || !artifactMedia.Valid {
			return value, audit.ErrArtifact
		}
		value.Artifact = &audit.ArtifactReference{ID: artifactID.String, SizeBytes: artifactSize.Int64, MediaType: artifactMedia.String}
		copy(value.Artifact.SHA256[:], artifactHash)
	}
	value.StartedAt, value.EndedAt = value.StartedAt.UTC(), value.EndedAt.UTC()
	value.CreatedAt, value.ExpiresAt = value.CreatedAt.UTC(), value.ExpiresAt.UTC()
	value.Attempts = make([]audit.Attempt, 0, attemptCount)
	return value, nil
}

func loadToolAuditAttempts(ctx context.Context, db DBTX, tenant domain.TenantID, auditID string) ([]audit.Attempt, error) {
	rows, err := db.QueryContext(ctx, selectToolAuditAttemptsSQL, tenant.String(), auditID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []audit.Attempt
	for rows.Next() {
		value, scanErr := scanAuditAttempt(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// loadToolAuditPageAttempts uses one query for the whole page, avoiding an N+1 query pattern.
func loadToolAuditPageAttempts(ctx context.Context, db DBTX, tenant domain.TenantID, values []audit.Record) error {
	if len(values) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, selectToolAuditPageAttemptsSQL, tenant.String(), values[0].RunID.String(), values[0].CreatedAt, values[0].ID, values[len(values)-1].CreatedAt, values[len(values)-1].ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]*audit.Record, len(values))
	for i := range values {
		byID[values[i].ID] = &values[i]
	}
	for rows.Next() {
		var auditID string
		var attempt audit.Attempt
		var id, status string
		var failure sql.NullString
		if err = rows.Scan(&auditID, &id, &attempt.AttemptNo, &status, &failure, &attempt.StartedAt, &attempt.EndedAt); err != nil {
			return err
		}
		if attempt.AttemptID, err = domain.ParseAttemptID(id); err != nil {
			return err
		}
		attempt.Status, attempt.FailureClass = domain.AttemptStatus(status), failure.String
		attempt.StartedAt, attempt.EndedAt = attempt.StartedAt.UTC(), attempt.EndedAt.UTC()
		if record := byID[auditID]; record != nil {
			record.Attempts = append(record.Attempts, attempt)
		}
	}
	return rows.Err()
}

func scanAuditAttempt(row rowScanner) (audit.Attempt, error) {
	var value audit.Attempt
	var id, status string
	var failure sql.NullString
	if err := row.Scan(&id, &value.AttemptNo, &status, &failure, &value.StartedAt, &value.EndedAt); err != nil {
		return value, err
	}
	var err error
	if value.AttemptID, err = domain.ParseAttemptID(id); err != nil {
		return value, err
	}
	value.Status, value.FailureClass = domain.AttemptStatus(status), failure.String
	value.StartedAt, value.EndedAt = value.StartedAt.UTC(), value.EndedAt.UTC()
	return value, nil
}

func auditHeaderArguments(value audit.Record) []any {
	var resultHash, artifactID, artifactHash, artifactSize, artifactMedia any
	if value.Outcome == audit.OutcomeSucceeded {
		resultHash = value.ResultSHA256[:]
	}
	if value.Artifact != nil {
		artifactID, artifactHash, artifactSize, artifactMedia = value.Artifact.ID, value.Artifact.SHA256[:], value.Artifact.SizeBytes, value.Artifact.MediaType
	}
	return []any{value.TenantID.String(), value.ID, value.SessionID.String(), value.TurnID.String(), value.RunID.String(), value.StepID.String(),
		value.ToolCallID, value.CallerSubject, value.ToolName, value.ToolVersion, string(value.Effect), string(value.PolicyDecision), nullableString(value.ApprovalID),
		value.ArgumentsSHA256[:], string(value.Outcome), resultHash, value.ResultSizeBytes, value.BusinessImpactCode, artifactID, artifactHash, artifactSize,
		artifactMedia, len(value.Attempts), value.StartedAt.UTC(), value.EndedAt.UTC(), value.CreatedAt.UTC(), value.ExpiresAt.UTC()}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const verifyToolAuditCallSQL = `SELECT 1 FROM tool_calls WHERE tenant_id=$1 AND id=$2 AND run_id=$3 AND step_id=$4 AND tool_name=$5 AND tool_version=$6 AND effect=$7 AND arguments_sha256=$8 AND (($9='SUCCEEDED' AND status='SUCCEEDED') OR ($9='FAILED' AND status='FAILED') OR ($9='UNKNOWN' AND status='UNKNOWN'))`
const verifyToolAuditArtifactSQL = `SELECT 1 FROM artifact_metadata WHERE tenant_id=$1 AND id=$2 AND content_sha256=$3 AND size_bytes=$4 AND media_type=$5 AND expires_at >= $6`
const insertToolAuditSQL = `INSERT INTO tool_audits(tenant_id,id,session_id,turn_id,run_id,step_id,tool_call_id,caller_subject,tool_name,tool_version,effect,policy_decision,approval_id,arguments_sha256,outcome,result_sha256,result_size_bytes,business_impact_code,artifact_id,artifact_sha256,artifact_size_bytes,artifact_media_type,attempt_count,started_at,ended_at,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`
const insertToolAuditAttemptSQL = `INSERT INTO tool_audit_attempts(tenant_id,audit_id,attempt_id,attempt_no,status,failure_class,started_at,ended_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`
const toolAuditColumns = `tenant_id,id,session_id,turn_id,run_id,step_id,tool_call_id,caller_subject,tool_name,tool_version,effect,policy_decision,approval_id,arguments_sha256,outcome,result_sha256,result_size_bytes,business_impact_code,artifact_id,artifact_sha256,artifact_size_bytes,artifact_media_type,attempt_count,started_at,ended_at,created_at,expires_at`
const selectToolAuditByCallSQL = `SELECT ` + toolAuditColumns + ` FROM tool_audits WHERE tenant_id=$1 AND tool_call_id=$2`
const listToolAuditsByRunSQL = `SELECT ` + toolAuditColumns + ` FROM tool_audits WHERE tenant_id=$1 AND run_id=$2 AND (NOT $3 OR (created_at,id)>($4,$5)) ORDER BY created_at,id LIMIT $6`
const selectToolAuditAttemptsSQL = `SELECT attempt_id,attempt_no,status,failure_class,started_at,ended_at FROM tool_audit_attempts WHERE tenant_id=$1 AND audit_id=$2 ORDER BY attempt_no`
const selectToolAuditPageAttemptsSQL = `SELECT x.audit_id,x.attempt_id,x.attempt_no,x.status,x.failure_class,x.started_at,x.ended_at FROM tool_audit_attempts x JOIN tool_audits a ON a.tenant_id=x.tenant_id AND a.id=x.audit_id WHERE x.tenant_id=$1 AND a.run_id=$2 AND (a.created_at,a.id)>=($3,$4) AND (a.created_at,a.id)<=($5,$6) ORDER BY a.created_at,a.id,x.attempt_no`

var _ audit.Store = (*ToolAuditStore)(nil)
