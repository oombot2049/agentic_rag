package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAggregateAccessorsAndStandaloneLifecycleMethods(t *testing.T) {
	t.Parallel()
	tenant, err := NewTenant("ten_0001", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if tenant.ID() != "ten_0001" || !tenant.CreatedAt().Equal(testTime.UTC()) {
		t.Fatal("tenant accessors disagree")
	}

	session := mustSession(t)
	if session.ID() != "ses_0001" || session.TenantID() != tenant.ID() || !session.CreatedAt().Equal(testTime.UTC()) || !session.UpdatedAt().Equal(testTime.UTC()) {
		t.Fatal("session accessors disagree")
	}
	if _, ok := session.HeadTurnID(); ok {
		t.Fatal("new session has a head")
	}

	run := newRunFor(t, "run_0001", 1, 7)
	if run.TenantID() != tenant.ID() || run.SessionID() != session.ID() || run.TurnID() != "trn_0001" || run.RunNo() != 1 || run.BaseContextVersion() != 7 || run.Terminal() || !run.CreatedAt().Equal(testTime.UTC()) {
		t.Fatal("run accessors disagree")
	}
	if _, ok := run.OutputMessageID(); ok {
		t.Fatal("queued run has output")
	}
	if err := run.Start(testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := run.Succeed("msg_0002", testTime.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if output, ok := run.OutputMessageID(); !ok || output != "msg_0002" || !run.UpdatedAt().Equal(testTime.Add(2*time.Minute).UTC()) {
		t.Fatal("run success accessors disagree")
	}

	turn, err := NewTurn("trn_0001", tenant.ID(), session.ID(), 1, 7, "msg_0001", run.ID(), testTime)
	if err != nil {
		t.Fatal(err)
	}
	if turn.TenantID() != tenant.ID() || turn.SessionID() != session.ID() || turn.Sequence() != 1 || turn.BaseContextVersion() != 7 || turn.UserMessageID() != "msg_0001" || !turn.CreatedAt().Equal(testTime.UTC()) {
		t.Fatal("turn accessors disagree")
	}
	if err := turn.Complete(run, testTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !turn.UpdatedAt().Equal(testTime.Add(3 * time.Minute).UTC()) {
		t.Fatal("turn update time disagrees")
	}

	message, err := NewAssistantCandidate("msg_0002", tenant.ID(), session.ID(), turn.ID(), run.ID(), "answer", testTime)
	if err != nil {
		t.Fatal(err)
	}
	messageRun, ok := message.RunID()
	if !ok || messageRun != run.ID() || message.TenantID() != tenant.ID() || message.SessionID() != session.ID() || message.TurnID() != turn.ID() || message.Role() != MessageRoleAssistant || message.Content() != "answer" || !message.CreatedAt().Equal(testTime.UTC()) {
		t.Fatal("message accessors disagree")
	}

	step, err := NewStep("stp_0001", tenant.ID(), run.ID(), 1, StepKindGeneration, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if step.TenantID() != tenant.ID() || step.RunID() != run.ID() || step.StepNo() != 1 || step.Kind() != StepKindGeneration || step.Status() != StepStatusPending || !step.CreatedAt().Equal(testTime.UTC()) {
		t.Fatal("step accessors disagree")
	}
	if err := step.Start(testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	attempt, err := NewAttempt("att_0001", tenant.ID(), step.ID(), 1, testTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if attempt.TenantID() != tenant.ID() || attempt.StepID() != step.ID() || attempt.AttemptNo() != 1 || attempt.Status() != AttemptStatusStarted || attempt.Terminal() || !attempt.StartedAt().Equal(testTime.Add(time.Minute).UTC()) {
		t.Fatal("attempt accessors disagree")
	}
	if err := step.AddAttempt(attempt, testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := attempt.Succeed(testTime.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	endedAt, ok := attempt.EndedAt()
	if !ok || !endedAt.Equal(testTime.Add(2*time.Minute).UTC()) || !attempt.Terminal() {
		t.Fatal("attempt completion accessors disagree")
	}
	if err := step.Succeed(attempt, testTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if step.Status() != StepStatusSucceeded || !step.UpdatedAt().Equal(testTime.Add(3*time.Minute).UTC()) {
		t.Fatal("step success accessors disagree")
	}
}

func TestConvenienceLifecycleMethods(t *testing.T) {
	t.Parallel()
	failedTurn := restoredTurn(t, TurnStatusRunning)
	if err := failedTurn.Fail(testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cancelledTurn := restoredTurn(t, TurnStatusRunning)
	if err := cancelledTurn.Cancel(testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := cancelledTurn.Cancel(testTime.Add(2 * time.Minute)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("repeat cancellation = %v", err)
	}

	skipped, err := NewStep("stp_0002", "ten_0001", "run_0001", 2, StepKindContext, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := skipped.Skip(testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cancelled, err := NewStep("stp_0003", "ten_0001", "run_0001", 3, StepKindRetrieval, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := cancelled.Start(testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := cancelled.Cancel(testTime.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}

	unknown, err := NewAttempt("att_0002", "ten_0001", "stp_0003", 1, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := unknown.MarkUnknown(testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if unknown.Status() != AttemptStatusUnknown {
		t.Fatal("unknown outcome not recorded")
	}
}

func TestDomainErrorFormattingAndIdentity(t *testing.T) {
	t.Parallel()
	validation := &ValidationError{Field: "run_id", Reason: "wrong owner"}
	if !errors.Is(validation, ErrValidation) || !strings.Contains(validation.Error(), "run_id") {
		t.Fatalf("validation error = %v", validation)
	}
	transition := &TransitionError{Aggregate: "run", From: "QUEUED", To: "SELECTED"}
	if !errors.Is(transition, ErrInvalidTransition) || !strings.Contains(transition.Error(), "QUEUED -> SELECTED") {
		t.Fatalf("transition error = %v", transition)
	}
	var nilValidation *ValidationError
	var nilTransition *TransitionError
	if nilValidation.Error() != ErrValidation.Error() || nilTransition.Error() != ErrInvalidTransition.Error() {
		t.Fatal("nil error formatting is unsafe")
	}
}

func TestTimestampFailuresNeverMutateState(t *testing.T) {
	t.Parallel()
	run := newRunFor(t, "run_0001", 1, 0)
	if err := run.Start(testTime.Add(-time.Minute)); !errors.Is(err, ErrValidation) {
		t.Fatalf("backward run time = %v", err)
	}
	if run.Status() != RunStatusQueued {
		t.Fatal("run mutated after backward time")
	}
	step, err := NewStep("stp_0001", "ten_0001", "run_0001", 1, StepKindTool, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Start(time.Time{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("zero step time = %v", err)
	}
	if step.Status() != StepStatusPending {
		t.Fatal("step mutated after zero time")
	}
	attempt, err := NewAttempt("att_0001", "ten_0001", "stp_0001", 1, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.Fail(testTime.Add(-time.Minute)); !errors.Is(err, ErrValidation) {
		t.Fatalf("backward attempt time = %v", err)
	}
	if attempt.Status() != AttemptStatusStarted {
		t.Fatal("attempt mutated after backward time")
	}
}
