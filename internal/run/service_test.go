package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/idempotency"
)

type cancelStoreStub struct {
	command CancelPersistence
	result  CancelResult
	err     error
}

func (s *cancelStoreStub) Cancel(_ context.Context, command CancelPersistence) (CancelResult, error) {
	s.command = command
	return s.result, s.err
}

func TestCancelServiceNormalizesCommandAndDecodesDurableResponse(t *testing.T) {
	response := CancelResponse{RunID: "run_0001", Status: "CANCEL_REQUESTED", CancellationRequested: true}
	body, _ := json.Marshal(response)
	store := &cancelStoreStub{result: CancelResult{Decision: idempotency.DecisionAcquired,
		Response: &idempotency.Response{StatusCode: 202, Body: body, ResourceType: "run", ResourceID: "run_0001"}}}
	service, _ := NewService(store, EventIDGeneratorFunc(func() (string, error) { return "evt_cancel0001", nil }))
	now := time.Date(2026, 8, 23, 4, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	got, err := service.Cancel(context.Background(), CancelRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub_0001",
		RunID: "run_0001", IdempotencyKey: "cancel-key-00000001"})
	if err != nil || got != response {
		t.Fatalf("Cancel = %+v, %v", got, err)
	}
	if store.command.Scope.Operation != cancelOperation || store.command.Scope.TenantID != "ten_0001" ||
		store.command.RunID != "run_0001" || store.command.EventID != "evt_cancel0001" || store.command.Now != now ||
		store.command.Fingerprint == (idempotency.Fingerprint{}) {
		t.Fatalf("persistence command = %+v", store.command)
	}
}

func TestCancelServiceMapsIdempotencyAndStorageDecisions(t *testing.T) {
	tests := []struct {
		name     string
		decision idempotency.Decision
		err      error
		code     apperror.Code
	}{
		{"in progress", idempotency.DecisionInProgress, nil, apperror.CodeIdempotencyInProgress},
		{"reused", idempotency.DecisionKeyReused, nil, apperror.CodeIdempotencyKeyReused},
		{"not found", "", ErrNotFound, apperror.CodeRunNotFound},
		{"unknown", "UNKNOWN", nil, apperror.CodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &cancelStoreStub{result: CancelResult{Decision: test.decision}, err: test.err}
			service, _ := NewService(store, EventIDGeneratorFunc(func() (string, error) { return "evt_cancel0001", nil }))
			_, err := service.Cancel(context.Background(), validCancelRequest())
			if !apperror.IsCode(err, test.code) {
				t.Fatalf("error=%v, want %s", err, test.code)
			}
		})
	}
}

func TestCancelServiceRejectsUntrustedInputBeforeStorage(t *testing.T) {
	tests := []CancelRequest{
		{TenantID: "bad", AuthenticatedSubjectID: "sub_0001", RunID: "run_0001", IdempotencyKey: "cancel-key-00000001"},
		{TenantID: "ten_0001", AuthenticatedSubjectID: " bad", RunID: "run_0001", IdempotencyKey: "cancel-key-00000001"},
		{TenantID: "ten_0001", AuthenticatedSubjectID: "sub_0001", RunID: "bad", IdempotencyKey: "cancel-key-00000001"},
		{TenantID: "ten_0001", AuthenticatedSubjectID: "sub_0001", RunID: "run_0001", IdempotencyKey: "short"},
	}
	for _, request := range tests {
		store := &cancelStoreStub{}
		service, _ := NewService(store, EventIDGeneratorFunc(func() (string, error) { return "evt_cancel0001", nil }))
		_, err := service.Cancel(context.Background(), request)
		if err == nil || store.command.RunID != "" {
			t.Fatalf("request=%+v error=%v command=%+v", request, err, store.command)
		}
	}
}

type safePointStoreStub struct {
	command SafePointPersistence
	result  SafePointResult
	err     error
}

func (s *safePointStoreStub) SafePoint(_ context.Context, command SafePointPersistence) (SafePointResult, error) {
	s.command = command
	return s.result, s.err
}

func TestSafePointServiceReturnsStopAndMapsFencing(t *testing.T) {
	store := &safePointStoreStub{result: SafePointResult{RunID: "run_0001", Status: "CANCELLED", Stop: true, ExecutionVersion: 4}}
	service, _ := NewSafePointService(store, EventIDGeneratorFunc(func() (string, error) { return "evt_cancel0002", nil }))
	result, err := service.Check(context.Background(), SafePointRequest{TenantID: "ten_0001", RunID: "run_0001", LeaseOwner: "worker-1", LeaseToken: 3})
	if err != nil || !result.Stop || store.command.LeaseToken != 3 {
		t.Fatalf("Check=%+v error=%v command=%+v", result, err, store.command)
	}
	store.err = ErrFenced
	if _, err := service.Check(context.Background(), SafePointRequest{TenantID: "ten_0001", RunID: "run_0001", LeaseOwner: "worker-1", LeaseToken: 3}); !apperror.IsCode(err, apperror.CodeRunFenced) {
		t.Fatalf("fenced error=%v", err)
	}
	store.err = errors.New("database")
	if _, err := service.Check(context.Background(), SafePointRequest{TenantID: "ten_0001", RunID: "run_0001", LeaseOwner: "worker-1", LeaseToken: 3}); !apperror.IsCode(err, apperror.CodeInternal) {
		t.Fatalf("internal error=%v", err)
	}
}

func validCancelRequest() CancelRequest {
	return CancelRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub_0001", RunID: "run_0001", IdempotencyKey: "cancel-key-00000001"}
}
