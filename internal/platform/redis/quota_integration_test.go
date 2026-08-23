package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/agenticrag/agenticrag/internal/quota"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestQuotaStoreIntegration(t *testing.T) {
	store := newQuotaIntegrationStore(t)
	at := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)

	t.Run("atomic active run cap and tenant isolation", func(t *testing.T) {
		limits := redisTestLimits()
		limits.ActiveRuns = 12
		firstAllowed := concurrentRedisReservations(t, store, "ten_1001", "active_a", 200, limits, 1, 1, at)
		if len(firstAllowed) != 12 {
			t.Fatalf("tenant one allowed = %d, want 12", len(firstAllowed))
		}
		secondAllowed := concurrentRedisReservations(t, store, "ten_1002", "active_b", 200, limits, 1, 1, at)
		if len(secondAllowed) != 12 {
			t.Fatalf("tenant two allowed = %d, want 12", len(secondAllowed))
		}
		for _, id := range firstAllowed {
			if err := store.Settle(context.Background(), "ten_1001", id, 1, 1, at); err != nil {
				t.Fatal(err)
			}
		}
		afterSettle := concurrentRedisReservations(t, store, "ten_1001", "active_c", 12, limits, 1, 1, at)
		if len(afterSettle) != 12 {
			t.Fatalf("after settlement allowed = %d, want 12", len(afterSettle))
		}
	})

	t.Run("atomic QPS and budgets", func(t *testing.T) {
		qps := redisTestLimits()
		qps.RequestsPerSecond = 1
		qps.Burst = 7
		if got := len(concurrentRedisReservations(t, store, "ten_2001", "qps", 100, qps, 0, 0, at)); got != 7 {
			t.Fatalf("QPS allowed = %d, want 7", got)
		}
		budget := redisTestLimits()
		budget.PeriodTokens = 50
		budget.PeriodCostNanos = 50
		if got := len(concurrentRedisReservations(t, store, "ten_2002", "budget", 100, budget, 7, 7, at)); got != 7 {
			t.Fatalf("budget allowed = %d, want 7", got)
		}
	})

	t.Run("receipts are idempotent and conflict safe", func(t *testing.T) {
		ctx := context.Background()
		limits := redisTestLimits()
		decision, err := store.Reserve(ctx, "ten_3001", "qrs_replay", limits, 10, 20, at)
		if err != nil || !decision.Allowed {
			t.Fatalf("Reserve() = %+v, %v", decision, err)
		}
		decision, err = store.Reserve(ctx, "ten_3001", "qrs_replay", limits, 10, 20, at)
		if err != nil || !decision.Allowed {
			t.Fatalf("replay Reserve() = %+v, %v", decision, err)
		}
		if _, err := store.Reserve(ctx, "ten_3001", "qrs_replay", limits, 11, 20, at); !errors.Is(err, quota.ErrConflict) {
			t.Fatalf("changed Reserve() error = %v", err)
		}
		if err := store.Settle(ctx, "ten_3001", "qrs_replay", 4, 5, at); err != nil {
			t.Fatal(err)
		}
		if err := store.Settle(ctx, "ten_3001", "qrs_replay", 4, 5, at); err != nil {
			t.Fatalf("replayed Settle() error = %v", err)
		}
		if err := store.Settle(ctx, "ten_3001", "qrs_replay", 3, 5, at); !errors.Is(err, quota.ErrConflict) {
			t.Fatalf("changed Settle() error = %v", err)
		}
		if err := store.Compensate(ctx, "ten_3001", "qrs_replay", at); !errors.Is(err, quota.ErrConflict) {
			t.Fatalf("settled Compensate() error = %v", err)
		}
		if decision, err = store.Reserve(ctx, "ten_3001", "qrs_compensate", limits, 1, 1, at); err != nil || !decision.Allowed {
			t.Fatalf("Reserve() to compensate = %+v, %v", decision, err)
		}
		if err := store.Compensate(ctx, "ten_3001", "qrs_compensate", at); err != nil {
			t.Fatal(err)
		}
		if err := store.Compensate(ctx, "ten_3001", "qrs_compensate", at); err != nil {
			t.Fatalf("replayed Compensate() error = %v", err)
		}
	})

	t.Run("large counters remain exact", func(t *testing.T) {
		ctx := context.Background()
		limits := redisTestLimits()
		limits.PeriodTokens = 9_000_000_000_000_000
		limits.PeriodCostNanos = 9_000_000_000_000_000
		decision, err := store.Reserve(ctx, "ten_4001", "qrs_large_one", limits,
			8_000_000_000_000_000, 8_000_000_000_000_000, at)
		if err != nil || !decision.Allowed {
			t.Fatalf("large Reserve() = %+v, %v", decision, err)
		}
		if err := store.Settle(ctx, "ten_4001", "qrs_large_one",
			7_000_000_000_000_000, 7_000_000_000_000_000, at); err != nil {
			t.Fatal(err)
		}
		decision, err = store.Reserve(ctx, "ten_4001", "qrs_large_two", limits,
			2_000_000_000_000_000, 2_000_000_000_000_000, at)
		if err != nil || !decision.Allowed {
			t.Fatalf("large Reserve() after refund = %+v, %v", decision, err)
		}
	})
}

func newQuotaIntegrationStore(t *testing.T) *QuotaStore {
	t.Helper()
	if os.Getenv("AGENTICRAG_REQUIRE_DOCKER") != "1" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	container, err := testcontainers.Run(ctx, "valkey/valkey:9.1.0-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, container)
	endpoint, err := container.Endpoint(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadSources(config.MapSource{
		"AGENTICRAG_POSTGRES_DSN":    "postgres://unused",
		"AGENTICRAG_REDIS_ADDRESS":   endpoint,
		"AGENTICRAG_REDIS_POOL_SIZE": "32",
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := Open(ctx, cfg.Redis)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	keys, err := NewKeyBuilder("quota_test")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewQuotaStore(client, keys)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func concurrentRedisReservations(
	t *testing.T,
	store *QuotaStore,
	scope string,
	prefix string,
	count int,
	limits quota.Limits,
	tokens int64,
	cost int64,
	at time.Time,
) []string {
	t.Helper()
	allowed := make(chan string, count)
	var failures atomic.Int64
	var group sync.WaitGroup
	group.Add(count)
	for index := range count {
		go func() {
			defer group.Done()
			id := fmt.Sprintf("qrs_%s_%04d", prefix, index)
			decision, err := store.Reserve(context.Background(), scope, id, limits, tokens, cost, at)
			if err != nil {
				failures.Add(1)
				return
			}
			if decision.Allowed {
				allowed <- id
			}
		}()
	}
	group.Wait()
	close(allowed)
	if failures.Load() != 0 {
		t.Fatalf("Reserve() failures = %d", failures.Load())
	}
	result := make([]string, 0, len(allowed))
	for id := range allowed {
		result = append(result, id)
	}
	return result
}

func redisTestLimits() quota.Limits {
	return quota.Limits{
		RequestsPerSecond: 1_000,
		Burst:             1_000,
		ActiveRuns:        1_000,
		PeriodTokens:      1_000_000,
		PeriodCostNanos:   1_000_000,
		Period:            time.Hour,
		ReservationTTL:    time.Minute,
	}
}
