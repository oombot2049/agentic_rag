package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/repository"
	"github.com/agenticrag/agenticrag/internal/runlease"
)

type Plan struct {
	Done      bool
	Directive Directive
	State     json.RawMessage
}

type Planner interface {
	Next(context.Context, json.RawMessage, uint64) (Plan, error)
}

type Invocation struct {
	TenantID  domain.TenantID
	RunID     domain.RunID
	StepID    domain.StepID
	StepNo    uint64
	AttemptID domain.AttemptID
	AttemptNo uint64
	Directive Directive
}

type Outcome struct{ State json.RawMessage }

type Executor interface {
	Execute(context.Context, Invocation) (Outcome, error)
}

type ExecutionError struct {
	Class     string
	Retryable bool
	Unknown   bool
	Cause     error
}

func (e *ExecutionError) Error() string {
	if e == nil || e.Cause == nil {
		return "agent execution failed"
	}
	return e.Cause.Error()
}
func (e *ExecutionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type Store interface {
	BeginStep(context.Context, repository.WorkerAuthority, BeginStepCommand) error
	RetryAttempt(context.Context, repository.WorkerAuthority, RetryAttemptCommand) error
	CompleteStep(context.Context, repository.WorkerAuthority, CompleteStepCommand) error
	FailStep(context.Context, repository.WorkerAuthority, FailStepCommand) error
	CompleteLoop(context.Context, repository.WorkerAuthority, CompleteLoopCommand) error
}

type IDs interface {
	NewStepID() (domain.StepID, error)
	NewAttemptID() (domain.AttemptID, error)
}

type Clock interface{ Now() time.Time }

type Config struct {
	MaxAttempts int
	Clock       Clock
}

func DefaultConfig() Config { return Config{MaxAttempts: 3} }

type Machine struct {
	store    Store
	planner  Planner
	executor Executor
	ids      IDs
	clock    Clock
	config   Config
}

func New(store Store, planner Planner, executor Executor, ids IDs, configuration Config) (*Machine, error) {
	if store == nil || planner == nil || executor == nil || ids == nil || configuration.MaxAttempts < 1 || configuration.MaxAttempts > 5 {
		return nil, fmt.Errorf("%w: dependencies or configuration", ErrInvalidArgument)
	}
	clock := configuration.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Machine{store: store, planner: planner, executor: executor, ids: ids, clock: clock, config: configuration}, nil
}

type Result struct {
	Steps uint64
	State json.RawMessage
}

func (m *Machine) Run(ctx context.Context, acquisition runlease.Acquisition) (Result, error) {
	if ctx == nil || acquisition.Handle == nil || acquisition.Run.Run == nil {
		return Result{}, fmt.Errorf("%w: context or acquisition", ErrInvalidArgument)
	}
	authority := acquisition.Handle.Snapshot()
	if authority.TenantID != acquisition.Run.Run.TenantID() || authority.RunID != acquisition.Run.Run.ID() {
		return Result{}, fmt.Errorf("%w: acquisition authority mismatch", ErrInvalidArgument)
	}
	if acquisition.Checkpoint != nil && (acquisition.Checkpoint.TenantID != authority.TenantID || acquisition.Checkpoint.RunID != authority.RunID) {
		return Result{}, fmt.Errorf("%w: checkpoint ownership", ErrInvalidCheckpoint)
	}
	maxSteps := acquisition.Run.MaxSteps
	var payload json.RawMessage
	if acquisition.Checkpoint != nil {
		payload = acquisition.Checkpoint.Payload
	}
	checkpoint, err := DecodeCheckpoint(payload, maxSteps)
	if err != nil {
		return Result{}, err
	}
	executionCtx, cancel := context.WithCancelCause(acquisition.Handle.Context())
	stop := context.AfterFunc(ctx, func() { cancel(ctx.Err()) })
	defer func() {
		stop()
		cancel(nil)
	}()

	if checkpoint.Phase == PhaseCompleted {
		return completedResult(checkpoint), nil
	}
	if checkpoint.Phase == PhaseFailed {
		return Result{}, fmt.Errorf("%w: %s", ErrStepFailed, checkpoint.FailureClass)
	}
	if checkpoint.Phase == PhaseExecuting {
		checkpoint, err = m.recoverAttempt(executionCtx, acquisition.Handle, checkpoint, maxSteps)
		if err != nil {
			return Result{}, err
		}
		if checkpoint.Phase == PhaseCompleted {
			return completedResult(checkpoint), nil
		}
	}

	for {
		if err := executionCtx.Err(); err != nil {
			return Result{}, context.Cause(executionCtx)
		}
		if checkpoint.Phase == PhaseReady {
			checkpoint, err = m.plan(executionCtx, acquisition.Handle, acquisition.Run.Run.TenantID(), acquisition.Run.Run.ID(), checkpoint, maxSteps)
			if err != nil {
				return Result{}, err
			}
			if checkpoint.Phase == PhaseCompleted {
				return completedResult(checkpoint), nil
			}
		}

		outcome, callErr := m.executor.Execute(executionCtx, invocation(*checkpoint.Active,
			acquisition.Run.Run.TenantID(), acquisition.Run.Run.ID()))
		if callErr == nil {
			checkpoint, err = m.complete(executionCtx, acquisition.Handle, checkpoint, outcome, maxSteps)
		} else {
			checkpoint, err = m.handleExecutionError(executionCtx, acquisition.Handle, checkpoint, callErr, maxSteps)
		}
		if err != nil {
			return Result{}, err
		}
	}
}

func (m *Machine) plan(ctx context.Context, handle *runlease.Handle, tenantID domain.TenantID, runID domain.RunID, current Checkpoint, maxSteps int) (Checkpoint, error) {
	plan, err := m.planner.Next(ctx, cloneJSON(current.State), current.NextStepNo)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("plan next Step: %w", err)
	}
	state := current.State
	if len(plan.State) > 0 {
		state = plan.State
	}
	if !validJSON(state) {
		return Checkpoint{}, fmt.Errorf("%w: planner state", ErrInvalidArgument)
	}
	if plan.Done {
		completed := Checkpoint{SchemaVersion: checkpointSchemaVersion, Phase: PhaseCompleted, NextStepNo: current.NextStepNo, State: cloneJSON(state)}
		payload, err := completed.Encode(maxSteps)
		if err != nil {
			return Checkpoint{}, err
		}
		err = durable(ctx, handle, func(ctx context.Context, authority repository.WorkerAuthority) error {
			return m.store.CompleteLoop(ctx, authority, CompleteLoopCommand{StepNo: current.NextStepNo - 1, Payload: payload})
		})
		return completed, err
	}
	if current.NextStepNo > uint64(maxSteps) {
		return Checkpoint{}, ErrStepLimit
	}
	if err := validateDirective(plan.Directive); err != nil {
		return Checkpoint{}, err
	}
	stepID, attemptID, err := m.newIDs()
	if err != nil {
		return Checkpoint{}, err
	}
	now := m.clock.Now().UTC()
	step, err := domain.NewStep(stepID, tenantID, runID, current.NextStepNo, plan.Directive.Kind, now)
	if err != nil {
		return Checkpoint{}, err
	}
	attempt, err := domain.NewAttempt(attemptID, tenantID, stepID, 1, now)
	if err != nil {
		return Checkpoint{}, err
	}
	active := &ActiveStep{StepID: stepID, StepNo: current.NextStepNo, StepRowVersion: 2,
		AttemptID: attemptID, AttemptNo: 1, AttemptRowVersion: 1, Directive: cloneDirective(plan.Directive)}
	executing := Checkpoint{SchemaVersion: checkpointSchemaVersion, Phase: PhaseExecuting, NextStepNo: current.NextStepNo,
		State: cloneJSON(state), Active: active}
	payload, _ := executing.Encode(maxSteps)
	err = durable(ctx, handle, func(ctx context.Context, authority repository.WorkerAuthority) error {
		return m.store.BeginStep(ctx, authority, BeginStepCommand{Step: step, Attempt: attempt, Payload: payload})
	})
	return executing, err
}

