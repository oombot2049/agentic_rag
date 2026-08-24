package domain

import (
	"errors"
	"testing"
	"time"
)

func TestSessionNewTurnReservationAndFinalCommit(t *testing.T) {
	t.Parallel()
	session := mustSession(t)
	if _, err := session.ReserveTurnRun(1, "run_0001", testTime.Add(time.Minute)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale decision error = %v, want version conflict", err)
	}
	sequence, err := session.ReserveTurnRun(0, "run_0001", testTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReserveTurnRun: %v", err)
	}
	if sequence != 1 || session.NextTurnSequence() != 2 || session.RowVersion() != 2 {
		t.Fatalf("reservation counters = %d/%d/%d", sequence, session.NextTurnSequence(), session.RowVersion())
	}
	if _, err := session.ReserveTurnRun(0, "run_9999", testTime.Add(time.Minute)); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second reservation error = %v, want busy", err)
	}

	run := newRunFor(t, "run_0001", 1, 0)
	turn, err := NewTurn("trn_0001", "ten_0001", "ses_0001", sequence, 0, "msg_0001", run.ID(), testTime)
	if err != nil {
		t.Fatalf("NewTurn: %v", err)
	}
	message, err := NewAssistantCandidate("msg_0002", "ten_0001", "ses_0001", turn.ID(), run.ID(), "answer", testTime)
	if err != nil {
		t.Fatalf("NewAssistantCandidate: %v", err)
	}
	if err := run.Start(testTime.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := run.Succeed(message.ID(), testTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := session.CommitRun(turn, run, message, testTime.Add(4*time.Minute)); err != nil {
		t.Fatalf("CommitRun: %v", err)
	}

	if session.ContextVersion() != 1 || session.RowVersion() != 3 || session.CommittedTurnSequence() != 1 {
		t.Fatalf("commit versions = context %d row %d sequence %d", session.ContextVersion(), session.RowVersion(), session.CommittedTurnSequence())
	}
	if _, active := session.ActiveRunID(); active {
		t.Fatal("active run was not cleared")
	}
	if turn.Status() != TurnStatusCompleted || run.Status() != RunStatusSelected || message.Visibility() != MessageVisibilityConversation {
		t.Fatalf("final participants = %s/%s/%s", turn.Status(), run.Status(), message.Visibility())
	}
}

func TestSessionRegenerationDoesNotConsumeTurnSequence(t *testing.T) {
	t.Parallel()
	session, turn := committedSessionAndTurn(t)
	beforeNext := session.NextTurnSequence()
	nextRun := newRunFor(t, "run_0002", 2, session.ContextVersion())
	if err := session.ReserveRegeneration(session.ContextVersion(), turn, nextRun, testTime.Add(5*time.Minute)); err != nil {
		t.Fatalf("ReserveRegeneration: %v", err)
	}
	if session.NextTurnSequence() != beforeNext {
		t.Fatalf("next sequence changed from %d to %d", beforeNext, session.NextTurnSequence())
	}
	if turn.Status() != TurnStatusRunning || len(turn.RunIDs()) != 2 {
		t.Fatalf("regeneration did not atomically reopen turn: %s/%v", turn.Status(), turn.RunIDs())
	}
	message, err := NewAssistantCandidate("msg_0003", "ten_0001", "ses_0001", turn.ID(), nextRun.ID(), "better answer", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := nextRun.Start(testTime.Add(6 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := nextRun.Succeed(message.ID(), testTime.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := session.CommitRun(turn, nextRun, message, testTime.Add(8*time.Minute)); err != nil {
		t.Fatalf("regeneration commit: %v", err)
	}
	selected, ok := turn.SelectedRunID()
	if !ok || selected != nextRun.ID() || len(turn.RunIDs()) != 2 {
		t.Fatalf("selected/run IDs = %s/%v", selected, turn.RunIDs())
	}
	if session.ContextVersion() != 2 || session.NextTurnSequence() != beforeNext {
		t.Fatalf("versions = context %d next %d", session.ContextVersion(), session.NextTurnSequence())
	}
}

func TestSessionRejectedCommitDoesNotMutateParticipants(t *testing.T) {
	t.Parallel()
	session := mustSession(t)
	sequence, err := session.ReserveTurnRun(0, "run_0001", testTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	run := newRunFor(t, "run_0001", 1, 0)
	turn, err := NewTurn("trn_0001", "ten_0001", "ses_0001", sequence, 0, "msg_0001", run.ID(), testTime)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := NewAssistantCandidate("msg_0002", "ten_9999", "ses_0001", turn.ID(), run.ID(), "answer", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Start(testTime.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := run.Succeed(foreign.ID(), testTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	beforeRow := session.RowVersion()
	if err := session.CommitRun(turn, run, foreign, testTime.Add(4*time.Minute)); !errors.Is(err, ErrValidation) {
		t.Fatalf("error = %v", err)
	}
	if session.RowVersion() != beforeRow || turn.Status() != TurnStatusRunning || run.Status() != RunStatusSucceeded || foreign.Visibility() != MessageVisibilityCandidate {
		t.Fatal("rejected commit mutated participants")
	}
}

func TestSessionReleaseRequiresOwnedTerminalRun(t *testing.T) {
	t.Parallel()
	session := mustSession(t)
	if _, err := session.ReserveTurnRun(0, "run_0001", testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	run := newRunFor(t, "run_0001", 1, 0)
	if err := session.ReleaseRun(run, testTime.Add(2*time.Minute)); !errors.Is(err, ErrValidation) {
		t.Fatalf("nonterminal error = %v", err)
	}
	if err := run.Cancel(testTime.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := session.ReleaseRun(run, testTime.Add(3*time.Minute)); err != nil {
		t.Fatalf("ReleaseRun: %v", err)
	}
	if session.ContextVersion() != 0 || session.RowVersion() != 3 {
		t.Fatalf("versions = %d/%d", session.ContextVersion(), session.RowVersion())
	}
}

func TestStepAttemptsAreContiguousOwnedAndDefensivelyCopied(t *testing.T) {
	t.Parallel()
	step, err := NewStep("stp_0001", "ten_0001", "run_0001", 1, StepKindTool, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.Start(testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	second, err := NewAttempt("att_0002", "ten_0001", step.ID(), 2, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.AddAttempt(second, testTime.Add(time.Minute)); !errors.Is(err, ErrValidation) {
		t.Fatalf("discontinuous error = %v", err)
	}
	first, err := NewAttempt("att_0001", "ten_0001", step.ID(), 1, testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := step.AddAttempt(first, testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ids := step.AttemptIDs()
	ids[0] = "att_9999"
	if step.AttemptIDs()[0] != first.ID() {
		t.Fatal("caller mutated attempt IDs")
	}
	if err := first.Fail(testTime.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := step.Fail(first, testTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !step.Terminal() {
		t.Fatal("failed step must be terminal")
	}
}

func TestMessageVisibilityAndRoleInvariants(t *testing.T) {
	t.Parallel()
	user, err := NewUserMessage("msg_0001", "ten_0001", "ses_0001", "trn_0001", "question", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := user.PromoteToConversation(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("user promotion error = %v", err)
	}
	candidate, err := NewAssistantCandidate("msg_0002", "ten_0001", "ses_0001", "trn_0001", "run_0001", "answer", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.PromoteToConversation(); err != nil {
		t.Fatal(err)
	}
	if err := candidate.PromoteToConversation(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second promotion error = %v", err)
	}
	if _, err := NewUserMessage("msg_0003", "ten_0001", "ses_0001", "trn_0001", "  ", testTime); !errors.Is(err, ErrValidation) {
		t.Fatalf("blank content error = %v", err)
	}
}

func TestRestoreConstructorsRejectBrokenSnapshots(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		restore func() error
	}{
		{"session head mismatch", func() error {
			_, err := RestoreSession(SessionSnapshot{ID: "ses_0001", TenantID: "ten_0001", RowVersion: 1, NextTurnSequence: 2, HeadTurnID: "trn_0001", CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"turn duplicate runs", func() error {
			_, err := RestoreTurn(TurnSnapshot{ID: "trn_0001", TenantID: "ten_0001", SessionID: "ses_0001", Sequence: 1, UserMessageID: "msg_0001", Status: TurnStatusRunning, RunIDs: []RunID{"run_0001", "run_0001"}, CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"successful run no output", func() error {
			_, err := RestoreRun(RunSnapshot{ID: "run_0001", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunNo: 1, Status: RunStatusSucceeded, CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"terminal attempt no end", func() error {
			_, err := RestoreAttempt(AttemptSnapshot{ID: "att_0001", TenantID: "ten_0001", StepID: "stp_0001", AttemptNo: 1, Status: AttemptStatusUnknown, StartedAt: testTime})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.restore(); !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSignedInt64RangeGuards(t *testing.T) {
	t.Parallel()
	overflow := maxSignedInt64 + 1
	tests := []struct {
		name    string
		restore func() error
	}{
		{"session context", func() error {
			_, err := RestoreSession(SessionSnapshot{ID: "ses_0001", TenantID: "ten_0001", ContextVersion: overflow, RowVersion: 1, NextTurnSequence: 1, CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"session row", func() error {
			_, err := RestoreSession(SessionSnapshot{ID: "ses_0001", TenantID: "ten_0001", RowVersion: overflow, NextTurnSequence: 1, CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"session sequence", func() error {
			_, err := RestoreSession(SessionSnapshot{ID: "ses_0001", TenantID: "ten_0001", RowVersion: 1, NextTurnSequence: overflow, CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"turn sequence", func() error {
			_, err := RestoreTurn(TurnSnapshot{ID: "trn_0001", TenantID: "ten_0001", SessionID: "ses_0001", Sequence: overflow, UserMessageID: "msg_0001", Status: TurnStatusRunning, RunIDs: []RunID{"run_0001"}, CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"turn context", func() error {
			_, err := RestoreTurn(TurnSnapshot{ID: "trn_0001", TenantID: "ten_0001", SessionID: "ses_0001", Sequence: 1, BaseContextVersion: overflow, UserMessageID: "msg_0001", Status: TurnStatusRunning, RunIDs: []RunID{"run_0001"}, CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"run number", func() error {
			_, err := RestoreRun(RunSnapshot{ID: "run_0001", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunNo: overflow, Status: RunStatusQueued, CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"run context", func() error {
			_, err := RestoreRun(RunSnapshot{ID: "run_0001", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunNo: 1, BaseContextVersion: overflow, Status: RunStatusQueued, CreatedAt: testTime, UpdatedAt: testTime})
			return err
		}},
		{"step number", func() error {
			_, err := NewStep("stp_0001", "ten_0001", "run_0001", overflow, StepKindTool, testTime)
			return err
		}},
		{"attempt number", func() error { _, err := NewAttempt("att_0001", "ten_0001", "stp_0001", overflow, testTime); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.restore(); !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}

	saturated, err := RestoreSession(SessionSnapshot{
		ID: "ses_0001", TenantID: "ten_0001", RowVersion: maxSignedInt64,
		NextTurnSequence: 1, CreatedAt: testTime, UpdatedAt: testTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saturated.ReserveTurnRun(0, "run_0001", testTime.Add(time.Minute)); !errors.Is(err, ErrValidation) {
		t.Fatalf("saturated reserve error = %v", err)
	}
}

func TestRegenerationConflictOrderingAndOwnership(t *testing.T) {
	t.Parallel()
	session, turn := committedSessionAndTurn(t)
	next := newRunFor(t, "run_0002", 2, session.ContextVersion())
	if err := session.ReserveRegeneration(0, nil, nil, testTime.Add(time.Minute)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale error = %v", err)
	}
	foreign, err := NewRun("run_0002", "ten_9999", "ses_0001", turn.ID(), 2, session.ContextVersion(), testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ReserveRegeneration(session.ContextVersion(), turn, foreign, testTime.Add(5*time.Minute)); !errors.Is(err, ErrValidation) {
		t.Fatalf("ownership error = %v", err)
	}
	if err := session.ReserveRegeneration(session.ContextVersion(), turn, next, testTime.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := session.ReserveRegeneration(session.ContextVersion(), turn, next, testTime.Add(6*time.Minute)); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("busy error = %v", err)
	}
}

func mustSession(t *testing.T) *Session {
	t.Helper()
	session, err := NewSession("ses_0001", "ten_0001", testTime)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return session
}

func committedSessionAndTurn(t *testing.T) (*Session, *Turn) {
	t.Helper()
	session := mustSession(t)
	sequence, err := session.ReserveTurnRun(0, "run_0001", testTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	run := newRunFor(t, "run_0001", 1, 0)
	turn, err := NewTurn("trn_0001", "ten_0001", "ses_0001", sequence, 0, "msg_0001", run.ID(), testTime)
	if err != nil {
		t.Fatal(err)
	}
	message, err := NewAssistantCandidate("msg_0002", "ten_0001", "ses_0001", turn.ID(), run.ID(), "answer", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Start(testTime.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := run.Succeed(message.ID(), testTime.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := session.CommitRun(turn, run, message, testTime.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return session, turn
}
