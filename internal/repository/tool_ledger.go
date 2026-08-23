package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/tools"
)

type ToolLedgerStore struct {
	db       DBTX
	withinTx WithinTxFunc
}

func NewToolLedgerStore(db DBTX, withinTx WithinTxFunc) (*ToolLedgerStore, error) {
	if db == nil || withinTx == nil {
		return nil, fmt.Errorf("%w: tool ledger database", ErrInvalidArgument)
	}
	return &ToolLedgerStore{db: db, withinTx: withinTx}, nil
}

func (s *ToolLedgerStore) Begin(ctx context.Context, call tools.CallRecord, attempt tools.AttemptRecord) (record tools.CallRecord, created bool, err error) {
	err = s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		var inserted int
		insertErr := tx.QueryRowContext(ctx, insertToolCallSQL,
			call.TenantID.String(), call.CallID, call.RunID.String(), call.StepID.String(), call.Name,
			call.Version, string(call.Effect), string(call.ScopeLevel), nullableToolID(call.Scope.SessionID.String()),
			nullableToolID(call.Scope.RunID.String()), call.ArgumentsHash[:], call.InvocationKey,
			string(call.Status), call.CreatedAt.UTC(), call.UpdatedAt.UTC()).Scan(&inserted)
		if insertErr == nil {
			if err := insertToolAttempt(ctx, tx, call.TenantID, call.CallID, attempt); err != nil {
				return err
			}
			call.Attempts = []tools.AttemptRecord{attempt}
			record, created = call, true
			return nil
		}
		if !errors.Is(insertErr, sql.ErrNoRows) {
			return fmt.Errorf("insert Tool Call: %w", insertErr)
		}

		current, loadErr := loadToolCall(ctx, tx, call.TenantID, call.CallID, true)
		if errors.Is(loadErr, tools.ErrLedgerNotFound) {
			return tools.ErrLedgerConflict
		}
		if loadErr != nil {
			return loadErr
		}
		if !sameToolCall(current, call) {
			return tools.ErrLedgerConflict
		}
		for _, existing := range current.Attempts {
			if existing.AttemptID == attempt.AttemptID {
				if existing.AttemptNo != attempt.AttemptNo {
					return tools.ErrLedgerConflict
				}
				record = current
				return nil
			}
			if existing.AttemptNo == attempt.AttemptNo {
				return tools.ErrLedgerConflict
			}
		}
		if current.Status.Terminal() {
			record = current
			return nil
		}
		if len(current.Attempts) == 0 || attempt.AttemptNo != current.Attempts[len(current.Attempts)-1].AttemptNo+1 ||
			attempt.StartedAt.Before(current.UpdatedAt) {
			return tools.ErrLedgerConflict
		}
		if _, updateErr := tx.ExecContext(ctx, abandonToolAttemptsSQL,
			call.TenantID.String(), call.CallID, attempt.StartedAt.UTC()); updateErr != nil {
			return fmt.Errorf("abandon Tool Attempts: %w", updateErr)
		}
		if err := insertToolAttempt(ctx, tx, call.TenantID, call.CallID, attempt); err != nil {
			return err
		}
		updated, updateErr := scanToolCall(tx.QueryRowContext(ctx, takeoverToolCallSQL,
			call.TenantID.String(), call.CallID, current.RowVersion, attempt.StartedAt.UTC()))
		if errors.Is(updateErr, sql.ErrNoRows) {
			return tools.ErrLedgerStale
		}
		if updateErr != nil {
			return fmt.Errorf("take over Tool Call: %w", updateErr)
		}
		updated.Attempts, updateErr = loadToolAttempts(ctx, tx, call.TenantID, call.CallID)
		if updateErr != nil {
			return updateErr
		}
		record = updated
		return nil
	})
	return record, created, err
}

