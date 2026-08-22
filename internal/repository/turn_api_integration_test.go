package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/turn"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestTurnAPIStoreAtomicConcurrencyAndFaultRollback(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, err := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	conversation, _ := NewConversation(pool)
	createdAt := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	for _, id := range []domain.SessionID{"ses_0001", "ses_0002"} {
		value, err := domain.NewSession(id, "ten_0001", createdAt)
		if err != nil {
			t.Fatal(err)
		}
		if err := conversation.InsertSession(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewTurnAPIStore(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids atomic.Uint64
	service, err := turn.NewService(store, turn.IDGeneratorFunc(func() (turn.IDs, error) {
		number := ids.Add(1)
		suffix := fmt.Sprintf("%032d", number)
		return turn.IDs{TurnID: domain.TurnID("trn_" + suffix), MessageID: domain.MessageID("msg_" + suffix),
			RunID: domain.RunID("run_" + suffix), EventID: "evt_" + suffix}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := turn.CreateRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "subject_0001", SessionID: "ses_0001",
		IdempotencyKey: "idem-key-00000001", ExpectedContextVersion: 0, MessageContent: "integration question"}
	const workers = 20
	responses := make(chan turn.CreateResponse, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := service.Create(ctx, request)
			if err != nil {
				errorsFound <- err
				return
			}
			responses <- response
		}()
	}
	group.Wait()
	close(responses)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent Create = %v", err)
	}
	var expected turn.CreateResponse
	for response := range responses {
		if expected.RunID == "" {
			expected = response
		}
		if response != expected {
			t.Errorf("response=%+v expected=%+v", response, expected)
		}
	}
	if expected.TurnSequence != 1 || expected.BaseContextVersion != 0 {
		t.Fatalf("allocated response = %+v", expected)
	}
	assertTurnDurableState(t, ctx, pool, expected)

	busy := request
	busy.IdempotencyKey = "idem-key-00000002"
	if _, err := service.Create(ctx, busy); !apperror.IsCode(err, apperror.CodeSessionBusy) {
		t.Fatalf("competing command = %v", err)
	}
	stale := busy
	stale.IdempotencyKey = "idem-key-00000003"
	stale.ExpectedContextVersion = 1
	if _, err := service.Create(ctx, stale); !apperror.IsCode(err, apperror.CodeSessionVersionConflict) {
		t.Fatalf("stale command = %v", err)
	}

	if _, err := pool.ExecContext(ctx, `ALTER TABLE outbox_events ADD CONSTRAINT reject_run_queued_for_test CHECK (payload->>'session_id' <> 'ses_0002')`); err != nil {
		t.Fatal(err)
	}
	failing := request
	failing.SessionID = "ses_0002"
	failing.IdempotencyKey = "idem-key-00000004"
	if _, err := service.Create(ctx, failing); !apperror.IsCode(err, apperror.CodeInternal) {
		t.Fatalf("injected Outbox failure = %v", err)
	}
	if _, err := pool.ExecContext(ctx, `ALTER TABLE outbox_events DROP CONSTRAINT reject_run_queued_for_test`); err != nil {
		t.Fatal(err)
	}
	var active sql.NullString
	var nextSequence, rowVersion int
	if err := pool.QueryRowContext(ctx, `SELECT active_run_id,next_turn_sequence,row_version FROM sessions WHERE tenant_id='ten_0001' AND id='ses_0002'`).Scan(&active, &nextSequence, &rowVersion); err != nil {
		t.Fatal(err)
	}
	if active.Valid || nextSequence != 1 || rowVersion != 1 {
		t.Fatalf("failed command leaked Session mutation active=%v next=%d row=%d", active, nextSequence, rowVersion)
	}
	var leaked int
	if err := pool.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM turns WHERE tenant_id='ten_0001' AND session_id='ses_0002') +
		(SELECT count(*) FROM messages WHERE tenant_id='ten_0001' AND session_id='ses_0002') +
		(SELECT count(*) FROM runs WHERE tenant_id='ten_0001' AND session_id='ses_0002') +
		(SELECT count(*) FROM idempotency_records WHERE tenant_id='ten_0001' AND idempotency_key='idem-key-00000004')`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("failed command leaked %d rows", leaked)
	}
}

func assertTurnDurableState(t *testing.T, ctx context.Context, pool *postgres.Pool, response turn.CreateResponse) {
	t.Helper()
	for table, want := range map[string]int{"turns": 1, "messages": 1, "runs": 1, "outbox_events": 1, "idempotency_records": 1} {
		var count int
		if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE tenant_id='ten_0001'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Errorf("%s rows=%d want=%d", table, count, want)
		}
	}
	var active string
	var nextSequence, rowVersion int
	if err := pool.QueryRowContext(ctx, `SELECT active_run_id,next_turn_sequence,row_version FROM sessions WHERE tenant_id='ten_0001' AND id='ses_0001'`).Scan(&active, &nextSequence, &rowVersion); err != nil {
		t.Fatal(err)
	}
	if active != response.RunID || nextSequence != 2 || rowVersion != 2 {
		t.Errorf("Session reservation active=%s next=%d row=%d", active, nextSequence, rowVersion)
	}
	var maxSteps int
	if err := pool.QueryRowContext(ctx, `SELECT max_steps FROM runs WHERE tenant_id='ten_0001' AND id=$1`, response.RunID).Scan(&maxSteps); err != nil || maxSteps != 12 {
		t.Errorf("Run max_steps=%d err=%v", maxSteps, err)
	}
	var payload []byte
	if err := pool.QueryRowContext(ctx, `SELECT payload FROM outbox_events WHERE tenant_id='ten_0001'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event struct {
		SchemaVersion int    `json:"schema_version"`
		RunID         string `json:"run_id"`
		MaxSteps      int    `json:"max_steps"`
	}
	if err := json.Unmarshal(payload, &event); err != nil || event.SchemaVersion != 1 || event.RunID != response.RunID || event.MaxSteps != 12 {
		t.Errorf("Outbox payload=%s decoded=%+v err=%v", payload, event, err)
	}
}
