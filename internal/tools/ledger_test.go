package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var ledgerTestTime = time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)

func TestLedgerTakeoverQueriesAndReplaysConfirmedResult(t *testing.T) {
	ledger := newTestLedger(t)
	first, err := ledger.Begin(context.Background(), ledgerCommand("att_0001", 1, ledgerTestTime))
	if err != nil || first.Decision != DecisionExecute {
		t.Fatalf("first Begin = %+v, %v", first, err)
	}

	// Simulate provider success followed by Worker death before RecordOutcome.
	takeover, err := ledger.Begin(context.Background(), ledgerCommand("att_0002", 2, ledgerTestTime.Add(time.Minute)))
	if err != nil || takeover.Decision != DecisionQueryResult {
		t.Fatalf("takeover Begin = %+v, %v", takeover, err)
	}
	if len(takeover.Call.Attempts) != 2 || takeover.Call.Attempts[0].Status != domain.AttemptStatusUnknown ||
		takeover.Call.InvocationKey != first.Call.InvocationKey {
		t.Fatalf("takeover call = %+v", takeover.Call)
	}

	confirmed, err := ledger.RecordOutcome(context.Background(), OutcomeCommand{
		TenantID: "ten_0001", CallID: "tcl_0001", AttemptID: "att_0002",
		ExpectedRowVersion: takeover.Call.RowVersion, Status: CallSucceeded,
		Result: json.RawMessage(`{"answer":"provider result"}`), At: ledgerTestTime.Add(2 * time.Minute),
	})
	if err != nil || confirmed.Status != CallSucceeded {
		t.Fatalf("confirmed outcome = %+v, %v", confirmed, err)
	}
	replay, err := ledger.Begin(context.Background(), ledgerCommand("att_0003", 3, ledgerTestTime.Add(3*time.Minute)))
	if err != nil || replay.Decision != DecisionReturnResult || string(replay.Call.Result) != `{"answer":"provider result"}` {
		t.Fatalf("terminal replay = %+v, %v", replay, err)
	}
	if len(replay.Call.Attempts) != 2 {
		t.Fatalf("terminal replay created attempt: %+v", replay.Call.Attempts)
	}
}

func TestLedgerUnknownIsDurableAndDistinctFromFailure(t *testing.T) {
	ledger := newTestLedger(t)
	first, err := ledger.Begin(context.Background(), ledgerCommand("att_0001", 1, ledgerTestTime))
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := ledger.RecordOutcome(context.Background(), OutcomeCommand{
		TenantID: "ten_0001", CallID: "tcl_0001", AttemptID: "att_0001",
		ExpectedRowVersion: first.Call.RowVersion, Status: CallUnknown,
		FailureClass: "provider_outcome_unknown", At: ledgerTestTime.Add(time.Second),
	})
	if err != nil || unknown.Status != CallUnknown || unknown.Status.Terminal() {
		t.Fatalf("unknown = %+v, %v", unknown, err)
	}
	takeover, err := ledger.Begin(context.Background(), ledgerCommand("att_0002", 2, ledgerTestTime.Add(time.Minute)))
	if err != nil || takeover.Decision != DecisionQueryResult {
		t.Fatalf("takeover = %+v, %v", takeover, err)
	}
	failed, err := ledger.RecordOutcome(context.Background(), OutcomeCommand{
		TenantID: "ten_0001", CallID: "tcl_0001", AttemptID: "att_0002",
		ExpectedRowVersion: takeover.Call.RowVersion, Status: CallFailed,
		FailureClass: "provider_rejected", At: ledgerTestTime.Add(2 * time.Minute),
	})
	if err != nil || !failed.Status.Terminal() {
		t.Fatalf("failure = %+v, %v", failed, err)
	}
	replay, err := ledger.Begin(context.Background(), ledgerCommand("att_0003", 3, ledgerTestTime.Add(3*time.Minute)))
	if err != nil || replay.Decision != DecisionReturnFailure {
		t.Fatalf("failure replay = %+v, %v", replay, err)
	}
}

