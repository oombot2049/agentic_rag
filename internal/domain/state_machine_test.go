package domain

import (
	"errors"
	"testing"
	"time"
)

var testTime = time.Date(2026, 8, 23, 10, 0, 0, 0, time.FixedZone("CST", 8*60*60))

func TestRunTransitionMatrix(t *testing.T) {
	t.Parallel()
	statuses := []RunStatus{
		RunStatusQueued, RunStatusRunning, RunStatusWaitingApproval, RunStatusCancelRequested, RunStatusSucceeded,
		RunStatusSelected, RunStatusSuperseded, RunStatusFailed, RunStatusCancelled, RunStatusTimedOut,
	}
	for _, from := range statuses {
		for _, to := range statuses {
			from, to := from, to
			t.Run(from.String()+"_to_"+to.String(), func(t *testing.T) {
				run := restoredRun(t, from)
				beforeStatus, beforeUpdated := run.status, run.updatedAt
				err := applyRunTransition(run, to, testTime.Add(time.Minute))
				wantValid := validRunTransition(from, to) || from == RunStatusRunning && to == RunStatusSucceeded
				if wantValid && err != nil {
					t.Fatalf("transition rejected: %v", err)
				}
				if !wantValid && !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("error = %v, want transition error", err)
				}
				if !wantValid && (run.status != beforeStatus || !run.updatedAt.Equal(beforeUpdated)) {
					t.Fatal("rejected transition mutated run")
				}
			})
		}
	}
}

func TestStepTransitionMatrix(t *testing.T) {
	t.Parallel()
	statuses := []StepStatus{StepStatusPending, StepStatusRunning, StepStatusSucceeded, StepStatusFailed, StepStatusCancelled, StepStatusSkipped}
	for _, from := range statuses {
		for _, to := range statuses {
			from, to := from, to
			t.Run(from.String()+"_to_"+to.String(), func(t *testing.T) {
				step := restoredStep(t, from)
				before := step.updatedAt
				err := step.transition(to, testTime.Add(time.Minute))
				if validStepTransition(from, to) && err != nil {
					t.Fatalf("transition rejected: %v", err)
				}
				if !validStepTransition(from, to) && !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("error = %v", err)
				}
				if !validStepTransition(from, to) && (step.status != from || !step.updatedAt.Equal(before)) {
					t.Fatal("rejection mutated step")
				}
			})
		}
	}
}

func TestAttemptTransitionMatrix(t *testing.T) {
	t.Parallel()
	statuses := []AttemptStatus{AttemptStatusStarted, AttemptStatusSucceeded, AttemptStatusFailed, AttemptStatusUnknown}
	targets := []AttemptStatus{AttemptStatusSucceeded, AttemptStatusFailed, AttemptStatusUnknown}
	for _, from := range statuses {
		for _, to := range targets {
			from, to := from, to
			t.Run(from.String()+"_to_"+to.String(), func(t *testing.T) {
				attempt := restoredAttempt(t, from)
				beforeEnded := attempt.endedAt
				err := attempt.finish(to, testTime.Add(2*time.Minute))
				if from == AttemptStatusStarted && err != nil {
					t.Fatalf("transition rejected: %v", err)
				}
				if from != AttemptStatusStarted && !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("error = %v", err)
				}
				if from != AttemptStatusStarted && (attempt.status != from || !attempt.endedAt.Equal(beforeEnded)) {
					t.Fatal("rejection mutated attempt")
				}
			})
		}
	}
}

