// Package turn implements the Create Turn application command.
package turn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	"github.com/agenticrag/agenticrag/internal/queue"
	sessioncontrol "github.com/agenticrag/agenticrag/internal/session"
)

const (
	createOperation  = "CREATE_TURN"
	defaultMaxSteps  = 12
	maximumMaxSteps  = 20
	maximumContent   = 131072
	defaultRetention = 7 * 24 * time.Hour
	queuedStatus     = "QUEUED"
)

type IDs struct {
	TurnID    domain.TurnID
	MessageID domain.MessageID
	RunID     domain.RunID
	EventID   string
}

type IDGenerator interface{ NewTurnIDs() (IDs, error) }
type IDGeneratorFunc func() (IDs, error)

func (f IDGeneratorFunc) NewTurnIDs() (IDs, error) { return f() }

type CreateRequest struct {
	TenantID               domain.TenantID
	AuthenticatedSubjectID string
	SessionID              string
	IdempotencyKey         string
	ExpectedContextVersion uint64
	MessageContent         string
	MaxSteps               *int
	Trace                  queue.TraceContext
}

type CreatePersistence struct {
	TenantID               domain.TenantID
	AuthenticatedSubjectID string
	SessionID              domain.SessionID
	ExpectedContextVersion uint64
	MessageContent         string
	MaxSteps               int
	Trace                  queue.TraceContext
	IDs                    IDs
	Scope                  idempotency.Scope
	Fingerprint            idempotency.Fingerprint
	Now                    time.Time
	Retention              time.Duration
}

type CreateResponse struct {
	SessionID          string `json:"session_id"`
	TurnID             string `json:"turn_id"`
	RunID              string `json:"run_id"`
	UserMessageID      string `json:"user_message_id"`
	TurnSequence       uint64 `json:"turn_sequence"`
	BaseContextVersion uint64 `json:"base_context_version"`
	Status             string `json:"status"`
	EventsURL          string `json:"events_url"`
	OutputURL          string `json:"output_url"`
}

type CreateResult struct {
	Decision idempotency.Decision
	Response *idempotency.Response
}

type Store interface {
	Create(context.Context, CreatePersistence) (CreateResult, error)
}

type Service struct {
	store     Store
	ids       IDGenerator
	now       func() time.Time
	retention time.Duration
}

