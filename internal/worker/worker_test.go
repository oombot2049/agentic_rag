package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/queue"
)

type fakeQueue struct {
	mu                          sync.Mutex
	items                       map[domain.TenantID][]queue.Delivery
	claims, renews, acks, nacks int
	renewErr                    error
}

func (f *fakeQueue) ReadyTenants(_ context.Context, _ queue.ReadyTenantsCommand) ([]domain.TenantID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]domain.TenantID, 0, len(f.items))
	for tenant, items := range f.items {
		if len(items) > 0 {
			result = append(result, tenant)
		}
	}
	return result, nil
}
func (f *fakeQueue) Claim(_ context.Context, command queue.ClaimCommand) ([]queue.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	items := f.items[command.TenantID]
	limit := command.Limit
	if limit > len(items) {
		limit = len(items)
	}
	result := append([]queue.Delivery(nil), items[:limit]...)
	f.items[command.TenantID] = items[limit:]
	return result, nil
}
func (f *fakeQueue) Renew(_ context.Context, _ queue.RenewCommand) (queue.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews++
	return queue.Delivery{}, f.renewErr
}
func (f *fakeQueue) Ack(_ context.Context, _ queue.LeaseCommand) error {
	f.mu.Lock()
	f.acks++
	f.mu.Unlock()
	return nil
}
func (f *fakeQueue) Nack(_ context.Context, _ queue.NackCommand) error {
	f.mu.Lock()
	f.nacks++
	f.mu.Unlock()
	return nil
}
func (f *fakeQueue) counts() (int, int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, f.renews, f.acks, f.nacks
}

func TestWorkerEnforcesGlobalAndPerTenantConcurrency(t *testing.T) {
	source := queueWithDeliveries(6, 6)
	release := make(chan struct{})
	var active, peak atomic.Int64
	processor := ProcessorFunc(func(context.Context, Task) Result {
		current := active.Add(1)
		for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
		}
		<-release
		active.Add(-1)
		return Result{Disposition: DispositionAck}
	})
	configuration := testConfig()
	configuration.MaxConcurrency = 4
	configuration.MaxConcurrencyPerTenant = 2
	value, err := New(source, processor, configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := value.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return value.Snapshot().Active == 4 })
	snapshot := value.Snapshot()
	if snapshot.PeakActive > 4 || snapshot.PeakByTenant["ten_0001"] > 2 || snapshot.PeakByTenant["ten_0002"] > 2 {
		t.Fatalf("concurrency snapshot = %+v", snapshot)
	}
	close(release)
	waitFor(t, func() bool { _, _, acks, _ := source.counts(); return acks == 12 })
	if err := value.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := value.Snapshot(); got.State != StateStopped || got.OwnedGoroutines != 0 || peak.Load() > 4 {
		t.Fatalf("final snapshot = %+v peak=%d", got, peak.Load())
	}
}

func TestWorkerGracefulStopSignalsDrainBeforeSettlement(t *testing.T) {
	source := queueWithDeliveries(2, 0)
	started, safe := make(chan struct{}), make(chan struct{})
	processor := ProcessorFunc(func(_ context.Context, task Task) Result {
		close(started)
		<-task.DrainRequested
		close(safe)
		return Result{Disposition: DispositionRetry, Reason: "graceful drain"}
	})
	value, _ := New(source, processor, testConfig(), nil)
	_ = value.Start(context.Background())
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- value.Stop(context.Background()) }()
	select {
	case <-safe:
	case <-time.After(time.Second):
		t.Fatal("processor did not observe drain")
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	claims, _, _, nacks := source.counts()
	if claims != 1 || nacks != 1 || value.Snapshot().OwnedGoroutines != 0 {
		t.Fatalf("claims=%d nacks=%d snapshot=%+v", claims, nacks, value.Snapshot())
	}
}

