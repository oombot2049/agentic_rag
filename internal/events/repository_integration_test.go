package events

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
	platformpostgres "github.com/agenticrag/agenticrag/internal/platform/postgres"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestEventRepositoryConcurrentAppendReplayRetentionAndTenantIsolation(t *testing.T) {
	pool := eventTestPool(t)
	ctx := context.Background()
	migrator, err := platformpostgres.NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	seedEventRun(t, ctx, pool)
	repository, _ := NewRepository(pool)
	occurredAt := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)

	const workers = 20
	failures := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(number int) {
			defer group.Done()
			_, err := repository.Append(ctx, AppendCommand{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001",
				Type: RunStarted, Payload: EmptyPayload{}, OccurredAt: occurredAt.Add(time.Duration(number) * time.Nanosecond), Retention: time.Hour})
			if err != nil {
				failures <- err
			}
		}(index)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("Append = %v", err)
	}

	bounds, err := repository.Bounds(ctx, "ten_0001", "run_0001")
	if err != nil || bounds.HighWater != workers || bounds.RetainedFrom != 1 {
		t.Fatalf("Bounds = %+v, %v", bounds, err)
	}
	replayed, err := repository.Replay(ctx, "ten_0001", "run_0001", 0, bounds.HighWater, 100)
	if err != nil || len(replayed) != workers {
		t.Fatalf("Replay count=%d error=%v", len(replayed), err)
	}
	for index, event := range replayed {
		if event.Sequence() != uint64(index+1) || event.TenantID() != "ten_0001" || event.RunID() != "run_0001" {
			t.Fatalf("event[%d]=sequence %d tenant %s Run %s", index, event.Sequence(), event.TenantID(), event.RunID())
		}
	}

	if _, err := repository.Append(ctx, AppendCommand{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001",
		Type: AssistantDelta, Payload: AssistantDeltaPayload{Text: "must stay transient"}, OccurredAt: occurredAt, Retention: time.Hour}); err == nil {
		t.Fatal("transient assistant delta was persisted")
	}
	afterInvalid, _ := repository.Bounds(ctx, "ten_0001", "run_0001")
	if afterInvalid.HighWater != workers {
		t.Fatalf("invalid append consumed sequence: %+v", afterInvalid)
	}

	pruned, err := repository.PruneExpired(ctx, "ten_0001", "run_0001", occurredAt.Add(2*time.Hour), 10)
	if err != nil || pruned.Deleted != 10 || pruned.RetainedFrom != 11 {
		t.Fatalf("PruneExpired = %+v, %v", pruned, err)
	}
	remaining, err := repository.Replay(ctx, "ten_0001", "run_0001", 10, workers, 100)
	if err != nil || len(remaining) != 10 || remaining[0].Sequence() != 11 {
		t.Fatalf("remaining=%d first=%d error=%v", len(remaining), remaining[0].Sequence(), err)
	}
	if _, err := repository.Bounds(ctx, "ten_0002", "run_0001"); err != ErrNotFound {
		t.Fatalf("cross-tenant Bounds error = %v", err)
	}
}

func eventTestPool(t *testing.T) *platformpostgres.Pool {
	t.Helper()
	if os.Getenv("AGENTICRAG_REQUIRE_DOCKER") != "1" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcpostgres.Run(ctx, "postgres:18.6-alpine", tcpostgres.WithDatabase("agenticrag"),
		tcpostgres.WithUsername("agenticrag"), tcpostgres.WithPassword("integration-secret"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, container) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.LoadSources(config.MapSource{"AGENTICRAG_POSTGRES_DSN": dsn, "AGENTICRAG_POSTGRES_MAX_OPEN_CONNS": "25"})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := platformpostgres.Open(ctx, configuration.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func seedEventRun(t *testing.T, ctx context.Context, pool *platformpostgres.Pool) {
	t.Helper()
	at := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO sessions(tenant_id,id,created_at,updated_at) VALUES($1,$2,$3,$3)`, []any{"ten_0001", "ses_0001", at}},
		{`INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at)
            VALUES($1,$2,$3,1,0,$4,'RUNNING',$5,$5)`, []any{"ten_0001", "trn_0001", "ses_0001", "msg_0001", at}},
		{`INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at)
            VALUES($1,$2,$3,$4,1,0,'RUNNING',$5,$5)`, []any{"ten_0001", "run_0001", "ses_0001", "trn_0001", at}},
	}
	for _, statement := range statements {
		if _, err := pool.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}