func (m *Machine) recoverAttempt(ctx context.Context, handle *runlease.Handle, current Checkpoint, maxSteps int) (Checkpoint, error) {
	if !current.Active.Directive.RetryUnknown {
		_, err := m.fail(ctx, handle, current, "unknown_outcome", domain.AttemptStatusUnknown, maxSteps)
		if err != nil {
			return Checkpoint{}, errors.Join(ErrUnknownOutcome, err)
		}
		return Checkpoint{}, ErrUnknownOutcome
	}
	if current.Active.AttemptNo >= uint64(m.config.MaxAttempts) {
		_, err := m.fail(ctx, handle, current, "unknown_outcome_attempt_limit", domain.AttemptStatusUnknown, maxSteps)
		return Checkpoint{}, errors.Join(ErrAttemptLimit, ErrUnknownOutcome, err)
	}
	return m.retry(ctx, handle, current, domain.AttemptStatusUnknown, maxSteps)
}

func (m *Machine) complete(ctx context.Context, handle *runlease.Handle, current Checkpoint, outcome Outcome, maxSteps int) (Checkpoint, error) {
	state := current.State
	if len(outcome.State) > 0 {
		state = outcome.State
	}
	if !validJSON(state) {
		return Checkpoint{}, fmt.Errorf("%w: executor state", ErrInvalidArgument)
	}
	next := Checkpoint{SchemaVersion: checkpointSchemaVersion, Phase: PhaseReady, NextStepNo: current.NextStepNo + 1, State: cloneJSON(state)}
	payload, err := next.Encode(maxSteps)
	if err != nil {
		return Checkpoint{}, err
	}
	active := *current.Active
	err = durable(ctx, handle, func(ctx context.Context, authority repository.WorkerAuthority) error {
		return m.store.CompleteStep(ctx, authority, CompleteStepCommand{Active: active, At: m.clock.Now().UTC(), Payload: payload})
	})
	return next, err
}

