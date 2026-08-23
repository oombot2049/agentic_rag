// Package worker consumes RunQueue deliveries with bounded tenant concurrency.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	observabilitylog "github.com/agenticrag/agenticrag/internal/observability/log"
	"github.com/agenticrag/agenticrag/internal/queue"
)

var (
	ErrInvalidArgument = errors.New("worker invalid argument")
	ErrAlreadyStarted  = errors.New("worker already started")
	ErrStopped         = errors.New("worker is stopped")
)

type Queue interface {
	ReadyTenants(context.Context, queue.ReadyTenantsCommand) ([]domain.TenantID, error)
	Claim(context.Context, queue.ClaimCommand) ([]queue.Delivery, error)
	Renew(context.Context, queue.RenewCommand) (queue.Delivery, error)
	Ack(context.Context, queue.LeaseCommand) error
	Nack(context.Context, queue.NackCommand) error
}

type Disposition uint8

const (
	DispositionAck Disposition = iota + 1
	DispositionRetry
)

type Result struct {
	Disposition Disposition
	RetryAfter  time.Duration
	Reason      string
}

type Task struct {
	Delivery       queue.Delivery
	DrainRequested <-chan struct{}
}

// Processor must return after ctx cancellation and should return Retry after
// observing DrainRequested at its next durable safe point.
type Processor interface {
	Process(context.Context, Task) Result
}

type ProcessorFunc func(context.Context, Task) Result

func (f ProcessorFunc) Process(ctx context.Context, task Task) Result { return f(ctx, task) }

type Config struct {
	ID                      string
	MaxConcurrency          int
	MaxConcurrencyPerTenant int
	PollInterval            time.Duration
	LeaseDuration           time.Duration
	RenewInterval           time.Duration
	RetryDelay              time.Duration
	TenantScanLimit         int
}

func DefaultConfig() Config {
	return Config{ID: "worker-1", MaxConcurrency: 32, MaxConcurrencyPerTenant: 4,
		PollInterval: 100 * time.Millisecond, LeaseDuration: 30 * time.Second,
		RenewInterval: 10 * time.Second, RetryDelay: time.Second, TenantScanLimit: 100}
}

type State string

const (
	StateReady    State = "READY"
	StateRunning  State = "RUNNING"
	StateDraining State = "DRAINING"
	StateStopped  State = "STOPPED"
)

type Snapshot struct {
	State           State
	Active          int
	PeakActive      int
	ActiveByTenant  map[string]int
	PeakByTenant    map[string]int
	Claims          uint64
	Completed       uint64
	Retried         uint64
	Fenced          uint64
	Panicked        uint64
	OwnedGoroutines int
}

type Worker struct {
	queue     Queue
	processor Processor
	config    Config
	logger    *slog.Logger
	now       func() time.Time

	mu              sync.Mutex
	state           State
	active          int
	peak            int
	byTenant        map[string]int
	peakTenant      map[string]int
	claims          uint64
	completed       uint64
	retried         uint64
	fenced          uint64
	panicked        uint64
	owned           int
	rotation        int
	pollSequence    uint64
	drain           chan struct{}
	schedulerCancel context.CancelFunc
	taskCancel      context.CancelFunc
	schedulerWG     sync.WaitGroup
	taskWG          sync.WaitGroup
}

func New(source Queue, processor Processor, configuration Config, logger *slog.Logger) (*Worker, error) {
	if source == nil || processor == nil || validateConfig(configuration) != nil {
		return nil, fmt.Errorf("%w: dependencies or configuration", ErrInvalidArgument)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Worker{queue: source, processor: processor, config: configuration, logger: logger, now: time.Now,
		state: StateReady, byTenant: make(map[string]int), peakTenant: make(map[string]int), drain: make(chan struct{})}, nil
}

func validateConfig(c Config) error {
	if strings.TrimSpace(c.ID) != c.ID || c.ID == "" || len(c.ID) > 256 || c.MaxConcurrency < 1 || c.MaxConcurrency > 1000 ||
		c.MaxConcurrencyPerTenant < 1 || c.MaxConcurrencyPerTenant > c.MaxConcurrency || c.PollInterval <= 0 ||
		c.PollInterval > time.Minute || c.LeaseDuration < queue.MinimumLeaseDuration || c.LeaseDuration > queue.MaximumLeaseDuration ||
		c.RenewInterval <= 0 || c.RenewInterval >= c.LeaseDuration || c.RetryDelay < 0 || c.RetryDelay > 24*time.Hour ||
		c.TenantScanLimit < 1 || c.TenantScanLimit > 1000 {
		return ErrInvalidArgument
	}
	return nil
}

func (w *Worker) Name() string { return "agent-worker" }

func (w *Worker) Start(_ context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == StateRunning || w.state == StateDraining {
		return ErrAlreadyStarted
	}
	if w.state == StateStopped {
		return ErrStopped
	}
	taskCtx, taskCancel := context.WithCancel(context.Background())
	schedulerCtx, schedulerCancel := context.WithCancel(taskCtx)
	w.taskCancel, w.schedulerCancel, w.state = taskCancel, schedulerCancel, StateRunning
	w.owned++
	w.schedulerWG.Add(1)
	go w.schedule(schedulerCtx, taskCtx)
	return nil
}

func (w *Worker) Stop(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil stop context", ErrInvalidArgument)
	}
	w.mu.Lock()
	if w.state == StateStopped {
		w.mu.Unlock()
		return nil
	}
	if w.state == StateReady {
		w.state = StateStopped
		close(w.drain)
		w.mu.Unlock()
		return nil
	}
	if w.state == StateRunning {
		w.state = StateDraining
		close(w.drain)
		w.schedulerCancel()
	}
	taskCancel := w.taskCancel
	w.mu.Unlock()

	done := make(chan struct{})
	go func() { w.schedulerWG.Wait(); w.taskWG.Wait(); close(done) }()
	select {
	case <-done:
		w.markStopped()
		return nil
	case <-ctx.Done():
		taskCancel()
		<-done
		w.markStopped()
		return ctx.Err()
	}
}

