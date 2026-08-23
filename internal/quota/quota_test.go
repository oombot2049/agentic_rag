package quota

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var quotaTestTime = time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)

func TestServiceCompensatesGlobalDenialAndIsolatesTenants(t *testing.T) {
	global := testLimits()
	global.ActiveRuns = 2
	tenantLimits := testLimits()
	tenantLimits.ActiveRuns = 1
	service := mustService(t, NewMemoryStore(), global, tenantLimits)

	first := testRequest("ten_0001", "qrs_first", quotaTestTime)
	if decision, err := service.Admit(context.Background(), first); err != nil || !decision.Allowed {
		t.Fatalf("first Admit() = %+v, %v", decision, err)
	}
	deniedRequest := testRequest("ten_0001", "qrs_denied", quotaTestTime)
	denied, err := service.Admit(context.Background(), deniedRequest)
	if err != nil || denied.Allowed || denied.Scope != "tenant" || denied.Reason != ReasonActiveRuns {
		t.Fatalf("denied Admit() = %+v, %v", denied, err)
	}

	// Reusing the public reservation ID in another tenant must not replay the
	// global receipt, and the compensated global slot must be available.
	other := testRequest("ten_0002", deniedRequest.ReservationID, quotaTestTime)
	decision, err := service.Admit(context.Background(), other)
	if err != nil || !decision.Allowed {
		t.Fatalf("other tenant Admit() = %+v, %v", decision, err)
	}
}

func TestServiceSettleRefundsUnusedBudgetAndReleasesActiveRun(t *testing.T) {
	limits := testLimits()
	limits.ActiveRuns = 1
	limits.PeriodTokens = 100
	limits.PeriodCostNanos = 100
	service := mustService(t, NewMemoryStore(), testLimits(), limits)
	request := testRequest("ten_0001", "qrs_settle", quotaTestTime)
	request.EstimatedTokens = 80
	request.EstimatedCostNanos = 80
	decision, err := service.Admit(context.Background(), request)
	if err != nil || !decision.Allowed {
		t.Fatalf("Admit() = %+v, %v", decision, err)
	}
	if err := service.Settle(context.Background(), decision.Reservation, 30, 30, quotaTestTime); err != nil {
		t.Fatal(err)
	}
	if err := service.Settle(context.Background(), decision.Reservation, 30, 30, quotaTestTime); err != nil {
		t.Fatalf("idempotent Settle() error = %v", err)
	}
	next := testRequest("ten_0001", "qrs_after_settle", quotaTestTime)
	next.EstimatedTokens = 70
	next.EstimatedCostNanos = 70
	if nextDecision, nextErr := service.Admit(context.Background(), next); nextErr != nil || !nextDecision.Allowed {
		t.Fatalf("Admit() after refund = %+v, %v", nextDecision, nextErr)
	}
	if err := service.Settle(context.Background(), decision.Reservation, 29, 30, quotaTestTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed Settle() error = %v, want conflict", err)
	}
}

func TestMemoryStoreDimensionsAndPeriodRollover(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	limits := testLimits()
	limits.RequestsPerSecond = 1
	limits.Burst = 1
	limits.ActiveRuns = 10
	limits.PeriodTokens = 10
	limits.PeriodCostNanos = 10

	assertAllowed(t, store, "tenant", "qrs_one1", limits, 6, 6, quotaTestTime)
	decision, err := store.Reserve(ctx, "tenant", "qrs_qps1", limits, 1, 1, quotaTestTime)
	if err != nil || decision.Reason != ReasonQPS || decision.RetryAfter != time.Second {
		t.Fatalf("QPS decision = %+v, %v", decision, err)
	}
	if err := store.Settle(ctx, "tenant", "qrs_one1", 6, 6, quotaTestTime); err != nil {
		t.Fatal(err)
	}
	decision, err = store.Reserve(ctx, "tenant", "qrs_tokens", limits, 5, 1, quotaTestTime.Add(time.Second))
	if err != nil || decision.Reason != ReasonTokens {
		t.Fatalf("Token decision = %+v, %v", decision, err)
	}
	decision, err = store.Reserve(ctx, "tenant", "qrs_cost", limits, 1, 5, quotaTestTime.Add(2*time.Second))
	if err != nil || decision.Reason != ReasonCost {
		t.Fatalf("Cost decision = %+v, %v", decision, err)
	}
	assertAllowed(t, store, "tenant", "qrs_next_period", limits, 10, 10, quotaTestTime.Add(time.Hour))
}

