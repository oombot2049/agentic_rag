package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/session"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestSessionAPIStoreAtomicConcurrentCreateAndTenantIsolation(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, err := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	store, err := NewSessionAPIStore(pool, func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids atomic.Uint64
	service, err := session.NewService(store, session.IDGeneratorFunc(func() (domain.SessionID, error) {
		return domain.ParseSessionID(fmt.Sprintf("ses_%032d", ids.Add(1)))
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := session.CreateRequest{
		TenantID: "ten_0001", AuthenticatedSubjectID: "subject_0001", IdempotencyKey: "idem-key-00000001",
		Metadata: session.Metadata{"channel": "integration"},
	}
	const workers = 20
	results := make(chan session.SessionView, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			value, err := service.Create(ctx, request)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- value
		}()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent Create = %v", err)
	}
	var expected string
	for result := range results {
		if expected == "" {
			expected = result.ID
		}
		if result.ID != expected || result.ContextVersion != 0 || result.RowVersion != 1 {
			t.Errorf("result = %+v, expected ID %s", result, expected)
		}
	}
	var sessions, records int
	var metadata string
	if err := pool.QueryRowContext(ctx, `SELECT count(*),min(metadata::text) FROM sessions WHERE tenant_id='ten_0001'`).Scan(&sessions, &metadata); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_records WHERE tenant_id='ten_0001' AND status='SUCCEEDED'`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || records != 1 || metadata != `{"channel": "integration"}` {
		t.Fatalf("durable state sessions=%d records=%d metadata=%s", sessions, records, metadata)
	}

	reused := request
	reused.Metadata = session.Metadata{"channel": "changed"}
	if _, err := service.Create(ctx, reused); !apperror.IsCode(err, apperror.CodeIdempotencyKeyReused) {
		t.Fatalf("changed replay = %v", err)
	}
	if _, err := service.Get(ctx, "ten_0002", expected); !apperror.IsCode(err, apperror.CodeSessionNotFound) {
		t.Fatalf("cross-tenant Get = %v", err)
	}
	got, err := service.Get(ctx, "ten_0001", expected)
	if err != nil || got.ID != expected {
		t.Fatalf("tenant Get = %+v, err=%v", got, err)
	}
}

func sessionAPITestPool(t *testing.T) *postgres.Pool {
	t.Helper()
	if os.Getenv("AGENTICRAG_REQUIRE_DOCKER") != "1" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcpostgres.Run(ctx, "postgres:18.6-alpine",
		tcpostgres.WithDatabase("agenticrag"), tcpostgres.WithUsername("agenticrag"),
		tcpostgres.WithPassword("integration-secret"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testcontainers.CleanupContainer(t, container) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.LoadSources(config.MapSource{
		"AGENTICRAG_POSTGRES_DSN": dsn, "AGENTICRAG_POSTGRES_MAX_OPEN_CONNS": "25",
		"AGENTICRAG_POSTGRES_MAX_IDLE_CONNS": "5",
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Open(ctx, configuration.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}
