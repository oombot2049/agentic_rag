package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/contextmgr"
	"github.com/agenticrag/agenticrag/internal/domain"
)

type ContextSnapshotStore struct{ withinTx WithinTxFunc }

func NewContextSnapshotStore(withinTx WithinTxFunc) (*ContextSnapshotStore, error) {
	if withinTx == nil {
		return nil, fmt.Errorf("%w: transaction runner is required", ErrInvalidArgument)
	}
	return &ContextSnapshotStore{withinTx: withinTx}, nil
}

func (s *ContextSnapshotStore) Load(ctx context.Context, query contextmgr.Query) (contextmgr.Conversation, error) {
	var result contextmgr.Conversation
	err := s.withinTx(ctx, readSnapshotOptions(), func(ctx context.Context, tx DBTX) error {
		if err := loadContextHead(ctx, tx, query, &result); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, contextHistorySQL, query.TenantID.String(), query.SessionID.String(), query.TurnID.String())
		if err != nil {
			return fmt.Errorf("list completed context history: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			message, err := scanContextMessage(rows)
			if err != nil {
				return err
			}
			result.History = append(result.History, message)
		}
		return rows.Err()
	})
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return contextmgr.Conversation{}, contextmgr.ErrNotFound
	}
	return result, err
}

func loadContextHead(ctx context.Context, tx DBTX, query contextmgr.Query, result *contextmgr.Conversation) error {
	var activeRun sql.NullString
	var sessionTenant, sessionID, turnID, runID, messageID, messageTurn, role, content string
	var turnSequence, runBase uint64
	err := tx.QueryRowContext(ctx, contextHeadSQL, query.TenantID.String(), query.SessionID.String(), query.TurnID.String(), query.RunID.String()).Scan(
		&sessionTenant, &sessionID, &result.ContextVersion, &activeRun, &turnID, &turnSequence, &runID, &runBase,
		&messageID, &messageTurn, &role, &content)
	if err != nil {
		return err
	}
	parsedTenant, err := domain.ParseTenantID(sessionTenant)
	if err != nil || parsedTenant != query.TenantID || sessionID != query.SessionID.String() || turnID != query.TurnID.String() || runID != query.RunID.String() || runBase != query.BaseContextVersion {
		return contextmgr.ErrCorruptSource
	}
	if !activeRun.Valid {
		return contextmgr.ErrFenced
	}
	result.ActiveRunID, err = domain.ParseRunID(activeRun.String)
	if err != nil {
		return contextmgr.ErrCorruptSource
	}
	result.Current, err = parseContextMessage(messageID, messageTurn, "", role, content, turnSequence)
	return err
}

type contextRowScanner interface{ Scan(...any) error }

func scanContextMessage(row contextRowScanner) (contextmgr.SourceMessage, error) {
	var messageID, turnID, runID, role, content string
	var sequence uint64
	if err := row.Scan(&messageID, &turnID, &runID, &role, &content, &sequence); err != nil {
		return contextmgr.SourceMessage{}, err
	}
	return parseContextMessage(messageID, turnID, runID, role, content, sequence)
}

func parseContextMessage(messageID, turnID, runID, role, content string, sequence uint64) (contextmgr.SourceMessage, error) {
	parsedMessage, err := domain.ParseMessageID(messageID)
	if err != nil {
		return contextmgr.SourceMessage{}, contextmgr.ErrCorruptSource
	}
	parsedTurn, err := domain.ParseTurnID(turnID)
	if err != nil {
		return contextmgr.SourceMessage{}, contextmgr.ErrCorruptSource
	}
	var parsedRun domain.RunID
	if runID != "" {
		parsedRun, err = domain.ParseRunID(runID)
		if err != nil {
			return contextmgr.SourceMessage{}, contextmgr.ErrCorruptSource
		}
	}
	return contextmgr.SourceMessage{MessageID: parsedMessage, TurnID: parsedTurn, RunID: parsedRun,
		TurnSequence: sequence, Role: domain.MessageRole(role), Content: content}, nil
}

const contextHeadSQL = `SELECT s.tenant_id,s.id,s.context_version,s.active_run_id,
    t.id,t.turn_sequence,r.id,r.base_context_version,m.id,m.turn_id,m.role,m.content
FROM sessions s
JOIN turns t ON t.tenant_id=s.tenant_id AND t.session_id=s.id AND t.id=$3 AND t.status='RUNNING'
JOIN runs r ON r.tenant_id=t.tenant_id AND r.session_id=t.session_id AND r.turn_id=t.id AND r.id=$4
JOIN messages m ON m.tenant_id=t.tenant_id AND m.session_id=t.session_id AND m.turn_id=t.id
    AND m.id=t.user_message_id AND m.role='USER' AND m.status='COMPLETED' AND m.visibility='CONVERSATION'
WHERE s.tenant_id=$1 AND s.id=$2`

const contextHistorySQL = `SELECT m.id,m.turn_id,COALESCE(m.run_id,''),m.role,m.content,t.turn_sequence
FROM turns t
JOIN runs selected ON selected.tenant_id=t.tenant_id AND selected.session_id=t.session_id
    AND selected.turn_id=t.id AND selected.id=t.selected_run_id
JOIN messages m ON m.tenant_id=t.tenant_id AND m.session_id=t.session_id AND m.turn_id=t.id
    AND (m.id=t.user_message_id OR m.id=selected.output_message_id)
WHERE t.tenant_id=$1 AND t.session_id=$2 AND t.id<>$3 AND t.status='COMPLETED'
    AND m.status='COMPLETED' AND m.visibility='CONVERSATION'
    AND ((m.role='USER' AND m.run_id IS NULL) OR (m.role='ASSISTANT' AND m.run_id=t.selected_run_id))
ORDER BY t.turn_sequence,CASE m.role WHEN 'USER' THEN 0 ELSE 1 END,m.id`

var _ contextmgr.Store = (*ContextSnapshotStore)(nil)
