package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/events"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	runcontrol "github.com/agenticrag/agenticrag/internal/run"
)

type RunCancelStore struct{ withinTx WithinTxFunc }

func NewRunCancelStore(withinTx WithinTxFunc) (*RunCancelStore, error) {
	if withinTx == nil {
		return nil, fmt.Errorf("%w: transaction runner is required", ErrInvalidArgument)
	}
	return &RunCancelStore{withinTx: withinTx}, nil
}

func (s *RunCancelStore) Cancel(ctx context.Context, command runcontrol.CancelPersistence) (runcontrol.CancelResult, error) {
	var result runcontrol.CancelResult
	err := s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		claims, err := idempotency.NewRepository(tx)
		if err != nil {
			return err
		}
		claim, err := claims.Claim(ctx, command.Scope, command.Fingerprint, command.Now, command.Retention)
		if err != nil {
			return err
		}
		result.Decision = claim.Decision
		if claim.Decision == idempotency.DecisionReplaySucceeded {
			if claim.Record.Response == nil {
				return errors.New("stored cancellation response is missing")
			}
			response := *claim.Record.Response
			response.Body = append([]byte(nil), response.Body...)
			result.Response = &response
			return nil
		}
		if claim.Decision != idempotency.DecisionAcquired {
			return nil
		}
		return s.cancelAcquired(ctx, tx, claims, command, &result)
	})
	return result, err
}

type lockedCancelRun struct {
	SessionID domain.SessionID
	TurnID    domain.TurnID
	Status    domain.RunStatus
}

func (s *RunCancelStore) cancelAcquired(ctx context.Context, tx DBTX, claims *idempotency.Repository, command runcontrol.CancelPersistence, result *runcontrol.CancelResult) error {
	locked, err := lockCancelRun(ctx, tx, command.TenantID, command.RunID)
	if err != nil {
		return err
	}
	status := locked.Status
	switch status {
	case domain.RunStatusQueued:
		if err := transitionQueuedCancellation(ctx, tx, command, locked); err != nil {
			return err
		}
		status = domain.RunStatusCancelled
		if err := appendCancellationEvent(ctx, tx, command, locked, events.RunCancelRequested, command.EventID); err != nil {
			return err
		}
		if err := appendCancellationEvent(ctx, tx, command, locked, events.RunCancelled, ""); err != nil {
			return err
		}
	case domain.RunStatusRunning:
		if err := transitionRunningCancellation(ctx, tx, command, locked); err != nil {
			return err
		}
		status = domain.RunStatusCancelRequested
		if err := appendCancellationEvent(ctx, tx, command, locked, events.RunCancelRequested, command.EventID); err != nil {
			return err
		}
	case domain.RunStatusCancelRequested, domain.RunStatusCancelled,
		domain.RunStatusSucceeded, domain.RunStatusSelected, domain.RunStatusSuperseded,
		domain.RunStatusFailed, domain.RunStatusTimedOut:
		// Naturally idempotent: immutable completed state is returned unchanged.
	default:
		return errors.New("stored Run status is invalid")
	}
	response := runcontrol.CancelResponse{RunID: command.RunID.String(), Status: status.String(),
		CancellationRequested: status == domain.RunStatusCancelRequested || status == domain.RunStatusCancelled,
		Terminal:              status.Terminal()}
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	durable := idempotency.Response{StatusCode: 202, Body: body, Location: "/v1/runs/" + command.RunID.String(),
		ResourceType: "run", ResourceID: command.RunID.String()}
	if err := claims.CompleteSucceeded(ctx, command.Scope, command.Fingerprint, durable, command.Now); err != nil {
		return err
	}
	result.Response = &durable
	return nil
}

func lockCancelRun(ctx context.Context, tx DBTX, tenantID domain.TenantID, runID domain.RunID) (lockedCancelRun, error) {
	var sessionID, turnID, status string
	err := tx.QueryRowContext(ctx, lockCancelRunSQL, tenantID.String(), runID.String()).Scan(&sessionID, &turnID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return lockedCancelRun{}, runcontrol.ErrNotFound
	}
	if err != nil {
		return lockedCancelRun{}, fmt.Errorf("lock Run cancellation target: %w", err)
	}
	parsedSession, err := domain.ParseSessionID(sessionID)
	if err != nil {
		return lockedCancelRun{}, err
	}
	parsedTurn, err := domain.ParseTurnID(turnID)
	if err != nil {
		return lockedCancelRun{}, err
	}
	return lockedCancelRun{SessionID: parsedSession, TurnID: parsedTurn, Status: domain.RunStatus(status)}, nil
}

