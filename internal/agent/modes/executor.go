package modes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/agenticrag/agenticrag/internal/agent"
	"github.com/agenticrag/agenticrag/internal/agent/action"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

type QueryRewriter interface {
	Rewrite(context.Context, DeterministicState) (string, error)
}

type AnswerGenerator interface {
	Generate(context.Context, DeterministicState) (action.ArtifactID, error)
}

type ReActDecider interface {
	Decide(context.Context, ReActState) (action.Action, action.ArtifactID, error)
}

type Executor struct {
	rewriter  QueryRewriter
	knowledge knowledge.QueryService
	generator AnswerGenerator
	decider   ReActDecider
}

func NewExecutor(rewriter QueryRewriter, knowledgeService knowledge.QueryService, generator AnswerGenerator,
	decider ReActDecider) (*Executor, error) {
	if rewriter == nil || knowledgeService == nil || generator == nil || decider == nil {
		return nil, fmt.Errorf("%w: mode Executor dependencies", agent.ErrInvalidArgument)
	}
	return &Executor{rewriter: rewriter, knowledge: knowledgeService, generator: generator, decider: decider}, nil
}

func (e *Executor) Execute(ctx context.Context, invocation agent.Invocation) (agent.Outcome, error) {
	if e == nil || ctx == nil || ctx.Err() != nil || invocation.StepNo < 1 || invocation.AttemptNo < 1 {
		return agent.Outcome{}, agent.ErrInvalidArgument
	}
	if _, err := domain.ParseTenantID(invocation.TenantID.String()); err != nil {
		return agent.Outcome{}, agent.ErrInvalidArgument
	}
	if _, err := domain.ParseRunID(invocation.RunID.String()); err != nil {
		return agent.Outcome{}, agent.ErrInvalidArgument
	}
	raw, err := decodeDirectiveInput(invocation.Directive.Input)
	if err != nil {
		return agent.Outcome{}, err
	}
	state, err := Decode(raw)
	if err != nil {
		return agent.Outcome{}, err
	}
	operation, err := operationFor(invocation)
	if err != nil {
		return agent.Outcome{}, err
	}
	var next json.RawMessage
	switch operation {
	case OperationQueryRewrite:
		if state.Mode != ModeDeterministicRAG || state.Deterministic.Stage != StageRewrite {
			return agent.Outcome{}, ErrInvalidState
		}
		rewritten, callErr := e.rewriter.Rewrite(ctx, *state.Deterministic)
		if callErr != nil {
			return agent.Outcome{}, callErr
		}
		next, err = ApplyRewrite(raw, rewritten)
	case OperationRetrieve:
		if state.Mode != ModeDeterministicRAG || state.Deterministic.Stage != StageRetrieve {
			return agent.Outcome{}, ErrInvalidState
		}
		result, callErr := e.knowledge.Retrieve(ctx, knowledge.QueryRequest{TenantID: invocation.TenantID,
			RunID: invocation.RunID, Query: state.Deterministic.RewrittenQuery, TopK: 5})
		if callErr != nil {
			return agent.Outcome{}, callErr
		}
		next, err = ApplyEvidence(raw, evidenceIDs(result))
	case OperationGenerate:
		if state.Mode != ModeDeterministicRAG || state.Deterministic.Stage != StageGenerate {
			return agent.Outcome{}, ErrInvalidState
		}
		artifact, callErr := e.generator.Generate(ctx, *state.Deterministic)
		if callErr != nil {
			return agent.Outcome{}, callErr
		}
		next, err = ApplyAnswer(raw, artifact)
	case OperationReActDecide:
		if state.Mode != ModeReAct || state.ReAct.Stage != StageDecide {
			return agent.Outcome{}, ErrInvalidState
		}
		decision, artifact, callErr := e.decider.Decide(ctx, *state.ReAct)
		if callErr != nil {
			return agent.Outcome{}, callErr
		}
		next, err = ApplyReActDecision(raw, decision, artifact)
	case OperationKnowledge:
		if state.Mode != ModeReAct || state.ReAct.Stage != StageAct || state.ReAct.PendingSearch == nil {
			return agent.Outcome{}, ErrInvalidState
		}
		request := state.ReAct.PendingSearch
		result, callErr := e.knowledge.Retrieve(ctx, knowledge.QueryRequest{TenantID: invocation.TenantID,
			RunID: invocation.RunID, Query: request.Query, TopK: request.TopK, KnowledgeBaseIDs: append([]string(nil), request.KnowledgeBaseIDs...),
			Filters: append(json.RawMessage(nil), request.Filters...)})
		if callErr != nil {
			return agent.Outcome{}, callErr
		}
		next, err = ApplyObservation(raw, evidenceIDs(result))
	default:
		return agent.Outcome{}, agent.ErrInvalidArgument
	}
	if err != nil {
		return agent.Outcome{}, err
	}
	return agent.Outcome{State: next}, nil
}

func operationFor(invocation agent.Invocation) (string, error) {
	operations := []struct {
		name string
		kind domain.StepKind
	}{
		{OperationQueryRewrite, domain.StepKindContext},
		{OperationRetrieve, domain.StepKindRetrieval},
		{OperationGenerate, domain.StepKindGeneration},
		{OperationReActDecide, domain.StepKindGeneration},
		{OperationKnowledge, domain.StepKindTool},
	}
	for _, operation := range operations {
		if invocation.Directive.OperationKey == fmt.Sprintf("%s.step.%d", operation.name, invocation.StepNo) &&
			invocation.Directive.Kind == operation.kind {
			return operation.name, nil
		}
	}
	return "", agent.ErrInvalidArgument
}

func decodeDirectiveInput(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input struct {
		State json.RawMessage `json:"state"`
	}
	if decoder.Decode(&input) != nil || len(input.State) == 0 {
		return nil, agent.ErrInvalidArgument
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, agent.ErrInvalidArgument
	}
	return append(json.RawMessage(nil), input.State...), nil
}

var _ agent.Executor = (*Executor)(nil)

func evidenceIDs(result knowledge.QueryResult) []action.ArtifactID {
	values := make([]action.ArtifactID, len(result.Evidence))
	for index, item := range result.Evidence {
		values[index] = action.ArtifactID(item.ArtifactID)
	}
	return values
}