func TestWorkerDeadlineCancelsCooperativeProcessor(t *testing.T) {
	source := queueWithDeliveries(1, 0)
	started := make(chan struct{})
	processor := ProcessorFunc(func(ctx context.Context, _ Task) Result {
		close(started)
		<-ctx.Done()
		return Result{Disposition: DispositionRetry, Reason: "forced stop"}
	})
	value, _ := New(source, processor, testConfig(), nil)
	_ = value.Start(context.Background())
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := value.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop error = %v", err)
	}
	if got := value.Snapshot(); got.OwnedGoroutines != 0 || got.State != StateStopped {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestWorkerRenewsAndSuppressesSettlementAfterLeaseLoss(t *testing.T) {
	source := queueWithDeliveries(1, 0)
	source.renewErr = queue.ErrFenced
	processor := ProcessorFunc(func(ctx context.Context, _ Task) Result { <-ctx.Done(); return Result{Disposition: DispositionAck} })
	configuration := testConfig()
	configuration.RenewInterval = 5 * time.Millisecond
	value, _ := New(source, processor, configuration, nil)
	_ = value.Start(context.Background())
	waitFor(t, func() bool { return value.Snapshot().Fenced == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := value.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	_, renews, acks, nacks := source.counts()
	if renews == 0 || acks != 0 || nacks != 0 {
		t.Fatalf("renews=%d acks=%d nacks=%d", renews, acks, nacks)
	}
}

func TestWorkerRecoversProcessorPanicAndLifecycleValidation(t *testing.T) {
	source := queueWithDeliveries(1, 0)
	value, _ := New(source, ProcessorFunc(func(context.Context, Task) Result { panic("secret panic") }), testConfig(), nil)
	if err := value.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := value.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start error = %v", err)
	}
	waitFor(t, func() bool { return value.Snapshot().Panicked == 1 })
	if err := value.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, _, nacks := source.counts()
	if nacks != 1 {
		t.Fatalf("nacks = %d", nacks)
	}
	if _, err := New(nil, nil, Config{}, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("New error = %v", err)
	}
	stopped, _ := New(&fakeQueue{}, ProcessorFunc(func(context.Context, Task) Result { return Result{} }), testConfig(), nil)
	if err := stopped.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := stopped.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("Start after Stop error = %v", err)
	}
}

func TestWorkerPollDelayHasBoundedJitterAndBackoff(t *testing.T) {
	value, _ := New(&fakeQueue{}, ProcessorFunc(func(context.Context, Task) Result { return Result{} }), testConfig(), nil)
	base := value.nextPollDelay(0)
	backoff := value.nextPollDelay(3)
	if base < 900*time.Microsecond || base > 1100*time.Microsecond || backoff < 7200*time.Microsecond || backoff > 8800*time.Microsecond {
		t.Fatalf("base=%s backoff=%s", base, backoff)
	}
}

func queueWithDeliveries(first, second int) *fakeQueue {
	result := &fakeQueue{items: make(map[domain.TenantID][]queue.Delivery)}
	for tenantNo, count := range []int{first, second} {
		tenant := domain.TenantID("ten_000" + string(rune('1'+tenantNo)))
		for index := 0; index < count; index++ {
			n := tenantNo*100 + index
			result.items[tenant] = append(result.items[tenant], queue.Delivery{Message: queue.Message{DeliveryID: fmtID("evt", n), TenantID: tenant,
				SessionID: domain.SessionID(fmtID("ses", n)), TurnID: domain.TurnID(fmtID("trn", n)), RunID: domain.RunID(fmtID("run", n)),
				Payload: []byte(`{"type":"RunQueued.v1"}`), CreatedAt: time.Now().UTC(), AvailableAt: time.Now().UTC()},
				LeaseOwner: "worker-test", LeaseToken: 1, LeaseExpires: time.Now().Add(time.Minute), DeliveryCount: 1})
		}
	}
	return result
}

func fmtID(prefix string, n int) string {
	return prefix + "_" + string(rune('a'+n/26%26)) + string(rune('a'+n%26)) + "00"
}

func testConfig() Config {
	return Config{ID: "worker-test", MaxConcurrency: 2, MaxConcurrencyPerTenant: 1, PollInterval: time.Millisecond,
		LeaseDuration: 5 * time.Second, RenewInterval: time.Second, RetryDelay: time.Millisecond, TenantScanLimit: 10}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached")
}