func transitionRunningCancellation(ctx context.Context, tx DBTX, command runcontrol.CancelPersistence, locked lockedCancelRun) error {
	if err := execExactlyOne(ctx, tx, requestRunningCancelSQL, command.Now, command.TenantID.String(), command.RunID.String()); err != nil {
		return err
	}
	return execExactlyOne(ctx, tx, markSessionCancelSQL, command.Now, command.TenantID.String(), locked.SessionID.String(), command.RunID.String())
}

func transitionQueuedCancellation(ctx context.Context, tx DBTX, command runcontrol.CancelPersistence, locked lockedCancelRun) error {
	if err := execExactlyOne(ctx, tx, cancelQueuedRunSQL, command.Now, command.TenantID.String(), command.RunID.String()); err != nil {
		return err
	}
	if err := execExactlyOne(ctx, tx, releaseCancelledSessionSQL, command.Now, command.TenantID.String(), locked.SessionID.String(), command.RunID.String()); err != nil {
		return err
	}
	return execExactlyOne(ctx, tx, cancelTurnSQL, command.Now, command.TenantID.String(), locked.TurnID.String())
}

func appendCancellationEvent(ctx context.Context, tx DBTX, command runcontrol.CancelPersistence, locked lockedCancelRun, eventType events.Type, outboxID string) error {
	repository, err := events.NewRepository(tx)
	if err != nil {
		return err
	}
	_, err = repository.AppendWithOutbox(ctx, events.AppendCommand{TenantID: command.TenantID, SessionID: locked.SessionID,
		TurnID: locked.TurnID, RunID: command.RunID, Type: eventType, Payload: events.EmptyPayload{},
		OccurredAt: command.Now, Retention: command.Retention, OutboxID: outboxID})
	return err
}

func execExactlyOne(ctx context.Context, tx DBTX, query string, arguments ...any) error {
	result, err := tx.ExecContext(ctx, query, arguments...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("run cancellation control invariant failed")
	}
	return nil
}

const lockCancelRunSQL = `SELECT session_id,turn_id,status FROM runs WHERE tenant_id=$1 AND id=$2 FOR UPDATE`
const requestRunningCancelSQL = `UPDATE runs SET status='CANCEL_REQUESTED',cancel_requested_at=$1,
    execution_version=execution_version+1,updated_at=GREATEST(updated_at,$1)
WHERE tenant_id=$2 AND id=$3 AND status='RUNNING' AND execution_version < 9223372036854775807`
const cancelQueuedRunSQL = `UPDATE runs SET status='CANCELLED',cancel_requested_at=$1,cancelled_at=$1,
    execution_version=execution_version+1,updated_at=GREATEST(updated_at,$1)
WHERE tenant_id=$2 AND id=$3 AND status='QUEUED' AND execution_version < 9223372036854775807`
const markSessionCancelSQL = `UPDATE sessions SET row_version=row_version+1,updated_at=GREATEST(updated_at,$1)
WHERE tenant_id=$2 AND id=$3 AND active_run_id=$4 AND row_version < 9223372036854775807`
const releaseCancelledSessionSQL = `UPDATE sessions SET active_run_id=NULL,row_version=row_version+1,updated_at=GREATEST(updated_at,$1)
WHERE tenant_id=$2 AND id=$3 AND active_run_id=$4 AND row_version < 9223372036854775807`
const cancelTurnSQL = `UPDATE turns SET status='CANCELLED',updated_at=GREATEST(updated_at,$1)
WHERE tenant_id=$2 AND id=$3 AND status='RUNNING'`

type lockedSafePointRun struct {
	lockedCancelRun
	ExecutionVersion uint64
	LeaseOwner       sql.NullString
	LeaseToken       uint64
	LeaseExpiresAt   sql.NullTime
}

