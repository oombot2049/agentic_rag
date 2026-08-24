package contextmgr

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
)

func TestBudgeterKeepsRecentWholeTurnAndExplainsDrops(t *testing.T) {
	snapshot := builtSnapshot(t)
	budgeter := testBudgeter(t, 19, BudgetConfig{SafetyMarginTokens: 2, EntryOverheadTokens: 1}, wordTokenizer{})
	plan, err := budgeter.Apply(BudgetRequest{ModelID: "model-v1", ReservedOutputTokens: 5, Snapshot: snapshot,
		RAGEvidence: []Evidence{{ID: "rag-1", Content: "retrieved evidence"}}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.InputUsedTokens()+plan.ReservedOutputTokens()+plan.SafetyMarginTokens() > plan.WindowTokens() {
		t.Fatalf("window invariant: %+v", plan)
	}
	entries := plan.Entries()
	if len(entries) != 4 || entries[0].Source != SourceSystem || entries[1].TurnSequence != 2 ||
		entries[2].TurnSequence != 2 || entries[3].Source != SourceCurrentUser {
		t.Fatalf("selected entries=%+v", entries)
	}
	droppedOld := 0
	for _, decision := range plan.Decisions() {
		if decision.Source == SourceHistory && decision.Action == DecisionDropped && decision.Reason == "history_pair_does_not_fit" {
			droppedOld++
		}
	}
	if droppedOld != 2 {
		t.Fatalf("decisions=%+v", plan.Decisions())
	}
}

func TestBudgeterTruncatesCurrentUserButNeverSystem(t *testing.T) {
	conversation := validConversation()
	conversation.Current.Content = "one two three four five six"
	builder, _ := New(&contextStoreStub{value: conversation})
	snapshot, _ := builder.Build(testContext(), BuildRequest{Query: validBuildRequest().Query, SystemInstructions: []string{"system policy"}})
	budgeter := testBudgeter(t, 10, BudgetConfig{SafetyMarginTokens: 1, EntryOverheadTokens: 1}, wordTokenizer{})
	plan, err := budgeter.Apply(BudgetRequest{ModelID: "model-v1", ReservedOutputTokens: 2, Snapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	entries := plan.Entries()
	if len(entries) != 2 || entries[0].Content != "system policy" || entries[1].Content != "one two three" {
		t.Fatalf("entries=%+v", entries)
	}
	if plan.Decisions()[len(plan.Decisions())-1].Action != DecisionTruncated {
		t.Fatalf("decisions=%+v", plan.Decisions())
	}
	overflow := testBudgeter(t, 5, BudgetConfig{SafetyMarginTokens: 1, EntryOverheadTokens: 1}, wordTokenizer{})
	if _, err := overflow.Apply(BudgetRequest{ModelID: "model-v1", ReservedOutputTokens: 2, Snapshot: snapshot}); !errors.Is(err, ErrMandatoryOverflow) {
		t.Fatalf("system overflow error=%v", err)
	}
}

func TestBudgeterWindowInvariantProperty(t *testing.T) {
	snapshot := builtSnapshot(t)
	random := rand.New(rand.NewSource(42))
	for iteration := 0; iteration < 1000; iteration++ {
		window := 8 + random.Intn(200)
		reserved := 1 + random.Intn(window-3)
		budgeter := testBudgeter(t, window, BudgetConfig{SafetyMarginTokens: 1, EntryOverheadTokens: random.Intn(3)}, wordTokenizer{})
		plan, err := budgeter.Apply(BudgetRequest{ModelID: "model-v1", ReservedOutputTokens: reserved, Snapshot: snapshot,
			RAGEvidence: []Evidence{{ID: "rag-1", Content: "alpha beta gamma delta"}}})
		if err != nil {
			if !errors.Is(err, ErrMandatoryOverflow) {
				t.Fatalf("iteration %d error=%v", iteration, err)
			}
			continue
		}
		if plan.InputUsedTokens() > plan.InputLimitTokens() || plan.InputUsedTokens()+reserved+1 > window {
			t.Fatalf("iteration %d plan exceeded window: used=%d limit=%d window=%d", iteration, plan.InputUsedTokens(), plan.InputLimitTokens(), window)
		}
		assertWholeHistory(t, plan.Entries())
	}
}

func TestBudgeterFailsClosedForModelAndTokenizerErrors(t *testing.T) {
	snapshot := builtSnapshot(t)
	registry, _ := NewRegistry([]Model{{ID: "model-v1", WindowTokens: 100, Tokenizer: wordTokenizer{}}})
	budgeter, _ := NewBudgeter(registry, DefaultBudgetConfig())
	if _, err := budgeter.Apply(BudgetRequest{ModelID: "model-latest", ReservedOutputTokens: 10, Snapshot: snapshot}); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown model error=%v", err)
	}
	conversation := validConversation()
	conversation.Current.Content = "one two three four five six"
	builder, _ := New(&contextStoreStub{value: conversation})
	longCurrent, _ := builder.Build(testContext(), BuildRequest{
		Query:              validBuildRequest().Query,
		SystemInstructions: []string{"system policy"},
	})
	bad := testBudgeter(t, 7, BudgetConfig{SafetyMarginTokens: 1, EntryOverheadTokens: 0}, dishonestTokenizer{})
	if _, err := bad.Apply(BudgetRequest{ModelID: "model-v1", ReservedOutputTokens: 2, Snapshot: longCurrent}); !errors.Is(err, ErrTokenizer) {
		t.Fatalf("dishonest tokenizer error=%v", err)
	}
	if _, err := NewRegistry([]Model{{ID: "model-v1", WindowTokens: 0, Tokenizer: wordTokenizer{}}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("registry error=%v", err)
	}
}

func TestBudgeterValidatesReservationsAndEvidence(t *testing.T) {
	snapshot := builtSnapshot(t)
	budgeter := testBudgeter(t, 20, BudgetConfig{SafetyMarginTokens: 2, EntryOverheadTokens: 1}, wordTokenizer{})

	for _, reserved := range []int{0, 18, 20} {
		if _, err := budgeter.Apply(BudgetRequest{
			ModelID:              "model-v1",
			ReservedOutputTokens: reserved,
			Snapshot:             snapshot,
		}); !errors.Is(err, ErrMandatoryOverflow) {
			t.Fatalf("reserved=%d error=%v", reserved, err)
		}
	}

	duplicate := []Evidence{{ID: "evidence-1", Content: "first"}, {ID: "evidence-1", Content: "second"}}
	if _, err := budgeter.Apply(BudgetRequest{
		ModelID:              "model-v1",
		ReservedOutputTokens: 2,
		Snapshot:             snapshot,
		RAGEvidence:          duplicate,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("duplicate evidence error=%v", err)
	}
}

func TestBudgetPlanAndRequestAreImmutable(t *testing.T) {
	snapshot := builtSnapshot(t)
	evidence := []Evidence{{ID: "evidence-1", Content: "retrieved fact"}}
	budgeter := testBudgeter(t, 100, BudgetConfig{SafetyMarginTokens: 2, EntryOverheadTokens: 1}, wordTokenizer{})
	plan, err := budgeter.Apply(BudgetRequest{
		ModelID:              "model-v1",
		ReservedOutputTokens: 10,
		Snapshot:             snapshot,
		RAGEvidence:          evidence,
	})
	if err != nil {
		t.Fatal(err)
	}

	evidence[0].Content = "mutated request"
	entries := plan.Entries()
	entries[0].Content = "mutated result"
	decisions := plan.Decisions()
	decisions[0].Reason = "mutated result"
	if plan.Entries()[0].Content == "mutated result" || plan.Decisions()[0].Reason == "mutated result" {
		t.Fatal("plan exposed mutable internal slices")
	}
	found := false
	for _, entry := range plan.Entries() {
		if entry.ReferenceID == "evidence-1" {
			found = entry.Content == "retrieved fact"
		}
	}
	if !found {
		t.Fatal("plan did not retain the original evidence value")
	}
}

type wordTokenizer struct{}

func (wordTokenizer) Count(value string) (int, error) { return len(strings.Fields(value)), nil }
func (wordTokenizer) Truncate(value string, limit int) (string, error) {
	values := strings.Fields(value)
	if limit < len(values) {
		values = values[:limit]
	}
	return strings.Join(values, " "), nil
}

type dishonestTokenizer struct{ wordTokenizer }

func (dishonestTokenizer) Truncate(value string, _ int) (string, error) { return value, nil }

func testBudgeter(t *testing.T, window int, configuration BudgetConfig, tokenizer Tokenizer) *Budgeter {
	t.Helper()
	registry, err := NewRegistry([]Model{{ID: "model-v1", WindowTokens: window, Tokenizer: tokenizer}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := NewBudgeter(registry, configuration)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func builtSnapshot(t *testing.T) Snapshot {
	t.Helper()
	builder, _ := New(&contextStoreStub{value: validConversation()})
	value, err := builder.Build(testContext(), validBuildRequest())
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testContext() context.Context { return context.Background() }

func assertWholeHistory(t *testing.T, entries []Entry) {
	t.Helper()
	counts := make(map[uint64]int)
	for _, entry := range entries {
		if entry.Source == SourceHistory {
			counts[entry.TurnSequence]++
		}
	}
	for sequence, count := range counts {
		if count != 2 {
			t.Fatalf("history sequence %d count=%d", sequence, count)
		}
	}
}
