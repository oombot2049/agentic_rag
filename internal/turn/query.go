package turn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
)

var ErrQueryNotFound = errors.New("query resource not found")

type MessageCursor struct {
	CreatedAt time.Time
	ID        domain.MessageID
}

type MessageResultPage struct {
	Items      []*domain.Message
	NextCursor *MessageCursor
}

type QueryStore interface {
	GetTurn(context.Context, domain.TenantID, domain.TurnID) (*domain.Turn, error)
	GetRun(context.Context, domain.TenantID, domain.RunID) (*domain.Run, error)
	ListMessages(context.Context, domain.TenantID, domain.SessionID, *MessageCursor, int) (MessageResultPage, error)
}

type QueryService struct {
	store  QueryStore
	cursor *CursorCodec
}

func NewQueryService(store QueryStore, cursor *CursorCodec) (*QueryService, error) {
	if store == nil || cursor == nil {
		return nil, errors.New("query store and cursor codec are required")
	}
	return &QueryService{store: store, cursor: cursor}, nil
}

type TurnView struct {
	ID                 string   `json:"id"`
	SessionID          string   `json:"session_id"`
	TurnSequence       uint64   `json:"turn_sequence"`
	BaseContextVersion uint64   `json:"base_context_version"`
	UserMessageID      string   `json:"user_message_id"`
	SelectedRunID      *string  `json:"selected_run_id"`
	Status             string   `json:"status"`
	RunIDs             []string `json:"run_ids"`
	CreatedAt          string   `json:"created_at"`
	CompletedAt        *string  `json:"completed_at"`
}

type RunView struct {
	ID                 string  `json:"id"`
	SessionID          string  `json:"session_id"`
	TurnID             string  `json:"turn_id"`
	RunNo              uint64  `json:"run_no"`
	BaseContextVersion uint64  `json:"base_context_version"`
	Status             string  `json:"status"`
	Terminal           bool    `json:"terminal"`
	ReasonCode         *string `json:"reason_code"`
	FinalMessageID     *string `json:"final_message_id"`
	CreatedAt          string  `json:"created_at"`
	StartedAt          *string `json:"started_at"`
	CompletedAt        *string `json:"completed_at"`
}

type MessageView struct {
	ID        string  `json:"id"`
	SessionID string  `json:"session_id"`
	TurnID    string  `json:"turn_id"`
	RunID     *string `json:"run_id"`
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	CreatedAt string  `json:"created_at"`
}

type MessagePageView struct {
	Items      []MessageView `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

func (s *QueryService) GetTurn(ctx context.Context, tenantID domain.TenantID, rawID string) (TurnView, error) {
	id, err := domain.ParseTurnID(rawID)
	if err != nil {
		return TurnView{}, invalid(err)
	}
	value, err := s.store.GetTurn(ctx, tenantID, id)
	if errors.Is(err, ErrQueryNotFound) {
		return TurnView{}, apperror.New(apperror.CodeTurnNotFound, false, nil)
	}
	if err != nil || value == nil {
		return TurnView{}, queryInternal("get Turn", err)
	}
	return turnView(value), nil
}

func (s *QueryService) GetRun(ctx context.Context, tenantID domain.TenantID, rawID string) (RunView, error) {
	id, err := domain.ParseRunID(rawID)
	if err != nil {
		return RunView{}, invalid(err)
	}
	value, err := s.store.GetRun(ctx, tenantID, id)
	if errors.Is(err, ErrQueryNotFound) {
		return RunView{}, apperror.New(apperror.CodeRunNotFound, false, nil)
	}
	if err != nil || value == nil {
		return RunView{}, queryInternal("get Run", err)
	}
	return runView(value), nil
}

func (s *QueryService) ListMessages(ctx context.Context, tenantID domain.TenantID, rawSessionID, token string, pageSize int) (MessagePageView, error) {
	sessionID, err := domain.ParseSessionID(rawSessionID)
	if err != nil {
		return MessagePageView{}, invalid(err)
	}
	if pageSize == 0 {
		pageSize = 50
	}
	if pageSize < 1 || pageSize > 100 {
		return MessagePageView{}, invalid(errors.New("page_size must be between 1 and 100"))
	}
	var after *MessageCursor
	if token != "" {
		decoded, err := s.cursor.Decode(tenantID, sessionID, token)
		if err != nil {
			return MessagePageView{}, invalid(err)
		}
		after = &decoded
	}
	page, err := s.store.ListMessages(ctx, tenantID, sessionID, after, pageSize)
	if errors.Is(err, ErrQueryNotFound) {
		return MessagePageView{}, apperror.New(apperror.CodeSessionNotFound, false, nil)
	}
	if err != nil {
		return MessagePageView{}, queryInternal("list Messages", err)
	}
	view := MessagePageView{Items: make([]MessageView, 0, len(page.Items))}
	for _, message := range page.Items {
		if message == nil || message.Visibility() != domain.MessageVisibilityConversation {
			return MessagePageView{}, queryInternal("project Message", errors.New("store returned a hidden or nil Message"))
		}
		view.Items = append(view.Items, messageView(message))
	}
	if page.NextCursor != nil {
		encoded, err := s.cursor.Encode(tenantID, sessionID, *page.NextCursor)
		if err != nil {
			return MessagePageView{}, queryInternal("encode cursor", err)
		}
		view.NextCursor = &encoded
	}
	return view, nil
}

func turnView(value *domain.Turn) TurnView {
	runs := value.RunIDs()
	view := TurnView{ID: value.ID().String(), SessionID: value.SessionID().String(), TurnSequence: value.Sequence(),
		BaseContextVersion: value.BaseContextVersion(), UserMessageID: value.UserMessageID().String(),
		Status: value.Status().String(), RunIDs: make([]string, len(runs)), CreatedAt: formatTime(value.CreatedAt())}
	for index, id := range runs {
		view.RunIDs[index] = id.String()
	}
	if id, ok := value.SelectedRunID(); ok {
		raw := id.String()
		view.SelectedRunID = &raw
	}
	if value.Status() != domain.TurnStatusRunning {
		completed := formatTime(value.UpdatedAt())
		view.CompletedAt = &completed
	}
	return view
}

func runView(value *domain.Run) RunView {
	terminal := value.Status().Terminal()
	view := RunView{ID: value.ID().String(), SessionID: value.SessionID().String(), TurnID: value.TurnID().String(),
		RunNo: value.RunNo(), BaseContextVersion: value.BaseContextVersion(), Status: value.Status().String(),
		Terminal: terminal, CreatedAt: formatTime(value.CreatedAt())}
	if id, ok := value.OutputMessageID(); ok && value.Status() == domain.RunStatusSelected {
		raw := id.String()
		view.FinalMessageID = &raw
	}
	if terminal {
		completed := formatTime(value.UpdatedAt())
		view.CompletedAt = &completed
	}
	return view
}

func messageView(value *domain.Message) MessageView {
	view := MessageView{ID: value.ID().String(), SessionID: value.SessionID().String(), TurnID: value.TurnID().String(),
		Role: string(value.Role()), Content: value.Content(), CreatedAt: formatTime(value.CreatedAt())}
	if id, ok := value.RunID(); ok {
		raw := id.String()
		view.RunID = &raw
	}
	return view
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func queryInternal(operation string, err error) error {
	if err == nil {
		err = errors.New("nil aggregate")
	}
	return apperror.New(apperror.CodeInternal, true, fmt.Errorf("%s: %w", operation, err))
}