func TestLedgerRejectsMutationAndStaleWriters(t *testing.T) {
	ledger := newTestLedger(t)
	first, err := ledger.Begin(context.Background(), ledgerCommand("att_0001", 1, ledgerTestTime))
	if err != nil {
		t.Fatal(err)
	}
	changed := ledgerCommand("att_0002", 2, ledgerTestTime.Add(time.Minute))
	changed.Invocation.Arguments = json.RawMessage(`{"query":"changed"}`)
	if _, err := ledger.Begin(context.Background(), changed); !errors.Is(err, ErrLedgerConflict) {
		t.Fatalf("request mutation: %v", err)
	}

	stale := OutcomeCommand{TenantID: "ten_0001", CallID: "tcl_0001", AttemptID: "att_0001",
		ExpectedRowVersion: first.Call.RowVersion + 1, Status: CallSucceeded,
		Result: json.RawMessage(`{"answer":"late"}`), At: ledgerTestTime.Add(time.Second)}
	if _, err := ledger.RecordOutcome(context.Background(), stale); !errors.Is(err, ErrLedgerStale) {
		t.Fatalf("stale writer: %v", err)
	}
	stale.ExpectedRowVersion = first.Call.RowVersion
	stale.AttemptID = "att_other"
	if _, err := ledger.RecordOutcome(context.Background(), stale); !errors.Is(err, ErrLedgerStale) {
		t.Fatalf("foreign attempt: %v", err)
	}

	loaded, err := ledger.Get(context.Background(), "ten_0001", "tcl_0001")
	if err != nil || loaded.Status != CallPrepared {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	if _, err := ledger.Get(context.Background(), "ten_0002", "tcl_0001"); !errors.Is(err, ErrLedgerNotFound) {
		t.Fatalf("foreign tenant: %v", err)
	}
	loaded.Attempts[0].Status = domain.AttemptStatusFailed
	loaded.InvocationKey = "changed"
	again, _ := ledger.Get(context.Background(), "ten_0001", "tcl_0001")
	if again.Attempts[0].Status != domain.AttemptStatusStarted || again.InvocationKey == "changed" {
		t.Fatal("Get leaked mutable state")
	}
}

func TestLedgerArgumentsHashIsCanonical(t *testing.T) {
	ledger := newTestLedger(t)
	firstCommand := ledgerCommand("att_0001", 1, ledgerTestTime)
	firstCommand.Invocation.Arguments = json.RawMessage(`{"limit":5,"query":"hello"}`)
	first, err := ledger.Begin(context.Background(), firstCommand)
	if err != nil || first.Decision != DecisionExecute {
		t.Fatalf("first = %+v, %v", first, err)
	}
	replayCommand := ledgerCommand("att_0001", 1, ledgerTestTime)
	replayCommand.Invocation.Arguments = json.RawMessage(`{ "query": "hello", "limit": 5 }`)
	replay, err := ledger.Begin(context.Background(), replayCommand)
	if err != nil || replay.Decision != DecisionQueryResult || replay.Call.ArgumentsHash != first.Call.ArgumentsHash {
		t.Fatalf("canonical replay = %+v, %v", replay, err)
	}
}

func TestLedgerConcurrentBeginHasOneExecuteDecision(t *testing.T) {
	ledger := newTestLedger(t)
	var executes atomic.Int64
	var wait sync.WaitGroup
	errorsFound := make(chan error, 64)
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := ledger.Begin(context.Background(), ledgerCommand("att_0001", 1, ledgerTestTime))
			if err != nil {
				errorsFound <- err
				return
			}
			if result.Decision == DecisionExecute {
				executes.Add(1)
			} else if result.Decision != DecisionQueryResult {
				errorsFound <- errors.New("unexpected decision")
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if executes.Load() != 1 {
		t.Fatalf("execute decisions = %d", executes.Load())
	}
}

func TestLedgerLostBeginResponseNeverReturnsExecuteAgain(t *testing.T) {
	store := &postCommitFailureStore{MemoryLedgerStore: NewMemoryLedgerStore()}
	keys, _ := NewHMACKeyDeriver([]byte("01234567890123456789012345678901"))
	ledger, _ := NewLedger(store, keys)
	command := ledgerCommand("att_0001", 1, ledgerTestTime)
	if _, err := ledger.Begin(context.Background(), command); !errors.Is(err, errInjectedLostResponse) {
		t.Fatalf("injected post-commit failure = %v", err)
	}
	recovered, err := ledger.Begin(context.Background(), command)
	if err != nil || recovered.Decision != DecisionQueryResult {
		t.Fatalf("recovered Begin = %+v, %v", recovered, err)
	}
	if len(recovered.Call.Attempts) != 1 {
		t.Fatalf("duplicate Attempt persisted: %+v", recovered.Call.Attempts)
	}
}

func TestLedgerValidationAndStableTenantScopedKeys(t *testing.T) {
	deriver, err := NewHMACKeyDeriver([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	left, _ := deriver.Derive("ten_0001", "tcl_0001")
	replay, _ := deriver.Derive("ten_0001", "tcl_0001")
	right, _ := deriver.Derive("ten_0002", "tcl_0001")
	if left == "" || left != replay || left == right {
		t.Fatalf("keys = %q/%q/%q", left, replay, right)
	}
	if _, err := NewHMACKeyDeriver([]byte("short")); !errors.Is(err, ErrLedgerInvalid) {
		t.Fatalf("short secret: %v", err)
	}
	if _, err := NewLedger(nil, deriver); !errors.Is(err, ErrLedgerInvalid) {
		t.Fatalf("nil store: %v", err)
	}

	ledger := newTestLedger(t)
	tests := []struct {
		name   string
		mutate func(*BeginCommand)
	}{
		{"call id", func(value *BeginCommand) { value.CallID = "call" }},
		{"run id", func(value *BeginCommand) { value.RunID = "bad" }},
		{"step id", func(value *BeginCommand) { value.StepID = "bad" }},
		{"attempt id", func(value *BeginCommand) { value.AttemptID = "bad" }},
		{"attempt number", func(value *BeginCommand) { value.AttemptNo = 0 }},
		{"time", func(value *BeginCommand) { value.At = time.Time{} }},
		{"arguments", func(value *BeginCommand) { value.Invocation.Arguments = json.RawMessage(`[]`) }},
		{"effect", func(value *BeginCommand) { value.Invocation.Definition.Effect = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := ledgerCommand("att_0001", 1, ledgerTestTime)
			test.mutate(&value)
			if _, err := ledger.Begin(context.Background(), value); !errors.Is(err, ErrLedgerInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	first, _ := ledger.Begin(context.Background(), ledgerCommand("att_0001", 1, ledgerTestTime))
	invalidOutcomes := []OutcomeCommand{
		{TenantID: "ten_0001", CallID: "tcl_0001", AttemptID: "att_0001", ExpectedRowVersion: first.Call.RowVersion, Status: CallSucceeded, Result: json.RawMessage(`{}`), FailureClass: "unexpected", At: ledgerTestTime},
		{TenantID: "ten_0001", CallID: "tcl_0001", AttemptID: "att_0001", ExpectedRowVersion: first.Call.RowVersion, Status: CallFailed, At: ledgerTestTime},
		{TenantID: "ten_0001", CallID: "tcl_0001", AttemptID: "att_0001", ExpectedRowVersion: first.Call.RowVersion, Status: CallSucceeded, Result: json.RawMessage(`[]`), At: ledgerTestTime},
	}
	for _, value := range invalidOutcomes {
		if _, err := ledger.RecordOutcome(context.Background(), value); !errors.Is(err, ErrLedgerInvalid) {
			t.Fatalf("invalid outcome: %v", err)
		}
	}
}

func ledgerCommand(attemptID domain.AttemptID, attemptNo uint64, at time.Time) BeginCommand {
	return BeginCommand{CallID: "tcl_0001", RunID: "run_0001", StepID: "stp_0001", AttemptID: attemptID,
		AttemptNo: attemptNo, At: at, Invocation: Invocation{
			Definition: Definition{Name: "knowledge.search", Version: "1.0.0", Scope: ScopeRun, Effect: EffectReadOnly},
			Scope:      Scope{TenantID: "ten_0001", SessionID: "ses_0001", RunID: "run_0001"},
			Arguments:  json.RawMessage(`{"query":"hello"}`),
		}}
}

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	keys, err := NewHMACKeyDeriver([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := NewLedger(NewMemoryLedgerStore(), keys)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

var errInjectedLostResponse = errors.New("injected lost response after commit")

type postCommitFailureStore struct {
	*MemoryLedgerStore
	failed bool
}

func (s *postCommitFailureStore) Begin(ctx context.Context, call CallRecord, attempt AttemptRecord) (CallRecord, bool, error) {
	record, created, err := s.MemoryLedgerStore.Begin(ctx, call, attempt)
	if err == nil && !s.failed {
		s.failed = true
		return CallRecord{}, false, errInjectedLostResponse
	}
	return record, created, err
}
