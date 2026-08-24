package modes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/agenticrag/agenticrag/internal/agent"
	"github.com/agenticrag/agenticrag/internal/agent/action"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

func TestDeterministicRAGTransitionsAndPlanner(t *testing.T) {
	raw, err := NewDeterministic("退款多久到账？", "art_context1")
	if err != nil {
		t.Fatal(err)
	}
	planner := NewPlanner()
	plan, err := planner.Next(context.Background(), raw, 1)
	if err != nil || plan.Done || plan.Directive.Kind != domain.StepKindContext || plan.Directive.OperationKey != "rag.query_rewrite.step.1" {
		t.Fatalf("rewrite plan=%+v error=%v", plan, err)
	}
	raw, err = ApplyRewrite(raw, "信用卡退款到账周期")
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = planner.Next(context.Background(), raw, 2)
	if plan.Directive.Kind != domain.StepKindRetrieval || plan.Directive.OperationKey != "rag.retrieve.step.2" {
		t.Fatalf("retrieve plan=%+v", plan)
	}
	raw, err = ApplyEvidence(raw, []action.ArtifactID{"art_chunk001", "art_chunk002"})
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = planner.Next(context.Background(), raw, 3)
	if plan.Directive.Kind != domain.StepKindGeneration || plan.Directive.OperationKey != "rag.generate.step.3" {
		t.Fatalf("generate plan=%+v", plan)
	}
	raw, err = ApplyAnswer(raw, "art_answer01")
	if err != nil {
		t.Fatal(err)
	}
	plan, err = planner.Next(context.Background(), raw, 4)
	if err != nil || !plan.Done {
		t.Fatalf("done plan=%+v error=%v", plan, err)
	}
	if _, err := ApplyRewrite(raw, "again"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("backward transition=%v", err)
	}
}

func TestReActSearchObservationAndFinal(t *testing.T) {
	raw, err := NewReAct("比较退款政策", "art_context1", 3)
	if err != nil {
		t.Fatal(err)
	}
	planner := NewPlanner()
	plan, _ := planner.Next(context.Background(), raw, 1)
	if plan.Directive.Kind != domain.StepKindGeneration || plan.Directive.OperationKey != "react.decide.step.1" {
		t.Fatalf("decision plan=%+v", plan)
	}
	search := decodeAction(t, `{"schema_version":1,"type":"SEARCH","reason_code":"NEED_POLICY","search":{"query":"退款政策","top_k":5,"knowledge_base_ids":["kb_policy1"]}}`)
	raw, err = ApplyReActDecision(raw, search, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = planner.Next(context.Background(), raw, 2)
	if plan.Directive.Kind != domain.StepKindTool || plan.Directive.OperationKey != "knowledge.search.step.2" {
		t.Fatalf("tool plan=%+v", plan)
	}
	raw, err = ApplyObservation(raw, []action.ArtifactID{"art_chunk001"})
	if err != nil {
		t.Fatal(err)
	}
	final := decodeAction(t, `{"schema_version":1,"type":"FINAL","reason_code":"ENOUGH_EVIDENCE","final":{"answer":"答案","citation_ids":["art_chunk001"]}}`)
	raw, err = ApplyReActDecision(raw, final, "art_answer01")
	if err != nil {
		t.Fatal(err)
	}
	plan, err = planner.Next(context.Background(), raw, 4)
	if err != nil || !plan.Done {
		t.Fatalf("done plan=%+v error=%v", plan, err)
	}
}

func TestReActRejectsDuplicateSearchForeignCitationAndIterationOverflow(t *testing.T) {
	search := decodeAction(t, `{"schema_version":1,"type":"SEARCH","reason_code":"NEED_DATA","search":{"query":"policy","top_k":5,"knowledge_base_ids":["kb_policy1"]}}`)
	raw, _ := NewReAct("question", "", 3)
	raw, _ = ApplyReActDecision(raw, search, "")
	raw, _ = ApplyObservation(raw, []action.ArtifactID{"art_chunk001"})
	if _, err := ApplyReActDecision(raw, search, ""); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate search=%v", err)
	}
	final := decodeAction(t, `{"schema_version":1,"type":"FINAL","reason_code":"DONE","final":{"answer":"answer","citation_ids":["art_foreign1"]}}`)
	if _, err := ApplyReActDecision(raw, final, "art_answer01"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("foreign citation=%v", err)
	}
	last, _ := NewReAct("question", "", 1)
	if _, err := ApplyReActDecision(last, search, ""); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("last iteration search=%v", err)
	}
}

func TestSearchFingerprintCanonicalizesFilterAndKnowledgeBaseOrder(t *testing.T) {
	left := SearchRequest{Query: "policy", TopK: 5, KnowledgeBaseIDs: []string{"kb_second", "kb_first1"},
		Filters: json.RawMessage(`{"region":"CN","active":true}`)}
	right := SearchRequest{Query: "policy", TopK: 5, KnowledgeBaseIDs: []string{"kb_first1", "kb_second"},
		Filters: json.RawMessage(`{"active":true,"region":"CN"}`)}
	leftHash, leftErr := searchFingerprint(left)
	rightHash, rightErr := searchFingerprint(right)
	if leftErr != nil || rightErr != nil || leftHash != rightHash {
		t.Fatalf("fingerprints %q/%q errors %v/%v", leftHash, rightHash, leftErr, rightErr)
	}
}