func (w *Worker) markStopped() {
	w.mu.Lock()
	w.state = StateStopped
	w.mu.Unlock()
}

func (w *Worker) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Snapshot{State: w.state, Active: w.active, PeakActive: w.peak, ActiveByTenant: cloneCounts(w.byTenant),
		PeakByTenant: cloneCounts(w.peakTenant), Claims: w.claims, Completed: w.completed, Retried: w.retried,
		Fenced: w.fenced, Panicked: w.panicked, OwnedGoroutines: w.owned}
}

func cloneCounts(source map[string]int) map[string]int {
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (w *Worker) schedule(schedulerCtx, taskCtx context.Context) {
	defer func() { w.mu.Lock(); w.owned--; w.mu.Unlock(); w.schedulerWG.Done() }()
	timer := time.NewTimer(0)
	defer timer.Stop()
	failures := uint(0)
	for {
		select {
		case <-schedulerCtx.Done():
			return
		case <-timer.C:
			if w.poll(schedulerCtx, taskCtx) {
				if failures < 3 {
					failures++
				}
			} else {
				failures = 0
			}
			timer.Reset(w.nextPollDelay(failures))
		}
	}
}

func (w *Worker) poll(ctx, taskCtx context.Context) bool {
	available := w.availableGlobal()
	if available == 0 {
		return false
	}
	tenants, err := w.queue.ReadyTenants(ctx, queue.ReadyTenantsCommand{At: w.now().UTC(), Limit: w.config.TenantScanLimit})
	if err != nil {
		w.logger.ErrorContext(ctx, "worker tenant scan failed", "error", err)
		return true
	}
	if len(tenants) == 0 {
		return false
	}
	failed := false
	start := w.nextRotation(len(tenants))
	for offset := 0; offset < len(tenants) && available > 0; offset++ {
		if ctx.Err() != nil {
			return false
		}
		tenantID := tenants[(start+offset)%len(tenants)]
		limit := w.availableForTenant(tenantID)
		if limit > available {
			limit = available
		}
		if limit == 0 {
			continue
		}
		deliveries, err := w.queue.Claim(ctx, queue.ClaimCommand{TenantID: tenantID, Owner: w.config.ID,
			Limit: limit, Duration: w.config.LeaseDuration, At: w.now().UTC()})
		if err != nil {
			w.logger.ErrorContext(ctx, "worker claim failed", "tenant_id", tenantID.String(), "error", err)
			failed = true
			continue
		}
		for _, delivery := range deliveries {
			if ctx.Err() != nil {
				w.returnClaim(delivery, "worker draining before launch")
				continue
			}
			if !w.reserve(delivery.TenantID) {
				w.returnClaim(delivery, "worker concurrency changed")
				continue
			}
			available--
			w.taskWG.Add(1)
			go w.execute(taskCtx, delivery)
		}
	}
	return failed
}

func (w *Worker) nextPollDelay(failures uint) time.Duration {
	base := w.config.PollInterval * time.Duration(1<<failures)
	if base > time.Minute {
		base = time.Minute
	}
	w.pollSequence++
	hash := w.pollSequence
	for _, value := range []byte(w.config.ID) {
		hash = hash*1099511628211 ^ uint64(value)
	}
	percent := int64(hash%21) - 10
	return base + time.Duration(int64(base)*percent/100)
}

func (w *Worker) nextRotation(length int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	start := w.rotation % length
	w.rotation = (start + 1) % length
	return start
}

func (w *Worker) availableGlobal() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.config.MaxConcurrency - w.active
}
func (w *Worker) availableForTenant(id domain.TenantID) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.config.MaxConcurrencyPerTenant - w.byTenant[id.String()]
}

