package repository

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/turn"
)

func TestAuthorizeFinalNewTurnRegenerationAndFencing(t *testing.T) {
	at := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	command := turn.FinalRequest{TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001",
		ExpectedContextVersion: 2, ExpectedSessionRowVersion: 5, At: at,
		Authority: turn.FinalAuthority{Owner: "worker", Token: 7, ExecutionVersion: 9}}
	base := finalSnapshot{contextVersion: 2, rowVersion: 5, committedSequence: 2, turnSequence: 3, turnBase: 2, runBase: 2,
		activeRunID: "run_0001", turnStatus: "RUNNING", runStatus: "RUNNING", leaseOwner: "worker", leaseToken: 7,
		executionVersion: 9, leaseExpires: validNullTime(at.Add(time.Minute))}
	if err := authorizeFinal(command, base); err != nil {
		t.Fatalf("new Turn: %v", err)
	}
	regeneration := base
	regeneration.turnSequence, regeneration.committedSequence, regeneration.turnBase = 2, 2, 0
	regeneration.headTurnID = "trn_0001"
	if err := authorizeFinal(command, regeneration); err != nil {
		t.Fatalf("regeneration: %v", err)
	}

	for name, mutate := range map[string]func(*finalSnapshot){
		"context": func(s *finalSnapshot) { s.contextVersion++ }, "row": func(s *finalSnapshot) { s.rowVersion++ },
		"active": func(s *finalSnapshot) { s.activeRunID = "run_other" }, "turn": func(s *finalSnapshot) { s.turnStatus = "FAILED" },
		"run base": func(s *finalSnapshot) { s.runBase++ }, "run status": func(s *finalSnapshot) { s.runStatus = "FAILED" },
	} {
		t.Run(name, func(t *testing.T) {
			state := base
			mutate(&state)
			if !errors.Is(authorizeFinal(command, state), turn.ErrFinalConflict) {
				t.Fatal("conflict accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*finalSnapshot){
		"owner": func(s *finalSnapshot) { s.leaseOwner = "old" }, "token": func(s *finalSnapshot) { s.leaseToken-- },
		"execution": func(s *finalSnapshot) { s.executionVersion-- }, "expired": func(s *finalSnapshot) { s.leaseExpires = validNullTime(at) },
	} {
		t.Run(name, func(t *testing.T) {
			state := base
			mutate(&state)
			if !errors.Is(authorizeFinal(command, state), turn.ErrFinalFenced) {
				t.Fatal("stale authority accepted")
			}
		})
	}
}

func validNullTime(value time.Time) sql.NullTime { return sql.NullTime{Time: value, Valid: true} }
