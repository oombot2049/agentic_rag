package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/events"
	"github.com/agenticrag/agenticrag/internal/turn"
)

// FinalCommitStore is the PostgreSQL authority for publishing an answer.
// Locking the Session makes the replay check and every participant mutation a
// single serialization point for one linear conversation.
type FinalCommitStore struct{ withinTx WithinTxFunc }

func NewFinalCommitStore(withinTx WithinTxFunc) (*FinalCommitStore, error) {
	if withinTx == nil {
		return nil, fmt.Errorf("%w: transaction runner is required", ErrInvalidArgument)
	}
	return &FinalCommitStore{withinTx: withinTx}, nil
}

type finalSnapshot struct {
	contextVersion, rowVersion, committedSequence, turnSequence, turnBase, runBase uint64
	activeRunID, headTurnID, turnStatus, selectedRunID, runStatus, outputMessageID string
	leaseOwner                                                                     string
	leaseToken, executionVersion                                                   uint64
	leaseExpires                                                                   sql.NullTime
}

func (s *FinalCommitStore) CommitFinal(ctx context.Context, command turn.FinalRequest) (turn.FinalResult, error) {
	var result turn.FinalResult
	err := s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		state, err := lockFinalSnapshot(ctx, tx, command)
		if err != nil {
			return err
		}
		if state.turnStatus == "COMPLETED" {
			return replayFinal(ctx, tx, command, state, &result)
		}
		if err := authorizeFinal(command, state); err != nil {
			return err
		}

		// Regeneration replaces the selected answer without leaving two visible
		// assistant answers for the same Turn.
		if state.selectedRunID != "" && state.selectedRunID != command.RunID.String() {
			if err := affectOne(ctx, tx, supersedePreviousRunSQL, command.TenantID.String(), state.selectedRunID, command.SessionID.String(), command.TurnID.String(), command.At); err != nil {
				return err
			}
			if err := affectOne(ctx, tx, demotePreviousMessageSQL, command.TenantID.String(), state.selectedRunID, command.SessionID.String(), command.TurnID.String()); err != nil {
				return err
			}
		}
		if err := affectOne(ctx, tx, insertFinalMessageSQL, command.TenantID.String(), command.MessageID.String(), command.SessionID.String(), command.TurnID.String(), command.RunID.String(), command.Content, command.At); err != nil {
			return err
		}
		if err := affectOne(ctx, tx, selectFinalRunSQL, command.TenantID.String(), command.RunID.String(), command.SessionID.String(), command.TurnID.String(), command.MessageID.String(), command.At,
			command.Authority.Owner, command.Authority.Token, command.Authority.ExecutionVersion); err != nil {
			return turn.ErrFinalFenced
		}
		if err := affectOne(ctx, tx, completeFinalTurnSQL, command.TenantID.String(), command.TurnID.String(), command.SessionID.String(), command.RunID.String(), command.At); err != nil {
			return turn.ErrFinalConflict
		}
		if err := affectOne(ctx, tx, advanceFinalSessionSQL, command.TenantID.String(), command.SessionID.String(), command.RunID.String(), command.TurnID.String(), command.ExpectedContextVersion, command.ExpectedSessionRowVersion, state.turnSequence, command.At); err != nil {
			return turn.ErrFinalConflict
		}

		eventRepository, err := events.NewRepository(tx)
		if err != nil {
			return err
		}
		event, err := eventRepository.AppendWithOutbox(ctx, events.AppendCommand{TenantID: command.TenantID, SessionID: command.SessionID, TurnID: command.TurnID, RunID: command.RunID,
			Type: events.RunCompleted, Payload: events.RunCompletedPayload{MessageID: command.MessageID}, OccurredAt: command.At, Retention: command.EventRetention})
		if err != nil {
			return err
		}
		result = turn.FinalResult{MessageID: command.MessageID, ContextVersion: command.ExpectedContextVersion + 1,
			SessionRowVersion: command.ExpectedSessionRowVersion + 1, EventSequence: event.Sequence()}
		return nil
	})
	return result, err
}

func lockFinalSnapshot(ctx context.Context, tx DBTX, command turn.FinalRequest) (finalSnapshot, error) {
	var value finalSnapshot
	var active, head, selected, output, owner sql.NullString
	err := tx.QueryRowContext(ctx, lockFinalSnapshotSQL, command.TenantID.String(), command.SessionID.String(), command.TurnID.String(), command.RunID.String()).Scan(
		&value.contextVersion, &value.rowVersion, &value.committedSequence, &active, &head,
		&value.turnSequence, &value.turnBase, &value.turnStatus, &selected,
		&value.runBase, &value.runStatus, &output, &owner, &value.leaseToken, &value.executionVersion, &value.leaseExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return value, turn.ErrFinalNotFound
	}
	if err != nil {
		return value, fmt.Errorf("lock final commit snapshot: %w", err)
	}
	value.activeRunID, value.headTurnID, value.selectedRunID, value.outputMessageID, value.leaseOwner = nullable(active), nullable(head), nullable(selected), nullable(output), nullable(owner)
	return value, nil
}

