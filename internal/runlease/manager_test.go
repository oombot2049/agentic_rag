package runlease

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/repository"
)

var leaseTestTime = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

func TestManagerConcurrentAcquireHasOneWinner(t *testing.T) {
	store := newLeaseStore(t)
	clock := newManualClock(leaseTestTime)
	const contenders = 40
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wait sync.WaitGroup
	var winner *Handle
	var winnerMu sync.Mutex
	for index := 0; index < contenders; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			manager, _ := New(store, testLeaseConfig("worker-contender", clock))
			<-start
			acquired, err := manager.Acquire(context.Background(), validAcquireRequest())
			if err == nil {
				winnerMu.Lock()
				winner = acquired.Handle
				winnerMu.Unlock()
			}
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	success, unavailable := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrLeaseUnavailable):
			unavailable++
		default:
			t.Errorf("Acquire error = %v", err)
		}
	}
	if success != 1 || unavailable != contenders-1 {
		t.Fatalf("success/unavailable = %d/%d", success, unavailable)
	}
	if winner == nil || winner.Snapshot().Token != 1 {
		t.Fatalf("winner = %+v", winner)
	}
	if err := winner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandleRenewSerializesExecutionVersions(t *testing.T) {
	store := newLeaseStore(t)
	clock := newManualClock(leaseTestTime)
	manager, _ := New(store, testLeaseConfig("worker-a", clock))
	acquired, err := manager.Acquire(context.Background(), validAcquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got := acquired.Handle.Snapshot(); got.ExecutionVersion != 2 || got.Token != 1 {
		t.Fatalf("initial snapshot = %+v", got)
	}
	clock.Advance(10 * time.Second)
	clock.Tick()
	waitLease(t, func() bool { return acquired.Handle.Snapshot().ExecutionVersion == 3 })
	if err := acquired.Handle.WithAuthority(context.Background(), func(_ context.Context, authority repository.WorkerAuthority) (uint64, error) {
		if authority.ExpectedExecutionVersion != 3 || authority.Token != 1 {
			t.Fatalf("authority = %+v", authority)
		}
		return 4, store.advance(authority, 4)
	}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Second)
	clock.Tick()
	waitLease(t, func() bool { return acquired.Handle.Snapshot().ExecutionVersion == 5 })
	if err := acquired.Handle.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := acquired.Handle.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if got := acquired.Handle.Snapshot(); got.State != StateClosed || got.ExecutionVersion != 5 || !errors.Is(context.Cause(acquired.Handle.Context()), ErrClosed) {
		t.Fatalf("closed snapshot = %+v cause=%v", got, context.Cause(acquired.Handle.Context()))
	}
	if err := acquired.Handle.WithAuthority(context.Background(), func(context.Context, repository.WorkerAuthority) (uint64, error) { return 0, nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("mutation after close = %v", err)
	}
}

func TestGracefulCloseWaitsForDurableMutation(t *testing.T) {
	store := newLeaseStore(t)
	clock := newManualClock(leaseTestTime)
	manager, _ := New(store, testLeaseConfig("worker-a", clock))
	acquired, _ := manager.Acquire(context.Background(), validAcquireRequest())
	started, release := make(chan struct{}), make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- acquired.Handle.WithAuthority(context.Background(), func(_ context.Context, authority repository.WorkerAuthority) (uint64, error) {
			close(started)
			<-release
			return authority.ExpectedExecutionVersion, nil
		})
	}()
	<-started
	closed := make(chan error, 1)
	go func() { closed <- acquired.Handle.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before mutation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestExpiredTakeoverLoadsCheckpointAndFencesOldWorker(t *testing.T) {
	store := newLeaseStore(t)
	clock := newManualClock(leaseTestTime)
	firstManager, _ := New(store, testLeaseConfig("worker-old", clock))
	first, err := firstManager.Acquire(context.Background(), validAcquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	var oldAuthority repository.WorkerAuthority
	if err := first.Handle.WithAuthority(context.Background(), func(_ context.Context, authority repository.WorkerAuthority) (uint64, error) {
		oldAuthority = authority
		store.saveCheckpoint(authority, 7, json.RawMessage(`{"next":"generate"}`))
		return authority.ExpectedExecutionVersion, nil
	}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(31 * time.Second)
	secondManager, _ := New(store, testLeaseConfig("worker-new", clock))
	second, err := secondManager.Acquire(context.Background(), validAcquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	if second.Checkpoint == nil || second.Checkpoint.StepNo != 7 || second.Checkpoint.LeaseToken != 1 || second.Handle.Snapshot().Token != 2 {
		t.Fatalf("takeover = checkpoint %+v snapshot %+v", second.Checkpoint, second.Handle.Snapshot())
	}
	if err := store.commit(oldAuthority); !errors.Is(err, repository.ErrLeaseFenced) {
		t.Fatalf("old worker commit error = %v", err)
	}
	if err := first.Handle.WithAuthority(context.Background(), func(context.Context, repository.WorkerAuthority) (uint64, error) { return 0, nil }); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old handle mutation error = %v", err)
	}
	select {
	case <-first.Handle.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("old handle context was not cancelled")
	}
	_ = first.Handle.Close(context.Background())
	if err := second.Handle.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRenewalLossAndParentCancellationCancelExecution(t *testing.T) {
	t.Run("renew fenced", func(t *testing.T) {
		store := newLeaseStore(t)
		clock := newManualClock(leaseTestTime)
		manager, _ := New(store, testLeaseConfig("worker-a", clock))
		acquired, _ := manager.Acquire(context.Background(), validAcquireRequest())
		store.failRenew = repository.ErrLeaseFenced
		clock.Advance(10 * time.Second)
		clock.Tick()
		waitLease(t, func() bool { return acquired.Handle.Snapshot().State == StateLost })
		if !errors.Is(context.Cause(acquired.Handle.Context()), ErrLeaseLost) {
			t.Fatalf("cause = %v", context.Cause(acquired.Handle.Context()))
		}
		_ = acquired.Handle.Close(context.Background())
	})
	t.Run("parent cancelled", func(t *testing.T) {
		store := newLeaseStore(t)
		clock := newManualClock(leaseTestTime)
		manager, _ := New(store, testLeaseConfig("worker-a", clock))
		ctx, cancel := context.WithCancel(context.Background())
		acquired, _ := manager.Acquire(ctx, validAcquireRequest())
		cancel()
		waitLease(t, func() bool { return acquired.Handle.Snapshot().State == StateLost })
		if !errors.Is(context.Cause(acquired.Handle.Context()), context.Canceled) {
			t.Fatalf("cause = %v", context.Cause(acquired.Handle.Context()))
		}
		_ = acquired.Handle.Close(context.Background())
	})
}

func TestManagerValidationAndCheckpointLoadRollback(t *testing.T) {
	clock := newManualClock(leaseTestTime)
	if _, err := New(nil, Config{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("New error = %v", err)
	}
	store := newLeaseStore(t)
	store.checkpointErr = errors.New("storage unavailable")
	manager, _ := New(store, testLeaseConfig("worker-a", clock))
	if _, err := manager.Acquire(context.Background(), validAcquireRequest()); err == nil || store.current.Lease != nil {
		t.Fatalf("Acquire error=%v lease=%+v", err, store.current.Lease)
	}
	if _, err := manager.Acquire(nilContext(), validAcquireRequest()); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil context error = %v", err)
	}
}

type leaseStore struct {
	mu            sync.Mutex
	current       repository.RunRecord
	checkpoint    *repository.Checkpoint
	checkpointErr error
	failRenew     error
}

func newLeaseStore(t *testing.T) *leaseStore {
	t.Helper()
	run, err := domain.NewRun("run_0001", "ten_0001", "ses_0001", "trn_0001", 1, 0, leaseTestTime)
	if err != nil {
		t.Fatal(err)
	}
	return &leaseStore{current: repository.RunRecord{Run: run, MaxSteps: 12, ExecutionVersion: 1}}
}

func (s *leaseStore) GetRun(_ context.Context, tenantID domain.TenantID, runID domain.RunID) (repository.RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current.Run.TenantID() != tenantID || s.current.Run.ID() != runID {
		return repository.RunRecord{}, repository.ErrNotFound
	}
	return cloneRecord(s.current), nil
}

func (s *leaseStore) AcquireLease(_ context.Context, command repository.AcquireLeaseCommand) (repository.RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if command.ExpectedExecutionVersion != s.current.ExecutionVersion {
		return repository.RunRecord{}, repository.ErrRowConflict
	}
	if s.current.Lease != nil && s.current.Lease.ExpiresAt.After(command.At) {
		return repository.RunRecord{}, repository.ErrLeaseHeld
	}
	token := uint64(1)
	if s.current.Lease != nil {
		token = s.current.Lease.Token + 1
	}
	s.current.ExecutionVersion++
	s.current.Lease = &repository.Lease{Owner: command.Owner, Token: token, ExpiresAt: command.At.Add(command.Duration)}
	return cloneRecord(s.current), nil
}

func (s *leaseStore) RenewLease(_ context.Context, command repository.LeaseMutationCommand) (repository.RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failRenew != nil {
		return repository.RunRecord{}, s.failRenew
	}
	if err := s.authorize(command.Owner, command.Token, command.ExpectedExecutionVersion, command.At); err != nil {
		return repository.RunRecord{}, err
	}
	s.current.ExecutionVersion++
	s.current.Lease.ExpiresAt = command.At.Add(command.Duration)
	return cloneRecord(s.current), nil
}

func (s *leaseStore) ReleaseLease(_ context.Context, command repository.LeaseMutationCommand) (repository.RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(command.Owner, command.Token, command.ExpectedExecutionVersion, command.At); err != nil {
		return repository.RunRecord{}, err
	}
	s.current.ExecutionVersion++
	s.current.Lease = nil
	return cloneRecord(s.current), nil
}

func (s *leaseStore) LoadCheckpoint(_ context.Context, tenantID domain.TenantID, runID domain.RunID) (repository.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkpointErr != nil {
		return repository.Checkpoint{}, s.checkpointErr
	}
	if s.checkpoint == nil || s.checkpoint.TenantID != tenantID || s.checkpoint.RunID != runID {
		return repository.Checkpoint{}, repository.ErrNotFound
	}
	result := *s.checkpoint
	result.Payload = append([]byte(nil), result.Payload...)
	return result, nil
}

func (s *leaseStore) authorize(owner string, token, version uint64, at time.Time) error {
	if s.current.Lease == nil || s.current.Lease.Owner != owner || s.current.Lease.Token != token {
		return repository.ErrLeaseFenced
	}
	if !s.current.Lease.ExpiresAt.After(at) {
		return repository.ErrLeaseExpired
	}
	if s.current.ExecutionVersion != version {
		return repository.ErrRowConflict
	}
	return nil
}

func (s *leaseStore) advance(authority repository.WorkerAuthority, version uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(authority.Owner, authority.Token, authority.ExpectedExecutionVersion, authority.At); err != nil {
		return err
	}
	s.current.ExecutionVersion = version
	return nil
}

func (s *leaseStore) commit(authority repository.WorkerAuthority) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authorize(authority.Owner, authority.Token, authority.ExpectedExecutionVersion, time.Now().UTC())
}

func (s *leaseStore) saveCheckpoint(authority repository.WorkerAuthority, step uint64, payload json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpoint = &repository.Checkpoint{TenantID: authority.TenantID, RunID: authority.RunID, LeaseToken: authority.Token,
		StepNo: step, Payload: append([]byte(nil), payload...), CreatedAt: authority.At, UpdatedAt: authority.At}
}

func cloneRecord(value repository.RunRecord) repository.RunRecord {
	if value.Lease != nil {
		lease := *value.Lease
		value.Lease = &lease
	}
	return value
}

type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*manualTicker
}

func newManualClock(now time.Time) *manualClock { return &manualClock{now: now} }
func (c *manualClock) Now() time.Time           { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *manualClock) NewTicker(_ time.Duration) Ticker {
	c.mu.Lock()
	defer c.mu.Unlock()
	ticker := &manualTicker{values: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, ticker)
	return ticker
}
func (c *manualClock) Advance(value time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(value)
	c.mu.Unlock()
}
func (c *manualClock) Tick() {
	deadline := time.Now().Add(time.Second)
	var now time.Time
	var tickers []*manualTicker
	for time.Now().Before(deadline) {
		c.mu.Lock()
		now, tickers = c.now, append([]*manualTicker(nil), c.tickers...)
		c.mu.Unlock()
		if len(tickers) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for _, ticker := range tickers {
		ticker.send(now)
	}
}

type manualTicker struct {
	mu      sync.Mutex
	values  chan time.Time
	stopped bool
}

func (t *manualTicker) C() <-chan time.Time { return t.values }
func (t *manualTicker) Stop()               { t.mu.Lock(); t.stopped = true; t.mu.Unlock() }
func (t *manualTicker) send(value time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	select {
	case t.values <- value:
	default:
	}
}

func validAcquireRequest() AcquireRequest {
	return AcquireRequest{TenantID: "ten_0001", RunID: "run_0001"}
}
func nilContext() context.Context { return nil }
func testLeaseConfig(owner string, clock Clock) Config {
	return Config{Owner: owner, LeaseDuration: 30 * time.Second, RenewInterval: 10 * time.Second, Clock: clock}
}
func waitLease(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached")
}
