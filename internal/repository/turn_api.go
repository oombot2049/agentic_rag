package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	"github.com/agenticrag/agenticrag/internal/outbox"
	sessioncontrol "github.com/agenticrag/agenticrag/internal/session"
	"github.com/agenticrag/agenticrag/internal/turn"
)

const maximumTurnCASAttempts = 3

type TurnAPIStore struct{ withinTx WithinTxFunc }

func NewTurnAPIStore(withinTx WithinTxFunc) (*TurnAPIStore, error) {
	if withinTx == nil {
		return nil, fmt.Errorf("%w: transaction runner is required", ErrInvalidArgument)
	}
	return &TurnAPIStore{withinTx: withinTx}, nil
}

func (s *TurnAPIStore) Create(ctx context.Context, command turn.CreatePersistence) (turn.CreateResult, error) {
	var result turn.CreateResult
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
		switch claim.Decision {
		case idempotency.DecisionReplaySucceeded:
			if claim.Record.Response == nil || claim.Record.Response.ResourceType != "run" {
				return errors.New("invalid stored Create Turn response")
			}
			response := *claim.Record.Response
			response.Body = append([]byte(nil), response.Body...)
			result.Response = &response
			return nil
		case idempotency.DecisionAcquired:
			return s.createAcquired(ctx, tx, claims, command, &result)
		default:
			return nil
		}
	})
	return result, err
}

func (s *TurnAPIStore) createAcquired(
	ctx context.Context,
	tx DBTX,
	claims *idempotency.Repository,
	command turn.CreatePersistence,
	result *turn.CreateResult,
) error {
	control, err := sessioncontrol.NewRepository(tx)
	if err != nil {
		return err
	}
	current, err := control.Load(ctx, command.TenantID, command.SessionID)
	if err != nil {
		return err
	}
	var reservation sessioncontrol.Reservation
	for attempt := 0; attempt < maximumTurnCASAttempts; attempt++ {
		if current.ContextVersion() != command.ExpectedContextVersion {
			return sessioncontrol.ErrVersionConflict
		}
		if _, active := current.ActiveRunID(); active {
			return sessioncontrol.ErrBusy
		}
		reservation, err = control.ReserveTurn(ctx, sessioncontrol.ReserveTurnCommand{
			TenantID: command.TenantID, SessionID: command.SessionID, RunID: command.IDs.RunID,
			ExpectedContextVersion: command.ExpectedContextVersion, ExpectedRowVersion: current.RowVersion(), At: command.Now,
		})
		if err == nil {
			break
		}
		if !errors.Is(err, sessioncontrol.ErrCASConflict) {
			return err
		}
		current, err = control.Load(ctx, command.TenantID, command.SessionID)
		if err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}
	sequence := reservation.TurnSequence
	logicalTurn, err := domain.NewTurn(command.IDs.TurnID, command.TenantID, command.SessionID, sequence,
		command.ExpectedContextVersion, command.IDs.MessageID, command.IDs.RunID, command.Now)
	if err != nil {
		return err
	}
	message, err := domain.NewUserMessage(command.IDs.MessageID, command.TenantID, command.SessionID,
		command.IDs.TurnID, command.MessageContent, command.Now)
	if err != nil {
		return err
	}
	run, err := domain.NewRun(command.IDs.RunID, command.TenantID, command.SessionID,
		command.IDs.TurnID, 1, command.ExpectedContextVersion, command.Now)
	if err != nil {
		return err
	}
	conversation, err := NewConversation(tx)
	if err != nil {
		return err
	}
	execution, err := NewExecution(tx)
	if err != nil {
		return err
	}
	events, err := outbox.NewRepository(tx)
	if err != nil {
		return err
	}
	if err := conversation.InsertTurn(ctx, logicalTurn); err != nil {
		return err
	}
	if err := conversation.InsertMessage(ctx, message); err != nil {
		return err
	}
	if err := execution.InsertRunWithOptions(ctx, run, command.MaxSteps); err != nil {
		return err
	}
	eventPayload, err := json.Marshal(struct {
		SchemaVersion      int    `json:"schema_version"`
		TenantID           string `json:"tenant_id"`
		SessionID          string `json:"session_id"`
		TurnID             string `json:"turn_id"`
		RunID              string `json:"run_id"`
		UserMessageID      string `json:"user_message_id"`
		TurnSequence       uint64 `json:"turn_sequence"`
		BaseContextVersion uint64 `json:"base_context_version"`
		MaxSteps           int    `json:"max_steps"`
	}{1, command.TenantID.String(), command.SessionID.String(), command.IDs.TurnID.String(),
		command.IDs.RunID.String(), command.IDs.MessageID.String(), sequence, command.ExpectedContextVersion, command.MaxSteps})
	if err != nil {
		return err
	}
	if err := events.Enqueue(ctx, outbox.Event{
		TenantID: command.TenantID, ID: command.IDs.EventID, AggregateType: "run",
		AggregateID: command.IDs.RunID.String(), Type: "RunQueued.v1", Payload: eventPayload,
		AvailableAt: command.Now, OccurredAt: command.Now,
	}); err != nil {
		return err
	}
	response := turn.CreateResponse{
		SessionID: command.SessionID.String(), TurnID: command.IDs.TurnID.String(), RunID: command.IDs.RunID.String(),
		UserMessageID: command.IDs.MessageID.String(), TurnSequence: sequence,
		BaseContextVersion: command.ExpectedContextVersion, Status: "QUEUED",
		EventsURL: "/v1/runs/" + command.IDs.RunID.String() + "/events",
	}
	body, err := json.Marshal(response)
	if err != nil {
		return err
	}
	durableResponse := idempotency.Response{StatusCode: 202, Body: body,
		Location: "/v1/runs/" + command.IDs.RunID.String(), ResourceType: "run", ResourceID: command.IDs.RunID.String()}
	if err := claims.CompleteSucceeded(ctx, command.Scope, command.Fingerprint, durableResponse, command.Now); err != nil {
		return err
	}
	result.Response = &durableResponse
	return nil
}
