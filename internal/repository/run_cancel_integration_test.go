package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	runcontrol "github.com/agenticrag/agenticrag/internal/run"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestRunCancellationConcurrentIdempotencySafePointAndSideEffectHistory(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	seedCancellationRun(t, ctx, pool, "ses_0001", "trn_0001", "run_0001", "RUNNING", true)
	store, _ := NewRunCancelStore(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	var ids atomic.Uint64
	identifier := runcontrol.EventIDGeneratorFunc(func() (string, error) {
		return fmt.Sprintf("evt_cancel%08d", ids.Add(1)), nil
	})
	service, _ := runcontrol.NewService(store, identifier)
	safePoints, _ := runcontrol.NewSafePointService(store, identifier)
	continuing, err := safePoints.Check(ctx, runcontrol.SafePointRequest{TenantID: "ten_0001", RunID: "run_0001", LeaseOwner: "worker-1", LeaseToken: 1})
	if err != nil || continuing.Stop || continuing.Status != "RUNNING" || continuing.ExecutionVersion != 1 {
		t.Fatalf("running SafePoint = %+v, %v", continuing, err)
	}
	request := runcontrol.CancelRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "subject_0001",
		RunID: "run_0001", IdempotencyKey: "cancel-key-00000001"}

	const workers = 50
	responses := make(chan runcontrol.CancelResponse, workers)
	failures := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := service.Cancel(ctx, request)
			if err != nil {
				failures <- err
				return
			}
			responses <- response
		}()
	}
	group.Wait()
	close(responses)
	close(failures)
	for err := range failures {
		t.Errorf("concurrent Cancel = %v", err)
	}
	for response := range responses {
		if response.RunID != "run_0001" || response.Status != "CANCEL_REQUESTED" || !response.CancellationRequested || response.Terminal {
			t.Errorf("response=%+v", response)
		}
	}
	assertCancellationState(t, ctx, pool, "CANCEL_REQUESTED", "RUNNING", "run_0001", 2, 1, 1)

	result, err := safePoints.Check(ctx, runcontrol.SafePointRequest{TenantID: "ten_0001", RunID: "run_0001", LeaseOwner: "worker-1", LeaseToken: 1})
	if err != nil || !result.Stop || result.Status != "CANCELLED" || result.ExecutionVersion != 3 {
		t.Fatalf("SafePoint = %+v, %v", result, err)
	}
	assertCancellationState(t, ctx, pool, "CANCELLED", "CANCELLED", "", 3, 2, 2)
	var stepStatus string
	if err := pool.QueryRowContext(ctx, `SELECT status FROM steps WHERE tenant_id='ten_0001' AND id='stp_0001'`).Scan(&stepStatus); err != nil || stepStatus != "SUCCEEDED" {
		t.Fatalf("completed side-effect Step status=%q error=%v", stepStatus, err)
	}

	repeat := request
	repeat.IdempotencyKey = "cancel-key-00000002"
	repeated, err := service.Cancel(ctx, repeat)
	if err != nil || repeated.Status != "CANCELLED" || !repeated.Terminal {
		t.Fatalf("repeat Cancel=%+v error=%v", repeated, err)
	}
	assertCancellationState(t, ctx, pool, "CANCELLED", "CANCELLED", "", 3, 2, 2)

	if _, err := safePoints.Check(ctx, runcontrol.SafePointRequest{TenantID: "ten_0001", RunID: "run_0001", LeaseOwner: "worker-1", LeaseToken: 1}); !apperror.IsCode(err, apperror.CodeRunFenced) {
		t.Fatalf("released lease SafePoint error=%v", err)
	}
	absent := request
	absent.RunID, absent.IdempotencyKey = "run_9999", "cancel-key-00000003"
	if _, err := service.Cancel(ctx, absent); !apperror.IsCode(err, apperror.CodeRunNotFound) {
		t.Fatalf("absent cancellation error=%v", err)
	}
	foreign := request
	foreign.TenantID, foreign.IdempotencyKey = "ten_0002", "cancel-key-00000004"
	if _, err := service.Cancel(ctx, foreign); !apperror.IsCode(err, apperror.CodeRunNotFound) {
		t.Fatalf("foreign cancellation error=%v", err)
	}
}

