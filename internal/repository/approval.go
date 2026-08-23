package repository

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/approval"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/tools"
)

type ApprovalStore struct {
	db       DBTX
	withinTx WithinTxFunc
}

func NewApprovalStore(db DBTX, withinTx WithinTxFunc) (*ApprovalStore, error) {
	if db == nil || withinTx == nil {
		return nil, fmt.Errorf("%w: approval database", ErrInvalidArgument)
	}
	return &ApprovalStore{db: db, withinTx: withinTx}, nil
}

func (s *ApprovalStore) Create(ctx context.Context, value approval.Record) (record approval.Record, err error) {
	err = s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		var inserted int
		err := tx.QueryRowContext(ctx, insertApprovalSQL, approvalArgs(value)...).Scan(&inserted)
		if errors.Is(err, sql.ErrNoRows) {
			existing, loadErr := loadApproval(ctx, tx, value.TenantID, value.ID, true)
			if errors.Is(loadErr, approval.ErrNotFound) {
				return approval.ErrConflict
			}
			if loadErr != nil {
				return loadErr
			}
			if !sameApprovalRequest(existing, value) {
				return approval.ErrConflict
			}
			record = existing
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert approval: %w", err)
		}
		result, err := tx.ExecContext(ctx, waitApprovalRunSQL, value.TenantID.String(), value.RunID.String(),
			value.WorkerOwner, value.LeaseToken, value.ExecutionVersion, value.CreatedAt)
		if err != nil {
			return fmt.Errorf("pause Run for approval: %w", err)
		}
		if err = requireOneAffected(result); err != nil {
			return ErrLeaseFenced
		}
		record = value
		return nil
	})
	return record, err
}

func (s *ApprovalStore) Decide(ctx context.Context, command approval.DecisionCommand) (record approval.Record, err error) {
	err = s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		current, err := loadApproval(ctx, tx, command.TenantID, command.ApprovalID, true)
		if err != nil {
			return err
		}
		target := approval.StatusRejected
		if command.Approve {
			target = approval.StatusApproved
		}
		if current.ApproverSubject != command.ActorSubject || !hmac.Equal(current.NonceHash[:], command.NonceHash[:]) {
			return approval.ErrDenied
		}
		if current.Status.Terminal() {
			if current.Status == target && current.DecisionActor == command.ActorSubject {
				record = current
				return nil
			}
			return approval.ErrConflict
		}
		if current.RowVersion != command.ExpectedRowVersion {
			return approval.ErrStale
		}
		if !command.At.Before(current.ExpiresAt) {
			target = approval.StatusExpired
		}
		updated, err := scanApproval(tx.QueryRowContext(ctx, decideApprovalSQL, command.TenantID.String(),
			command.ApprovalID, command.ExpectedRowVersion, string(target), command.ActorSubject, command.At))
		if errors.Is(err, sql.ErrNoRows) {
			return approval.ErrStale
		}
		if err != nil {
			return fmt.Errorf("decide approval: %w", err)
		}
		if target == approval.StatusApproved {
			result, updateErr := tx.ExecContext(ctx, resumeApprovalRunSQL, current.TenantID.String(), current.RunID.String(), command.At)
			if updateErr != nil {
				return updateErr
			}
			if updateErr = requireOneAffected(result); updateErr != nil {
				return approval.ErrStale
			}
			payload, _ := json.Marshal(map[string]any{"schema_version": 1, "tenant_id": current.TenantID.String(),
				"run_id": current.RunID.String(), "step_id": current.StepID.String(), "approval_id": current.ID})
			if _, updateErr = tx.ExecContext(ctx, insertApprovalOutboxSQL, current.TenantID.String(), approvalEventID(current),
				current.RunID.String(), string(payload), command.At); updateErr != nil {
				return updateErr
			}
		} else {
			result, updateErr := tx.ExecContext(ctx, rejectApprovalExecutionSQL, current.TenantID.String(), current.RunID.String(),
				current.StepID.String(), current.AttemptID.String(), current.ToolCallID, command.At)
			if updateErr != nil {
				return updateErr
			}
			if updateErr = requireOneAffected(result); updateErr != nil {
				return approval.ErrStale
			}
			_, updateErr = tx.ExecContext(ctx, clearApprovalSessionSQL, current.TenantID.String(), current.SessionID.String(), current.RunID.String(), command.At)
			if updateErr != nil {
				return updateErr
			}
		}
		record = updated
		return nil
	})
	return record, err
}