func TestTurnTerminalStatesOnlyReopenWithNewRun(t *testing.T) {
	t.Parallel()
	for _, status := range []TurnStatus{TurnStatusCompleted, TurnStatusFailed, TurnStatusCancelled} {
		status := status
		t.Run(status.String(), func(t *testing.T) {
			turn := restoredTurn(t, status)
			next := newRunFor(t, "run_0002", 2, turn.baseContextVersion)
			if err := turn.StartRun(next, testTime.Add(time.Minute)); err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			if turn.Status() != TurnStatusRunning || len(turn.RunIDs()) != 2 {
				t.Fatalf("turn = %s/%v", turn.Status(), turn.RunIDs())
			}
		})
	}
	turn := restoredTurn(t, TurnStatusRunning)
	before := turn.RunIDs()
	if err := turn.StartRun(newRunFor(t, "run_0002", 2, 0), testTime.Add(time.Minute)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error = %v", err)
	}
	if len(turn.RunIDs()) != len(before) {
		t.Fatal("rejected transition appended run")
	}
}

func applyRunTransition(run *Run, target RunStatus, at time.Time) error {
	switch target {
	case RunStatusQueued:
		return run.ResumeAfterApproval(at)
	case RunStatusRunning:
		return run.Start(at)
	case RunStatusWaitingApproval:
		return run.WaitForApproval(at)
	case RunStatusCancelRequested:
		return run.RequestCancel(at)
	case RunStatusSucceeded:
		return run.Succeed(MessageID("msg_0002"), at)
	case RunStatusSelected:
		return run.Select(at)
	case RunStatusSuperseded:
		return run.Supersede(at)
	case RunStatusFailed:
		return run.Fail(at)
	case RunStatusCancelled:
		return run.Cancel(at)
	case RunStatusTimedOut:
		return run.Timeout(at)
	default:
		return invalidTransition("run", run.status, target)
	}
}

func restoredRun(t *testing.T, status RunStatus) *Run {
	t.Helper()
	output := MessageID("")
	if status == RunStatusSucceeded || status == RunStatusSelected || status == RunStatusSuperseded {
		output = "msg_0002"
	}
	run, err := RestoreRun(RunSnapshot{
		ID: "run_0001", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001",
		RunNo: 1, Status: status, OutputMessageID: output, CreatedAt: testTime, UpdatedAt: testTime,
	})
	if err != nil {
		t.Fatalf("RestoreRun: %v", err)
	}
	return run
}

func restoredStep(t *testing.T, status StepStatus) *Step {
	t.Helper()
	step, err := RestoreStep(StepSnapshot{
		ID: "stp_0001", TenantID: "ten_0001", RunID: "run_0001", StepNo: 1,
		Kind: StepKindTool, Status: status, CreatedAt: testTime, UpdatedAt: testTime,
	})
	if err != nil {
		t.Fatalf("RestoreStep: %v", err)
	}
	return step
}

func restoredAttempt(t *testing.T, status AttemptStatus) *Attempt {
	t.Helper()
	ended := time.Time{}
	if status.Terminal() {
		ended = testTime.Add(time.Minute)
	}
	attempt, err := RestoreAttempt(AttemptSnapshot{
		ID: "att_0001", TenantID: "ten_0001", StepID: "stp_0001", AttemptNo: 1,
		Status: status, StartedAt: testTime, EndedAt: ended,
	})
	if err != nil {
		t.Fatalf("RestoreAttempt: %v", err)
	}
	return attempt
}

func restoredTurn(t *testing.T, status TurnStatus) *Turn {
	t.Helper()
	selected := RunID("")
	if status == TurnStatusCompleted {
		selected = "run_0001"
	}
	turn, err := RestoreTurn(TurnSnapshot{
		ID: "trn_0001", TenantID: "ten_0001", SessionID: "ses_0001", Sequence: 1,
		UserMessageID: "msg_0001", Status: status, RunIDs: []RunID{"run_0001"},
		SelectedRunID: selected, CreatedAt: testTime, UpdatedAt: testTime,
	})
	if err != nil {
		t.Fatalf("RestoreTurn: %v", err)
	}
	return turn
}

func newRunFor(t *testing.T, id RunID, runNo, base uint64) *Run {
	t.Helper()
	run, err := NewRun(id, "ten_0001", "ses_0001", "trn_0001", runNo, base, testTime)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	return run
}