func (m *Machine) handleExecutionError(ctx context.Context, handle *runlease.Handle, current Checkpoint, callErr error, maxSteps int) (Checkpoint, error) {
	if ctx.Err() != nil {
		return Checkpoint{}, context.Cause(ctx)
	}
	failure := classifyExecutionError(callErr)
	status := domain.AttemptStatusFailed
	if failure.Unknown {
		status = domain.AttemptStatusUnknown
		if !current.Active.Directive.RetryUnknown {
			_, persistErr := m.fail(ctx, handle, current, failure.Class, status, maxSteps)
			return Checkpoint{}, errors.Join(ErrUnknownOutcome, callErr, persistErr)
		}
	}
	if failure.Retryable && current.Active.AttemptNo < uint64(m.config.MaxAttempts) {
		return m.retry(ctx, handle, current, status, maxSteps)
	}
	_, persistErr := m.fail(ctx, handle, current, failure.Class, status, maxSteps)
	if current.Active.AttemptNo >= uint64(m.config.MaxAttempts) && failure.Retryable {
		return Checkpoint{}, errors.Join(ErrAttemptLimit, callErr, persistErr)
	}
	return Checkpoint{}, errors.Join(ErrStepFailed, callErr, persistErr)
}

func (m *Machine) retry(ctx context.Context, handle *runlease.Handle, current Checkpoint, status domain.AttemptStatus, maxSteps int) (Checkpoint, error) {
	if current.Active.AttemptNo >= uint64(m.config.MaxAttempts) {
		return Checkpoint{}, ErrAttemptLimit
	}
	attemptID, err := m.ids.NewAttemptID()
	if err != nil {
		return Checkpoint{}, fmt.Errorf("generate Attempt ID: %w", err)
	}
	now := m.clock.Now().UTC()
	attempt, err := domain.NewAttempt(attemptID, currentTenant(handle), current.Active.StepID, current.Active.AttemptNo+1, now)
	if err != nil {
		return Checkpoint{}, err
	}
	next := current
	copyActive := *current.Active
	copyActive.AttemptID, copyActive.AttemptNo, copyActive.AttemptRowVersion = attemptID, attempt.AttemptNo(), 1
	next.Active = &copyActive
	payload, _ := next.Encode(maxSteps)
	err = durable(ctx, handle, func(ctx context.Context, authority repository.WorkerAuthority) error {
		return m.store.RetryAttempt(ctx, authority, RetryAttemptCommand{Previous: *current.Active, PreviousStatus: status,
			Attempt: attempt, At: now, Payload: payload})
	})
	return next, err
}