func (s *ToolLedgerStore) RecordOutcome(ctx context.Context, command tools.OutcomeCommand) (record tools.CallRecord, err error) {
	err = s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		current, loadErr := loadToolCall(ctx, tx, command.TenantID, command.CallID, true)
		if loadErr != nil {
			return loadErr
		}
		if current.RowVersion != command.ExpectedRowVersion || current.Status.Terminal() {
			return tools.ErrLedgerStale
		}
		index := -1
		for candidate := range current.Attempts {
			if current.Attempts[candidate].AttemptID == command.AttemptID {
				index = candidate
				break
			}
		}
		if index < 0 || current.Attempts[index].Status != domain.AttemptStatusStarted {
			return tools.ErrLedgerStale
		}
		if command.At.UTC().Before(current.Attempts[index].StartedAt) {
			return tools.ErrLedgerInvalid
		}
		attemptStatus := mapToolAttemptStatus(command.Status)
		result, execErr := tx.ExecContext(ctx, finishToolAttemptSQL, command.TenantID.String(), command.CallID,
			command.AttemptID.String(), string(attemptStatus), command.At.UTC())
		if execErr != nil {
			return fmt.Errorf("finish Tool Attempt: %w", execErr)
		}
		if err := requireOneAffected(result); err != nil {
			return tools.ErrLedgerStale
		}
		updated, updateErr := scanToolCall(tx.QueryRowContext(ctx, finishToolCallSQL,
			command.TenantID.String(), command.CallID, command.ExpectedRowVersion, string(command.Status),
			nullableToolJSON(command.Result), nullableToolID(command.FailureClass), command.At.UTC()))
		if errors.Is(updateErr, sql.ErrNoRows) {
			return tools.ErrLedgerStale
		}
		if updateErr != nil {
			return fmt.Errorf("finish Tool Call: %w", updateErr)
		}
		updated.Attempts, updateErr = loadToolAttempts(ctx, tx, command.TenantID, command.CallID)
		if updateErr != nil {
			return updateErr
		}
		record = updated
		return nil
	})
	return record, err
}

func (s *ToolLedgerStore) Get(ctx context.Context, tenantID domain.TenantID, callID string) (tools.CallRecord, error) {
	return loadToolCall(ctx, s.db, tenantID, callID, false)
}

func loadToolCall(ctx context.Context, db DBTX, tenantID domain.TenantID, callID string, lock bool) (tools.CallRecord, error) {
	query := selectToolCallSQL
	if lock {
		query += " FOR UPDATE"
	}
	record, err := scanToolCall(db.QueryRowContext(ctx, query, tenantID.String(), callID))
	if errors.Is(err, sql.ErrNoRows) {
		return record, tools.ErrLedgerNotFound
	}
	if err != nil {
		return record, fmt.Errorf("select Tool Call: %w", err)
	}
	record.Attempts, err = loadToolAttempts(ctx, db, tenantID, callID)
	if err != nil {
		return tools.CallRecord{}, err
	}
	return record, nil
}

func loadToolAttempts(ctx context.Context, db DBTX, tenantID domain.TenantID, callID string) ([]tools.AttemptRecord, error) {
	rows, err := db.QueryContext(ctx, selectToolAttemptsSQL, tenantID.String(), callID)
	if err != nil {
		return nil, fmt.Errorf("select Tool Attempts: %w", err)
	}
	defer rows.Close()
	var attempts []tools.AttemptRecord
	for rows.Next() {
		var value tools.AttemptRecord
		var id, status string
		var ended sql.NullTime
		if err = rows.Scan(&id, &value.AttemptNo, &status, &value.StartedAt, &ended); err != nil {
			return nil, err
		}
		if value.AttemptID, err = domain.ParseAttemptID(id); err != nil {
			return nil, err
		}
		value.Status = domain.AttemptStatus(status)
		if ended.Valid {
			value.EndedAt = ended.Time.UTC()
		}
		value.StartedAt = value.StartedAt.UTC()
		attempts = append(attempts, value)
	}
	return attempts, rows.Err()
}

type toolCallScanner interface{ Scan(...any) error }

func scanToolCall(row toolCallScanner) (tools.CallRecord, error) {
	var value tools.CallRecord
	var tenant, run, step, effect, scopeLevel, status string
	var sessionID, scopeRunID, failure sql.NullString
	var hash []byte
	var result []byte
	err := row.Scan(&tenant, &value.CallID, &run, &step, &value.Name, &value.Version, &effect,
		&scopeLevel, &sessionID, &scopeRunID, &hash, &value.InvocationKey, &status, &result,
		&failure, &value.RowVersion, &value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return value, err
	}
	if len(hash) != sha256.Size {
		return value, errors.New("stored Tool Call hash is invalid")
	}
	var parseErr error
	if value.TenantID, parseErr = domain.ParseTenantID(tenant); parseErr != nil {
		return value, parseErr
	}
	if value.RunID, parseErr = domain.ParseRunID(run); parseErr != nil {
		return value, parseErr
	}
	if value.StepID, parseErr = domain.ParseStepID(step); parseErr != nil {
		return value, parseErr
	}
	value.Effect, value.Status = tools.Effect(effect), tools.CallStatus(status)
	value.Scope.TenantID = value.TenantID
	if sessionID.Valid {
		if value.Scope.SessionID, parseErr = domain.ParseSessionID(sessionID.String); parseErr != nil {
			return value, parseErr
		}
	}
	if scopeRunID.Valid {
		if value.Scope.RunID, parseErr = domain.ParseRunID(scopeRunID.String); parseErr != nil {
			return value, parseErr
		}
	}
	value.ScopeLevel = tools.ScopeLevel(scopeLevel)
	copy(value.ArgumentsHash[:], hash)
	value.Result = append(json.RawMessage(nil), result...)
	value.FailureClass = failure.String
	value.CreatedAt, value.UpdatedAt = value.CreatedAt.UTC(), value.UpdatedAt.UTC()
	return value, nil
}