func TestMemoryStoreLeaseExpiryAndCompensation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	limits := testLimits()
	limits.ActiveRuns = 1
	limits.ReservationTTL = time.Minute
	assertAllowed(t, store, "tenant", "qrs_lease_one", limits, 1, 1, quotaTestTime)
	decision, err := store.Reserve(ctx, "tenant", "qrs_lease_two", limits, 1, 1, quotaTestTime)
	if err != nil || decision.Reason != ReasonActiveRuns {
		t.Fatalf("active decision = %+v, %v", decision, err)
	}
	assertAllowed(t, store, "tenant", "qrs_after_expiry", limits, 1, 1, quotaTestTime.Add(time.Minute))
	if err := store.Compensate(ctx, "tenant", "qrs_after_expiry", quotaTestTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Compensate(ctx, "tenant", "qrs_after_expiry", quotaTestTime.Add(time.Minute)); err != nil {
		t.Fatalf("idempotent Compensate() error = %v", err)
	}
	assertAllowed(t, store, "tenant", "qrs_after_compensate", limits, 1, 1, quotaTestTime.Add(time.Minute))
	if _, err := store.Reserve(ctx, "tenant", "qrs_after_expiry", limits, 1, 1, quotaTestTime.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("Reserve() compensated receipt error = %v", err)
	}
}

func TestServiceConcurrentTenantCapAndFairness(t *testing.T) {
	global := testLimits()
	global.ActiveRuns = 1_000
	global.Burst = 1_000
	global.RequestsPerSecond = 1_000
	tenant := global
	tenant.ActiveRuns = 7
	service := mustService(t, NewMemoryStore(), global, tenant)

	if got := concurrentAdmissions(t, service, "ten_0001", 100, quotaTestTime); got != 7 {
		t.Fatalf("tenant one allowed = %d, want 7", got)
	}
	if got := concurrentAdmissions(t, service, "ten_0002", 100, quotaTestTime); got != 7 {
		t.Fatalf("tenant two allowed = %d, want 7", got)
	}
}

func TestServiceFailsClosedOnInvalidPolicy(t *testing.T) {
	service, err := NewService(NewMemoryStore(), PolicyResolverFunc(func(context.Context, domain.TenantID) (Limits, error) {
		return Limits{}, nil
	}), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Admit(context.Background(), testRequest("ten_0001", "qrs_policy", quotaTestTime)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Admit() error = %v, want invalid", err)
	}
}

func TestReservationReplayAndValidation(t *testing.T) {
	store := NewMemoryStore()
	limits := testLimits()
	request := testRequest("ten_0001", "qrs_replay", quotaTestTime)
	service := mustService(t, store, limits, limits)
	first, err := service.Admit(context.Background(), request)
	if err != nil || !first.Allowed {
		t.Fatalf("first Admit() = %+v, %v", first, err)
	}
	replay, err := service.Admit(context.Background(), request)
	if err != nil || !replay.Allowed || replay.Reservation.ID != first.Reservation.ID {
		t.Fatalf("replay Admit() = %+v, %v", replay, err)
	}
	request.EstimatedTokens++
	if _, err := service.Admit(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed replay error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Admit(cancelled, testRequest("ten_0001", "qrs_cancelled", quotaTestTime)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cancelled Admit() error = %v", err)
	}
}

func TestServiceCompensateReleasesBothScopes(t *testing.T) {
	limits := testLimits()
	limits.ActiveRuns = 1
	service := mustService(t, NewMemoryStore(), limits, limits)
	decision, err := service.Admit(context.Background(), testRequest("ten_0001", "qrs_release", quotaTestTime))
	if err != nil || !decision.Allowed {
		t.Fatalf("Admit() = %+v, %v", decision, err)
	}
	if err := service.Compensate(context.Background(), decision.Reservation, quotaTestTime); err != nil {
		t.Fatal(err)
	}
	if err := service.Compensate(context.Background(), decision.Reservation, quotaTestTime); err != nil {
		t.Fatalf("replayed Compensate() error = %v", err)
	}
	next, err := service.Admit(context.Background(), testRequest("ten_0001", "qrs_release_next", quotaTestTime))
	if err != nil || !next.Allowed {
		t.Fatalf("Admit() after compensation = %+v, %v", next, err)
	}
}

func TestServiceCompensatesAmbiguousTenantStoreFailure(t *testing.T) {
	limits := testLimits()
	limits.ActiveRuns = 1
	base := NewMemoryStore()
	store := &tenantFailureStore{Store: base, failScope: "ten_0001", failAfterReserve: true}
	service := mustService(t, store, limits, limits)
	_, err := service.Admit(context.Background(), testRequest("ten_0001", "qrs_ambiguous", quotaTestTime))
	if !errors.Is(err, errTenantStore) {
		t.Fatalf("Admit() error = %v", err)
	}
	store.failScope = ""
	decision, err := service.Admit(context.Background(), testRequest("ten_0001", "qrs_recovered", quotaTestTime))
	if err != nil || !decision.Allowed {
		t.Fatalf("Admit() after ambiguous failure = %+v, %v", decision, err)
	}

	missing := &tenantFailureStore{Store: NewMemoryStore(), failScope: "ten_0001"}
	missingService := mustService(t, missing, limits, limits)
	if _, err := missingService.Admit(context.Background(), testRequest("ten_0001", "qrs_not_written", quotaTestTime)); !errors.Is(err, errTenantStore) {
		t.Fatalf("Admit() before-write error = %v", err)
	}
}

func TestQuotaPublicValidationAndInvalidOperations(t *testing.T) {
	limits := testLimits()
	if err := limits.Validate(); err != nil || !ValidReservationID("qrs_valid") || ValidReservationID("bad") ||
		!ValidUsage(0, 0) || ValidUsage(-1, 0) || ValidUsage(maxLuaInteger+1, 0) {
		t.Fatal("public quota validation returned unexpected result")
	}
	if _, err := NewService(nil, PolicyResolverFunc(func(context.Context, domain.TenantID) (Limits, error) {
		return limits, nil
	}), limits); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewService(nil) error = %v", err)
	}
	store := NewMemoryStore()
	if err := store.Settle(context.Background(), "tenant", "qrs_missing", 0, 0, quotaTestTime); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Settle() error = %v", err)
	}
	if err := store.Compensate(context.Background(), "tenant", "qrs_missing", quotaTestTime); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Compensate() error = %v", err)
	}
	assertAllowed(t, store, "tenant", "qrs_invalid_actual", limits, 1, 1, quotaTestTime)
	if err := store.Settle(context.Background(), "tenant", "qrs_invalid_actual", 2, 1, quotaTestTime); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized Settle() error = %v", err)
	}
	if _, err := store.Reserve(context.Background(), "tenant", "qrs_backwards", limits, 1, 1, quotaTestTime.Add(-time.Second)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("backward Reserve() error = %v", err)
	}
}