func NewService(store Store, ids IDGenerator) (*Service, error) {
	if store == nil {
		return nil, errors.New("turn service store is required")
	}
	if ids == nil {
		ids = IDGeneratorFunc(randomIDs)
	}
	return &Service{store: store, ids: ids, now: time.Now, retention: defaultRetention}, nil
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (CreateResponse, error) {
	if ctx == nil {
		return CreateResponse{}, invalid(errors.New("context is nil"))
	}
	if _, err := domain.ParseTenantID(request.TenantID.String()); err != nil {
		return CreateResponse{}, invalid(err)
	}
	sessionID, err := domain.ParseSessionID(request.SessionID)
	if err != nil {
		return CreateResponse{}, invalid(err)
	}
	if request.ExpectedContextVersion > math.MaxInt64 {
		return CreateResponse{}, invalid(errors.New("expected context version exceeds signed 64-bit range"))
	}
	if len(request.MessageContent) == 0 || len(request.MessageContent) > maximumContent || strings.TrimSpace(request.MessageContent) == "" {
		return CreateResponse{}, invalid(errors.New("message content must be non-blank and no larger than 131072 bytes"))
	}
	if !validBoundedText(request.AuthenticatedSubjectID, 1, 128) {
		return CreateResponse{}, invalid(errors.New("authenticated subject is invalid"))
	}
	if !validBoundedText(request.IdempotencyKey, 16, 128) {
		return CreateResponse{}, apperror.New(apperror.CodeInvalidIdempotencyKey, false, nil)
	}
	maxSteps := defaultMaxSteps
	if request.MaxSteps != nil {
		maxSteps = *request.MaxSteps
	}
	if maxSteps < 1 || maxSteps > maximumMaxSteps {
		return CreateResponse{}, invalid(errors.New("max_steps must be between 1 and 20"))
	}
	trace, err := queue.NewTraceContext(request.Trace.TraceParent, request.Trace.TraceState)
	if err != nil {
		return CreateResponse{}, invalid(err)
	}
	normalizedBody := struct {
		ExpectedContextVersion uint64 `json:"expected_context_version"`
		Message                struct {
			Content string `json:"content"`
		} `json:"message"`
		Execution struct {
			MaxSteps int `json:"max_steps"`
		} `json:"execution"`
	}{ExpectedContextVersion: request.ExpectedContextVersion}
	normalizedBody.Message.Content = request.MessageContent
	normalizedBody.Execution.MaxSteps = maxSteps
	fingerprint, err := idempotency.NewFingerprint(idempotency.Request{
		Operation: createOperation, PathParameters: map[string]string{"session_id": sessionID.String()}, Body: normalizedBody,
	})
	if err != nil {
		return CreateResponse{}, invalid(err)
	}
	ids, err := s.ids.NewTurnIDs()
	if err != nil {
		return CreateResponse{}, apperror.New(apperror.CodeInternal, false, fmt.Errorf("generate Turn IDs: %w", err))
	}
	if err := validateIDs(ids); err != nil {
		return CreateResponse{}, apperror.New(apperror.CodeInternal, false, fmt.Errorf("ID generator returned invalid IDs: %w", err))
	}
	now := s.now().UTC()
	result, err := s.store.Create(ctx, CreatePersistence{
		TenantID: request.TenantID, AuthenticatedSubjectID: request.AuthenticatedSubjectID,
		SessionID: sessionID, ExpectedContextVersion: request.ExpectedContextVersion,
		MessageContent: request.MessageContent, MaxSteps: maxSteps, Trace: trace, IDs: ids,
		Scope: idempotency.Scope{TenantID: request.TenantID, AuthenticatedSubjectID: request.AuthenticatedSubjectID,
			Operation: createOperation, Key: request.IdempotencyKey},
		Fingerprint: fingerprint, Now: now, Retention: s.retention,
	})
	if err != nil {
		return CreateResponse{}, mapStoreError(err)
	}
	switch result.Decision {
	case idempotency.DecisionAcquired, idempotency.DecisionReplaySucceeded:
		return decodeResponse(result.Response)
	case idempotency.DecisionInProgress:
		return CreateResponse{}, apperror.New(apperror.CodeIdempotencyInProgress, true, nil)
	case idempotency.DecisionKeyReused, idempotency.DecisionReplayFailedFinal:
		return CreateResponse{}, apperror.New(apperror.CodeIdempotencyKeyReused, false, nil)
	default:
		return CreateResponse{}, apperror.New(apperror.CodeInternal, false, errors.New("unknown idempotency decision"))
	}
}

func decodeResponse(stored *idempotency.Response) (CreateResponse, error) {
	if stored == nil || stored.StatusCode != 202 || stored.ResourceType != "run" {
		return CreateResponse{}, apperror.New(apperror.CodeInternal, false, errors.New("stored Turn response is invalid"))
	}
	var response CreateResponse
	if err := json.Unmarshal(stored.Body, &response); err != nil {
		return CreateResponse{}, apperror.New(apperror.CodeInternal, false, fmt.Errorf("decode stored Turn response: %w", err))
	}
	if response.Status != queuedStatus || response.RunID == "" || response.RunID != stored.ResourceID {
		return CreateResponse{}, apperror.New(apperror.CodeInternal, false, errors.New("stored Turn response reference is inconsistent"))
	}
	expectedEvents := "/v1/runs/" + response.RunID + "/events"
	expectedOutput := "/v1/runs/" + response.RunID + "/output"
	if response.EventsURL != expectedEvents || response.OutputURL != "" && response.OutputURL != expectedOutput {
		return CreateResponse{}, apperror.New(apperror.CodeInternal, false, errors.New("stored Turn response URL is inconsistent"))
	}
	// Idempotency records created before the split output endpoint do not carry
	// output_url. It is a deterministic projection of RunID, so replay can add it
	// without changing the logical command result.
	response.OutputURL = expectedOutput
	return response, nil
}

func mapStoreError(err error) error {
	switch {
	case errors.Is(err, sessioncontrol.ErrNotFound):
		return apperror.New(apperror.CodeSessionNotFound, false, nil)
	case errors.Is(err, sessioncontrol.ErrVersionConflict):
		return apperror.New(apperror.CodeSessionVersionConflict, false, nil)
	case errors.Is(err, sessioncontrol.ErrBusy):
		return apperror.New(apperror.CodeSessionBusy, false, nil)
	case errors.Is(err, sessioncontrol.ErrCASConflict):
		return apperror.New(apperror.CodeServiceUnavailable, true, err)
	default:
		return apperror.New(apperror.CodeInternal, true, fmt.Errorf("persist Turn command: %w", err))
	}
}

func validateIDs(ids IDs) error {
	if _, err := domain.ParseTurnID(ids.TurnID.String()); err != nil {
		return err
	}
	if _, err := domain.ParseMessageID(ids.MessageID.String()); err != nil {
		return err
	}
	if _, err := domain.ParseRunID(ids.RunID.String()); err != nil {
		return err
	}
	if len(ids.EventID) < 8 || len(ids.EventID) > 128 || !strings.HasPrefix(ids.EventID, "evt_") {
		return errors.New("invalid event ID")
	}
	for _, character := range ids.EventID {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-') {
			return errors.New("invalid event ID")
		}
	}
	return nil
}

func validBoundedText(value string, minimum, maximum int) bool {
	return len(value) >= minimum && len(value) <= maximum && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func invalid(cause error) error { return apperror.New(apperror.CodeInvalidArgument, false, cause) }

func randomIDs() (IDs, error) {
	values := make([]string, 4)
	for index, prefix := range []string{"trn_", "msg_", "run_", "evt_"} {
		var entropy [16]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return IDs{}, err
		}
		values[index] = prefix + hex.EncodeToString(entropy[:])
	}
	turnID, err := domain.ParseTurnID(values[0])
	if err != nil {
		return IDs{}, err
	}
	messageID, err := domain.ParseMessageID(values[1])
	if err != nil {
		return IDs{}, err
	}
	runID, err := domain.ParseRunID(values[2])
	if err != nil {
		return IDs{}, err
	}
	return IDs{TurnID: turnID, MessageID: messageID, RunID: runID, EventID: values[3]}, nil
}
