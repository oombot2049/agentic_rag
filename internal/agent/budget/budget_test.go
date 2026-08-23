package budget

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/agent/action"
)

var budgetTestTime = time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)

func TestModelReservationExactFitIdempotencySettlementAndRelease(t *testing.T) {
	clock := &budgetClock{now: budgetTestTime}
	controller := testController(t, Limits{MaxSteps: 4, MaxDuration: time.Minute, MaxTokens: 10,
		MaxToolCalls: 2, MaxCostNanos: 100, MaxConsecutiveSameAction: 2, CycleWindow: 6, MaxCycleRepeats: 3}, clock)
	initial := controller.Start()
	request := ModelReservation{OperationKey: "op-1", InputTokens: 4, MaxOutputTokens: 6, MaxCostNanos: 100}
	reserved, decision, err := controller.ReserveModel(initial, request)
	if err != nil || !decision.Allowed() {
		t.Fatalf("decision=%+v error=%v", decision, err)
	}
	again, decision, err := controller.ReserveModel(reserved, request)
	if err != nil || !decision.Allowed() || !reflect.DeepEqual(controller.Snapshot(reserved), controller.Snapshot(again)) {
		t.Fatalf("idempotent reserve decision=%+v error=%v", decision, err)
	}
	if _, _, err := controller.ReserveModel(reserved, ModelReservation{OperationKey: "op-2", InputTokens: 1, MaxOutputTokens: 1}); !errors.Is(err, ErrOutstandingReservation) {
		t.Fatalf("second operation error=%v", err)
	}
	committed, err := controller.CommitModel(reserved, ModelSettlement{OperationKey: "op-1", InputTokens: 4, OutputTokens: 6, CostNanos: 100})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := controller.Snapshot(committed)
	if snapshot.Tokens != 10 || snapshot.CostNanos != 100 || snapshot.Outstanding != nil ||
		controller.Check(committed).Reason != ReasonMaxTokens {
		t.Fatalf("snapshot=%+v decision=%+v", snapshot, controller.Check(committed))
	}

	reserved, _, _ = controller.ReserveModel(initial, ModelReservation{OperationKey: "op-release", InputTokens: 1, MaxOutputTokens: 1, MaxCostNanos: 1})
	released, err := controller.ReleaseModel(reserved, "op-release")
	if err != nil || controller.Snapshot(released).Outstanding != nil {
		t.Fatalf("released=%+v error=%v", controller.Snapshot(released), err)
	}
}

func TestEveryBudgetExactFitAndOneUnitOverflow(t *testing.T) {
	limits := Limits{MaxSteps: 2, MaxDuration: 10 * time.Second, MaxTokens: 10,
		MaxToolCalls: 1, MaxCostNanos: 20, MaxConsecutiveSameAction: 3, CycleWindow: 6, MaxCycleRepeats: 3}
	clock := &budgetClock{now: budgetTestTime}
	controller := testController(t, limits, clock)

	tests := []struct {
		name   string
		value  ModelReservation
		reason LimitReason
	}{
		{"token overflow", ModelReservation{OperationKey: "op-token", InputTokens: 5, MaxOutputTokens: 6, MaxCostNanos: 1}, ReasonMaxTokens},
		{"cost overflow", ModelReservation{OperationKey: "op-cost", InputTokens: 1, MaxOutputTokens: 1, MaxCostNanos: 21}, ReasonMaxCost},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initial := controller.Start()
			next, decision, err := controller.ReserveModel(initial, test.value)
			if err != nil || decision.Status != StatusCompletedWithLimit || decision.Reason != test.reason ||
				!reflect.DeepEqual(controller.Snapshot(initial), controller.Snapshot(next)) {
				t.Fatalf("decision=%+v error=%v next=%+v", decision, err, controller.Snapshot(next))
			}
		})
	}

	ledger := controller.Start()
	first := mustAction(t, `{"schema_version":1,"type":"TOOL","reason_code":"CALL","tool":{"name":"crm.lookup","arguments":{"id":1}}}`)
	ledger, decision, err := controller.AdmitAction(ledger, first)
	if err != nil || !decision.Allowed() {
		t.Fatal(err)
	}
	before := controller.Snapshot(ledger)
	secondTool := mustAction(t, `{"schema_version":1,"type":"TOOL","reason_code":"CALL_AGAIN","tool":{"name":"crm.lookup","arguments":{"id":2}}}`)
	after, decision, err := controller.AdmitAction(ledger, secondTool)
	if err != nil || decision.Reason != ReasonMaxToolCalls || !reflect.DeepEqual(before, controller.Snapshot(after)) {
		t.Fatalf("tool decision=%+v error=%v", decision, err)
	}

	final := mustAction(t, `{"schema_version":1,"type":"FINAL","reason_code":"READY","final":{"answer":"done","citation_ids":[]}}`)
	ledger, decision, _ = controller.AdmitAction(ledger, final)
	if !decision.Allowed() || controller.Snapshot(ledger).Steps != 2 {
		t.Fatalf("final decision=%+v snapshot=%+v", decision, controller.Snapshot(ledger))
	}
	after, decision, _ = controller.AdmitAction(ledger, final)
	if decision.Reason != ReasonMaxSteps || !reflect.DeepEqual(controller.Snapshot(ledger), controller.Snapshot(after)) {
		t.Fatalf("step decision=%+v", decision)
	}

	clock.advance(10 * time.Second)
	after, decision, err = controller.ReserveModel(controller.Start(), ModelReservation{OperationKey: "op-time", InputTokens: 1, MaxOutputTokens: 1})
	// Start uses the advanced clock, so test deadline equality with a ledger started earlier.
	_ = after
	_ = decision
	_ = err
	earlier := Ledger{startedAt: budgetTestTime}
	after, decision, err = controller.ReserveModel(earlier, ModelReservation{OperationKey: "op-time", InputTokens: 1, MaxOutputTokens: 1})
	if err != nil || decision.Reason != ReasonMaxDuration || !reflect.DeepEqual(controller.Snapshot(earlier), controller.Snapshot(after)) {
		t.Fatalf("time decision=%+v error=%v", decision, err)
	}
}