func (m *Machine) fail(ctx context.Context, handle *runlease.Handle, current Checkpoint, class string, status domain.AttemptStatus, maxSteps int) (Checkpoint, error) {
	if !validFailureClass(class) {
		class = "external_failure"
	}
	failed := Checkpoint{SchemaVersion: checkpointSchemaVersion, Phase: PhaseFailed, NextStepNo: current.NextStepNo,
		State: cloneJSON(current.State), FailureClass: class}
	payload, _ := failed.Encode(maxSteps)
	active := *current.Active
	err := durable(ctx, handle, func(ctx context.Context, authority repository.WorkerAuthority) error {
		return m.store.FailStep(ctx, authority, FailStepCommand{Active: active, AttemptStatus: status,
			At: m.clock.Now().UTC(), Payload: payload})
	})
	return failed, err
}

func durable(ctx context.Context, handle *runlease.Handle, operation func(context.Context, repository.WorkerAuthority) error) error {
	return handle.WithAuthority(ctx, func(ctx context.Context, authority repository.WorkerAuthority) (uint64, error) {
		if err := operation(ctx, authority); err != nil {
			return authority.ExpectedExecutionVersion, err
		}
		return authority.ExpectedExecutionVersion, nil
	})
}

func (m *Machine) newIDs() (domain.StepID, domain.AttemptID, error) {
	stepID, err := m.ids.NewStepID()
	if err != nil {
		return "", "", fmt.Errorf("generate Step ID: %w", err)
	}
	if _, err := domain.ParseStepID(stepID.String()); err != nil {
		return "", "", fmt.Errorf("%w: generated Step ID", ErrInvalidArgument)
	}
	attemptID, err := m.ids.NewAttemptID()
	if err != nil {
		return "", "", fmt.Errorf("generate Attempt ID: %w", err)
	}
	if _, err := domain.ParseAttemptID(attemptID.String()); err != nil {
		return "", "", fmt.Errorf("%w: generated Attempt ID", ErrInvalidArgument)
	}
	return stepID, attemptID, nil
}

func classifyExecutionError(err error) *ExecutionError {
	var classified *ExecutionError
	if errors.As(err, &classified) {
		result := *classified
		if !validFailureClass(result.Class) {
			result.Class = "external_failure"
		}
		return &result
	}
	return &ExecutionError{Class: "external_failure", Cause: err}
}

func invocation(active ActiveStep, tenantID domain.TenantID, runID domain.RunID) Invocation {
	return Invocation{TenantID: tenantID, RunID: runID, StepID: active.StepID, StepNo: active.StepNo, AttemptID: active.AttemptID,
		AttemptNo: active.AttemptNo, Directive: cloneDirective(active.Directive)}
}

func cloneDirective(value Directive) Directive {
	value.Input = cloneJSON(value.Input)
	return value
}

func currentTenant(handle *runlease.Handle) domain.TenantID { return handle.Snapshot().TenantID }
func completedResult(value Checkpoint) Result {
	return Result{Steps: value.NextStepNo - 1, State: cloneJSON(value.State)}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
