package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
)

type SafePointRequest struct {
	TenantID   domain.TenantID
	RunID      string
	LeaseOwner string
	LeaseToken uint64
}

type SafePointPersistence struct {
	TenantID   domain.TenantID
	RunID      domain.RunID
	LeaseOwner string
	LeaseToken uint64
	EventID    string
	Now        time.Time
	Retention  time.Duration
}

type SafePointResult struct {
	RunID            string `json:"run_id"`
	Status           string `json:"status"`
	Stop             bool   `json:"stop"`
	ExecutionVersion uint64 `json:"execution_version"`
}

type SafePointStore interface {
	SafePoint(context.Context, SafePointPersistence) (SafePointResult, error)
}

type SafePointService struct {
	store     SafePointStore
	ids       EventIDGenerator
	now       func() time.Time
	retention time.Duration
}

func NewSafePointService(store SafePointStore, ids EventIDGenerator) (*SafePointService, error) {
	if store == nil {
		return nil, errors.New("run safe-point store is required")
	}
	if ids == nil {
		ids = EventIDGeneratorFunc(randomEventID)
	}
	return &SafePointService{store: store, ids: ids, now: time.Now, retention: 7 * 24 * time.Hour}, nil
}

func (s *SafePointService) Check(ctx context.Context, request SafePointRequest) (SafePointResult, error) {
	if ctx == nil {
		return SafePointResult{}, invalid(errors.New("context is nil"))
	}
	if _, err := domain.ParseTenantID(request.TenantID.String()); err != nil {
		return SafePointResult{}, invalid(err)
	}
	runID, err := domain.ParseRunID(request.RunID)
	if err != nil {
		return SafePointResult{}, invalid(err)
	}
	if !validText(request.LeaseOwner, 1, 256) || request.LeaseToken == 0 {
		return SafePointResult{}, invalid(errors.New("lease authority is invalid"))
	}
	eventID, err := s.ids.NewEventID()
	if err != nil || !validEventID(eventID) {
		if err == nil {
			err = errors.New("event ID generator returned invalid value")
		}
		return SafePointResult{}, apperror.New(apperror.CodeInternal, false, fmt.Errorf("generate safe-point event ID: %w", err))
	}
	result, err := s.store.SafePoint(ctx, SafePointPersistence{TenantID: request.TenantID, RunID: runID,
		LeaseOwner: request.LeaseOwner, LeaseToken: request.LeaseToken, EventID: eventID,
		Now: s.now().UTC(), Retention: s.retention})
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return SafePointResult{}, apperror.New(apperror.CodeRunNotFound, false, nil)
		case errors.Is(err, ErrFenced):
			return SafePointResult{}, apperror.New(apperror.CodeRunFenced, false, nil)
		default:
			return SafePointResult{}, apperror.New(apperror.CodeInternal, true, fmt.Errorf("persist Run safe point: %w", err))
		}
	}
	return result, nil
}
