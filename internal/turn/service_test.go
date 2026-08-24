package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	"github.com/agenticrag/agenticrag/internal/queue"
	sessioncontrol "github.com/agenticrag/agenticrag/internal/session"
)

type memoryStore struct {
	mu        sync.Mutex
	records   map[string]CreatePersistence
	responses map[string]idempotency.Response
	decision  idempotency.Decision
	err       error
	creates   int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{records: map[string]CreatePersistence{}, responses: map[string]idempotency.Response{}}
}

func (m *memoryStore) Create(_ context.Context, command CreatePersistence) (CreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return CreateResult{}, m.err
	}
	if m.decision != "" {
		return CreateResult{Decision: m.decision}, nil
	}
	key := command.Scope.TenantID.String() + "/" + command.Scope.AuthenticatedSubjectID + "/" + command.Scope.Key
	if previous, ok := m.records[key]; ok {
		if previous.Fingerprint != command.Fingerprint {
			return CreateResult{Decision: idempotency.DecisionKeyReused}, nil
		}
		response := m.responses[key]
		return CreateResult{Decision: idempotency.DecisionReplaySucceeded, Response: &response}, nil
	}
	created := CreateResponse{
		SessionID: command.SessionID.String(), TurnID: command.IDs.TurnID.String(), RunID: command.IDs.RunID.String(),
		UserMessageID: command.IDs.MessageID.String(), TurnSequence: 1,
		BaseContextVersion: command.ExpectedContextVersion, Status: queuedStatus,
		EventsURL: "/v1/runs/" + command.IDs.RunID.String() + "/events",
		OutputURL: "/v1/runs/" + command.IDs.RunID.String() + "/output",
	}
	body, _ := json.Marshal(created)
	response := idempotency.Response{StatusCode: 202, Body: body, Location: "/v1/runs/" + command.IDs.RunID.String(), ResourceType: "run", ResourceID: command.IDs.RunID.String()}
	m.records[key], m.responses[key] = command, response
	m.creates++
	return CreateResult{Decision: idempotency.DecisionAcquired, Response: &response}, nil
}

func TestCreateNormalizesDefaultsAndReplays(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	var count atomic.Uint64
	service, err := NewService(store, IDGeneratorFunc(func() (IDs, error) { return numberedIDs(count.Add(1)), nil }))
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 8, 23, 9, 0, 0, 0, time.FixedZone("CST", 8*60*60)) }
	base := CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub_0001", SessionID: "ses_0001",
		IdempotencyKey: "idem-key-00000001", ExpectedContextVersion: 0, MessageContent: "hello",
		Trace: queue.TraceContext{TraceParent: "00-11111111111111111111111111111111-2222222222222222-01"}}
	first, err := service.Create(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	explicit := defaultMaxSteps
	base.MaxSteps = &explicit
	base.Trace.TraceParent = "00-33333333333333333333333333333333-4444444444444444-01"
	replay, err := service.Create(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if first != replay || first.RunID != "run_0001" || first.Status != "QUEUED" || first.EventsURL != "/v1/runs/run_0001/events" || first.OutputURL != "/v1/runs/run_0001/output" {
		t.Fatalf("first=%+v replay=%+v", first, replay)
	}
	if store.creates != 1 {
		t.Fatalf("physical creates = %d", store.creates)
	}
	for _, persisted := range store.records {
		if persisted.MaxSteps != 12 || persisted.Now.Location() != time.UTC || persisted.Retention < idempotency.MinimumRetention ||
			persisted.Trace.TraceParent != "00-11111111111111111111111111111111-2222222222222222-01" {
			t.Fatalf("normalized persistence = %+v", persisted)
		}
	}
	base.MessageContent = "changed"
	if _, err := service.Create(context.Background(), base); !apperror.IsCode(err, apperror.CodeIdempotencyKeyReused) {
		t.Fatalf("changed replay = %v", err)
	}
}

func TestCreateConcurrentDuplicateReturnsOneLogicalTurn(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	var count atomic.Uint64
	service, _ := NewService(store, IDGeneratorFunc(func() (IDs, error) { return numberedIDs(count.Add(1)), nil }))
	const workers = 100
	responses := make(chan CreateResponse, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := service.Create(context.Background(), validRequest())
			if err != nil {
				errorsFound <- err
				return
			}
			responses <- response
		}()
	}
	group.Wait()
	close(responses)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("Create = %v", err)
	}
	var expected CreateResponse
	for response := range responses {
		if expected.RunID == "" {
			expected = response
		}
		if response != expected {
			t.Errorf("response=%+v expected=%+v", response, expected)
		}
	}
	if store.creates != 1 {
		t.Fatalf("physical creates = %d", store.creates)
	}
}

