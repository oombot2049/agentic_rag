package modes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/agent"
	"github.com/agenticrag/agenticrag/internal/domain"
)

const (
	OperationQueryRewrite = "rag.query_rewrite"
	OperationRetrieve     = "rag.retrieve"
	OperationGenerate     = "rag.generate"
	OperationReActDecide  = "react.decide"
	OperationKnowledge    = "knowledge.search"
)

type Planner struct{}

func NewPlanner() *Planner { return &Planner{} }

func (*Planner) Next(ctx context.Context, raw json.RawMessage, nextStepNo uint64) (agent.Plan, error) {
	if ctx == nil || ctx.Err() != nil || nextStepNo < 1 {
		return agent.Plan{}, agent.ErrInvalidArgument
	}
	state, err := Decode(raw)
	if err != nil {
		return agent.Plan{}, err
	}
	switch state.Mode {
	case ModeDeterministicRAG:
		return deterministicPlan(raw, *state.Deterministic, nextStepNo)
	case ModeReAct:
		return reactPlan(raw, *state.ReAct, nextStepNo)
	default:
		return agent.Plan{}, ErrInvalidState
	}
}

func deterministicPlan(raw json.RawMessage, state DeterministicState, step uint64) (agent.Plan, error) {
	switch state.Stage {
	case StageRewrite:
		return directivePlan(raw, domain.StepKindContext, OperationQueryRewrite, step)
	case StageRetrieve:
		return directivePlan(raw, domain.StepKindRetrieval, OperationRetrieve, step)
	case StageGenerate:
		return directivePlan(raw, domain.StepKindGeneration, OperationGenerate, step)
	case StageDone:
		return agent.Plan{Done: true, State: cloneRaw(raw)}, nil
	default:
		return agent.Plan{}, ErrInvalidState
	}
}

func reactPlan(raw json.RawMessage, state ReActState, step uint64) (agent.Plan, error) {
	switch state.Stage {
	case StageDecide:
		return directivePlan(raw, domain.StepKindGeneration, OperationReActDecide, step)
	case StageAct:
		return directivePlan(raw, domain.StepKindTool, OperationKnowledge, step)
	case StageFinish:
		return agent.Plan{Done: true, State: cloneRaw(raw)}, nil
	default:
		return agent.Plan{}, ErrInvalidState
	}
}

func directivePlan(state json.RawMessage, kind domain.StepKind, operation string, step uint64) (agent.Plan, error) {
	input, err := json.Marshal(struct {
		State json.RawMessage `json:"state"`
	}{State: state})
	if err != nil {
		return agent.Plan{}, fmt.Errorf("%w: directive input", ErrInvalidState)
	}
	return agent.Plan{State: cloneRaw(state), Directive: agent.Directive{Kind: kind,
		OperationKey: fmt.Sprintf("%s.step.%d", operation, step), Input: input, RetryUnknown: true}}, nil
}

func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

var _ agent.Planner = (*Planner)(nil)
