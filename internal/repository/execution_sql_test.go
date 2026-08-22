package repository

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

func TestExecutionLeaseLifecycleAndClassification(t *testing.T) {
	t.Parallel()
	at := repositoryTime
	tests := []struct {
		name        string
		queries     []conversationQuery
		run         func(*Execution) (RunRecord, error)
		wantVersion uint64
		wantError   error
	}{
		{"get", []conversationQuery{{rows: [][]driver.Value{runRecordRow(1, nil, 0, nil)}}}, func(r *Execution) (RunRecord, error) { return r.GetRun(context.Background(), "ten_0001", "run_0001") }, 1, nil},
		{"acquire", []conversationQuery{{rows: [][]driver.Value{runRecordRow(2, "worker-a", 1, at.Add(time.Minute))}}}, func(r *Execution) (RunRecord, error) {
			return r.AcquireLease(context.Background(), AcquireLeaseCommand{TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-a", ExpectedExecutionVersion: 1, At: at, Duration: time.Minute})
		}, 2, nil},
		{"acquire stale version", []conversationQuery{{}, {rows: [][]driver.Value{runRecordRow(2, nil, 1, nil)}}}, func(r *Execution) (RunRecord, error) {
			return r.AcquireLease(context.Background(), AcquireLeaseCommand{TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-a", ExpectedExecutionVersion: 1, At: at, Duration: time.Minute})
		}, 0, ErrRowConflict},
		{"lease held", []conversationQuery{{}, {rows: [][]driver.Value{runRecordRow(1, "worker-b", 1, at.Add(time.Minute))}}}, func(r *Execution) (RunRecord, error) {
			return r.AcquireLease(context.Background(), AcquireLeaseCommand{TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-a", ExpectedExecutionVersion: 1, At: at, Duration: time.Minute})
		}, 0, ErrLeaseHeld},
		{"renew", []conversationQuery{{rows: [][]driver.Value{runRecordRow(3, "worker-a", 1, at.Add(2*time.Minute))}}}, func(r *Execution) (RunRecord, error) { return r.RenewLease(context.Background(), validLeaseMutation()) }, 3, nil},
		{"release", []conversationQuery{{rows: [][]driver.Value{runRecordRow(3, nil, 1, nil)}}}, func(r *Execution) (RunRecord, error) {
			return r.ReleaseLease(context.Background(), validLeaseMutation())
		}, 3, nil},
		{"renew fenced", []conversationQuery{{}, {rows: [][]driver.Value{runRecordRow(2, "worker-b", 2, at.Add(time.Minute))}}}, func(r *Execution) (RunRecord, error) { return r.RenewLease(context.Background(), validLeaseMutation()) }, 0, ErrLeaseFenced},
		{"renew expired", []conversationQuery{{}, {rows: [][]driver.Value{runRecordRow(2, "worker-a", 1, at)}}}, func(r *Execution) (RunRecord, error) { return r.RenewLease(context.Background(), validLeaseMutation()) }, 0, ErrLeaseExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openConversationDB(t, &conversationScript{queries: test.queries})
			repository, err := NewExecution(database)
			if err != nil {
				t.Fatal(err)
			}
			record, err := test.run(repository)
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) {
					t.Fatalf("error = %v, want %v", err, test.wantError)
				}
				return
			}
			if err != nil || record.ExecutionVersion != test.wantVersion {
				t.Fatalf("record = %+v/%v", record, err)
			}
		})
	}
}

func TestExecutionChildWritesAndCheckpoint(t *testing.T) {
	t.Parallel()
	authority := validAuthority()
	step, err := domain.NewStep("stp_0001", "ten_0001", "run_0001", 1, domain.StepKindTool, repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := domain.NewAttempt("att_0001", "ten_0001", step.ID(), 1, repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	script := &conversationScript{
		execs: []conversationExec{{affected: 1}, {affected: 1}, {affected: 1}, {affected: 0}},
		queries: []conversationQuery{
			{rows: [][]driver.Value{stepRecordRow("RUNNING", 2)}},
			{rows: [][]driver.Value{attemptRecordRow("SUCCEEDED", repositoryTime.Add(time.Second), 2)}},
			{rows: [][]driver.Value{checkpointRow(5)}},
			{rows: [][]driver.Value{checkpointRow(5)}},
		},
	}
	database := openConversationDB(t, script)
	repository, err := NewExecution(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertStep(context.Background(), authority, step); err != nil {
		t.Fatal(err)
	}
	stepRecord, err := repository.TransitionStep(context.Background(), StepTransitionCommand{Authority: authority, StepID: step.ID(), ExpectedRowVersion: 1, Target: domain.StepStatusRunning, At: repositoryTime})
	if err != nil || stepRecord.RowVersion != 2 || stepRecord.Step.Status() != domain.StepStatusRunning {
		t.Fatalf("step transition = %+v/%v", stepRecord, err)
	}
	if err := repository.InsertAttempt(context.Background(), authority, attempt); err != nil {
		t.Fatal(err)
	}
	attemptRecord, err := repository.FinishAttempt(context.Background(), AttemptFinishCommand{Authority: authority, AttemptID: attempt.ID(), ExpectedRowVersion: 1, Target: domain.AttemptStatusSucceeded, At: repositoryTime.Add(time.Second)})
	if err != nil || attemptRecord.RowVersion != 2 {
		t.Fatalf("attempt finish = %+v/%v", attemptRecord, err)
	}
	if err := repository.SaveCheckpoint(context.Background(), authority, 5, json.RawMessage(`{"next":"generate"}`)); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := repository.LoadCheckpoint(context.Background(), "ten_0001", "run_0001")
	if err != nil || checkpoint.StepNo != 5 {
		t.Fatalf("checkpoint = %+v/%v", checkpoint, err)
	}
	if err := repository.SaveCheckpoint(context.Background(), authority, 4, json.RawMessage(`{}`)); !errors.Is(err, ErrCheckpointConflict) {
		t.Fatalf("backward checkpoint error = %v", err)
	}
}

func TestExecutionInsertRunAndHydrateChildren(t *testing.T) {
	t.Parallel()
	run, err := domain.NewRun("run_0001", "ten_0001", "ses_0001", "trn_0001", 1, 0, repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	script := &conversationScript{
		execs: []conversationExec{{affected: 1}},
		queries: []conversationQuery{
			{rows: [][]driver.Value{stepRecordRow("RUNNING", 2)}},
			{rows: [][]driver.Value{{"att_0001"}, {"att_0002"}}},
			{rows: [][]driver.Value{attemptRecordRow("SUCCEEDED", repositoryTime.Add(time.Second), 2)}},
		},
	}
	database := openConversationDB(t, script)
	repository, err := NewExecution(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	step, err := repository.GetStep(context.Background(), "ten_0001", "stp_0001")
	if err != nil || len(step.Step.AttemptIDs()) != 2 || step.Step.AttemptIDs()[1] != "att_0002" {
		t.Fatalf("GetStep = %+v/%v", step, err)
	}
	attempt, err := repository.GetAttempt(context.Background(), "ten_0001", "att_0001")
	if err != nil || attempt.Attempt.Status() != domain.AttemptStatusSucceeded {
		t.Fatalf("GetAttempt = %+v/%v", attempt, err)
	}
}

func TestExecutionValidationAndFencedWrites(t *testing.T) {
	t.Parallel()
	if _, err := NewExecution(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil executor error = %v", err)
	}
	database := openConversationDB(t, &conversationScript{execs: []conversationExec{{affected: 0}}})
	repository, err := NewExecution(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertRun(context.Background(), nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil run error = %v", err)
	}
	step, err := domain.NewStep("stp_0001", "ten_0001", "run_0001", 1, domain.StepKindTool, repositoryTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InsertStep(context.Background(), validAuthority(), step); !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("fenced insert error = %v", err)
	}
	if _, err := repository.AcquireLease(context.Background(), AcquireLeaseCommand{}); !errors.Is(err, ErrInvalidArgument) && err == nil {
		t.Fatalf("invalid acquire error = %v", err)
	}
	if _, err := repository.RenewLease(context.Background(), LeaseMutationCommand{}); err == nil {
		t.Fatal("invalid renew error = nil")
	}
	if err := repository.SaveCheckpoint(context.Background(), validAuthority(), 1, json.RawMessage(`{`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid checkpoint error = %v", err)
	}
	if _, err := repository.FinishAttempt(context.Background(), AttemptFinishCommand{Authority: validAuthority(), AttemptID: "att_0001", ExpectedRowVersion: 1, Target: domain.AttemptStatusStarted, At: repositoryTime}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nonterminal attempt error = %v", err)
	}
}

func validLeaseMutation() LeaseMutationCommand {
	return LeaseMutationCommand{TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-a", Token: 1, ExpectedExecutionVersion: 2, At: repositoryTime, Duration: time.Minute}
}

func validAuthority() WorkerAuthority {
	return WorkerAuthority{TenantID: "ten_0001", RunID: "run_0001", Owner: "worker-a", Token: 1, ExpectedExecutionVersion: 2, At: repositoryTime}
}

func runRecordRow(version uint64, owner any, token uint64, expires any) []driver.Value {
	return []driver.Value{"ten_0001", "run_0001", "ses_0001", "trn_0001", int64(1), int64(0), "QUEUED", nil, repositoryTime, repositoryTime, int64(version), owner, int64(token), expires}
}

func stepRecordRow(status string, version uint64) []driver.Value {
	return []driver.Value{"ten_0001", "stp_0001", "run_0001", int64(1), "TOOL", status, repositoryTime, repositoryTime, int64(version)}
}

func attemptRecordRow(status string, ended time.Time, version uint64) []driver.Value {
	return []driver.Value{"ten_0001", "att_0001", "stp_0001", int64(1), status, repositoryTime, ended, int64(version)}
}

func checkpointRow(stepNo uint64) []driver.Value {
	return []driver.Value{"ten_0001", "run_0001", int64(1), int64(stepNo), []byte(`{"next":"generate"}`), repositoryTime, repositoryTime}
}
