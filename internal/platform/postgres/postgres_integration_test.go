package postgres

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func integrationPool(t *testing.T) *Pool {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
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
	cfg, err := config.LoadSources(config.MapSource{
		"AGENTICRAG_POSTGRES_DSN": dsn, "AGENTICRAG_POSTGRES_MAX_OPEN_CONNS": "7",
		"AGENTICRAG_POSTGRES_MAX_IDLE_CONNS": "3",
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := Open(ctx, cfg.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func TestPostgresPoolTransactionsAndMigrations(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	if err := pool.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := pool.Stats(); stats.MaxOpenConnections != 7 {
		t.Fatalf("max open = %d", stats.MaxOpenConnections)
	}
	files := fstest.MapFS{
		"migrations/000001_widgets.up.sql":   {Data: []byte("CREATE TABLE widgets(id bigint PRIMARY KEY, name text NOT NULL)")},
		"migrations/000001_widgets.down.sql": {Data: []byte("DROP TABLE widgets")},
		"migrations/000002_index.up.sql":     {Data: []byte("CREATE INDEX widgets_name_idx ON widgets(name)")},
		"migrations/000002_index.down.sql":   {Data: []byte("DROP INDEX widgets_name_idx")},
	}
	migrator, err := NewMigrator(pool, files, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("idempotent Up: %v", err)
	}
	drifted := fstest.MapFS{
		"migrations/000001_widgets.up.sql":   {Data: []byte("CREATE TABLE widgets(id bigint PRIMARY KEY)")},
		"migrations/000001_widgets.down.sql": {Data: []byte("DROP TABLE widgets")},
		"migrations/000002_index.up.sql":     files["migrations/000002_index.up.sql"],
		"migrations/000002_index.down.sql":   files["migrations/000002_index.down.sql"],
	}
	driftMigrator, err := NewMigrator(pool, drifted, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err := driftMigrator.Up(ctx); err == nil {
		t.Fatal("checksum drift error = nil")
	}
	rollbackMarker := errors.New("rollback marker")
	err = pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO widgets(id,name) VALUES(1,'rolled back')`)
		if err != nil {
			return err
		}
		return rollbackMarker
	})
	if !errors.Is(err, rollbackMarker) {
		t.Fatalf("WithinTx = %v", err)
	}
	var count int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM widgets`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback count=%d err=%v", count, err)
	}
	if err := pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO widgets(id,name) VALUES(2,'committed')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("transaction panic was not propagated")
			}
		}()
		_ = pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
			_, _ = tx.ExecContext(ctx, `INSERT INTO widgets(id,name) VALUES(3,'panic rollback')`)
			panic("panic marker")
		})
	}()
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM widgets WHERE id=3`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("panic rollback count=%d err=%v", count, err)
	}
	if err := migrator.Down(ctx, 1); err != nil {
		t.Fatal(err)
	}
}