func insertToolAttempt(ctx context.Context, tx DBTX, tenantID domain.TenantID, callID string, attempt tools.AttemptRecord) error {
	_, err := tx.ExecContext(ctx, insertToolAttemptSQL, tenantID.String(), callID, attempt.AttemptID.String(),
		attempt.AttemptNo, attempt.Status.String(), attempt.StartedAt.UTC())
	if err != nil {
		return fmt.Errorf("insert Tool Attempt: %w", err)
	}
	return nil
}

func sameToolCall(left, right tools.CallRecord) bool {
	return left.TenantID == right.TenantID && left.CallID == right.CallID && left.RunID == right.RunID &&
		left.StepID == right.StepID && left.Name == right.Name && left.Version == right.Version &&
		left.Effect == right.Effect && left.ScopeLevel == right.ScopeLevel && left.Scope == right.Scope && left.ArgumentsHash == right.ArgumentsHash &&
		left.InvocationKey == right.InvocationKey
}

func nullableToolID(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableToolJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}
func mapToolAttemptStatus(status tools.CallStatus) domain.AttemptStatus {
	switch status {
	case tools.CallSucceeded:
		return domain.AttemptStatusSucceeded
	case tools.CallFailed:
		return domain.AttemptStatusFailed
	default:
		return domain.AttemptStatusUnknown
	}
}

const toolCallColumns = `tenant_id,id,run_id,step_id,tool_name,tool_version,effect,scope_level,
scope_session_id,scope_run_id,arguments_sha256,invocation_key,status,result,failure_class,row_version,created_at,updated_at`
const insertToolCallSQL = `INSERT INTO tool_calls(tenant_id,id,run_id,step_id,tool_name,tool_version,effect,
scope_level,scope_session_id,scope_run_id,arguments_sha256,invocation_key,status,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT DO NOTHING RETURNING 1`
const selectToolCallSQL = `SELECT ` + toolCallColumns + ` FROM tool_calls WHERE tenant_id=$1 AND id=$2`
const insertToolAttemptSQL = `INSERT INTO tool_call_attempts(tenant_id,call_id,attempt_id,attempt_no,status,started_at)
VALUES($1,$2,$3,$4,$5,$6)`
const selectToolAttemptsSQL = `SELECT attempt_id,attempt_no,status,started_at,ended_at FROM tool_call_attempts
WHERE tenant_id=$1 AND call_id=$2 ORDER BY attempt_no`
const abandonToolAttemptsSQL = `UPDATE tool_call_attempts SET status='UNKNOWN',ended_at=$3
WHERE tenant_id=$1 AND call_id=$2 AND status='STARTED'`
const takeoverToolCallSQL = `UPDATE tool_calls SET status='UNKNOWN',result=NULL,failure_class='worker_takeover',
row_version=row_version+1,updated_at=$4 WHERE tenant_id=$1 AND id=$2 AND row_version=$3
AND status IN ('PREPARED','UNKNOWN') RETURNING ` + toolCallColumns
const finishToolAttemptSQL = `UPDATE tool_call_attempts SET status=$4,ended_at=$5
WHERE tenant_id=$1 AND call_id=$2 AND attempt_id=$3 AND status='STARTED'`
const finishToolCallSQL = `UPDATE tool_calls SET status=$4,result=$5::jsonb,failure_class=$6,
row_version=row_version+1,updated_at=$7 WHERE tenant_id=$1 AND id=$2 AND row_version=$3
AND status IN ('PREPARED','UNKNOWN') RETURNING ` + toolCallColumns

var _ tools.LedgerStore = (*ToolLedgerStore)(nil)