func authorizeFinal(command turn.FinalRequest, state finalSnapshot) error {
	if state.contextVersion != command.ExpectedContextVersion || state.rowVersion != command.ExpectedSessionRowVersion ||
		state.activeRunID != command.RunID.String() || state.turnStatus != "RUNNING" || state.runBase != command.ExpectedContextVersion ||
		(state.runStatus != "QUEUED" && state.runStatus != "RUNNING" && state.runStatus != "SUCCEEDED") {
		return turn.ErrFinalConflict
	}
	newTurn := state.turnSequence > state.committedSequence && state.turnBase == command.ExpectedContextVersion
	regeneration := state.turnSequence == state.committedSequence && state.headTurnID == command.TurnID.String()
	if !newTurn && !regeneration {
		return turn.ErrFinalConflict
	}
	if state.leaseOwner != command.Authority.Owner || state.leaseToken != command.Authority.Token || state.executionVersion != command.Authority.ExecutionVersion ||
		!state.leaseExpires.Valid || !state.leaseExpires.Time.After(command.At) {
		return turn.ErrFinalFenced
	}
	return nil
}

func replayFinal(ctx context.Context, tx DBTX, command turn.FinalRequest, state finalSnapshot, result *turn.FinalResult) error {
	if state.selectedRunID != command.RunID.String() || state.runStatus != "SELECTED" || state.outputMessageID != command.MessageID.String() {
		return turn.ErrFinalConflict
	}
	var content, visibility string
	if err := tx.QueryRowContext(ctx, selectFinalMessageSQL, command.TenantID.String(), command.MessageID.String(), command.SessionID.String(), command.TurnID.String(), command.RunID.String()).Scan(&content, &visibility); errors.Is(err, sql.ErrNoRows) {
		return turn.ErrFinalConflict
	} else if err != nil {
		return err
	}
	if content != command.Content || visibility != "CONVERSATION" {
		return turn.ErrFinalConflict
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, selectCompletedEventSequenceSQL, command.TenantID.String(), command.RunID.String(), command.MessageID.String()).Scan(&sequence); err != nil {
		return err
	}
	*result = turn.FinalResult{MessageID: command.MessageID, ContextVersion: state.contextVersion, SessionRowVersion: state.rowVersion, EventSequence: sequence, Replayed: true}
	return nil
}

func affectOne(ctx context.Context, tx DBTX, statement string, arguments ...any) error {
	result, err := tx.ExecContext(ctx, statement, arguments...)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return turn.ErrFinalConflict
	}
	return nil
}
func nullable(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

const lockFinalSnapshotSQL = `SELECT s.context_version,s.row_version,s.committed_turn_sequence,s.active_run_id,s.head_turn_id,
 t.turn_sequence,t.base_context_version,t.status,t.selected_run_id,r.base_context_version,r.status,r.output_message_id,
 r.lease_owner,r.lease_token,r.execution_version,r.lease_expires_at
 FROM sessions s JOIN turns t ON t.tenant_id=s.tenant_id AND t.session_id=s.id AND t.id=$3
 JOIN runs r ON r.tenant_id=t.tenant_id AND r.session_id=t.session_id AND r.turn_id=t.id AND r.id=$4
 WHERE s.tenant_id=$1 AND s.id=$2 FOR UPDATE OF s`
const insertFinalMessageSQL = `INSERT INTO messages(tenant_id,id,session_id,turn_id,run_id,role,status,visibility,content,created_at)
 VALUES($1,$2,$3,$4,$5,'ASSISTANT','COMPLETED','CONVERSATION',$6,$7)`
const selectFinalRunSQL = `UPDATE runs SET status='SELECTED',output_message_id=$5,updated_at=$6
 WHERE tenant_id=$1 AND id=$2 AND session_id=$3 AND turn_id=$4 AND status IN ('QUEUED','RUNNING','SUCCEEDED')
 AND lease_owner=$7 AND lease_token=$8 AND execution_version=$9 AND lease_expires_at>$6`
const completeFinalTurnSQL = `UPDATE turns SET status='COMPLETED',selected_run_id=$4,updated_at=$5
 WHERE tenant_id=$1 AND id=$2 AND session_id=$3 AND status='RUNNING'`
const advanceFinalSessionSQL = `UPDATE sessions SET context_version=context_version+1,row_version=row_version+1,
 committed_turn_sequence=$7,head_turn_id=$4,active_run_id=NULL,updated_at=$8
 WHERE tenant_id=$1 AND id=$2 AND active_run_id=$3 AND context_version=$5 AND row_version=$6`
const supersedePreviousRunSQL = `UPDATE runs SET status='SUPERSEDED',updated_at=$5 WHERE tenant_id=$1 AND id=$2 AND session_id=$3 AND turn_id=$4 AND status='SELECTED'`
const demotePreviousMessageSQL = `UPDATE messages SET visibility='CANDIDATE' WHERE tenant_id=$1 AND run_id=$2 AND session_id=$3 AND turn_id=$4 AND role='ASSISTANT' AND visibility='CONVERSATION'`
const selectFinalMessageSQL = `SELECT content,visibility FROM messages WHERE tenant_id=$1 AND id=$2 AND session_id=$3 AND turn_id=$4 AND run_id=$5 AND role='ASSISTANT'`
const selectCompletedEventSequenceSQL = `SELECT sequence FROM run_events WHERE tenant_id=$1 AND run_id=$2 AND event_type='run.completed' AND event_data->'payload'->>'message_id'=$3 ORDER BY sequence DESC LIMIT 1`

var _ turn.FinalStore = (*FinalCommitStore)(nil)
