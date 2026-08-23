// Package run implements Run control-plane application commands.
package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
)

const cancelOperation = "CANCEL_RUN"

var (
	ErrNotFound = errors.New("run not found")
	ErrFenced   = errors.New("run worker is fenced")
)

type CancelRequest struct {
	TenantID               domain.TenantID
	AuthenticatedSubjectID string
	RunID                  string
	IdempotencyKey         string
}

type CancelPersistence struct {
	TenantID    domain.TenantID
	RunID       domain.RunID
	Scope       idempotency.Scope
	Fingerprint idempotency.Fingerprint
	EventID     string
	Now         time.Time
	Retention   time.Duration
}

type CancelResponse struct {
	RunID                 string `json:"run_id"`
	Status                string `json:"status"`
	CancellationRequested bool   `json:"cancellation_requested"`
	Terminal              bool   `json:"terminal"`
}

type CancelResult struct {
	Decision idempotency.Decision
	Response *idempotency.Response
}

type CancelStore interface {
	Cancel(context.Context, CancelPersistence) (CancelResult, error)
}

type EventIDGenerator interface{ NewEventID() (string, error) }
type EventIDGeneratorFunc func() (string, error)

func (f EventIDGeneratorFunc) NewEventID() (string, error) { return f() }

type Service struct {
	store     CancelStore
	ids       EventIDGenerator
	now       func() time.Time
	retention time.Duration
}

func NewService(store CancelStore, ids EventIDGenerator) (*Service, error) {
	if store == nil {
		return nil, errors.New("run cancellation store is required")
	}
	if ids == nil {
		ids = EventIDGeneratorFunc(randomEventID)
	}
	return &Service{store: store, ids: ids, now: time.Now, retention: 7 * 24 * time.Hour}, nil
}

func (s *Service) Cancel(ctx context.Context, request CancelRequest) (CancelResponse, error) {
	if ctx == nil {
		return CancelResponse{}, invalid(errors.New("context is nil"))
	}
	if _, err := domain.ParseTenantID(request.TenantID.String()); err != nil {
		return CancelResponse{}, invalid(err)
	}
	runID, err := domain.ParseRunID(request.RunID)
	if err != nil {
		return CancelResponse{}, apperror.New(apperror.CodeRunNotFound, false, err)
	}
	if !validText(request.AuthenticatedSubjectID, 1, 128) {
		return CancelResponse{}, invalid(errors.New("authenticated subject is invalid"))
	}
	if !validText(request.IdempotencyKey, 16, 128) {
		return CancelResponse{}, apperror.New(apperror.CodeInvalidIdempotencyKey, false, nil)
	}
	fingerprint, err := idempotency.NewFingerprint(idempotency.Request{Operation: cancelOperation,
		PathParameters: map[string]string{"run_id": runID.String()}, Body: struct{}{}})
	if err != nil {
		return CancelResponse{}, invalid(err)
	}
	eventID, err := s.ids.NewEventID()
	if err != nil || !validEventID(eventID) {
		if err == nil {
			err = errors.New("event ID generator returned invalid value")
		}
		return CancelResponse{}, apperror.New(apperror.CodeInternal, false, fmt.Errorf("generate cancellation event ID: %w", err))
	}
	result, err := s.store.Cancel(ctx, CancelPersistence{TenantID: request.TenantID, RunID: runID,
		Scope: idempotency.Scope{TenantID: request.TenantID, AuthenticatedSubjectID: request.AuthenticatedSubjectID,
			Operation: cancelOperation, Key: request.IdempotencyKey}, Fingerprint: fingerprint,
		EventID: eventID, Now: s.now().UTC(), Retention: s.retention})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return CancelResponse{}, apperror.New(apperror.CodeRunNotFound, false, nil)
		}
		return CancelResponse{}, apperror.New(apperror.CodeInternal, true, fmt.Errorf("persist Run cancellation: %w", err))
	}
	switch result.Decision {
	case idempotency.DecisionAcquired, idempotency.DecisionReplaySucceeded:
		return decodeCancelResponse(result.Response)
	case idempotency.DecisionInProgress:
		return CancelResponse{}, apperror.New(apperror.CodeIdempotencyInProgress, true, nil)
	case idempotency.DecisionKeyReused, idempotency.DecisionReplayFailedFinal:
		return CancelResponse{}, apperror.New(apperror.CodeIdempotencyKeyReused, false, nil)
	default:
		return CancelResponse{}, apperror.New(apperror.CodeInternal, false, errors.New("unknown cancellation idempotency decision"))
	}
}

func decodeCancelResponse(stored *idempotency.Response) (CancelResponse, error) {
	if stored == nil || stored.StatusCode != 202 || stored.ResourceType != "run" || stored.ResourceID == "" {
		return CancelResponse{}, apperror.New(apperror.CodeInternal, false, errors.New("stored cancellation response is invalid"))
	}
	var response CancelResponse
	if err := json.Unmarshal(stored.Body, &response); err != nil || response.RunID != stored.ResourceID || response.Status == "" {
		return CancelResponse{}, apperror.New(apperror.CodeInternal, false, errors.New("stored cancellation response is inconsistent"))
	}
	return response, nil
}

func validText(value string, minimum, maximum int) bool {
	return len(value) >= minimum && len(value) <= maximum && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validEventID(value string) bool {
	if len(value) < 8 || len(value) > 128 || !strings.HasPrefix(value, "evt_") {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func randomEventID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "evt_" + hex.EncodeToString(buffer), nil
}

func invalid(err error) error { return apperror.New(apperror.CodeInvalidArgument, false, err) }
