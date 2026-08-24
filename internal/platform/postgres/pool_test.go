package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/config"
)

func TestOpenDoesNotExposeInvalidDSN(t *testing.T) {
	marker := "super-secret-password"
	_, err := Open(context.Background(), config.PostgresConfig{
		DSN:          config.NewSecret("postgres://user:" + marker + "@%zz"),
		MaxOpenConns: 1, MaxIdleConns: 0, ConnMaxLifetime: time.Minute,
		ConnectTimeout: 100 * time.Millisecond, HealthTimeout: 100 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Open() error = nil")
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("Open() leaked DSN: %q", err)
	}
}

func TestWithinTxRejectsNilWork(t *testing.T) {
	pool := &Pool{}
	if err := pool.WithinTx(context.Background(), nil, nil); err == nil {
		t.Fatal("WithinTx(nil) error = nil")
	}
}
