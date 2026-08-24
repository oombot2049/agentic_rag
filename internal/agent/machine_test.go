package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/repository"
	"github.com/agenticrag/agenticrag/internal/runlease"
)

var agentTestTime = time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)

func TestMachineCompletesTwoStepsWithoutTransactionAroundExecutor(t *testing.T) {
	acquisition, cleanup := testAcquisition(t, 4, nil)
	defer cleanup()
	store := &agentStoreFake{}
	planner := plannerFunc(func(_ context.Context, state json.RawMessage, stepNo uint64) (Plan, error) {
		if stepNo == 3 {
			return Plan{Done: true, State: state}, nil
		}
		return Plan{Directive: testDirective(stepNo), State: state}, nil
	})
	executorCalls := 0
	executor := executorFunc(func(_ context.Context, invocation Invocation) (Outcome, error) {
		executorCalls++
		if store.inTransaction {
			t.Fatal("executor called while Store transaction is active")
		}
		if invocation.StepNo != uint64(executorCalls) || invocation.AttemptNo != 1 {
			t.Fatalf("invocation=%+v", invocation)
		}
		return Outcome{State: json.RawMessage(fmt.Sprintf(`{"completed":%d}`, invocation.StepNo))}, nil
	})
	machine := testMachine(t, store, planner, executor, 3)

	result, err := machine.Run(context.Background(), acquisition)
	if err != nil {
		t.Fatal(err)
	}
	if result.Steps != 2 || string(result.State) != `{"completed":2}` || executorCalls != 2 {
		t.Fatalf("result=%+v calls=%d", result, executorCalls)
	}
	if got := store.operations; fmt.Sprint(got) != "[begin complete begin complete loop]" {
		t.Fatalf("operations=%v", got)
	}
	checkpoint, err := DecodeCheckpoint(store.lastPayload, 4)
	if err != nil || checkpoint.Phase != PhaseCompleted {
		t.Fatalf("checkpoint=%+v error=%v", checkpoint, err)
	}
}

func TestMachineRetriesSameLogicalStepAndOperationKey(t *testing.T) {
	acquisition, cleanup := testAcquisition(t, 3, nil)
	defer cleanup()
	store := &agentStoreFake{}
	plannerCalls := 0
	planner := plannerFunc(func(_ context.Context, state json.RawMessage, stepNo uint64) (Plan, error) {
		plannerCalls++
		if stepNo == 2 {
			return Plan{Done: true, State: state}, nil
		}
		return Plan{Directive: testDirective(stepNo)}, nil
	})
	var invocations []Invocation
	executor := executorFunc(func(_ context.Context, invocation Invocation) (Outcome, error) {
		invocations = append(invocations, invocation)
		if len(invocations) == 1 {
			return Outcome{}, &ExecutionError{Class: "provider_overload", Retryable: true, Cause: errors.New("busy")}
		}
		return Outcome{State: json.RawMessage(`{"answer":"ok"}`)}, nil
	})
	machine := testMachine(t, store, planner, executor, 3)

	if _, err := machine.Run(context.Background(), acquisition); err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 2 || invocations[0].StepID != invocations[1].StepID ||
		invocations[0].AttemptID == invocations[1].AttemptID || invocations[1].AttemptNo != 2 ||
		invocations[0].Directive.OperationKey != invocations[1].Directive.OperationKey {
		t.Fatalf("invocations=%+v", invocations)
	}
	if plannerCalls != 2 || fmt.Sprint(store.operations) != "[begin retry complete loop]" || store.retryStatuses[0] != domain.AttemptStatusFailed {
		t.Fatalf("planner=%d operations=%v statuses=%v", plannerCalls, store.operations, store.retryStatuses)
	}
}

func TestMachineTakeoverMarksAbandonedAttemptUnknownBeforeRetry(t *testing.T) {
	checkpoint := executingCheckpoint(t, true, 1)
	payload, _ := checkpoint.Encode(3)
	acquisition, cleanup := testAcquisition(t, 3, payload)
	defer cleanup()
	store := &agentStoreFake{}
	planner := plannerFunc(func(_ context.Context, state json.RawMessage, stepNo uint64) (Plan, error) {
		return Plan{Done: true, State: state}, nil
	})
	var invocation Invocation
	executor := executorFunc(func(_ context.Context, value Invocation) (Outcome, error) {
		invocation = value
		return Outcome{State: json.RawMessage(`{"recovered":true}`)}, nil
	})
	machine := testMachine(t, store, planner, executor, 3)

	if _, err := machine.Run(context.Background(), acquisition); err != nil {
		t.Fatal(err)
	}
	if invocation.AttemptNo != 2 || invocation.StepID != "stp_existing" || invocation.Directive.OperationKey != "op-stable" {
		t.Fatalf("invocation=%+v", invocation)
	}
	if fmt.Sprint(store.operations) != "[retry complete loop]" || store.retryStatuses[0] != domain.AttemptStatusUnknown {
		t.Fatalf("operations=%v statuses=%v", store.operations, store.retryStatuses)
	}
}