func TestSettlementRejectsProviderUsageBeyondReservation(t *testing.T) {
	controller := testController(t, DefaultLimits(), &budgetClock{now: budgetTestTime})
	reserved, _, _ := controller.ReserveModel(controller.Start(), ModelReservation{OperationKey: "op-1", InputTokens: 10, MaxOutputTokens: 5, MaxCostNanos: 100})
	tests := []ModelSettlement{
		{OperationKey: "other", InputTokens: 10, OutputTokens: 5, CostNanos: 100},
		{OperationKey: "op-1", InputTokens: 11, OutputTokens: 4, CostNanos: 100},
		{OperationKey: "op-1", InputTokens: 10, OutputTokens: 6, CostNanos: 100},
		{OperationKey: "op-1", InputTokens: 10, OutputTokens: 5, CostNanos: 101},
	}
	for _, settlement := range tests {
		if _, err := controller.CommitModel(reserved, settlement); !errors.Is(err, ErrSettlementMismatch) {
			t.Fatalf("settlement=%+v error=%v", settlement, err)
		}
	}
	if _, err := controller.ReleaseModel(reserved, "other"); !errors.Is(err, ErrSettlementMismatch) {
		t.Fatalf("release error=%v", err)
	}
}

func TestActionLoopDetectionIgnoresReasonCodeAndFindsCycles(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxSteps, limits.MaxToolCalls = 20, 10
	limits.MaxConsecutiveSameAction, limits.CycleWindow, limits.MaxCycleRepeats = 2, 12, 3
	controller := testController(t, limits, &budgetClock{now: budgetTestTime})

	t.Run("consecutive same payload", func(t *testing.T) {
		ledger := controller.Start()
		for _, reason := range []string{"FIRST", "SECOND"} {
			value := mustAction(t, `{"schema_version":1,"type":"SEARCH","reason_code":"`+reason+`","search":{"query":"same","top_k":1,"knowledge_base_ids":["kb_0001"]}}`)
			var decision Decision
			ledger, decision, _ = controller.AdmitAction(ledger, value)
			if !decision.Allowed() {
				t.Fatalf("early decision=%+v", decision)
			}
		}
		before := controller.Snapshot(ledger)
		third := mustAction(t, `{"schema_version":1,"type":"SEARCH","reason_code":"CHANGED_REASON","search":{"query":"same","top_k":1,"knowledge_base_ids":["kb_0001"]}}`)
		after, decision, err := controller.AdmitAction(ledger, third)
		if err != nil || decision.Reason != ReasonActionLoop || !reflect.DeepEqual(before, controller.Snapshot(after)) {
			t.Fatalf("decision=%+v error=%v", decision, err)
		}
	})

	t.Run("alternating cycle", func(t *testing.T) {
		ledger := controller.Start()
		queries := []string{"a", "b", "a", "b", "a"}
		for index, query := range queries {
			value := searchAction(t, query, index)
			var decision Decision
			ledger, decision, _ = controller.AdmitAction(ledger, value)
			if !decision.Allowed() {
				t.Fatalf("early index=%d decision=%+v", index, decision)
			}
		}
		before := controller.Snapshot(ledger)
		after, decision, err := controller.AdmitAction(ledger, searchAction(t, "b", 99))
		if err != nil || decision.Reason != ReasonActionLoop || !reflect.DeepEqual(before, controller.Snapshot(after)) {
			t.Fatalf("decision=%+v error=%v", decision, err)
		}
	})
}