func (s *RunCancelStore) SafePoint(ctx context.Context, command runcontrol.SafePointPersistence) (runcontrol.SafePointResult, error) {
	var result runcontrol.SafePointResult
	err := s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		locked, err := lockSafePointRun(ctx, tx, command.TenantID, command.RunID)
		if err != nil {
			return err
		}
		if !locked.LeaseOwner.Valid || locked.LeaseOwner.String != command.LeaseOwner || locked.LeaseToken != command.LeaseToken ||
			!locked.LeaseExpiresAt.Valid || !locked.LeaseExpiresAt.Time.After(command.Now) {
			return runcontrol.ErrFenced
		}
		status := locked.Status
		version := locked.ExecutionVersion
		switch status {
		case domain.RunStatusCancelRequested:
			if err := acknowledgeCancellation(ctx, tx, command, locked); err != nil {
				return err
			}
			status = domain.RunStatusCancelled
			version++
			cancelCommand := runcontrol.CancelPersistence{TenantID: command.TenantID, RunID: command.RunID,
				EventID: command.EventID, Now: command.Now, Retention: command.Retention}
			if err := appendCancellationEvent(ctx, tx, cancelCommand, locked.lockedCancelRun, events.RunCancelled, command.EventID); err != nil {
				return err
			}
		case domain.RunStatusQueued, domain.RunStatusRunning, domain.RunStatusSucceeded, domain.RunStatusSelected,
			domain.RunStatusSuperseded, domain.RunStatusFailed, domain.RunStatusCancelled, domain.RunStatusTimedOut:
		default:
			return errors.New("stored run status is invalid")
		}
		result = runcontrol.SafePointResult{RunID: command.RunID.String(), Status: status.String(),
			Stop: status != domain.RunStatusRunning, ExecutionVersion: version}
		return nil
	})
	return result, err
}

func lockSafePointRun(ctx context.Context, tx DBTX, tenantID domain.TenantID, runID domain.RunID) (lockedSafePointRun, error) {
	var result lockedSafePointRun
	var sessionID, turnID, status string
	err := tx.QueryRowContext(ctx, lockSafePointRunSQL, tenantID.String(), runID.String()).Scan(&sessionID, &turnID, &status,
		&result.ExecutionVersion, &result.LeaseOwner, &result.LeaseToken, &result.LeaseExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return lockedSafePointRun{}, runcontrol.ErrNotFound
	}
	if err != nil {
		return lockedSafePointRun{}, fmt.Errorf("lock Run safe point: %w", err)
	}
	var parseErr error
	if result.SessionID, parseErr = domain.ParseSessionID(sessionID); parseErr != nil {
		return lockedSafePointRun{}, parseErr
	}
	if result.TurnID, parseErr = domain.ParseTurnID(turnID); parseErr != nil {
		return lockedSafePointRun{}, parseErr
	}
	result.Status = domain.RunStatus(status)
	return result, nil
}

func acknowledgeCancellation(ctx context.Context, tx DBTX, command runcontrol.SafePointPersistence, locked lockedSafePointRun) error {
	if err := execExactlyOne(ctx, tx, acknowledgeCancellationSQL, command.Now, command.TenantID.String(), command.RunID.String(),
		command.LeaseOwner, command.LeaseToken, locked.ExecutionVersion); err != nil {
		return runcontrol.ErrFenced
	}
	if err := execExactlyOne(ctx, tx, releaseCancelledSessionSQL, command.Now, command.TenantID.String(), locked.SessionID.String(), command.RunID.String()); err != nil {
		return err
	}
	return execExactlyOne(ctx, tx, cancelTurnSQL, command.Now, command.TenantID.String(), locked.TurnID.String())
}

const lockSafePointRunSQL = `SELECT session_id,turn_id,status,execution_version,lease_owner,lease_token,lease_expires_at
FROM runs WHERE tenant_id=$1 AND id=$2 FOR UPDATE`
const acknowledgeCancellationSQL = `UPDATE runs SET status='CANCELLED',cancelled_at=$1,lease_owner=NULL,lease_expires_at=NULL,
    execution_version=execution_version+1,updated_at=GREATEST(updated_at,$1)
WHERE tenant_id=$2 AND id=$3 AND status='CANCEL_REQUESTED' AND lease_owner=$4 AND lease_token=$5
    AND execution_version=$6 AND lease_expires_at>$1 AND execution_version < 9223372036854775807`