func TestMachineDoesNotRetryUnsafeUnknownOutcome(t *testing.T) {
	checkpoint := executingCheckpoint(t, false, 1)
	payload, _ := checkpoint.Encode(3)
	acquisition, cleanup := testAcquisition(t, 3, payload)
	defer cleanup()
	store := &agentStoreFake{}
	executions := 0
	machine := testMachine(t, store, plannerFunc(func(context.Context, json.RawMessage, uint64) (Plan, error) {
		return Plan{}, errors.New("must not plan")
	}), executorFunc(func(context.Context, Invocation) (Outcome, error) {
		executions++
		return Outcome{}, nil
	}), 3)

	_, err := machine.Run(context.Background(), acquisition)
	if !errors.Is(err, ErrUnknownOutcome) || executions != 0 || fmt.Sprint(store.operations) != "[fail]" ||
		store.failStatuses[0] != domain.AttemptStatusUnknown {
		t.Fatalf("error=%v executions=%d operations=%v", err, executions, store.operations)
	}
}

func TestMachineFailureInjectionHonorsDurableBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		failureAt string
		wantCalls int
		wantOps   string
	}{
		{name: "begin rollback", failureAt: "begin", wantCalls: 0, wantOps: "[begin]"},
		{name: "complete rollback", failureAt: "complete", wantCalls: 1, wantOps: "[begin complete]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			acquisition, cleanup := testAcquisition(t, 3, nil)
			defer cleanup()
			injected := errors.New("injected transaction rollback")
			store := &agentStoreFake{failAt: test.failureAt, failure: injected}
			calls := 0
			planner := plannerFunc(func(_ context.Context, _ json.RawMessage, stepNo uint64) (Plan, error) {
				return Plan{Directive: testDirective(stepNo)}, nil
			})
			executor := executorFunc(func(context.Context, Invocation) (Outcome, error) {
				calls++
				return Outcome{}, nil
			})
			machine := testMachine(t, store, planner, executor, 3)
			_, err := machine.Run(context.Background(), acquisition)
			if !errors.Is(err, injected) || calls != test.wantCalls || fmt.Sprint(store.operations) != test.wantOps {
				t.Fatalf("error=%v calls=%d operations=%v", err, calls, store.operations)
			}
		})
	}
}

