package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/repository"
)

type TransactionRunner interface {
	WithinTx(context.Context, *sql.TxOptions, func(context.Context, postgres.DBTX) error) error
}

var _ Store = (*PostgresStore)(nil)
var _ TransactionRunner = (*postgres.Pool)(nil)

// PostgresStore implements every State Machine durable boundary as one
// PostgreSQL transaction. External calls are deliberately absent from this API.
type PostgresStore struct{ transactions TransactionRunner }

func NewPostgresStore(transactions TransactionRunner) (*PostgresStore, error) {
	if transactions == nil {
		return nil, fmt.Errorf("%w: transaction runner", ErrInvalidArgument)
	}
	return &PostgresStore{transactions: transactions}, nil
}

func (s *PostgresStore) BeginStep(ctx context.Context, authority repository.WorkerAuthority, command BeginStepCommand) error {
	if command.Step == nil || command.Attempt == nil || command.Step.Status() != domain.StepStatusPending ||
		command.Attempt.Status() != domain.AttemptStatusStarted || command.Attempt.StepID() != command.Step.ID() {
		return fmt.Errorf("%w: begin Step command", ErrInvalidArgument)
	}
	return s.withExecution(ctx, func(ctx context.Context, execution *repository.Execution) error {
		if err := execution.InsertStep(ctx, authority, command.Step); err != nil {
			return err
		}
		if _, err := execution.TransitionStep(ctx, repository.StepTransitionCommand{Authority: authority, StepID: command.Step.ID(),
			ExpectedRowVersion: 1, Target: domain.StepStatusRunning, At: command.Attempt.StartedAt()}); err != nil {
			return err
		}
		if err := execution.InsertAttempt(ctx, authority, command.Attempt); err != nil {
			return err
		}
		return execution.SaveCheckpoint(ctx, authority, command.Step.StepNo(), command.Payload)
	})
}

func (s *PostgresStore) RetryAttempt(ctx context.Context, authority repository.WorkerAuthority, command RetryAttemptCommand) error {
	if command.Attempt == nil || command.Attempt.StepID() != command.Previous.StepID ||
		command.Attempt.AttemptNo() != command.Previous.AttemptNo+1 ||
		(command.PreviousStatus != domain.AttemptStatusFailed && command.PreviousStatus != domain.AttemptStatusUnknown) {
		return fmt.Errorf("%w: retry Attempt command", ErrInvalidArgument)
	}
	return s.withExecution(ctx, func(ctx context.Context, execution *repository.Execution) error {
		if _, err := execution.FinishAttempt(ctx, repository.AttemptFinishCommand{Authority: authority,
			AttemptID: command.Previous.AttemptID, ExpectedRowVersion: command.Previous.AttemptRowVersion,
			Target: command.PreviousStatus, At: command.At}); err != nil {
			return err
		}
		if err := execution.InsertAttempt(ctx, authority, command.Attempt); err != nil {
			return err
		}
		return execution.SaveCheckpoint(ctx, authority, command.Previous.StepNo, command.Payload)
	})
}

func (s *PostgresStore) CompleteStep(ctx context.Context, authority repository.WorkerAuthority, command CompleteStepCommand) error {
	return s.finishStep(ctx, authority, command.Active, domain.AttemptStatusSucceeded, domain.StepStatusSucceeded, command.At, command.Payload)
}

func (s *PostgresStore) FailStep(ctx context.Context, authority repository.WorkerAuthority, command FailStepCommand) error {
	if command.AttemptStatus != domain.AttemptStatusFailed && command.AttemptStatus != domain.AttemptStatusUnknown {
		return fmt.Errorf("%w: failed Attempt status", ErrInvalidArgument)
	}
	return s.finishStep(ctx, authority, command.Active, command.AttemptStatus, domain.StepStatusFailed, command.At, command.Payload)
}

func (s *PostgresStore) CompleteLoop(ctx context.Context, authority repository.WorkerAuthority, command CompleteLoopCommand) error {
	return s.withExecution(ctx, func(ctx context.Context, execution *repository.Execution) error {
		return execution.SaveCheckpoint(ctx, authority, command.StepNo, command.Payload)
	})
}

func (s *PostgresStore) finishStep(ctx context.Context, authority repository.WorkerAuthority, active ActiveStep,
	attemptStatus domain.AttemptStatus, stepStatus domain.StepStatus, atTime time.Time, payload json.RawMessage) error {
	if err := active.validate(); err != nil {
		return err
	}
	return s.withExecution(ctx, func(ctx context.Context, execution *repository.Execution) error {
		if _, err := execution.FinishAttempt(ctx, repository.AttemptFinishCommand{Authority: authority,
			AttemptID: active.AttemptID, ExpectedRowVersion: active.AttemptRowVersion, Target: attemptStatus, At: atTime}); err != nil {
			return err
		}
		if _, err := execution.TransitionStep(ctx, repository.StepTransitionCommand{Authority: authority,
			StepID: active.StepID, ExpectedRowVersion: active.StepRowVersion, Target: stepStatus, At: atTime}); err != nil {
			return err
		}
		return execution.SaveCheckpoint(ctx, authority, active.StepNo, payload)
	})
}

func (s *PostgresStore) withExecution(ctx context.Context, work func(context.Context, *repository.Execution) error) error {
	if ctx == nil || work == nil {
		return fmt.Errorf("%w: transaction context or work", ErrInvalidArgument)
	}
	err := s.transactions.WithinTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(ctx context.Context, tx postgres.DBTX) error {
		execution, err := repository.NewExecution(tx)
		if err != nil {
			return err
		}
		return work(ctx, execution)
	})
	if err != nil {
		if errors.Is(err, repository.ErrLeaseFenced) || errors.Is(err, repository.ErrLeaseExpired) || errors.Is(err, repository.ErrRowConflict) {
			return err
		}
		return fmt.Errorf("agent durable transaction: %w", err)
	}
	return nil
}