func (w *Worker) reserve(id domain.TenantID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := id.String()
	if w.state != StateRunning || w.active >= w.config.MaxConcurrency || w.byTenant[key] >= w.config.MaxConcurrencyPerTenant {
		return false
	}
	w.active++
	w.byTenant[key]++
	w.claims++
	if w.active > w.peak {
		w.peak = w.active
	}
	if w.byTenant[key] > w.peakTenant[key] {
		w.peakTenant[key] = w.byTenant[key]
	}
	return true
}

func (w *Worker) execute(taskCtx context.Context, delivery queue.Delivery) {
	w.mu.Lock()
	w.owned++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.active--
		w.byTenant[delivery.TenantID.String()]--
		w.owned--
		w.mu.Unlock()
		w.taskWG.Done()
	}()
	ctx, cancel := context.WithCancel(taskCtx)
	defer cancel()
	ctx = observabilitylog.WithCorrelation(ctx, observabilitylog.Correlation{TenantID: delivery.TenantID.String(),
		SessionID: delivery.SessionID.String(), TurnID: delivery.TurnID.String(), RunID: delivery.RunID.String(), TraceID: traceID(delivery.Trace.TraceParent)})
	renewDone := make(chan struct{})
	renewLost := make(chan error, 1)
	w.mu.Lock()
	w.owned++
	w.mu.Unlock()
	go w.renew(ctx, cancel, delivery, renewDone, renewLost)
	result, panicked := w.process(ctx, Task{Delivery: delivery, DrainRequested: w.drain})
	close(renewDone)
	if err := <-renewLost; err != nil {
		w.mu.Lock()
		w.fenced++
		w.mu.Unlock()
		w.logger.WarnContext(ctx, "worker lease authority lost", "error", err)
		return
	}
	if panicked {
		result = Result{Disposition: DispositionRetry, RetryAfter: w.config.RetryDelay, Reason: "processor panic"}
	}
	w.settle(ctx, delivery, result)
}

func (w *Worker) process(ctx context.Context, task Task) (result Result, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
			w.mu.Lock()
			w.panicked++
			w.mu.Unlock()
			w.logger.ErrorContext(ctx, "worker processor panic", "stack_size", len(debug.Stack()))
		}
	}()
	return w.processor.Process(ctx, task), false
}

func (w *Worker) renew(ctx context.Context, cancel context.CancelFunc, delivery queue.Delivery, done <-chan struct{}, result chan<- error) {
	defer func() { w.mu.Lock(); w.owned--; w.mu.Unlock() }()
	ticker := time.NewTicker(w.config.RenewInterval)
	defer ticker.Stop()
	defer close(result)
	for {
		select {
		case <-done:
			result <- nil
			return
		case <-ctx.Done():
			result <- ctx.Err()
			return
		case <-ticker.C:
			_, err := w.queue.Renew(ctx, queue.RenewCommand{LeaseCommand: leaseCommand(delivery, w.now()), Duration: w.config.LeaseDuration})
			if err != nil {
				cancel()
				result <- err
				return
			}
		}
	}
}

func (w *Worker) settle(ctx context.Context, delivery queue.Delivery, result Result) {
	command := leaseCommand(delivery, w.now())
	switch result.Disposition {
	case DispositionAck:
		if err := w.queue.Ack(ctx, command); err != nil {
			w.logger.WarnContext(ctx, "worker ack failed", "error", err)
			return
		}
		w.mu.Lock()
		w.completed++
		w.mu.Unlock()
	case DispositionRetry:
		delay, reason := result.RetryAfter, strings.TrimSpace(result.Reason)
		if delay < 0 || delay > 24*time.Hour {
			delay = w.config.RetryDelay
		}
		if reason == "" || len(reason) > queue.MaximumErrorLength || strings.ContainsAny(reason, "\r\n") {
			reason = "processor requested retry"
		}
		if err := w.queue.Nack(ctx, queue.NackCommand{LeaseCommand: command, RetryAfter: delay, Reason: reason}); err != nil {
			w.logger.WarnContext(ctx, "worker nack failed", "error", err)
			return
		}
		w.mu.Lock()
		w.retried++
		w.mu.Unlock()
	default:
		w.returnClaim(delivery, "invalid processor disposition")
	}
}

func (w *Worker) returnClaim(delivery queue.Delivery, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = w.queue.Nack(ctx, queue.NackCommand{LeaseCommand: leaseCommand(delivery, w.now()), RetryAfter: w.config.RetryDelay, Reason: reason})
}

func leaseCommand(delivery queue.Delivery, at time.Time) queue.LeaseCommand {
	return queue.LeaseCommand{TenantID: delivery.TenantID, RunID: delivery.RunID, Owner: delivery.LeaseOwner, Token: delivery.LeaseToken, At: at.UTC()}
}

func traceID(traceParent string) string {
	parts := strings.Split(traceParent, "-")
	if len(parts) == 4 {
		return parts[1]
	}
	return ""
}

type discardWriter struct{}

func (discardWriter) Write(value []byte) (int, error) { return len(value), nil }

var _ interface {
	Name() string
	Start(context.Context) error
	Stop(context.Context) error
} = (*Worker)(nil)
