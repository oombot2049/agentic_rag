package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/agenticrag/agenticrag/internal/domain"
	platformpostgres "github.com/agenticrag/agenticrag/internal/platform/postgres"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestPostgresRunQueuePartitionRedeliveryFencingAndCleanup(t *testing.T) {
	pool := queueTestPool(t)
	ctx := context.Background()
	applyQueueMigrations(t, ctx, pool)
	created := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	seedQueueRun(t, ctx, pool, "ten_0001", "ses_0001", "trn_0001", "run_0001", 1, created)
	seedQueueRun(t, ctx, pool, "ten_0001", "ses_0001", "trn_0001", "run_0002", 2, created)
	seedQueueRun(t, ctx, pool, "ten_0002", "ses_0002", "trn_0002", "run_0003", 1, created)
	adapter := openQueueAdapter(t, pool, DefaultConfig())
	trace := TraceContext{TraceParent: "00-11111111111111111111111111111111-2222222222222222-01", TraceState: "vendor=value"}
	first := Message{DeliveryID: "evt_0001", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001",
		Payload: []byte(`{"schema_version":1,"run_id":"run_0001"}`), Trace: trace, CreatedAt: created, AvailableAt: created}
	second := Message{DeliveryID: "evt_0002", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0002",
		Payload: []byte(`{"schema_version":1,"run_id":"run_0002"}`), CreatedAt: created.Add(time.Microsecond), AvailableAt: created.Add(time.Microsecond)}
	if err := adapter.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Enqueue(ctx, second); err != nil {
		t.Fatal(err)
	}
	ready, err := adapter.ReadyTenants(ctx, ReadyTenantsCommand{At: created.Add(time.Second), Limit: 10})
	if err != nil || len(ready) != 1 || ready[0] != first.TenantID {
		t.Fatalf("ReadyTenants=%v error=%v", ready, err)
	}
	if err := adapter.Enqueue(ctx, first); err != nil {
		t.Fatalf("idempotent replay=%v", err)
	}
	conflict := first
	conflict.DeliveryID = "evt_other1"
	if err := adapter.Enqueue(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("identity conflict=%v", err)
	}

	claimed, err := adapter.Claim(ctx, ClaimCommand{TenantID: "ten_0001", Owner: "worker-a", Limit: 10, Duration: 10 * time.Second, At: created.Add(time.Second)})
	if err != nil || len(claimed) != 1 || claimed[0].RunID != "run_0001" || claimed[0].Trace != trace || claimed[0].LeaseToken != 1 {
		t.Fatalf("first Claim=%+v error=%v", claimed, err)
	}
	if err := adapter.Ack(ctx, LeaseCommand{TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-a", Token: 1, At: created.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	claimed, err = adapter.Claim(ctx, ClaimCommand{TenantID: "ten_0001", Owner: "worker-a", Limit: 10, Duration: 10 * time.Second, At: created.Add(2 * time.Second)})
	if err != nil || len(claimed) != 1 || claimed[0].RunID != "run_0002" || claimed[0].LeaseToken != 1 {
		t.Fatalf("second Claim=%+v error=%v", claimed, err)
	}
	if err := adapter.Nack(ctx, NackCommand{LeaseCommand: LeaseCommand{TenantID: "ten_0001", RunID: "run_0002", Owner: "worker-a", Token: 1,
		At: created.Add(3 * time.Second)}, RetryAfter: 5 * time.Second, Reason: "provider unavailable"}); err != nil {
		t.Fatal(err)
	}
	tooEarly, err := adapter.Claim(ctx, ClaimCommand{TenantID: "ten_0001", Owner: "worker-b", Limit: 1, Duration: 10 * time.Second, At: created.Add(7 * time.Second)})
	if err != nil || len(tooEarly) != 0 {
		t.Fatalf("early retry=%+v error=%v", tooEarly, err)
	}
	claimed, err = adapter.Claim(ctx, ClaimCommand{TenantID: "ten_0001", Owner: "worker-b", Limit: 1, Duration: 10 * time.Second, At: created.Add(8 * time.Second)})
	if err != nil || len(claimed) != 1 || claimed[0].LeaseToken != 2 || claimed[0].DeliveryCount != 2 {
		t.Fatalf("Nack redelivery=%+v error=%v", claimed, err)
	}
	reclaimed, err := adapter.Claim(ctx, ClaimCommand{TenantID: "ten_0001", Owner: "worker-c", Limit: 1, Duration: 10 * time.Second, At: created.Add(19 * time.Second)})
	if err != nil || len(reclaimed) != 1 || reclaimed[0].LeaseToken != 3 || reclaimed[0].DeliveryCount != 3 {
		t.Fatalf("expired redelivery=%+v error=%v", reclaimed, err)
	}
	if err := adapter.Ack(ctx, LeaseCommand{TenantID: "ten_0001", RunID: "run_0002", Owner: "worker-b", Token: 2, At: created.Add(19 * time.Second)}); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale Ack=%v", err)
	}
	renewed, err := adapter.Renew(ctx, RenewCommand{LeaseCommand: LeaseCommand{TenantID: "ten_0001", RunID: "run_0002", Owner: "worker-c", Token: 3,
		At: created.Add(20 * time.Second)}, Duration: 20 * time.Second})
	if err != nil || !renewed.LeaseExpires.Equal(created.Add(40*time.Second)) {
		t.Fatalf("Renew=%+v error=%v", renewed, err)
	}
	if err := adapter.Ack(ctx, LeaseCommand{TenantID: "ten_0001", RunID: "run_0002", Owner: "worker-c", Token: 3, At: created.Add(21 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if foreign, err := adapter.Claim(ctx, ClaimCommand{TenantID: "ten_0002", Owner: "worker-z", Limit: 10, Duration: 10 * time.Second, At: created.Add(time.Minute)}); err != nil || len(foreign) != 0 {
		t.Fatalf("foreign Claim=%+v error=%v", foreign, err)
	}
	deleted, err := adapter.Cleanup(ctx, CleanupCommand{TenantID: "ten_0001", Cutoff: created.Add(time.Hour), Limit: 10})
	if err != nil || deleted != 2 {
		t.Fatalf("Cleanup=%d error=%v", deleted, err)
	}
}

func TestPostgresRunQueueConcurrentCapacityIsHardBound(t *testing.T) {
	pool := queueTestPool(t)
	ctx := context.Background()
	applyQueueMigrations(t, ctx, pool)
	created := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	const candidates = 20
	for index := 1; index <= candidates; index++ {
		seedQueueRun(t, ctx, pool, "ten_0001", fmt.Sprintf("ses_cap%02d", index), fmt.Sprintf("trn_cap%02d", index), fmt.Sprintf("run_cap%02d", index), 1, created)
	}
	adapter := openQueueAdapter(t, pool, Config{MaxPendingPerTenant: 10, MaxPendingGlobal: 15})
	var succeeded, capacity atomic.Int64
	failures := make(chan error, candidates)
	var group sync.WaitGroup
	for index := 1; index <= candidates; index++ {
		group.Add(1)
		go func(number int) {
			defer group.Done()
			err := adapter.Enqueue(ctx, Message{DeliveryID: fmt.Sprintf("evt_cap%02d", number), TenantID: "ten_0001",
				SessionID: domain.SessionID(fmt.Sprintf("ses_cap%02d", number)), TurnID: domain.TurnID(fmt.Sprintf("trn_cap%02d", number)),
				RunID: domain.RunID(fmt.Sprintf("run_cap%02d", number)), Payload: []byte(`{"schema_version":1}`), CreatedAt: created, AvailableAt: created})
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, ErrTenantCapacity):
				capacity.Add(1)
			default:
				failures <- err
			}
		}(index)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("Enqueue=%v", err)
	}
	if succeeded.Load() != 10 || capacity.Load() != 10 {
		t.Fatalf("succeeded=%d capacity=%d", succeeded.Load(), capacity.Load())
	}
	for index := 1; index <= 6; index++ {
		sessionID, turnID, runID := fmt.Sprintf("ses_glb%02d", index), fmt.Sprintf("trn_glb%02d", index), fmt.Sprintf("run_glb%02d", index)
		seedQueueRun(t, ctx, pool, "ten_0002", sessionID, turnID, runID, 1, created)
		err := adapter.Enqueue(ctx, Message{DeliveryID: fmt.Sprintf("evt_glb%02d", index), TenantID: "ten_0002",
			SessionID: domain.SessionID(sessionID), TurnID: domain.TurnID(turnID), RunID: domain.RunID(runID),
			Payload: []byte(`{"schema_version":1}`), CreatedAt: created, AvailableAt: created})
		if index <= 5 && err != nil {
			t.Fatalf("global fill %d error=%v", index, err)
		}
		if index == 6 && !errors.Is(err, ErrGlobalCapacity) {
			t.Fatalf("global overflow error=%v", err)
		}
	}
	var pending int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM run_queue_messages WHERE tenant_id='ten_0001' AND status<>'ACKED'`).Scan(&pending); err != nil || pending != 10 {
		t.Fatalf("pending=%d error=%v", pending, err)
	}
}

func queueTestPool(t *testing.T) *platformpostgres.Pool {
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
	configuration, err := config.LoadSources(config.MapSource{"AGENTICRAG_POSTGRES_DSN": dsn, "AGENTICRAG_POSTGRES_MAX_OPEN_CONNS": "30"})
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

func applyQueueMigrations(t *testing.T, ctx context.Context, pool *platformpostgres.Pool) {
	t.Helper()
	migrator, _ := platformpostgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
}

func openQueueAdapter(t *testing.T, pool *platformpostgres.Pool, configuration Config) *PostgresAdapter {
	t.Helper()
	adapter, err := NewPostgresAdapter(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx platformpostgres.DBTX) error { return work(ctx, tx) })
	}, configuration)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func seedQueueRun(t *testing.T, ctx context.Context, pool *platformpostgres.Pool, tenantID, sessionID, turnID, runID string, runNo int, at time.Time) {
	t.Helper()
	if _, err := pool.ExecContext(ctx, `INSERT INTO sessions(tenant_id,id,created_at,updated_at) VALUES($1,$2,$3,$3) ON CONFLICT DO NOTHING`, tenantID, sessionID, at); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at)
        VALUES($1,$2,$3,1,0,$4,'RUNNING',$5,$5) ON CONFLICT DO NOTHING`, tenantID, turnID, sessionID, "msg_"+turnID[4:], at); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,created_at,updated_at)
        VALUES($1,$2,$3,$4,$5,0,'QUEUED',$6,$6)`, tenantID, runID, sessionID, turnID, runNo, at); err != nil {
		t.Fatal(err)
	}
}
