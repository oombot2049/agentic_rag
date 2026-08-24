// Package runlease manages fenced PostgreSQL Run execution leases.
package runlease

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/repository"
)

var (
	ErrInvalidArgument  = errors.New("run lease invalid argument")
	ErrLeaseUnavailable = errors.New("run lease unavailable")
	ErrLeaseLost        = errors.New("run lease lost")
	ErrClosed           = errors.New("run lease handle closed")
)

type Store interface {
	GetRun(context.Context, domain.TenantID, domain.RunID) (repository.RunRecord, error)
	AcquireLease(context.Context, repository.AcquireLeaseCommand) (repository.RunRecord, error)
	RenewLease(context.Context, repository.LeaseMutationCommand) (repository.RunRecord, error)
	ReleaseLease(context.Context, repository.LeaseMutationCommand) (repository.RunRecord, error)
	LoadCheckpoint(context.Context, domain.TenantID, domain.RunID) (repository.Checkpoint, error)
}

type Clock interface {
	Now() time.Time
	NewTicker(time.Duration) Ticker
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type Config struct {
	Owner         string
	LeaseDuration time.Duration
	RenewInterval time.Duration
	Clock         Clock
}

func DefaultConfig(owner string) Config {
	return Config{Owner: owner, LeaseDuration: 30 * time.Second, RenewInterval: 10 * time.Second}
}

type Manager struct {
	store  Store
	config Config
	clock  Clock
}

func New(store Store, configuration Config) (*Manager, error) {
	if store == nil || !validOwner(configuration.Owner) ||
		configuration.LeaseDuration < repository.MinimumLeaseDuration || configuration.LeaseDuration > repository.MaximumLeaseDuration ||
		configuration.RenewInterval <= 0 || configuration.RenewInterval >= configuration.LeaseDuration {
		return nil, fmt.Errorf("%w: store or configuration", ErrInvalidArgument)
	}
	clock := configuration.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Manager{store: store, config: configuration, clock: clock}, nil
}

type AcquireRequest struct {
	TenantID domain.TenantID
	RunID    domain.RunID
}

type Acquisition struct {
	Handle     *Handle
	Run        repository.RunRecord
	Checkpoint *repository.Checkpoint
}

func (m *Manager) Acquire(ctx context.Context, request AcquireRequest) (Acquisition, error) {
	if ctx == nil {
		return Acquisition{}, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	if _, err := domain.ParseTenantID(request.TenantID.String()); err != nil {
		return Acquisition{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if _, err := domain.ParseRunID(request.RunID.String()); err != nil {
		return Acquisition{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if err := ctx.Err(); err != nil {
		return Acquisition{}, err
	}
	current, err := m.store.GetRun(ctx, request.TenantID, request.RunID)
	if err != nil {
		return Acquisition{}, fmt.Errorf("load Run before lease acquisition: %w", err)
	}
	now := m.clock.Now().UTC()
	acquired, err := m.store.AcquireLease(ctx, repository.AcquireLeaseCommand{TenantID: request.TenantID, RunID: request.RunID,
		Owner: m.config.Owner, ExpectedExecutionVersion: current.ExecutionVersion, At: now, Duration: m.config.LeaseDuration})
	if err != nil {
		if errors.Is(err, repository.ErrLeaseHeld) || errors.Is(err, repository.ErrRowConflict) {
			return Acquisition{}, fmt.Errorf("%w: %w", ErrLeaseUnavailable, err)
		}
		return Acquisition{}, fmt.Errorf("acquire Run lease: %w", err)
	}
	checkpoint, checkpointErr := m.loadCheckpoint(ctx, request)
	if checkpointErr != nil || ctx.Err() != nil {
		if checkpointErr == nil {
			checkpointErr = ctx.Err()
		}
		releaseErr := m.releaseAcquisition(acquired)
		return Acquisition{}, errors.Join(fmt.Errorf("load takeover checkpoint: %w", checkpointErr), releaseErr)
	}
	executionCtx, cancel := context.WithCancelCause(ctx)
	handle := &Handle{store: m.store, config: m.config, clock: m.clock, record: acquired, state: StateActive,
		ctx: executionCtx, cancel: cancel, gate: make(chan struct{}, 1), stop: make(chan struct{}), heartbeatDone: make(chan struct{}), closeDone: make(chan struct{})}
	handle.gate <- struct{}{}
	go handle.heartbeat()
	return Acquisition{Handle: handle, Run: acquired, Checkpoint: checkpoint}, nil
}

func (m *Manager) loadCheckpoint(ctx context.Context, request AcquireRequest) (*repository.Checkpoint, error) {
	value, err := m.store.LoadCheckpoint(ctx, request.TenantID, request.RunID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value.Payload = append([]byte(nil), value.Payload...)
	return &value, nil
}

func (m *Manager) releaseAcquisition(record repository.RunRecord) error {
	if record.Lease == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := m.store.ReleaseLease(ctx, leaseMutation(record, m.clock.Now(), 0))
	if err != nil {
		return fmt.Errorf("release failed acquisition: %w", err)
	}
	return nil
}

type State string

const (
	StateActive State = "ACTIVE"
	StateLost   State = "LOST"
	StateClosed State = "CLOSED"
)

type Snapshot struct {
	State            State
	TenantID         domain.TenantID
	RunID            domain.RunID
	Owner            string
	Token            uint64
	ExecutionVersion uint64
	ExpiresAt        time.Time
	Cause            error
}

type Mutation func(context.Context, repository.WorkerAuthority) (executionVersion uint64, err error)

type Handle struct {
	store  Store
	config Config
	clock  Clock

	mu            sync.Mutex
	record        repository.RunRecord
	state         State
	cause         error
	ctx           context.Context
	cancel        context.CancelCauseFunc
	gate          chan struct{}
	stop          chan struct{}
	stopOnce      sync.Once
	heartbeatDone chan struct{}
	closeOnce     sync.Once
	closeDone     chan struct{}
	closeErr      error
}

func (h *Handle) Context() context.Context { return h.ctx }

func (h *Handle) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := Snapshot{State: h.state, Cause: h.cause}
	if h.record.Run != nil {
		result.TenantID, result.RunID = h.record.Run.TenantID(), h.record.Run.ID()
	}
	if h.record.Lease != nil {
		result.Owner, result.Token, result.ExpiresAt = h.record.Lease.Owner, h.record.Lease.Token, h.record.Lease.ExpiresAt
	}
	result.ExecutionVersion = h.record.ExecutionVersion
	return result
}

// WithAuthority serializes a short durable mutation with heartbeat renewal.
// The callback must return the Run execution version after its operation.
func (h *Handle) WithAuthority(ctx context.Context, mutation Mutation) error {
	if ctx == nil || mutation == nil {
		return fmt.Errorf("%w: context or mutation", ErrInvalidArgument)
	}
	if err := h.acquireGate(ctx); err != nil {
		return err
	}
	defer h.releaseGate()
	record, err := h.activeRecord()
	if err != nil {
		return err
	}
	now := h.clock.Now().UTC()
	if record.Lease == nil || !record.Lease.ExpiresAt.After(now) {
		return h.lose(repository.ErrLeaseExpired)
	}
	authority := repository.WorkerAuthority{TenantID: record.Run.TenantID(), RunID: record.Run.ID(), Owner: record.Lease.Owner,
		Token: record.Lease.Token, ExpectedExecutionVersion: record.ExecutionVersion, At: now}
	operationCtx, cancel := context.WithCancelCause(ctx)
	stopCancellation := context.AfterFunc(h.ctx, func() { cancel(context.Cause(h.ctx)) })
	nextVersion, err := mutation(operationCtx, authority)
	stopCancellation()
	cancel(nil)
	if err != nil {
		if authorityError(err) {
			return h.lose(err)
		}
		return err
	}
	if nextVersion < record.ExecutionVersion || nextVersion > record.ExecutionVersion+1 {
		return fmt.Errorf("%w: mutation returned invalid execution version", ErrInvalidArgument)
	}
	h.mu.Lock()
	if h.state == StateActive {
		h.record.ExecutionVersion = nextVersion
	}
	h.mu.Unlock()
	return nil
}

func (h *Handle) heartbeat() {
	defer close(h.heartbeatDone)
	ticker := h.clock.NewTicker(h.config.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-h.ctx.Done():
			h.lose(context.Cause(h.ctx))
			return
		case <-ticker.C():
			if err := h.renew(); err != nil {
				return
			}
		}
	}
}

func (h *Handle) renew() error {
	if err := h.acquireGate(h.ctx); err != nil {
		return h.lose(err)
	}
	defer h.releaseGate()
	record, err := h.activeRecord()
	if err != nil {
		return err
	}
	now := h.clock.Now().UTC()
	if record.Lease == nil || !record.Lease.ExpiresAt.After(now) {
		return h.lose(repository.ErrLeaseExpired)
	}
	renewed, err := h.store.RenewLease(h.ctx, leaseMutation(record, now, h.config.LeaseDuration))
	if err != nil {
		return h.lose(err)
	}
	if renewed.Lease == nil || renewed.Lease.Token != record.Lease.Token || renewed.Lease.Owner != record.Lease.Owner ||
		renewed.ExecutionVersion != record.ExecutionVersion+1 {
		return h.lose(errors.New("renew returned inconsistent authority"))
	}
	h.mu.Lock()
	if h.state == StateActive {
		h.record = renewed
	}
	h.mu.Unlock()
	return nil
}

func (h *Handle) Close(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil close context", ErrInvalidArgument)
	}
	h.closeOnce.Do(func() {
		h.stopOnce.Do(func() { close(h.stop) })
		select {
		case <-h.heartbeatDone:
		case <-ctx.Done():
			h.cancel(ctx.Err())
			<-h.heartbeatDone
		}
		if err := h.acquireGate(ctx); err != nil {
			h.closeErr = err
			h.lose(err)
			close(h.closeDone)
			return
		}
		defer h.releaseGate()
		record, err := h.activeRecord()
		if err == nil {
			_, h.closeErr = h.store.ReleaseLease(ctx, leaseMutation(record, h.clock.Now(), 0))
			if h.closeErr != nil {
				h.lose(h.closeErr)
			} else {
				h.mu.Lock()
				h.state, h.cause = StateClosed, ErrClosed
				h.cancel(ErrClosed)
				h.mu.Unlock()
			}
		} else if !errors.Is(err, ErrLeaseLost) {
			h.closeErr = err
		}
		close(h.closeDone)
	})
	<-h.closeDone
	return h.closeErr
}

func (h *Handle) activeRecord() (repository.RunRecord, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == StateClosed {
		return repository.RunRecord{}, ErrClosed
	}
	if h.state != StateActive {
		return repository.RunRecord{}, errors.Join(ErrLeaseLost, h.cause)
	}
	return h.record, nil
}

func (h *Handle) lose(cause error) error {
	if cause == nil {
		cause = ErrLeaseLost
	}
	h.mu.Lock()
	if h.state == StateActive {
		h.state, h.cause = StateLost, cause
		h.cancel(errors.Join(ErrLeaseLost, cause))
	}
	h.mu.Unlock()
	return errors.Join(ErrLeaseLost, cause)
}

func (h *Handle) acquireGate(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.gate:
		return nil
	}
}

func (h *Handle) releaseGate() { h.gate <- struct{}{} }

func leaseMutation(record repository.RunRecord, at time.Time, duration time.Duration) repository.LeaseMutationCommand {
	return repository.LeaseMutationCommand{TenantID: record.Run.TenantID(), RunID: record.Run.ID(), Owner: record.Lease.Owner,
		Token: record.Lease.Token, ExpectedExecutionVersion: record.ExecutionVersion, At: at.UTC(), Duration: duration}
}

func authorityError(err error) bool {
	return errors.Is(err, repository.ErrLeaseFenced) || errors.Is(err, repository.ErrLeaseExpired) || errors.Is(err, repository.ErrRowConflict)
}

func validOwner(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
func (systemClock) NewTicker(value time.Duration) Ticker {
	return systemTicker{Ticker: time.NewTicker(value)}
}

type systemTicker struct{ *time.Ticker }

func (t systemTicker) C() <-chan time.Time { return t.Ticker.C }