func TestQueuedRunCancellationIsImmediateAndAtomic(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	seedCancellationRun(t, ctx, pool, "ses_0002", "trn_0002", "run_0002", "QUEUED", false)
	store, _ := NewRunCancelStore(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	service, _ := runcontrol.NewService(store, runcontrol.EventIDGeneratorFunc(func() (string, error) { return "evt_queuedcancel", nil }))
	response, err := service.Cancel(ctx, runcontrol.CancelRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "subject_0001",
		RunID: "run_0002", IdempotencyKey: "cancel-key-queued001"})
	if err != nil || response.Status != "CANCELLED" || !response.Terminal {
		t.Fatalf("queued Cancel=%+v error=%v", response, err)
	}
	assertCancellationState(t, ctx, pool, "CANCELLED", "CANCELLED", "", 2, 2, 2)
}

func TestCancellationAndNextStepStartAreExecutionVersionFenced(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	seedCancellationRun(t, ctx, pool, "ses_race1", "trn_race1", "run_race1", "RUNNING", false)
	store, _ := NewRunCancelStore(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	service, _ := runcontrol.NewService(store, runcontrol.EventIDGeneratorFunc(func() (string, error) { return "evt_cancelrace1", nil }))
	execution, _ := NewExecution(pool)
	now := time.Now().UTC()
	step, err := domain.NewStep("stp_race1", "ten_0001", "run_race1", 1, domain.StepKindTool, now)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := service.Cancel(ctx, runcontrol.CancelRequest{TenantID: "ten_0001", AuthenticatedSubjectID: "subject_0001",
			RunID: "run_race1", IdempotencyKey: "cancel-key-race0001"})
		results <- err
	}()
	go func() {
		<-start
		results <- execution.InsertStep(ctx, WorkerAuthority{TenantID: "ten_0001", RunID: "run_race1", Owner: "worker-1",
			Token: 1, ExpectedExecutionVersion: 1, At: now}, step)
	}()
	close(start)
	first, second := <-results, <-results
	if first != nil && !errors.Is(first, ErrLeaseFenced) || second != nil && !errors.Is(second, ErrLeaseFenced) {
		t.Fatalf("race results = %v, %v", first, second)
	}
	if first != nil && second != nil {
		t.Fatalf("both serializations failed: %v, %v", first, second)
	}
	var steps int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM steps WHERE tenant_id='ten_0001' AND run_id='run_race1'`).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if steps < 0 || steps > 1 {
		t.Fatalf("step count=%d", steps)
	}
}

func TestCancellationOutboxFailureRollsBackStateEventsAndIdempotency(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	seedCancellationRun(t, ctx, pool, "ses_fault", "trn_fault", "run_fault", "RUNNING", false)
	now := time.Now().UTC()
	if _, err := pool.ExecContext(ctx, `INSERT INTO outbox_events(tenant_id,id,aggregate_type,aggregate_id,event_type,payload,available_at,occurred_at)
        VALUES('ten_0001','evt_collision','run','run_fault','Existing.v1','{}',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	store, _ := NewRunCancelStore(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	fingerprint, _ := idempotency.NewFingerprint(idempotency.Request{Operation: "CANCEL_RUN",
		PathParameters: map[string]string{"run_id": "run_fault"}, Body: struct{}{}})
	_, err := store.Cancel(ctx, runcontrol.CancelPersistence{TenantID: "ten_0001", RunID: "run_fault",
		Scope:       idempotency.Scope{TenantID: "ten_0001", AuthenticatedSubjectID: "subject_0001", Operation: "CANCEL_RUN", Key: "cancel-key-fault001"},
		Fingerprint: fingerprint, EventID: "evt_collision", Now: now, Retention: 7 * 24 * time.Hour})
	if err == nil {
		t.Fatal("cancellation unexpectedly committed through outbox collision")
	}
	var status string
	var version, eventCount, claimCount int
	if err := pool.QueryRowContext(ctx, `SELECT status,execution_version FROM runs WHERE tenant_id='ten_0001' AND id='run_fault'`).Scan(&status, &version); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM run_events WHERE tenant_id='ten_0001' AND run_id='run_fault'`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_records WHERE tenant_id='ten_0001' AND idempotency_key='cancel-key-fault001'`).Scan(&claimCount); err != nil {
		t.Fatal(err)
	}
	if status != "RUNNING" || version != 1 || eventCount != 0 || claimCount != 0 {
		t.Fatalf("rollback state status=%s version=%d events=%d claims=%d", status, version, eventCount, claimCount)
	}
}

func seedCancellationRun(t *testing.T, ctx context.Context, pool *postgres.Pool, sessionID, turnID, runID, status string, completedStep bool) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Hour)
	if _, err := pool.ExecContext(ctx, `INSERT INTO sessions(tenant_id,id,created_at,updated_at) VALUES('ten_0001',$1,$2,$2)`, sessionID, at); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO turns(tenant_id,id,session_id,turn_sequence,base_context_version,user_message_id,status,created_at,updated_at)
        VALUES('ten_0001',$1,$2,1,0,$3,'RUNNING',$4,$4)`, turnID, sessionID, "msg_"+runID[4:], at); err != nil {
		t.Fatal(err)
	}
	leaseOwner := any(nil)
	leaseExpires := any(nil)
	leaseToken := 0
	if status == "RUNNING" {
		leaseOwner, leaseToken, leaseExpires = "worker-1", 1, time.Now().UTC().Add(time.Hour)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO runs(tenant_id,id,session_id,turn_id,run_no,base_context_version,status,
        created_at,updated_at,lease_owner,lease_token,lease_expires_at) VALUES('ten_0001',$1,$2,$3,1,0,$4,$5,$5,$6,$7,$8)`,
		runID, sessionID, turnID, status, at, leaseOwner, leaseToken, leaseExpires); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `UPDATE sessions SET active_run_id=$1 WHERE tenant_id='ten_0001' AND id=$2`, runID, sessionID); err != nil {
		t.Fatal(err)
	}
	if completedStep {
		if _, err := pool.ExecContext(ctx, `INSERT INTO steps(tenant_id,id,run_id,step_no,kind,status,created_at,updated_at)
            VALUES('ten_0001','stp_0001',$1,1,'TOOL','SUCCEEDED',$2,$2)`, runID, at); err != nil {
			t.Fatal(err)
		}
	}
}

