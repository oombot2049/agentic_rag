package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
)

type memoryStore struct {
	mu       sync.Mutex
	records  map[string]CreatePersistence
	sessions map[string]*domain.Session
	decision idempotency.Decision
	err      error
	creates  int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{records: map[string]CreatePersistence{}, sessions: map[string]*domain.Session{}}
}

func (m *memoryStore) Create(_ context.Context, value CreatePersistence) (CreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return CreateResult{}, m.err
	}
	if m.decision != "" {
		return CreateResult{Decision: m.decision}, nil
	}
	key := value.Scope.TenantID.String() + "/" + value.Scope.AuthenticatedSubjectID + "/" + value.Scope.Key
	if previous, ok := m.records[key]; ok {
		if previous.Fingerprint != value.Fingerprint {
			return CreateResult{Decision: idempotency.DecisionKeyReused}, nil
		}
		response := previous.Response
		return CreateResult{Decision: idempotency.DecisionReplaySucceeded, Response: &response}, nil
	}
	m.creates++
	m.records[key] = value
	m.sessions[value.Scope.TenantID.String()+"/"+value.Session.ID().String()] = value.Session
	return CreateResult{Decision: idempotency.DecisionAcquired, Session: value.Session}, nil
}

func (m *memoryStore) Get(_ context.Context, tenant domain.TenantID, id domain.SessionID) (*domain.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.sessions[tenant.String()+"/"+id.String()]
	if !ok {
		return nil, ErrNotFound
	}
	return value, nil
}

func TestServiceCreateNormalizesAuditsAndReplays(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	var sequence atomic.Int64
	service, err := NewService(store, IDGeneratorFunc(func() (domain.SessionID, error) {
		return domain.ParseSessionID("ses_000" + string(rune('0'+sequence.Add(1))))
	}))
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 8, 23, 8, 9, 10, 123, time.FixedZone("offset", 8*60*60))
	service.now = func() time.Time { return fixed }
	request := CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub_0001", IdempotencyKey: "idem-key-00000001"}
	first, err := service.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.ID != "ses_0001" || first.ContextVersion != 0 || first.RowVersion != 1 || first.NextTurnSequence != 1 {
		t.Fatalf("unexpected views: first=%+v second=%+v", first, second)
	}
	if first.CreatedAt != "2026-08-23T00:09:10.000000123Z" || first.UpdatedAt != first.CreatedAt {
		t.Fatalf("audit fields = %s / %s", first.CreatedAt, first.UpdatedAt)
	}
	if store.creates != 1 || len(store.records) != 1 {
		t.Fatalf("durable creates=%d records=%d", store.creates, len(store.records))
	}
	for _, persisted := range store.records {
		if persisted.Response.StatusCode != 201 || persisted.Response.Location != "/v1/sessions/ses_0001" || persisted.Retention < idempotency.MinimumRetention {
			t.Fatalf("persistence contract = %+v", persisted)
		}
		if persisted.Metadata == nil {
			t.Fatal("missing metadata was not normalized to an object")
		}
	}
}

func TestServiceCreateConcurrentDuplicateReturnsOneLogicalSession(t *testing.T) {
	t.Parallel()
	store := newMemoryStore()
	var ids atomic.Int64
	service, _ := NewService(store, IDGeneratorFunc(func() (domain.SessionID, error) {
		return domain.ParseSessionID("ses_" + leftPad(ids.Add(1)))
	}))
	service.now = func() time.Time { return time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC) }
	const workers = 100
	results := make(chan string, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			view, err := service.Create(context.Background(), CreateRequest{
				TenantID: "ten_0001", AuthenticatedSubjectID: "sub_0001", IdempotencyKey: "idem-key-00000001",
			})
			if err != nil {
				errorsFound <- err
				return
			}
			results <- view.ID
		}()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("Create = %v", err)
	}
	var expected string
	for id := range results {
		if expected == "" {
			expected = id
		}
		if id != expected {
			t.Errorf("replay ID = %s, want %s", id, expected)
		}
	}
	if store.creates != 1 {
		t.Fatalf("physical create count = %d", store.creates)
	}
}

func TestServiceValidationDecisionAndGetMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		request CreateRequest
		code    apperror.Code
	}{
		{"tenant", CreateRequest{TenantID: "bad", AuthenticatedSubjectID: "sub", IdempotencyKey: "idem-key-00000001"}, apperror.CodeInvalidArgument},
		{"subject", CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: " bad", IdempotencyKey: "idem-key-00000001"}, apperror.CodeInvalidArgument},
		{"key", CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub", IdempotencyKey: "short"}, apperror.CodeInvalidIdempotencyKey},
		{"metadata key", CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub", IdempotencyKey: "idem-key-00000001", Metadata: Metadata{" bad": "x"}}, apperror.CodeInvalidArgument},
		{"metadata value", CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub", IdempotencyKey: "idem-key-00000001", Metadata: Metadata{"key": "bad\n"}}, apperror.CodeInvalidArgument},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			service, _ := NewService(newMemoryStore(), IDGeneratorFunc(func() (domain.SessionID, error) { return "ses_0001", nil }))
			_, err := service.Create(context.Background(), test.request)
			if !apperror.IsCode(err, test.code) {
				t.Fatalf("Create error = %v, want %s", err, test.code)
			}
		})
	}

	for _, decision := range []struct {
		value idempotency.Decision
		code  apperror.Code
	}{
		{idempotency.DecisionInProgress, apperror.CodeIdempotencyInProgress},
		{idempotency.DecisionKeyReused, apperror.CodeIdempotencyKeyReused},
		{idempotency.DecisionReplayFailedFinal, apperror.CodeIdempotencyKeyReused},
	} {
		store := newMemoryStore()
		store.decision = decision.value
		service, _ := NewService(store, IDGeneratorFunc(func() (domain.SessionID, error) { return "ses_0001", nil }))
		service.now = func() time.Time { return time.Now().UTC() }
		_, err := service.Create(context.Background(), CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub", IdempotencyKey: "idem-key-00000001"})
		if !apperror.IsCode(err, decision.code) {
			t.Errorf("decision %s error = %v", decision.value, err)
		}
	}

	store := newMemoryStore()
	service, _ := NewService(store, nil)
	if _, err := service.Get(context.Background(), "ten_0001", "ses_4040"); !apperror.IsCode(err, apperror.CodeSessionNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if _, err := service.Get(context.Background(), "ten_0001", "bad"); !apperror.IsCode(err, apperror.CodeInvalidArgument) {
		t.Fatalf("Get invalid = %v", err)
	}
	store.err = errors.New("database unavailable")
	if _, err := service.Create(context.Background(), CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "sub", IdempotencyKey: "idem-key-00000001"}); !apperror.IsCode(err, apperror.CodeInternal) {
		t.Fatalf("Create store failure = %v", err)
	}
}

func TestRandomSessionIDAndETag(t *testing.T) {
	t.Parallel()
	first, err := randomSessionID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := randomSessionID()
	if err != nil || first == second || len(first) != 36 {
		t.Fatalf("random IDs = %q / %q, err=%v", first, second, err)
	}
	if ETag(42) != `"rv-42"` {
		t.Fatalf("ETag = %s", ETag(42))
	}
}

func leftPad(value int64) string {
	const digits = "00000000000000000000000000000000"
	raw := []byte(digits)
	for index := len(raw) - 1; value > 0; index-- {
		raw[index] = byte('0' + value%10)
		value /= 10
	}
	return string(raw)
}
