package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	executionrepo "github.com/agenticrag/agenticrag/internal/repository"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestExecutionRepositoryLeaseTakeoverStepAttemptAndCheckpoint(t *testing.T) {
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
	repository, err := executionrepo.NewExecution(pool)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Second)

	const contenders = 40
	start := make(chan struct{})
	type outcome struct {
		worker string
		record executionrepo.RunRecord
		err    error
	}
	outcomes := make(chan outcome, contenders)
	var wait sync.WaitGroup
	for index := range contenders {
		worker := fmt.Sprintf("worker-%02d", index)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			record, err := repository.AcquireLease(ctx, executionrepo.AcquireLeaseCommand{
				TenantID: "ten_0001", RunID: "run_0001", Owner: worker,
				ExpectedExecutionVersion: 1, At: at, Duration: time.Minute,
			})
			outcomes <- outcome{worker: worker, record: record, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(outcomes)
	var winner outcome
	successes, conflicts := 0, 0
	for result := range outcomes {
		switch {
		case result.err == nil:
			successes++
			winner = result
		case errors.Is(result.err, executionrepo.ErrRowConflict), errors.Is(result.err, executionrepo.ErrLeaseHeld):
			conflicts++
		default:
			t.Errorf("lease outcome: %v", result.err)
		}
	}
	if successes != 1 || conflicts != contenders-1 {
		t.Fatalf("lease success/conflicts = %d/%d", successes, conflicts)
	}
	if winner.record.Lease == nil || winner.record.Lease.Token != 1 || winner.record.ExecutionVersion != 2 {
		t.Fatalf("winner = %+v", winner)
	}

	renewed, err := repository.RenewLease(ctx, executionrepo.LeaseMutationCommand{
		TenantID: "ten_0001", RunID: "run_0001", Owner: winner.worker, Token: 1,
		ExpectedExecutionVersion: 2, At: at.Add(time.Second), Duration: 2 * time.Minute,
	})
	if err != nil || renewed.ExecutionVersion != 3 || renewed.Lease == nil {
		t.Fatalf("renewed = %+v/%v", renewed, err)
	}
	takeoverAt := renewed.Lease.ExpiresAt
	taken, err := repository.AcquireLease(ctx, executionrepo.AcquireLeaseCommand{
		TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-takeover",
		ExpectedExecutionVersion: 3, At: takeoverAt, Duration: time.Minute,
	})
	if err != nil || taken.ExecutionVersion != 4 || taken.Lease == nil || taken.Lease.Token != 2 {
		t.Fatalf("takeover = %+v/%v", taken, err)
	}
	if _, err := repository.RenewLease(ctx, executionrepo.LeaseMutationCommand{
		TenantID: "ten_0001", RunID: "run_0001", Owner: winner.worker, Token: 1,
		ExpectedExecutionVersion: 3, At: takeoverAt.Add(time.Second), Duration: time.Minute,
	}); !errors.Is(err, executionrepo.ErrLeaseFenced) {
		t.Fatalf("old worker error = %v", err)
	}

	workAt := takeoverAt.Add(time.Second)
	authority := executionrepo.WorkerAuthority{
		TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-takeover", Token: 2,
		ExpectedExecutionVersion: 4, At: workAt,
	}
	step, err := domain.NewStep("stp_exec1", "ten_0001", "run_0001", 1, domain.StepKindTool, workAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertStep(ctx, authority, step); err != nil {
		t.Fatal(err)
	}
	running, err := repository.TransitionStep(ctx, executionrepo.StepTransitionCommand{
		Authority: authority, StepID: step.ID(), ExpectedRowVersion: 1,
		Target: domain.StepStatusRunning, At: workAt,
	})
	if err != nil || running.RowVersion != 2 {
		t.Fatalf("running step = %+v/%v", running, err)
	}
	attempt, err := domain.NewAttempt("att_exec1", "ten_0001", step.ID(), 1, workAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertAttempt(ctx, authority, attempt); err != nil {
		t.Fatal(err)
	}
	finished, err := repository.FinishAttempt(ctx, executionrepo.AttemptFinishCommand{
		Authority: authority, AttemptID: attempt.ID(), ExpectedRowVersion: 1,
		Target: domain.AttemptStatusSucceeded, At: workAt.Add(time.Second),
	})
	if err != nil || finished.RowVersion != 2 {
		t.Fatalf("finished attempt = %+v/%v", finished, err)
	}
	succeeded, err := repository.TransitionStep(ctx, executionrepo.StepTransitionCommand{
		Authority: authority, StepID: step.ID(), ExpectedRowVersion: 2,
		Target: domain.StepStatusSucceeded, At: workAt.Add(time.Second),
	})
	if err != nil || succeeded.RowVersion != 3 {
		t.Fatalf("succeeded step = %+v/%v", succeeded, err)
	}
	if _, err := repository.TransitionStep(ctx, executionrepo.StepTransitionCommand{
		Authority: authority, StepID: step.ID(), ExpectedRowVersion: 2,
		Target: domain.StepStatusFailed, At: workAt.Add(2 * time.Second),
	}); !errors.Is(err, executionrepo.ErrRowConflict) {
		t.Fatalf("stale step error = %v", err)
	}

	payload := json.RawMessage(`{"next_action":"generate","step":1}`)
	if err := repository.SaveCheckpoint(ctx, authority, 1, payload); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := repository.LoadCheckpoint(ctx, "ten_0001", "run_0001")
	if err != nil || checkpoint.StepNo != 1 || checkpoint.LeaseToken != 2 || !json.Valid(checkpoint.Payload) {
		t.Fatalf("checkpoint = %+v/%v", checkpoint, err)
	}
	if err := repository.SaveCheckpoint(ctx, authority, 0, json.RawMessage(`{}`)); !errors.Is(err, executionrepo.ErrCheckpointConflict) {
		t.Fatalf("backward checkpoint error = %v", err)
	}

	released, err := repository.ReleaseLease(ctx, executionrepo.LeaseMutationCommand{
		TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-takeover", Token: 2,
		ExpectedExecutionVersion: 4, At: workAt.Add(3 * time.Second),
	})
	if err != nil || released.ExecutionVersion != 5 || released.Lease != nil {
		t.Fatalf("released = %+v/%v", released, err)
	}
	if err := repository.SaveCheckpoint(ctx, authority, 2, json.RawMessage(`{}`)); !errors.Is(err, executionrepo.ErrLeaseFenced) {
		t.Fatalf("released lease checkpoint error = %v", err)
	}
	if _, err := repository.GetRun(ctx, "ten_9999", "run_0001"); !errors.Is(err, executionrepo.ErrNotFound) {
		t.Fatalf("cross-tenant run error = %v", err)
	}
}