func assertCancellationState(t *testing.T, ctx context.Context, pool *postgres.Pool, runStatus, turnStatus, activeRun string, executionVersion uint64, eventCount, outboxCount int) {
	t.Helper()
	var gotRun, gotTurn string
	var gotActive sql.NullString
	var gotVersion uint64
	if err := pool.QueryRowContext(ctx, `SELECT r.status,r.execution_version,t.status,s.active_run_id
        FROM runs r JOIN turns t ON t.tenant_id=r.tenant_id AND t.id=r.turn_id
        JOIN sessions s ON s.tenant_id=r.tenant_id AND s.id=r.session_id
        WHERE r.tenant_id='ten_0001' AND r.id IN ('run_0001','run_0002') ORDER BY r.id DESC LIMIT 1`).Scan(&gotRun, &gotVersion, &gotTurn, &gotActive); err != nil {
		t.Fatal(err)
	}
	if gotRun != runStatus || gotTurn != turnStatus || gotActive.String != activeRun || gotVersion != executionVersion {
		t.Fatalf("state Run=%s/%d Turn=%s Active=%q", gotRun, gotVersion, gotTurn, gotActive.String)
	}
	var eventsFound, outboxFound int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM run_events`).Scan(&eventsFound); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM outbox_events`).Scan(&outboxFound); err != nil {
		t.Fatal(err)
	}
	if eventsFound != eventCount || outboxFound != outboxCount {
		t.Fatalf("events=%d/%d outbox=%d/%d", eventsFound, eventCount, outboxFound, outboxCount)
	}
}