var errTenantStore = errors.New("tenant store failed")

type tenantFailureStore struct {
	Store
	failScope        string
	failAfterReserve bool
}

func (s *tenantFailureStore) Reserve(
	ctx context.Context,
	scope string,
	id string,
	limits Limits,
	tokens int64,
	cost int64,
	at time.Time,
) (StoreDecision, error) {
	if scope != s.failScope {
		return s.Store.Reserve(ctx, scope, id, limits, tokens, cost, at)
	}
	if s.failAfterReserve {
		_, _ = s.Store.Reserve(ctx, scope, id, limits, tokens, cost, at)
	}
	return StoreDecision{}, errTenantStore
}

func concurrentAdmissions(t *testing.T, service *Service, tenant string, count int, at time.Time) int64 {
	t.Helper()
	var allowed atomic.Int64
	var wait sync.WaitGroup
	wait.Add(count)
	for index := range count {
		go func() {
			defer wait.Done()
			request := testRequest(tenant, fmt.Sprintf("qrs_concurrent_%04d", index), at)
			decision, err := service.Admit(context.Background(), request)
			if err != nil {
				t.Errorf("Admit() error = %v", err)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wait.Wait()
	return allowed.Load()
}

func assertAllowed(
	t *testing.T,
	store Store,
	scope string,
	id string,
	limits Limits,
	tokens int64,
	cost int64,
	at time.Time,
) {
	t.Helper()
	decision, err := store.Reserve(context.Background(), scope, id, limits, tokens, cost, at)
	if err != nil || !decision.Allowed {
		t.Fatalf("Reserve(%q) = %+v, %v", id, decision, err)
	}
}

func mustService(t *testing.T, store Store, global Limits, tenant Limits) *Service {
	t.Helper()
	service, err := NewService(store, PolicyResolverFunc(func(context.Context, domain.TenantID) (Limits, error) {
		return tenant, nil
	}), global)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testLimits() Limits {
	return Limits{
		RequestsPerSecond: 100,
		Burst:             100,
		ActiveRuns:        100,
		PeriodTokens:      10_000,
		PeriodCostNanos:   10_000,
		Period:            time.Hour,
		ReservationTTL:    time.Minute,
	}
}

func testRequest(tenant string, id string, at time.Time) Request {
	return Request{
		TenantID:           domain.TenantID(tenant),
		ReservationID:      id,
		EstimatedTokens:    1,
		EstimatedCostNanos: 1,
		At:                 at,
	}
}