func (s *ApprovalStore) Get(ctx context.Context, tenant domain.TenantID, id string) (approval.Record, error) {
	return loadApproval(ctx, s.db, tenant, id, false)
}

func loadApproval(ctx context.Context, db DBTX, tenant domain.TenantID, id string, lock bool) (approval.Record, error) {
	query := selectApprovalSQL
	if lock {
		query += " FOR UPDATE"
	}
	value, err := scanApproval(db.QueryRowContext(ctx, query, tenant.String(), id))
	if errors.Is(err, sql.ErrNoRows) {
		return value, approval.ErrNotFound
	}
	if err != nil {
		return value, fmt.Errorf("select approval: %w", err)
	}
	return value, nil
}

type approvalScanner interface{ Scan(...any) error }

func scanApproval(row approvalScanner) (approval.Record, error) {
	var value approval.Record
	var tenant, session, turn, run, step, attempt, effect, status string
	var argumentHash, nonceHash []byte
	var actor sql.NullString
	var decided sql.NullTime
	err := row.Scan(&tenant, &value.ID, &value.RequesterSubject, &value.ApproverSubject, &session, &turn, &run, &step, &attempt,
		&value.ToolCallID, &value.ToolName, &value.ToolVersion, &effect, &argumentHash, &nonceHash, &value.WorkerOwner,
		&value.LeaseToken, &value.ExecutionVersion, &status, &actor, &value.RowVersion, &value.CreatedAt, &value.ExpiresAt, &decided)
	if err != nil {
		return value, err
	}
	if len(argumentHash) != sha256.Size || len(nonceHash) != sha256.Size {
		return value, errors.New("stored approval hash invalid")
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
	if value.AttemptID, parseErr = domain.ParseAttemptID(attempt); parseErr != nil {
		return value, parseErr
	}
	value.Effect, value.Status = tools.Effect(effect), approval.Status(status)
	copy(value.ArgumentsHash[:], argumentHash)
	copy(value.NonceHash[:], nonceHash)
	value.DecisionActor = actor.String
	if decided.Valid {
		value.DecidedAt = decided.Time.UTC()
	}
	value.CreatedAt, value.ExpiresAt = value.CreatedAt.UTC(), value.ExpiresAt.UTC()
	return value, nil
}

func approvalArgs(v approval.Record) []any {
	return []any{v.TenantID.String(), v.ID, v.RequesterSubject, v.ApproverSubject,
		v.SessionID.String(), v.TurnID.String(), v.RunID.String(), v.StepID.String(), v.AttemptID.String(), v.ToolCallID, v.ToolName,
		v.ToolVersion, string(v.Effect), v.ArgumentsHash[:], v.NonceHash[:], v.WorkerOwner, v.LeaseToken, v.ExecutionVersion,
		string(v.Status), v.CreatedAt, v.ExpiresAt}
}
func sameApprovalRequest(a, b approval.Record) bool {
	return a.TenantID == b.TenantID && a.ID == b.ID && a.RequesterSubject == b.RequesterSubject &&
		a.ApproverSubject == b.ApproverSubject && a.SessionID == b.SessionID && a.TurnID == b.TurnID && a.RunID == b.RunID && a.StepID == b.StepID &&
		a.AttemptID == b.AttemptID && a.ToolCallID == b.ToolCallID && a.ToolName == b.ToolName && a.ToolVersion == b.ToolVersion && a.Effect == b.Effect &&
		a.ArgumentsHash == b.ArgumentsHash && hmac.Equal(a.NonceHash[:], b.NonceHash[:]) && a.CreatedAt.Equal(b.CreatedAt) && a.ExpiresAt.Equal(b.ExpiresAt) &&
		a.WorkerOwner == b.WorkerOwner && a.LeaseToken == b.LeaseToken && a.ExecutionVersion == b.ExecutionVersion
}
func approvalEventID(v approval.Record) string {
	sum := sha256.Sum256([]byte(v.TenantID.String() + "\x00" + v.ID))
	return "evt_" + hex.EncodeToString(sum[:16])
}

const approvalColumns = `tenant_id,id,requester_subject,approver_subject,session_id,turn_id,run_id,step_id,attempt_id,
tool_call_id,tool_name,tool_version,effect,arguments_sha256,nonce_sha256,worker_owner,lease_token,execution_version,
status,decision_actor,row_version,created_at,expires_at,decided_at`
const insertApprovalSQL = `INSERT INTO tool_approvals(` + approvalColumns + `) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,
$12,$13,$14,$15,$16,$17,$18,$19,NULL,1,$20,$21,NULL) ON CONFLICT DO NOTHING RETURNING 1`
const selectApprovalSQL = `SELECT ` + approvalColumns + ` FROM tool_approvals WHERE tenant_id=$1 AND id=$2`
const waitApprovalRunSQL = `UPDATE runs SET status='WAITING_APPROVAL',lease_owner=NULL,lease_expires_at=NULL,
execution_version=execution_version+1,updated_at=$6 WHERE tenant_id=$1 AND id=$2 AND status='RUNNING' AND lease_owner=$3
AND lease_token=$4 AND execution_version=$5 AND lease_expires_at>$6`
const decideApprovalSQL = `UPDATE tool_approvals SET status=$4,decision_actor=$5,decided_at=$6,row_version=row_version+1
WHERE tenant_id=$1 AND id=$2 AND row_version=$3 AND status='WAITING' RETURNING ` + approvalColumns
const resumeApprovalRunSQL = `UPDATE runs SET status='QUEUED',execution_version=execution_version+1,updated_at=$3
WHERE tenant_id=$1 AND id=$2 AND status='WAITING_APPROVAL' AND lease_owner IS NULL`
const insertApprovalOutboxSQL = `INSERT INTO outbox_events(tenant_id,id,aggregate_type,aggregate_id,event_type,payload,available_at,occurred_at)
VALUES($1,$2,'run',$3,'RunApprovalGranted.v1',$4::jsonb,$5,$5)`
const rejectApprovalExecutionSQL = `WITH finished_attempt AS (
UPDATE attempts SET status='FAILED',ended_at=$6,row_version=row_version+1 WHERE tenant_id=$1 AND id=$4 AND status='STARTED' RETURNING 1),
finished_tool_attempt AS (UPDATE tool_call_attempts SET status='FAILED',ended_at=$6 WHERE tenant_id=$1 AND attempt_id=$4 AND status='STARTED'),
finished_call AS (UPDATE tool_calls SET status='FAILED',failure_class='approval_denied',row_version=row_version+1,updated_at=$6 WHERE tenant_id=$1 AND id=$5 AND status IN ('PREPARED','UNKNOWN')),
finished_step AS (UPDATE steps SET status='FAILED',row_version=row_version+1,updated_at=$6 WHERE tenant_id=$1 AND id=$3 AND status='RUNNING')
UPDATE runs SET status='FAILED',execution_version=execution_version+1,updated_at=$6 WHERE tenant_id=$1 AND id=$2
AND status='WAITING_APPROVAL' AND EXISTS(SELECT 1 FROM finished_attempt)`
const clearApprovalSessionSQL = `UPDATE sessions SET active_run_id=NULL,row_version=row_version+1,updated_at=$4
WHERE tenant_id=$1 AND id=$2 AND active_run_id=$3`

var _ approval.Store = (*ApprovalStore)(nil)
