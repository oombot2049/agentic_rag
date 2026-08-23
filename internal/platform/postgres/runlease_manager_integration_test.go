package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/repository"
	"github.com/agenticrag/agenticrag/internal/runlease"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestRunLeaseManagerPostgresCheckpointTakeoverFencesOldWorker(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	migrator, err := NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	insertInitialAggregate(t, ctx, pool, "ten_0001")
	execution, err := repository.NewExecution(pool)
	if err != nil {
		t.Fatal(err)
	}
	oldManager, err := runlease.New(execution, runlease.Config{Owner: "worker-old", LeaseDuration: 5 * time.Minute, RenewInterval: 4 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	old, err := oldManager.Acquire(ctx, runlease.AcquireRequest{TenantID: "ten_0001", RunID: "run_0001"})
	if err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"next_action":"generate","step":3}`)
	if err := old.Handle.WithAuthority(ctx, func(ctx context.Context, authority repository.WorkerAuthority) (uint64, error) {
		return authority.ExpectedExecutionVersion, execution.SaveCheckpoint(ctx, authority, 3, payload)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `UPDATE runs SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE tenant_id='ten_0001' AND id='run_0001'`); err != nil {
		t.Fatal(err)
	}
	newManager, err := runlease.New(execution, runlease.Config{Owner: "worker-new", LeaseDuration: 5 * time.Minute, RenewInterval: 4 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	taken, err := newManager.Acquire(ctx, runlease.AcquireRequest{TenantID: "ten_0001", RunID: "run_0001"})
	if err != nil {
		t.Fatal(err)
	}
	if taken.Checkpoint == nil || taken.Checkpoint.StepNo != 3 || taken.Checkpoint.LeaseToken != 1 || taken.Handle.Snapshot().Token != 2 {
		t.Fatalf("takeover checkpoint=%+v lease=%+v", taken.Checkpoint, taken.Handle.Snapshot())
	}
	err = old.Handle.WithAuthority(ctx, func(ctx context.Context, authority repository.WorkerAuthority) (uint64, error) {
		return authority.ExpectedExecutionVersion, execution.SaveCheckpoint(ctx, authority, 4, json.RawMessage(`{"stale":true}`))
	})
	if !errors.Is(err, runlease.ErrLeaseLost) || !errors.Is(err, repository.ErrLeaseFenced) {
		t.Fatalf("old worker write error = %v", err)
	}
	checkpoint, err := execution.LoadCheckpoint(ctx, "ten_0001", "run_0001")
	if err != nil || checkpoint.StepNo != 3 || string(checkpoint.Payload) != string(payload) {
		t.Fatalf("checkpoint after stale write = %+v, %v", checkpoint, err)
	}
	_ = old.Handle.Close(ctx)
	if err := taken.Handle.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
