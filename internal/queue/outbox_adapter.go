package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/outbox"
)

type Enqueuer interface {
	Enqueue(context.Context, Message) error
}

type RunQueuedAdapter struct{ queue Enqueuer }

func NewRunQueuedAdapter(target Enqueuer) (*RunQueuedAdapter, error) {
	if target == nil {
		return nil, fmt.Errorf("%w: target queue is nil", ErrInvalidArgument)
	}
	return &RunQueuedAdapter{queue: target}, nil
}

type runQueuedPayload struct {
	SchemaVersion      int    `json:"schema_version"`
	TenantID           string `json:"tenant_id"`
	SessionID          string `json:"session_id"`
	TurnID             string `json:"turn_id"`
	RunID              string `json:"run_id"`
	UserMessageID      string `json:"user_message_id"`
	TurnSequence       uint64 `json:"turn_sequence"`
	BaseContextVersion uint64 `json:"base_context_version"`
	MaxSteps           int    `json:"max_steps"`
	TraceParent        string `json:"traceparent,omitempty"`
	TraceState         string `json:"tracestate,omitempty"`
}

func (a *RunQueuedAdapter) Publish(ctx context.Context, event outbox.Event) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", ErrInvalidArgument)
	}
	if event.Type != "RunQueued.v1" || event.AggregateType != "run" || event.Status != outbox.StatusPending ||
		event.ID == "" || event.AvailableAt.IsZero() || event.OccurredAt.IsZero() {
		return fmt.Errorf("%w: outbox event is not a pending RunQueued.v1 delivery", ErrInvalidArgument)
	}
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	decoder.DisallowUnknownFields()
	var payload runQueuedPayload
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("%w: decode RunQueued payload: %v", ErrInvalidArgument, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing RunQueued payload", ErrInvalidArgument)
	}
	tenantID, err := domain.ParseTenantID(payload.TenantID)
	if err != nil || tenantID != event.TenantID {
		return fmt.Errorf("%w: payload tenant mismatch", ErrInvalidArgument)
	}
	sessionID, err := domain.ParseSessionID(payload.SessionID)
	if err != nil {
		return fmt.Errorf("%w: payload Session", ErrInvalidArgument)
	}
	turnID, err := domain.ParseTurnID(payload.TurnID)
	if err != nil {
		return fmt.Errorf("%w: payload Turn", ErrInvalidArgument)
	}
	runID, err := domain.ParseRunID(payload.RunID)
	if err != nil || payload.RunID != event.AggregateID || payload.SchemaVersion != 1 || payload.TurnSequence == 0 ||
		payload.MaxSteps < 1 || payload.MaxSteps > 20 {
		return fmt.Errorf("%w: payload Run identity or execution options", ErrInvalidArgument)
	}
	message := Message{DeliveryID: event.ID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID, RunID: runID,
		Payload: append(json.RawMessage(nil), event.Payload...), Trace: TraceContext{TraceParent: payload.TraceParent, TraceState: payload.TraceState},
		AvailableAt: event.AvailableAt, CreatedAt: event.OccurredAt}
	if err := validateMessage(message); err != nil {
		return err
	}
	return a.queue.Enqueue(ctx, message)
}