func TestMachineFailureInjectionCoversRetryFailAndLoopBoundaries(t *testing.T) {
	injected := errors.New("injected transaction rollback")
	tests := []struct {
		name      string
		failureAt string
		planner   Planner
		executor  Executor
		wantOps   string
	}{
		{
			name: "retry rollback", failureAt: "retry", wantOps: "[begin retry]",
			planner: plannerFunc(func(_ context.Context, _ json.RawMessage, stepNo uint64) (Plan, error) {
				return Plan{Directive: testDirective(stepNo)}, nil
			}),
			executor: executorFunc(func(context.Context, Invocation) (Outcome, error) {
				return Outcome{}, &ExecutionError{Class: "overload", Retryable: true, Cause: errors.New("busy")}
			}),
		},
		{
			name: "fail rollback", failureAt: "fail", wantOps: "[begin fail]",
			planner: plannerFunc(func(_ context.Context, _ json.RawMessage, stepNo uint64) (Plan, error) {
				return Plan{Directive: testDirective(stepNo)}, nil
			}),
			executor: executorFunc(func(context.Context, Invocation) (Outcome, error) {
				return Outcome{}, errors.New("permanent")
			}),
		},
		{
			name: "loop rollback", failureAt: "loop", wantOps: "[loop]",
			planner: plannerFunc(func(_ context.Context, state json.RawMessage, _ uint64) (Plan, error) {
				return Plan{Done: true, State: state}, nil
			}),
			executor: executorFunc(func(context.Context, Invocation) (Outcome, error) {
				t.Fatal("executor must not be called for completed plan")
				return Outcome{}, nil
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			acquisition, cleanup := testAcquisition(t, 3, nil)
			defer cleanup()
			store := &agentStoreFake{failAt: test.failureAt, failure: injected}
			machine := testMachine(t, store, test.planner, test.executor, 3)
			_, err := machine.Run(context.Background(), acquisition)
			if !errors.Is(err, injected) || fmt.Sprint(store.operations) != test.wantOps {
				t.Fatalf("error=%v operations=%v", err, store.operations)
			}
		})
	}
}

func TestMachineStopsOnFencedCASAndDoesNotCallExecutor(t *testing.T) {
	acquisition, cleanup := testAcquisition(t, 3, nil)
	defer cleanup()
	store := &agentStoreFake{failAt: "begin", failure: repository.ErrRowConflict}
	calls := 0
	machine := testMachine(t, store, plannerFunc(func(_ context.Context, _ json.RawMessage, stepNo uint64) (Plan, error) {
		return Plan{Directive: testDirective(stepNo)}, nil
	}), executorFunc(func(context.Context, Invocation) (Outcome, error) {
		calls++
		return Outcome{}, nil
	}), 3)

	_, err := machine.Run(context.Background(), acquisition)
	if !errors.Is(err, runlease.ErrLeaseLost) || calls != 0 || acquisition.Handle.Snapshot().State != runlease.StateLost {
		t.Fatalf("error=%v calls=%d lease=%+v", err, calls, acquisition.Handle.Snapshot())
	}
}

func TestMachineAttemptLimitAndMalformedCheckpoint(t *testing.T) {
	t.Run("attempt exhaustion", func(t *testing.T) {
		acquisition, cleanup := testAcquisition(t, 3, nil)
		defer cleanup()
		store := &agentStoreFake{}
		machine := testMachine(t, store, plannerFunc(func(_ context.Context, _ json.RawMessage, stepNo uint64) (Plan, error) {
			return Plan{Directive: testDirective(stepNo)}, nil
		}), executorFunc(func(context.Context, Invocation) (Outcome, error) {
			return Outcome{}, &ExecutionError{Class: "timeout", Retryable: true, Cause: errors.New("timeout")}
		}), 2)
		_, err := machine.Run(context.Background(), acquisition)
		if !errors.Is(err, ErrAttemptLimit) || fmt.Sprint(store.operations) != "[begin retry fail]" {
			t.Fatalf("error=%v operations=%v", err, store.operations)
		}
	})

	t.Run("unknown checkpoint field", func(t *testing.T) {
		acquisition, cleanup := testAcquisition(t, 3, json.RawMessage(`{"schema_version":1,"phase":"READY","next_step_no":1,"state":{},"surprise":true}`))
		defer cleanup()
		machine := testMachine(t, &agentStoreFake{}, plannerFunc(func(context.Context, json.RawMessage, uint64) (Plan, error) {
			return Plan{}, nil
		}), executorFunc(func(context.Context, Invocation) (Outcome, error) { return Outcome{}, nil }), 2)
		if _, err := machine.Run(context.Background(), acquisition); !errors.Is(err, ErrInvalidCheckpoint) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("foreign checkpoint ownership", func(t *testing.T) {
		checkpoint := initialCheckpoint()
		payload, _ := checkpoint.Encode(3)
		acquisition, cleanup := testAcquisition(t, 3, payload)
		defer cleanup()
		acquisition.Checkpoint.TenantID = "ten_9999"
		machine := testMachine(t, &agentStoreFake{}, plannerFunc(func(context.Context, json.RawMessage, uint64) (Plan, error) {
			return Plan{}, nil
		}), executorFunc(func(context.Context, Invocation) (Outcome, error) { return Outcome{}, nil }), 2)
		if _, err := machine.Run(context.Background(), acquisition); !errors.Is(err, ErrInvalidCheckpoint) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestMachineEnforcesStepLimitWithoutStartingAnotherStep(t *testing.T) {
	acquisition, cleanup := testAcquisition(t, 1, nil)
	defer cleanup()
	store := &agentStoreFake{}
	plannerCalls := 0
	machine := testMachine(t, store, plannerFunc(func(_ context.Context, _ json.RawMessage, stepNo uint64) (Plan, error) {
		plannerCalls++
		return Plan{Directive: testDirective(stepNo)}, nil
	}), executorFunc(func(context.Context, Invocation) (Outcome, error) {
		return Outcome{State: json.RawMessage(`{"step":1}`)}, nil
	}), 2)

	_, err := machine.Run(context.Background(), acquisition)
	if !errors.Is(err, ErrStepLimit) || plannerCalls != 2 || fmt.Sprint(store.operations) != "[begin complete]" {
		t.Fatalf("error=%v planner=%d operations=%v", err, plannerCalls, store.operations)
	}
	checkpoint, decodeErr := DecodeCheckpoint(store.lastPayload, 1)
	if decodeErr != nil || checkpoint.Phase != PhaseReady || checkpoint.NextStepNo != 2 {
		t.Fatalf("checkpoint=%+v error=%v", checkpoint, decodeErr)
	}
}

func TestMachinePersistsRuntimeUnknownOutcomeWithoutRetry(t *testing.T) {
	acquisition, cleanup := testAcquisition(t, 2, nil)
	defer cleanup()
	store := &agentStoreFake{}
	machine := testMachine(t, store, plannerFunc(func(_ context.Context, _ json.RawMessage, stepNo uint64) (Plan, error) {
		directive := testDirective(stepNo)
		directive.RetryUnknown = false
		return Plan{Directive: directive}, nil
	}), executorFunc(func(context.Context, Invocation) (Outcome, error) {
		return Outcome{}, &ExecutionError{Class: "tool_timeout", Retryable: true, Unknown: true, Cause: errors.New("connection lost")}
	}), 3)

	_, err := machine.Run(context.Background(), acquisition)
	if !errors.Is(err, ErrUnknownOutcome) || fmt.Sprint(store.operations) != "[begin fail]" ||
		len(store.failStatuses) != 1 || store.failStatuses[0] != domain.AttemptStatusUnknown {
		t.Fatalf("error=%v operations=%v statuses=%v", err, store.operations, store.failStatuses)
	}
}

func TestCheckpointRejectsTrailingAndImpossibleShapes(t *testing.T) {
	values := []json.RawMessage{
		json.RawMessage(`{"schema_version":1,"phase":"READY","next_step_no":1,"state":{}} {}`),
		json.RawMessage(`{"schema_version":2,"phase":"READY","next_step_no":1,"state":{}}`),
		json.RawMessage(`{"schema_version":1,"phase":"EXECUTING","next_step_no":1,"state":{}}`),
		json.RawMessage(`{"schema_version":1,"phase":"FAILED","next_step_no":1,"state":{},"failure_class":""}`),
		json.RawMessage(`{"schema_version":1,"phase":"EXECUTING","next_step_no":1,"state":{},"active":{"step_id":"bad","step_no":1,"step_row_version":0,"attempt_id":"bad","attempt_no":1,"attempt_row_version":1,"directive":{"kind":"TOOL","operation_key":"op","input":{},"retry_unknown":true}}}`),
	}
	for _, value := range values {
		if _, err := DecodeCheckpoint(value, 3); !errors.Is(err, ErrInvalidCheckpoint) {
			t.Fatalf("payload=%s error=%v", value, err)
		}
	}
}

type plannerFunc func(context.Context, json.RawMessage, uint64) (Plan, error)

func (f plannerFunc) Next(ctx context.Context, state json.RawMessage, stepNo uint64) (Plan, error) {
	return f(ctx, state, stepNo)
}

type executorFunc func(context.Context, Invocation) (Outcome, error)

func (f executorFunc) Execute(ctx context.Context, invocation Invocation) (Outcome, error) {
	return f(ctx, invocation)
}

type sequenceIDs struct {
	mu       sync.Mutex
	steps    int
	attempts int
}

func (s *sequenceIDs) NewStepID() (domain.StepID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps++
	return domain.StepID(fmt.Sprintf("stp_%04d", s.steps)), nil
}
func (s *sequenceIDs) NewAttemptID() (domain.AttemptID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	return domain.AttemptID(fmt.Sprintf("att_%04d", s.attempts)), nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type agentStoreFake struct {
	inTransaction bool
	operations    []string
	retryStatuses []domain.AttemptStatus
	failStatuses  []domain.AttemptStatus
	lastPayload   json.RawMessage
	failAt        string
	failure       error
}

func (s *agentStoreFake) operation(name string, payload json.RawMessage) error {
	s.operations = append(s.operations, name)
	s.inTransaction = true
	defer func() { s.inTransaction = false }()
	if s.failAt == name {
		return s.failure
	}
	s.lastPayload = cloneJSON(payload)
	return nil
}
func (s *agentStoreFake) BeginStep(_ context.Context, _ repository.WorkerAuthority, command BeginStepCommand) error {
	return s.operation("begin", command.Payload)
}
func (s *agentStoreFake) RetryAttempt(_ context.Context, _ repository.WorkerAuthority, command RetryAttemptCommand) error {
	s.retryStatuses = append(s.retryStatuses, command.PreviousStatus)
	return s.operation("retry", command.Payload)
}
func (s *agentStoreFake) CompleteStep(_ context.Context, _ repository.WorkerAuthority, command CompleteStepCommand) error {
	return s.operation("complete", command.Payload)
}
func (s *agentStoreFake) FailStep(_ context.Context, _ repository.WorkerAuthority, command FailStepCommand) error {
	s.failStatuses = append(s.failStatuses, command.AttemptStatus)
	return s.operation("fail", command.Payload)
}
func (s *agentStoreFake) CompleteLoop(_ context.Context, _ repository.WorkerAuthority, command CompleteLoopCommand) error {
	return s.operation("loop", command.Payload)
}

type leaseStoreFake struct {
	record     repository.RunRecord
	checkpoint json.RawMessage
}

func (s *leaseStoreFake) GetRun(context.Context, domain.TenantID, domain.RunID) (repository.RunRecord, error) {
	return s.record, nil
}
func (s *leaseStoreFake) AcquireLease(_ context.Context, command repository.AcquireLeaseCommand) (repository.RunRecord, error) {
	s.record.ExecutionVersion++
	s.record.Lease = &repository.Lease{Owner: command.Owner, Token: 1, ExpiresAt: command.At.Add(command.Duration)}
	return s.record, nil
}
func (s *leaseStoreFake) RenewLease(_ context.Context, command repository.LeaseMutationCommand) (repository.RunRecord, error) {
	s.record.ExecutionVersion++
	s.record.Lease.ExpiresAt = command.At.Add(command.Duration)
	return s.record, nil
}
func (s *leaseStoreFake) ReleaseLease(_ context.Context, _ repository.LeaseMutationCommand) (repository.RunRecord, error) {
	s.record.ExecutionVersion++
	s.record.Lease = nil
	return s.record, nil
}
func (s *leaseStoreFake) LoadCheckpoint(context.Context, domain.TenantID, domain.RunID) (repository.Checkpoint, error) {
	if len(s.checkpoint) == 0 {
		return repository.Checkpoint{}, repository.ErrNotFound
	}
	return repository.Checkpoint{TenantID: "ten_0001", RunID: "run_0001", LeaseToken: 1, Payload: cloneJSON(s.checkpoint)}, nil
}

type quietTicker struct{ channel chan time.Time }

func (t *quietTicker) C() <-chan time.Time { return t.channel }
func (*quietTicker) Stop()                 {}

type leaseClock struct{ now time.Time }

func (c leaseClock) Now() time.Time { return c.now }
func (c leaseClock) NewTicker(time.Duration) runlease.Ticker {
	return &quietTicker{channel: make(chan time.Time)}
}

func testAcquisition(t *testing.T, maxSteps int, checkpoint json.RawMessage) (runlease.Acquisition, func()) {
	t.Helper()
	run, err := domain.NewRun("run_0001", "ten_0001", "ses_0001", "trn_0001", 1, 0, agentTestTime)
	if err != nil {
		t.Fatal(err)
	}
	store := &leaseStoreFake{record: repository.RunRecord{Run: run, MaxSteps: maxSteps, ExecutionVersion: 1}, checkpoint: checkpoint}
	ctx, cancel := context.WithCancel(context.Background())
	manager, err := runlease.New(store, runlease.Config{Owner: "worker-test", LeaseDuration: time.Minute,
		RenewInterval: 20 * time.Second, Clock: leaseClock{now: agentTestTime}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	acquisition, err := manager.Acquire(ctx, runlease.AcquireRequest{TenantID: run.TenantID(), RunID: run.ID()})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return acquisition, cancel
}

func testMachine(t *testing.T, store Store, planner Planner, executor Executor, maxAttempts int) *Machine {
	t.Helper()
	value, err := New(store, planner, executor, &sequenceIDs{}, Config{MaxAttempts: maxAttempts, Clock: fixedClock{now: agentTestTime.Add(time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testDirective(stepNo uint64) Directive {
	return Directive{Kind: domain.StepKindTool, OperationKey: fmt.Sprintf("op-%d", stepNo), Input: json.RawMessage(`{"query":"hello"}`), RetryUnknown: true}
}

func executingCheckpoint(t *testing.T, retryUnknown bool, attemptNo uint64) Checkpoint {
	t.Helper()
	return Checkpoint{SchemaVersion: checkpointSchemaVersion, Phase: PhaseExecuting, NextStepNo: 1, State: json.RawMessage(`{}`), Active: &ActiveStep{
		StepID: "stp_existing", StepNo: 1, StepRowVersion: 2, AttemptID: "att_existing", AttemptNo: attemptNo,
		AttemptRowVersion: 1, Directive: Directive{Kind: domain.StepKindTool, OperationKey: "op-stable",
			Input: json.RawMessage(`{"value":1}`), RetryUnknown: retryUnknown},
	}}
}
