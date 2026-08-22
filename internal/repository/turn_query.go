package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/turn"
)

type TurnQueryStore struct{ withinTx WithinTxFunc }

func NewTurnQueryStore(withinTx WithinTxFunc) (*TurnQueryStore, error) {
	if withinTx == nil {
		return nil, fmt.Errorf("%w: transaction runner is required", ErrInvalidArgument)
	}
	return &TurnQueryStore{withinTx: withinTx}, nil
}

func (s *TurnQueryStore) GetTurn(ctx context.Context, tenantID domain.TenantID, turnID domain.TurnID) (*domain.Turn, error) {
	var value *domain.Turn
	err := s.withinTx(ctx, readSnapshotOptions(), func(ctx context.Context, tx DBTX) error {
		conversation, err := NewConversation(tx)
		if err != nil {
			return err
		}
		value, err = conversation.GetTurn(ctx, tenantID, turnID)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil, turn.ErrQueryNotFound
	}
	return value, err
}

func (s *TurnQueryStore) GetRun(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (*domain.Run, error) {
	var value *domain.Run
	err := s.withinTx(ctx, readSnapshotOptions(), func(ctx context.Context, tx DBTX) error {
		execution, err := NewExecution(tx)
		if err != nil {
			return err
		}
		record, err := execution.GetRun(ctx, tenantID, runID)
		if err == nil {
			value = record.Run
		}
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return nil, turn.ErrQueryNotFound
	}
	return value, err
}

func (s *TurnQueryStore) ListMessages(
	ctx context.Context,
	tenantID domain.TenantID,
	sessionID domain.SessionID,
	after *turn.MessageCursor,
	limit int,
) (turn.MessageResultPage, error) {
	var result turn.MessageResultPage
	err := s.withinTx(ctx, readSnapshotOptions(), func(ctx context.Context, tx DBTX) error {
		conversation, err := NewConversation(tx)
		if err != nil {
			return err
		}
		if _, err := conversation.GetSession(ctx, tenantID, sessionID); err != nil {
			return err
		}
		var repositoryCursor *MessageCursor
		if after != nil {
			repositoryCursor = &MessageCursor{CreatedAt: after.CreatedAt, ID: after.ID}
		}
		page, err := conversation.ListConversationMessages(ctx, tenantID, sessionID, repositoryCursor, limit)
		if err != nil {
			return err
		}
		result.Items = page.Items
		if page.NextCursor != nil {
			result.NextCursor = &turn.MessageCursor{CreatedAt: page.NextCursor.CreatedAt, ID: page.NextCursor.ID}
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return turn.MessageResultPage{}, turn.ErrQueryNotFound
	}
	return result, err
}

func readSnapshotOptions() *sql.TxOptions {
	return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
}