func TestSnapshotRestoreCopiesAndRejectsCorruption(t *testing.T) {
	controller := testController(t, DefaultLimits(), &budgetClock{now: budgetTestTime})
	ledger, _, _ := controller.AdmitAction(controller.Start(), searchAction(t, "restore", 1))
	ledger, _, _ = controller.ReserveModel(ledger, ModelReservation{OperationKey: "op-restore", InputTokens: 10, MaxOutputTokens: 5, MaxCostNanos: 100})
	snapshot := controller.Snapshot(ledger)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := controller.Restore(decoded)
	if err != nil || !reflect.DeepEqual(controller.Snapshot(ledger), controller.Snapshot(restored)) {
		t.Fatalf("restored=%+v error=%v", controller.Snapshot(restored), err)
	}
	decoded.Fingerprints[0] = "mutated"
	decoded.Outstanding.OperationKey = "mutated"
	if controller.Snapshot(restored).Fingerprints[0] == "mutated" || controller.Snapshot(restored).Outstanding.OperationKey == "mutated" {
		t.Fatal("Restore retained mutable snapshot data")
	}

	corruptions := []Snapshot{
		{SchemaVersion: 2, StartedAt: budgetTestTime},
		{SchemaVersion: 1, StartedAt: budgetTestTime, Steps: -1},
		{SchemaVersion: 1, StartedAt: budgetTestTime, Tokens: DefaultLimits().MaxTokens + 1},
		{SchemaVersion: 1, StartedAt: budgetTestTime, Steps: 1, Fingerprints: []string{"not-a-digest"}},
		{SchemaVersion: 1, StartedAt: budgetTestTime, Outstanding: &ReservationSnapshot{OperationKey: "op", InputTokens: DefaultLimits().MaxTokens, MaxOutputTokens: 1}},
	}
	for _, corrupt := range corruptions {
		if _, err := controller.Restore(corrupt); !errors.Is(err, ErrCorruptSnapshot) {
			t.Fatalf("corrupt=%+v error=%v", corrupt, err)
		}
	}
}

func TestBudgetInvariantProperty(t *testing.T) {
	limits := Limits{MaxSteps: 20, MaxDuration: time.Hour, MaxTokens: 10_000,
		MaxToolCalls: 10, MaxCostNanos: 1_000_000, MaxConsecutiveSameAction: 5, CycleWindow: 30, MaxCycleRepeats: 3}
	controller := testController(t, limits, &budgetClock{now: budgetTestTime})
	random := rand.New(rand.NewSource(42))
	for iteration := 0; iteration < 1_000; iteration++ {
		ledger := controller.Start()
		for operation := 0; operation < 50; operation++ {
			before := controller.Snapshot(ledger)
			input := int64(random.Intn(500))
			output := int64(1 + random.Intn(500))
			cost := int64(random.Intn(100_000))
			key := "op-" + time.Unix(int64(iteration), int64(operation)).UTC().Format("150405.000000000")
			reserved, decision, err := controller.ReserveModel(ledger, ModelReservation{OperationKey: key,
				InputTokens: input, MaxOutputTokens: output, MaxCostNanos: cost})
			if err != nil {
				t.Fatalf("iteration=%d operation=%d reserve error=%v", iteration, operation, err)
			}
			if !decision.Allowed() {
				if !reflect.DeepEqual(before, controller.Snapshot(reserved)) {
					t.Fatal("rejected reservation mutated Ledger")
				}
				break
			}
			ledger, err = controller.CommitModel(reserved, ModelSettlement{OperationKey: key,
				InputTokens: input, OutputTokens: output, CostNanos: cost})
			if err != nil {
				t.Fatal(err)
			}
			ledger, decision, err = controller.AdmitAction(ledger, searchAction(t, key, operation))
			if err != nil {
				t.Fatal(err)
			}
			if !decision.Allowed() {
				break
			}
			snapshot := controller.Snapshot(ledger)
			if snapshot.Steps > limits.MaxSteps || snapshot.Tokens > limits.MaxTokens || snapshot.ToolCalls > limits.MaxToolCalls ||
				snapshot.CostNanos > limits.MaxCostNanos || snapshot.Outstanding != nil {
				t.Fatalf("invariant iteration=%d operation=%d snapshot=%+v", iteration, operation, snapshot)
			}
		}
	}
}

type budgetClock struct{ now time.Time }

func (c *budgetClock) Now() time.Time              { return c.now }
func (c *budgetClock) advance(value time.Duration) { c.now = c.now.Add(value) }

func testController(t *testing.T, limits Limits, clock Clock) *Controller {
	t.Helper()
	value, err := New(limits, clock)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustAction(t *testing.T, raw string) action.Action {
	t.Helper()
	value, err := action.Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func searchAction(t *testing.T, query string, sequence int) action.Action {
	t.Helper()
	return mustAction(t, `{"schema_version":1,"type":"SEARCH","reason_code":"SEARCH_`+
		string(rune('A'+sequence%26))+`","search":{"query":"`+query+`","top_k":1,"knowledge_base_ids":["kb_0001"]}}`)
}
