package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/tools"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
)

func TestToolLedgerRepositoryTakeoverReplayAndTenantIsolation(t *testing.T) {
	pool := sessionAPITestPool(t)
	ctx := context.Background()
	migrator, _ := postgres.NewMigrator(pool, coremigrations.Files, ".")
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)
	seedFinalCommit(t, ctx, pool, at)
	if _, err := pool.ExecContext(ctx, `INSERT INTO steps(tenant_id,id,run_id,step_no,kind,status,created_at,updated_at,row_version) VALUES('ten_0001','stp_0001','run_0001',1,'TOOL','RUNNING',$1,$1,1)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO attempts(tenant_id,id,step_id,attempt_no,status,started_at,row_version) VALUES('ten_0001','att_0001','stp_0001',1,'STARTED',$1,1)`, at); err != nil {
		t.Fatal(err)
	}

	store, err := NewToolLedgerStore(pool, func(ctx context.Context, options *sql.TxOptions, work func(context.Context, DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx postgres.DBTX) error { return work(ctx, tx) })
	})
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := tools.NewHMACKeyDeriver([]byte("01234567890123456789012345678901"))
	ledger, _ := tools.NewLedger(store, keys)
	first, err := ledger.Begin(ctx, repositoryLedgerCommand("att_0001", 1, at))
	if err != nil || first.Decision != tools.DecisionExecute {
		t.Fatalf("first=%+v err=%v", first, err)
	}

	if _, err = pool.ExecContext(ctx, `INSERT INTO attempts(tenant_id,id,step_id,attempt_no,status,started_at,row_version) VALUES('ten_0001','att_0002','stp_0001',2,'STARTED',$1,1)`, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	takeover, err := ledger.Begin(ctx, repositoryLedgerCommand("att_0002", 2, at.Add(time.Minute)))
	if err != nil || takeover.Decision != tools.DecisionQueryResult || takeover.Call.Attempts[0].Status != "UNKNOWN" {
		t.Fatalf("takeover=%+v err=%v", takeover, err)
	}
	completed, err := ledger.RecordOutcome(ctx, tools.OutcomeCommand{
		TenantID: "ten_0001", CallID: "tcl_0001", AttemptID: "att_0002",
		ExpectedRowVersion: takeover.Call.RowVersion, Status: tools.CallSucceeded,
		Result: json.RawMessage(`{"answer":"durable"}`), At: at.Add(2 * time.Minute),
	})
	if err != nil || completed.Status != tools.CallSucceeded {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	replay, err := ledger.Begin(ctx, repositoryLedgerCommand("att_0002", 2, at.Add(3*time.Minute)))
	var replayResult map[string]string
	decodeErr := json.Unmarshal(replay.Call.Result, &replayResult)
	if err != nil || decodeErr != nil || replay.Decision != tools.DecisionReturnResult || replayResult["answer"] != "durable" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err = ledger.Get(ctx, "ten_9999", "tcl_0001"); !errors.Is(err, tools.ErrLedgerNotFound) {
		t.Fatalf("foreign tenant=%v", err)
	}
	if _, err = ledger.RecordOutcome(ctx, tools.OutcomeCommand{TenantID: "ten_0001", CallID: "tcl_0001",
		AttemptID: "att_0002", ExpectedRowVersion: takeover.Call.RowVersion, Status: tools.CallUnknown,
		FailureClass: "late_writer", At: at.Add(4 * time.Minute)}); !errors.Is(err, tools.ErrLedgerStale) {
		t.Fatalf("late writer=%v", err)
	}
}

func repositoryLedgerCommand(attemptID string, attemptNo uint64, at time.Time) tools.BeginCommand {
	return tools.BeginCommand{CallID: "tcl_0001", RunID: "run_0001", StepID: "stp_0001",
		AttemptID: domain.AttemptID(attemptID), AttemptNo: attemptNo, At: at,
		Invocation: tools.Invocation{
			Definition: tools.Definition{Name: "knowledge.search", Version: "1.0.0", Scope: tools.ScopeRun, Effect: tools.EffectReadOnly},
			Scope:      tools.Scope{TenantID: "ten_0001", SessionID: "ses_0001", RunID: "run_0001"},
			Arguments:  json.RawMessage(`{"query":"hello"}`),
		}}
}
