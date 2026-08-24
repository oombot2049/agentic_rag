package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/outbox"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestTransactionalOutboxAtomicityConcurrentClaimsTakeoverAndCleanup(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	migrator, err := NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	rollbackMarker := errors.New("injected business failure")
	err = pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(tenant_id,id,created_at,updated_at) VALUES('ten_9001','ses_9001',$1,$1)`, at); err != nil {
			return err
		}
		repository, err := outbox.NewRepository(tx)
		if err != nil {
			return err
		}
		if err := repository.Enqueue(ctx, integrationEvent("ten_9001", "evt_9001", at)); err != nil {
			return err
		}
		return rollbackMarker
	})
	if !errors.Is(err, rollbackMarker) {
		t.Fatalf("rollback error = %v", err)
	}
	assertTableCount(t, ctx, pool, "sessions", "tenant_id='ten_9001'", 0)
	assertTableCount(t, ctx, pool, "outbox_events", "tenant_id='ten_9001'", 0)

	if err := pool.WithinTx(ctx, nil, func(ctx context.Context, tx DBTX) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(tenant_id,id,created_at,updated_at) VALUES('ten_9001','ses_9001',$1,$1)`, at); err != nil {
			return err
		}
		repository, err := outbox.NewRepository(tx)
		if err != nil {
			return err
		}
		return repository.Enqueue(ctx, integrationEvent("ten_9001", "evt_9001", at))
	}); err != nil {
		t.Fatalf("commit business and outbox: %v", err)
	}
	assertTableCount(t, ctx, pool, "sessions", "tenant_id='ten_9001'", 1)
	assertTableCount(t, ctx, pool, "outbox_events", "tenant_id='ten_9001'", 1)

	repository, err := outbox.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 40; index++ {
		event := integrationEvent("ten_0001", fmt.Sprintf("evt_race%03d", index), at)
		if err := repository.Enqueue(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	var wait sync.WaitGroup
	claimed := make(chan string, 40)
	errorsFound := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			events, err := repository.ClaimBatch(ctx, outbox.ClaimCommand{
				TenantID: "ten_0001", Owner: fmt.Sprintf("relay-%d", worker), Duration: time.Minute, Limit: 10,
			})
			if err != nil {
				errorsFound <- err
				return
			}
			for _, event := range events {
				claimed <- event.ID
			}
		}(worker)
	}
	wait.Wait()
	close(claimed)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent claim: %v", err)
	}
	seen := make(map[string]struct{}, 40)
	for id := range claimed {
		if _, duplicate := seen[id]; duplicate {
			t.Errorf("event claimed twice: %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != 40 {
		t.Fatalf("claimed events = %d, want 40", len(seen))
	}

	for sequence := 1; sequence <= 3; sequence++ {
		event := outbox.Event{TenantID: "ten_0003", ID: fmt.Sprintf("evt_stream%02d", sequence),
			AggregateType: "run", AggregateID: "run_stream", Type: "RunEvent.v1",
			Payload:     []byte(fmt.Sprintf(`{"schema_version":1,"type":"run.started","turn_id":"trn_stream","run_id":"run_stream","sequence":%d,"occurred_at":"2026-08-24T00:00:00Z","payload":{}}`, sequence)),
			AvailableAt: at, OccurredAt: at}
		if err := repository.Enqueue(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	ordered, err := repository.ClaimBatch(ctx, outbox.ClaimCommand{TenantID: "ten_0003", Owner: "relay-order", Duration: time.Minute, Limit: 10})
	if err != nil || len(ordered) != 1 || ordered[0].ID != "evt_stream01" {
		t.Fatalf("first ordered claim=%+v error=%v", ordered, err)
	}
	if err := repository.MarkPublished(ctx, outbox.Acknowledgement{TenantID: "ten_0003", EventID: ordered[0].ID, Owner: "relay-order", Token: ordered[0].ClaimToken}); err != nil {
		t.Fatal(err)
	}
	ordered, err = repository.ClaimBatch(ctx, outbox.ClaimCommand{TenantID: "ten_0003", Owner: "relay-order", Duration: time.Minute, Limit: 10})
	if err != nil || len(ordered) != 1 || ordered[0].ID != "evt_stream02" {
		t.Fatalf("second ordered claim=%+v error=%v", ordered, err)
	}

	if err := repository.Enqueue(ctx, integrationEvent("ten_0002", "evt_takeover", at)); err != nil {
		t.Fatal(err)
	}
	first, err := repository.ClaimBatch(ctx, outbox.ClaimCommand{TenantID: "ten_0002", Owner: "relay-old", Duration: 5 * time.Second, Limit: 1})
	if err != nil || len(first) != 1 || first[0].ClaimToken != 1 {
		t.Fatalf("first claim = %+v/%v", first, err)
	}
	if _, err := pool.ExecContext(ctx, `UPDATE outbox_events SET claim_expires_at=CURRENT_TIMESTAMP WHERE tenant_id='ten_0002' AND id='evt_takeover'`); err != nil {
		t.Fatal(err)
	}
	second, err := repository.ClaimBatch(ctx, outbox.ClaimCommand{TenantID: "ten_0002", Owner: "relay-new", Duration: time.Minute, Limit: 1})
	if err != nil || len(second) != 1 || second[0].ClaimToken != 2 || second[0].AttemptCount != 2 {
		t.Fatalf("takeover = %+v/%v", second, err)
	}
	oldAck := outbox.Acknowledgement{TenantID: "ten_0002", EventID: "evt_takeover", Owner: "relay-old", Token: 1}
	if err := repository.MarkPublished(ctx, oldAck); !errors.Is(err, outbox.ErrClaimFenced) {
		t.Fatalf("old relay error = %v", err)
	}
	newAck := outbox.Acknowledgement{TenantID: "ten_0002", EventID: "evt_takeover", Owner: "relay-new", Token: 2}
	if err := repository.MarkPublished(ctx, newAck); err != nil {
		t.Fatal(err)
	}
	if err := repository.Enqueue(ctx, integrationEvent("ten_0002", "evt_pending", at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	deleted, err := repository.CleanupPublished(ctx, outbox.CleanupCommand{TenantID: "ten_0002", Cutoff: time.Now().UTC().Add(time.Second), Limit: 10})
	if err != nil || deleted != 1 {
		t.Fatalf("cleanup = %d/%v", deleted, err)
	}
	if _, err := repository.Get(ctx, "ten_0002", "evt_pending"); err != nil {
		t.Fatalf("cleanup deleted pending event: %v", err)
	}
	if _, err := repository.Get(ctx, "ten_9999", "evt_pending"); !errors.Is(err, outbox.ErrNotFound) {
		t.Fatalf("cross-tenant lookup = %v", err)
	}
}

func integrationEvent(tenantID, eventID string, at time.Time) outbox.Event {
	return outbox.Event{TenantID: domain.TenantID(tenantID), ID: eventID, AggregateType: "run", AggregateID: "run_0001",
		Type: "run.queued", Payload: []byte(`{"run_id":"run_0001"}`), AvailableAt: at, OccurredAt: at}
}

func assertTableCount(t *testing.T, ctx context.Context, pool *Pool, table, predicate string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE "+predicate).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("%s count = %d, want %d", table, count, want)
	}
}
