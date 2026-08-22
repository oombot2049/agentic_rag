// Package repository contains PostgreSQL persistence adapters for domain aggregates.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	sessioncontrol "github.com/agenticrag/agenticrag/internal/session"
)

var (
	ErrNotFound        = errors.New("repository resource not found")
	ErrInvalidArgument = errors.New("repository invalid argument")
)

type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Conversation struct{ db DBTX }

func NewConversation(db DBTX) (*Conversation, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: database executor is nil", ErrInvalidArgument)
	}
	return &Conversation{db: db}, nil
}

func (r *Conversation) InsertSession(ctx context.Context, value *domain.Session) error {
	if value == nil {
		return fmt.Errorf("%w: session is nil", ErrInvalidArgument)
	}
	activeRunID, active := value.ActiveRunID()
	headTurnID, head := value.HeadTurnID()
	_, err := r.db.ExecContext(ctx, insertSessionSQL,
		value.TenantID().String(), value.ID().String(), value.ContextVersion(), value.RowVersion(),
		value.NextTurnSequence(), value.CommittedTurnSequence(), optionalString(activeRunID.String(), active),
		optionalString(headTurnID.String(), head), value.CreatedAt(), value.UpdatedAt(),
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

func (r *Conversation) GetSession(ctx context.Context, tenantID domain.TenantID, sessionID domain.SessionID) (*domain.Session, error) {
	control, err := sessioncontrol.NewRepository(r.db)
	if err != nil {
		return nil, err
	}
	value, err := control.Load(ctx, tenantID, sessionID)
	if errors.Is(err, sessioncontrol.ErrNotFound) {
		return nil, ErrNotFound
	}
	return value, err
}

func (r *Conversation) InsertTurn(ctx context.Context, value *domain.Turn) error {
	if value == nil {
		return fmt.Errorf("%w: turn is nil", ErrInvalidArgument)
	}
	selectedRunID, selected := value.SelectedRunID()
	_, err := r.db.ExecContext(ctx, insertTurnSQL,
		value.TenantID().String(), value.ID().String(), value.SessionID().String(), value.Sequence(),
		value.BaseContextVersion(), value.UserMessageID().String(), value.Status().String(),
		optionalString(selectedRunID.String(), selected), value.CreatedAt(), value.UpdatedAt(),
	)
	if err != nil {
		return fmt.Errorf("insert turn: %w", err)
	}
	return nil
}

func (r *Conversation) GetTurn(ctx context.Context, tenantID domain.TenantID, turnID domain.TurnID) (*domain.Turn, error) {
	if err := validateTenantAndTurn(tenantID, turnID); err != nil {
		return nil, err
	}
	var snapshot domain.TurnSnapshot
	var rawTenant, rawID, sessionID, userMessageID, status string
	var selectedRunID sql.NullString
	err := r.db.QueryRowContext(ctx, selectTurnSQL, tenantID.String(), turnID.String()).Scan(
		&rawTenant, &rawID, &sessionID, &snapshot.Sequence, &snapshot.BaseContextVersion,
		&userMessageID, &status, &selectedRunID, &snapshot.CreatedAt, &snapshot.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select turn: %w", err)
	}
	if err := parseTurnSnapshot(&snapshot, rawTenant, rawID, sessionID, userMessageID, status, selectedRunID); err != nil {
		return nil, err
	}
	runIDs, err := r.loadTurnRunIDs(ctx, tenantID, turnID)
	if err != nil {
		return nil, err
	}
	snapshot.RunIDs = runIDs
	value, err := domain.RestoreTurn(snapshot)
	if err != nil {
		return nil, fmt.Errorf("hydrate turn: %w", err)
	}
	return value, nil
}

func (r *Conversation) InsertMessage(ctx context.Context, value *domain.Message) error {
	if value == nil {
		return fmt.Errorf("%w: message is nil", ErrInvalidArgument)
	}
	runID, hasRun := value.RunID()
	_, err := r.db.ExecContext(ctx, insertMessageSQL,
		value.TenantID().String(), value.ID().String(), value.SessionID().String(), value.TurnID().String(),
		optionalString(runID.String(), hasRun), string(value.Role()), "COMPLETED", value.Visibility().String(),
		value.Content(), value.CreatedAt(),
	)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

func (r *Conversation) GetMessage(ctx context.Context, tenantID domain.TenantID, messageID domain.MessageID) (*domain.Message, error) {
	if err := validateTenantAndMessage(tenantID, messageID); err != nil {
		return nil, err
	}
	value, err := scanMessage(r.db.QueryRowContext(ctx, selectMessageSQL, tenantID.String(), messageID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select message: %w", err)
	}
	return value, nil
}

type MessageCursor struct {
	CreatedAt time.Time
	ID        domain.MessageID
}

type MessagePage struct {
	Items      []*domain.Message
	NextCursor *MessageCursor
}

func (r *Conversation) ListConversationMessages(ctx context.Context, tenantID domain.TenantID, sessionID domain.SessionID, after *MessageCursor, limit int) (MessagePage, error) {
	if err := validateTenantAndSession(tenantID, sessionID); err != nil {
		return MessagePage{}, err
	}
	if limit < 1 || limit > 100 {
		return MessagePage{}, fmt.Errorf("%w: limit must be between 1 and 100", ErrInvalidArgument)
	}
	var afterTime, afterID any
	if after != nil {
		if after.CreatedAt.IsZero() {
			return MessagePage{}, fmt.Errorf("%w: cursor time is zero", ErrInvalidArgument)
		}
		if _, err := domain.ParseMessageID(after.ID.String()); err != nil {
			return MessagePage{}, err
		}
		afterTime, afterID = after.CreatedAt.UTC(), after.ID.String()
	}
	rows, err := r.db.QueryContext(ctx, listMessagesSQL, tenantID.String(), sessionID.String(), afterTime, afterID, limit+1)
	if err != nil {
		return MessagePage{}, fmt.Errorf("list conversation messages: %w", err)
	}
	defer rows.Close()
	items := make([]*domain.Message, 0, limit+1)
	for rows.Next() {
		value, err := scanMessage(rows)
		if err != nil {
			return MessagePage{}, fmt.Errorf("hydrate conversation message: %w", err)
		}
		items = append(items, value)
	}
	if err := rows.Err(); err != nil {
		return MessagePage{}, fmt.Errorf("iterate conversation messages: %w", err)
	}
	page := MessagePage{Items: items}
	if len(items) > limit {
		last := items[limit-1]
		page.Items = items[:limit]
		page.NextCursor = &MessageCursor{CreatedAt: last.CreatedAt(), ID: last.ID()}
	}
	return page, nil
}

func (r *Conversation) loadTurnRunIDs(ctx context.Context, tenantID domain.TenantID, turnID domain.TurnID) ([]domain.RunID, error) {
	rows, err := r.db.QueryContext(ctx, selectTurnRunsSQL, tenantID.String(), turnID.String())
	if err != nil {
		return nil, fmt.Errorf("select turn runs: %w", err)
	}
	defer rows.Close()
	var result []domain.RunID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, err := domain.ParseRunID(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

type rowScanner interface{ Scan(...any) error }

func scanMessage(row rowScanner) (*domain.Message, error) {
	var snapshot domain.MessageSnapshot
	var tenantID, messageID, sessionID, turnID, role, status, visibility string
	var runID sql.NullString
	err := row.Scan(&tenantID, &messageID, &sessionID, &turnID, &runID, &role, &status, &visibility, &snapshot.Content, &snapshot.CreatedAt)
	if err != nil {
		return nil, err
	}
	if status != "COMPLETED" {
		return nil, errors.New("stored message status is not COMPLETED")
	}
	var parseErr error
	if snapshot.TenantID, parseErr = domain.ParseTenantID(tenantID); parseErr != nil {
		return nil, parseErr
	}
	if snapshot.ID, parseErr = domain.ParseMessageID(messageID); parseErr != nil {
		return nil, parseErr
	}
	if snapshot.SessionID, parseErr = domain.ParseSessionID(sessionID); parseErr != nil {
		return nil, parseErr
	}
	if snapshot.TurnID, parseErr = domain.ParseTurnID(turnID); parseErr != nil {
		return nil, parseErr
	}
	if runID.Valid {
		if snapshot.RunID, parseErr = domain.ParseRunID(runID.String); parseErr != nil {
			return nil, parseErr
		}
	}
	snapshot.Role = domain.MessageRole(role)
	snapshot.Visibility = domain.MessageVisibility(visibility)
	return domain.RestoreMessage(snapshot)
}

func parseTurnSnapshot(snapshot *domain.TurnSnapshot, tenantID, turnID, sessionID, userMessageID, status string, selectedRunID sql.NullString) error {
	var err error
	if snapshot.TenantID, err = domain.ParseTenantID(tenantID); err != nil {
		return err
	}
	if snapshot.ID, err = domain.ParseTurnID(turnID); err != nil {
		return err
	}
	if snapshot.SessionID, err = domain.ParseSessionID(sessionID); err != nil {
		return err
	}
	if snapshot.UserMessageID, err = domain.ParseMessageID(userMessageID); err != nil {
		return err
	}
	if selectedRunID.Valid {
		if snapshot.SelectedRunID, err = domain.ParseRunID(selectedRunID.String); err != nil {
			return err
		}
	}
	snapshot.Status = domain.TurnStatus(status)
	return nil
}

func validateTenantAndSession(tenantID domain.TenantID, sessionID domain.SessionID) error {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return err
	}
	_, err := domain.ParseSessionID(sessionID.String())
	return err
}

func validateTenantAndTurn(tenantID domain.TenantID, turnID domain.TurnID) error {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return err
	}
	_, err := domain.ParseTurnID(turnID.String())
	return err
}

func validateTenantAndMessage(tenantID domain.TenantID, messageID domain.MessageID) error {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return err
	}
	_, err := domain.ParseMessageID(messageID.String())
	return err
}

func optionalString(value string, present bool) any {
	if !present {
		return nil
	}
	return value
}

const insertSessionSQL = `INSERT INTO sessions(
    tenant_id,id,context_version,row_version,next_turn_sequence,committed_turn_sequence,
    active_run_id,head_turn_id,created_at,updated_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

const insertTurnSQL = `INSERT INTO turns(
    tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,
    selected_run_id,created_at,updated_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

const selectTurnSQL = `SELECT tenant_id,id,session_id,turn_sequence,base_context_version,
    user_message_id,status,selected_run_id,created_at,updated_at
FROM turns WHERE tenant_id=$1 AND id=$2`

const selectTurnRunsSQL = `SELECT id FROM runs WHERE tenant_id=$1 AND turn_id=$2 ORDER BY run_no`

const insertMessageSQL = `INSERT INTO messages(
    tenant_id,id,session_id,turn_id,run_id,role,status,visibility,content,created_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

const selectMessageSQL = `SELECT tenant_id,id,session_id,turn_id,run_id,role,status,visibility,content,created_at
FROM messages WHERE tenant_id=$1 AND id=$2`

const listMessagesSQL = `SELECT tenant_id,id,session_id,turn_id,run_id,role,status,visibility,content,created_at
FROM messages
WHERE tenant_id=$1 AND session_id=$2 AND visibility='CONVERSATION'
    AND ($3::timestamptz IS NULL OR (created_at,id) > ($3,$4))
ORDER BY created_at,id LIMIT $5`
