// Package postgres provides PostgreSQL pool, transaction and migration infrastructure.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Pool struct {
	db            *sql.DB
	healthTimeout time.Duration
}

// Open creates, configures and verifies a PostgreSQL pool.
func Open(ctx context.Context, cfg config.PostgresConfig) (*Pool, error) {
	db, err := sql.Open("pgx", cfg.DSN.Reveal())
	if err != nil {
		return nil, &operationError{operation: "open driver", cause: err}
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	connectCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := db.PingContext(connectCtx); err != nil {
		_ = db.Close()
		return nil, &operationError{operation: "verify connection", cause: err}
	}
	return &Pool{db: db, healthTimeout: cfg.HealthTimeout}, nil
}

func (p *Pool) Close() error { return p.db.Close() }

func (p *Pool) Health(ctx context.Context) error {
	healthCtx, cancel := context.WithTimeout(ctx, p.healthTimeout)
	defer cancel()
	if err := p.db.PingContext(healthCtx); err != nil {
		return &operationError{operation: "health check", cause: err}
	}
	return nil
}

func (p *Pool) Stats() sql.DBStats { return p.db.Stats() }

type operationError struct {
	operation string
	cause     error
}

func (e *operationError) Error() string { return fmt.Sprintf("postgres: %s failed", e.operation) }
func (e *operationError) Unwrap() error { return e.cause }