func TestStateStrictValidationAndDefensiveCopies(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"schema_version":1,"mode":"REACT","react":{"stage":"DECIDE","original_question":"q","iteration":0,"max_iterations":2,"observations":[],"citation_ids":[]},"extra":true}`),
		json.RawMessage(`{"schema_version":1,"mode":"REACT","react":{"stage":"ACT","original_question":"q","iteration":1,"max_iterations":2,"observations":[],"citation_ids":[]}}`),
	} {
		if _, err := Decode(raw); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("accepted %s: %v", raw, err)
		}
	}
	raw, _ := NewDeterministic("question", "")
	value, _ := Decode(raw)
	value.Deterministic.OriginalQuestion = "mutated"
	again, _ := Decode(raw)
	if again.Deterministic.OriginalQuestion != "question" {
		t.Fatal("Decode retained mutable state")
	}
}

func TestExecutorRunsDeterministicAndReActOperations(t *testing.T) {
	knowledge := &knowledgeFake{}
	executor, err := NewExecutor(rewriterFake{}, knowledge, generatorFake{}, deciderFake{})
	if err != nil {
		t.Fatal(err)
	}
	deterministic, _ := NewDeterministic("question", "")
	outcome := execute(t, executor, deterministic, domain.StepKindContext, OperationQueryRewrite, 1)
	state, _ := Decode(outcome.State)
	if state.Deterministic.Stage != StageRetrieve || state.Deterministic.RewrittenQuery != "rewritten" {
		t.Fatalf("rewrite state=%+v", state)
	}
	react, _ := NewReAct("question", "", 3)
	outcome = execute(t, executor, react, domain.StepKindGeneration, OperationReActDecide, 1)
	state, _ = Decode(outcome.State)
	if state.ReAct.Stage != StageAct || state.ReAct.PendingSearch == nil {
		t.Fatalf("decision state=%+v", state)
	}
	outcome = execute(t, executor, outcome.State, domain.StepKindTool, OperationKnowledge, 2)
	state, _ = Decode(outcome.State)
	if state.ReAct.Stage != StageDecide || len(state.ReAct.Observations) != 1 {
		t.Fatalf("observation state=%+v", state)
	}
	if len(knowledge.requests) != 1 || knowledge.requests[0].TenantID != "ten_0001" || knowledge.requests[0].RunID != "run_0001" || knowledge.requests[0].Query != "policy" {
		t.Fatalf("ReAct retrieval requests=%+v", knowledge.requests)
	}
	deterministic, _ = ApplyRewrite(deterministic, "rewritten")
	_ = execute(t, executor, deterministic, domain.StepKindRetrieval, OperationRetrieve, 2)
	if len(knowledge.requests) != 2 || knowledge.requests[1].TenantID != "ten_0001" || knowledge.requests[1].RunID != "run_0001" || knowledge.requests[1].Query != "rewritten" {
		t.Fatalf("shared retrieval requests=%+v", knowledge.requests)
	}
	bad := agent.Invocation{TenantID: "ten_0001", RunID: "run_0001", StepNo: 3, AttemptNo: 1, Directive: agent.Directive{Kind: domain.StepKindTool,
		OperationKey: OperationKnowledge + ".step.2", Input: json.RawMessage(`{"state":{}}`)}}
	if _, err := executor.Execute(context.Background(), bad); err == nil {
		t.Fatal("accepted mismatched operation identity")
	}
}

func execute(t *testing.T, executor *Executor, state json.RawMessage, kind domain.StepKind, operation string, step uint64) agent.Outcome {
	t.Helper()
	input, _ := json.Marshal(struct {
		State json.RawMessage `json:"state"`
	}{State: state})
	outcome, err := executor.Execute(context.Background(), agent.Invocation{TenantID: "ten_0001", RunID: "run_0001", StepNo: step, AttemptNo: 1,
		Directive: agent.Directive{Kind: kind, OperationKey: fmt.Sprintf("%s.step.%d", operation, step), Input: input}})
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

type rewriterFake struct{}

func (rewriterFake) Rewrite(context.Context, DeterministicState) (string, error) {
	return "rewritten", nil
}

type generatorFake struct{}

func (generatorFake) Generate(context.Context, DeterministicState) (action.ArtifactID, error) {
	return "art_answer01", nil
}

type deciderFake struct{}

func (deciderFake) Decide(context.Context, ReActState) (action.Action, action.ArtifactID, error) {
	value, err := action.Decode(json.RawMessage(`{"schema_version":1,"type":"SEARCH","reason_code":"NEED_DATA","search":{"query":"policy","top_k":5,"knowledge_base_ids":["kb_policy1"]}}`))
	return value, "", err
}

type knowledgeFake struct{ requests []knowledge.QueryRequest }

func (f *knowledgeFake) Retrieve(_ context.Context, request knowledge.QueryRequest) (knowledge.QueryResult, error) {
	request.KnowledgeBaseIDs = append([]string(nil), request.KnowledgeBaseIDs...)
	request.Filters = append(json.RawMessage(nil), request.Filters...)
	f.requests = append(f.requests, request)
	return knowledge.QueryResult{Evidence: []knowledge.QueryEvidence{{ArtifactID: "art_chunk001"}}}, nil
}

func decodeAction(t *testing.T, raw string) action.Action {
	t.Helper()
	value, err := action.Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
