package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	"github.com/agenticrag/agenticrag/internal/session"
)

// SessionAPIStore implements the Session application port with one PostgreSQL
// transaction for idempotency claim, aggregate insert and response completion.
type WithinTxFunc func(context.Context, *sql.TxOptions, func(context.Context, DBTX) error) error

type SessionAPIStore struct {
	db       DBTX
	withinTx WithinTxFunc
}

func NewSessionAPIStore(db DBTX, withinTx WithinTxFunc) (*SessionAPIStore, error) {
	if db == nil || withinTx == nil {
		return nil, fmt.Errorf("%w: database and transaction runner are required", ErrInvalidArgument)
	}
	return &SessionAPIStore{db: db, withinTx: withinTx}, nil
}

func (s *SessionAPIStore) Create(ctx context.Context, command session.CreatePersistence) (session.CreateResult, error) {
	var result session.CreateResult
	err := s.withinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		claims, err := idempotency.NewRepository(tx)
		if err != nil {
			return err
		}
		decision, err := claims.Claim(ctx, command.Scope, command.Fingerprint, command.Now, command.Retention)
		if err != nil {
			return err
		}
		result.Decision = decision.Decision
		switch decision.Decision {
		case idempotency.DecisionAcquired:
			conversation, err := NewConversation(tx)
			if err != nil {
				return err
			}
			if err := conversation.InsertSessionWithMetadata(ctx, command.Session, command.Metadata); err != nil {
				return err
			}
			if err := claims.CompleteSucceeded(ctx, command.Scope, command.Fingerprint, command.Response, command.Now); err != nil {
				return err
			}
			result.Session = command.Session
		case idempotency.DecisionReplaySucceeded:
			if decision.Record.Response == nil || decision.Record.Response.ResourceType != "session" {
				return errors.New("invalid stored Session idempotency response")
			}
			_, err := domain.ParseSessionID(decision.Record.Response.ResourceID)
			if err != nil {
				return fmt.Errorf("invalid stored Session resource ID: %w", err)
			}
			response := *decision.Record.Response
			response.Body = append([]byte(nil), response.Body...)
			result.Response = &response
		}
		return nil
	})
	return result, err
}

func (s *SessionAPIStore) Get(ctx context.Context, tenantID domain.TenantID, sessionID domain.SessionID) (*domain.Session, error) {
	conversation, err := NewConversation(s.db)
	if err != nil {
		return nil, err
	}
	value, err := conversation.GetSession(ctx, tenantID, sessionID)
	if errors.Is(err, ErrNotFound) {
		return nil, session.ErrNotFound
	}
	return value, err
}
