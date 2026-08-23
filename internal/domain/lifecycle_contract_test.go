package domain

import (
	"errors"
	"testing"
	"time"
)

func TestRunPublicApprovalSuccessAndCancellationLifecycles(t *testing.T) {
	t.Parallel()
	run := newRunFor(t, "run_approval", 1, 7)
	if run.ID() != "run_approval" || run.TenantID() != "ten_0001" || run.SessionID() != "ses_0001" ||
		run.TurnID() != "trn_0001" || run.RunNo() != 1 || run.BaseContextVersion() != 7 || run.Status() != RunStatusQueued ||
		run.Terminal() || !run.CreatedAt().Equal(testTime) || !run.UpdatedAt().Equal(testTime) {
		t.Fatalf("new Run getters = %+v", run)
	}
	if err := run.Start(testTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := run.WaitForApproval(testTime.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := run.ResumeAfterApproval(testTime.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := run.Start(testTime.Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := run.Succeed("msg_answer", testTime.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if output, ok := run.OutputMessageID(); !ok || output != "msg_answer" {
		t.Fatalf("output = %q, %v", output, ok)
	}
	if err := run.Select(testTime.Add(6 * time.Second)); err != nil || !run.Terminal() {
		t.Fatalf("select = %v, terminal=%v", err, run.Terminal())
	}

	queued := newRunFor(t, "run_queued_cancel", 1, 0)
	if err := queued.Cancel(testTime.Add(time.Second)); err != nil || queued.Status() != RunStatusCancelled {
		t.Fatalf("queued cancel = %s, %v", queued.Status(), err)
	}
	running := newRunFor(t, "run_running_cancel", 1, 0)
	_ = running.Start(testTime.Add(time.Second))
	if err := running.RequestCancel(testTime.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := running.Cancel(testTime.Add(3 * time.Second)); err != nil || running.Status() != RunStatusCancelled {
		t.Fatalf("requested cancel = %s, %v", running.Status(), err)
	}
}

func TestStepAttemptPublicOutcomeContracts(t *testing.T) {
	t.Parallel()
	step, err := NewStep("stp_success", "ten_0001", "run_0001", 1, StepKindTool, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if step.ID() != "stp_success" || step.TenantID() != "ten_0001" || step.RunID() != "run_0001" || step.StepNo() != 1 ||
		step.Kind() != StepKindTool || step.Status() != StepStatusPending || step.Terminal() {
		t.Fatalf("step getters = %+v", step)
	}
	if err := step.Start(testTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	attempt, err := NewAttempt("att_success", "ten_0001", step.ID(), 1, testTime.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := step.AddAttempt(attempt, testTime.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := attempt.Succeed(testTime.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := step.Succeed(attempt, testTime.Add(4*time.Second)); err != nil || !step.Terminal() {
		t.Fatalf("step success = %v terminal=%v", err, step.Terminal())
	}
	ended, ok := attempt.EndedAt()
	if attempt.ID() != "att_success" || attempt.TenantID() != "ten_0001" || attempt.StepID() != step.ID() ||
		attempt.AttemptNo() != 1 || attempt.Status() != AttemptStatusSucceeded || !attempt.Terminal() ||
		!attempt.StartedAt().Equal(testTime.Add(time.Second)) || !ok || !ended.Equal(testTime.Add(3*time.Second)) {
		t.Fatalf("attempt getters = %+v", attempt)
	}
	copyIDs := step.AttemptIDs()
	copyIDs[0] = "att_tampered"
	if step.AttemptIDs()[0] != "att_success" {
		t.Fatal("AttemptIDs leaked mutable state")
	}

	for index, unknown := range []bool{false, true} {
		failedStep, _ := NewStep(StepID("stp_failed_"+string(rune('a'+index))), "ten_0001", "run_0001", uint64(index+2), StepKindRetrieval, testTime)
		_ = failedStep.Start(testTime.Add(time.Second))
		failedAttempt, _ := NewAttempt(AttemptID("att_failed_"+string(rune('a'+index))), "ten_0001", failedStep.ID(), 1, testTime.Add(time.Second))
		_ = failedStep.AddAttempt(failedAttempt, testTime.Add(2*time.Second))
		if unknown {
			err = failedAttempt.MarkUnknown(testTime.Add(3 * time.Second))
		} else {
			err = failedAttempt.Fail(testTime.Add(3 * time.Second))
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := failedStep.Fail(failedAttempt, testTime.Add(4*time.Second)); err != nil || failedStep.Status() != StepStatusFailed {
			t.Fatalf("failed step = %s, %v", failedStep.Status(), err)
		}
	}
}

func TestTurnPublicTerminalContractsAndRejectedMutationAtomicity(t *testing.T) {
	t.Parallel()
	turn, err := NewTurn("trn_0001", "ten_0001", "ses_0001", 1, 0, "msg_user", "run_0001", testTime)
	if err != nil {
		t.Fatal(err)
	}
	run := newRunFor(t, "run_0001", 1, 0)
	_ = run.Start(testTime.Add(time.Second))
	_ = run.Succeed("msg_answer", testTime.Add(2*time.Second))
	if err := turn.Complete(run, testTime.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	selected, ok := turn.SelectedRunID()
	if turn.ID() != "trn_0001" || turn.TenantID() != "ten_0001" || turn.SessionID() != "ses_0001" || turn.Sequence() != 1 ||
		turn.BaseContextVersion() != 0 || turn.UserMessageID() != "msg_user" || turn.Status() != TurnStatusCompleted ||
		!ok || selected != run.ID() || !turn.CreatedAt().Equal(testTime) || !turn.UpdatedAt().Equal(testTime.Add(3*time.Second)) {
		t.Fatalf("completed Turn getters = %+v", turn)
	}
	before := turn.UpdatedAt()
	if err := turn.Fail(testTime.Add(4 * time.Second)); !errors.Is(err, ErrInvalidTransition) || turn.Status() != TurnStatusCompleted || !turn.UpdatedAt().Equal(before) {
		t.Fatalf("terminal mutation = %v, %+v", err, turn)
	}

	for index, cancel := range []bool{false, true} {
		candidate, _ := NewTurn(TurnID("trn_terminal_"+string(rune('a'+index))), "ten_0001", "ses_0001", uint64(index+2), 0,
			MessageID("msg_terminal_"+string(rune('a'+index))), RunID("run_terminal_"+string(rune('a'+index))), testTime)
		if cancel {
			err = candidate.Cancel(testTime.Add(time.Second))
		} else {
			err = candidate.Fail(testTime.Add(time.Second))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}