func TestDecodeResponseAddsOutputURLToLegacyIdempotencyRecord(t *testing.T) {
	t.Parallel()
	stored := &idempotency.Response{
		StatusCode:   202,
		ResourceType: "run",
		ResourceID:   "run_0001",
		Body:         []byte(`{"run_id":"run_0001","status":"QUEUED","events_url":"/v1/runs/run_0001/events"}`),
	}

	response, err := decodeResponse(stored)
	if err != nil {
		t.Fatal(err)
	}
	if response.OutputURL != "/v1/runs/run_0001/output" {
		t.Fatalf("output URL = %q", response.OutputURL)
	}

	stored.Body = []byte(`{"run_id":"run_0001","status":"QUEUED","events_url":"/v1/runs/run_0001/events","output_url":"/v1/runs/run_other/output"}`)
	if _, err := decodeResponse(stored); !apperror.IsCode(err, apperror.CodeInternal) {
		t.Fatalf("inconsistent output URL error = %v", err)
	}
}

func TestCreateValidationAndStoreErrorMapping(t *testing.T) {
	t.Parallel()
	tooMany := 21
	invalidCases := []struct {
		name   string
		mutate func(*CreateRequest)
		code   apperror.Code
	}{
		{"tenant", func(r *CreateRequest) { r.TenantID = "bad" }, apperror.CodeInvalidArgument},
		{"session", func(r *CreateRequest) { r.SessionID = "bad" }, apperror.CodeInvalidArgument},
		{"subject", func(r *CreateRequest) { r.AuthenticatedSubjectID = " bad" }, apperror.CodeInvalidArgument},
		{"key", func(r *CreateRequest) { r.IdempotencyKey = "short" }, apperror.CodeInvalidIdempotencyKey},
		{"blank message", func(r *CreateRequest) { r.MessageContent = "  " }, apperror.CodeInvalidArgument},
		{"large message", func(r *CreateRequest) { r.MessageContent = string(make([]byte, maximumContent+1)) }, apperror.CodeInvalidArgument},
		{"max steps", func(r *CreateRequest) { r.MaxSteps = &tooMany }, apperror.CodeInvalidArgument},
		{"trace", func(r *CreateRequest) { r.Trace.TraceParent = "invalid" }, apperror.CodeInvalidArgument},
	}
	for _, test := range invalidCases {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest()
			test.mutate(&request)
			service, _ := NewService(newMemoryStore(), IDGeneratorFunc(func() (IDs, error) { return numberedIDs(1), nil }))
			_, err := service.Create(context.Background(), request)
			if !apperror.IsCode(err, test.code) {
				t.Fatalf("error=%v want=%s", err, test.code)
			}
		})
	}
	for _, test := range []struct {
		err  error
		code apperror.Code
	}{
		{sessioncontrol.ErrNotFound, apperror.CodeSessionNotFound},
		{sessioncontrol.ErrVersionConflict, apperror.CodeSessionVersionConflict},
		{sessioncontrol.ErrBusy, apperror.CodeSessionBusy},
		{sessioncontrol.ErrCASConflict, apperror.CodeServiceUnavailable},
		{errors.New("database"), apperror.CodeInternal},
	} {
		store := newMemoryStore()
		store.err = test.err
		service, _ := NewService(store, IDGeneratorFunc(func() (IDs, error) { return numberedIDs(1), nil }))
		_, err := service.Create(context.Background(), validRequest())
		if !apperror.IsCode(err, test.code) {
			t.Errorf("store error %v mapped to %v, want %s", test.err, err, test.code)
		}
	}
	store := newMemoryStore()
	store.decision = idempotency.DecisionInProgress
	service, _ := NewService(store, IDGeneratorFunc(func() (IDs, error) { return numberedIDs(1), nil }))
	if _, err := service.Create(context.Background(), validRequest()); !apperror.IsCode(err, apperror.CodeIdempotencyInProgress) {
		t.Fatalf("in progress = %v", err)
	}
}

func TestRandomIDsAndConstructionFailures(t *testing.T) {
	t.Parallel()
	if _, err := NewService(nil, nil); err == nil {
		t.Fatal("NewService(nil) error = nil")
	}
	first, err := randomIDs()
	if err != nil {
		t.Fatal(err)
	}
	second, err := randomIDs()
	if err != nil || first == second || validateIDs(first) != nil {
		t.Fatalf("random IDs first=%+v second=%+v err=%v", first, second, err)
	}
	service, _ := NewService(newMemoryStore(), IDGeneratorFunc(func() (IDs, error) { return IDs{}, errors.New("entropy") }))
	if _, err := service.Create(context.Background(), validRequest()); !apperror.IsCode(err, apperror.CodeInternal) {
		t.Fatalf("generator failure = %v", err)
	}
}

func validRequest() CreateRequest {
	return CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub_0001", SessionID: "ses_0001",
		IdempotencyKey: "idem-key-00000001", ExpectedContextVersion: 0, MessageContent: "hello"}
}

func numberedIDs(number uint64) IDs {
	suffix := fmt.Sprintf("%04d", number)
	return IDs{TurnID: domain.TurnID("trn_" + suffix), MessageID: domain.MessageID("msg_" + suffix),
		RunID: domain.RunID("run_" + suffix), EventID: "evt_" + suffix}
}
