package quota

import (
	"context"
	"math"
	"sync"
	"time"
)

type receiptStatus uint8

const (
	receiptReserved receiptStatus = iota + 1
	receiptSettled
	receiptCompensated
)

type memoryReceipt struct {
	tokens       int64
	cost         int64
	actualTokens int64
	actualCost   int64
	burst        int64
	periodStart  time.Time
	expiresAt    time.Time
	status       receiptStatus
}

type memoryScope struct {
	available   float64
	last        time.Time
	periodStart time.Time
	usedTokens  int64
	usedCost    int64
	receipts    map[string]*memoryReceipt
}

// MemoryStore is a process-local Store intended for tests and single-instance
// development. Production deployments should use the Redis-backed store.
type MemoryStore struct {
	mu     sync.Mutex
	scopes map[string]*memoryScope
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{scopes: make(map[string]*memoryScope)}
}

func (m *MemoryStore) Reserve(
	ctx context.Context,
	scope string,
	id string,
	limits Limits,
	tokens int64,
	cost int64,
	at time.Time,
) (StoreDecision, error) {
	if ctx == nil || ctx.Err() != nil || scope == "" || !reservationPattern.MatchString(id) ||
		validateLimits(limits) != nil || tokens < 0 || cost < 0 || at.IsZero() {
		return StoreDecision{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.scopes[scope]
	if state == nil {
		state = &memoryScope{
			available:   float64(limits.Burst),
			last:        at,
			periodStart: periodBoundary(at, limits.Period),
			receipts:    make(map[string]*memoryReceipt),
		}
		m.scopes[scope] = state
	}
	if at.Before(state.last) {
		return StoreDecision{}, ErrInvalid
	}
	if existing := state.receipts[id]; existing != nil {
		if existing.tokens != tokens || existing.cost != cost || existing.status == receiptCompensated {
			return StoreDecision{}, ErrConflict
		}
		return StoreDecision{Allowed: true}, nil
	}

	boundary := periodBoundary(at, limits.Period)
	if !state.periodStart.Equal(boundary) {
		state.periodStart = boundary
		state.usedTokens = 0
		state.usedCost = 0
	}
	available := math.Min(
		float64(limits.Burst),
		state.available+at.Sub(state.last).Seconds()*float64(limits.RequestsPerSecond),
	)
	if available < 1 {
		retry := time.Duration(math.Ceil(
			(1 - available) / float64(limits.RequestsPerSecond) * float64(time.Second),
		))
		return StoreDecision{Reason: ReasonQPS, RetryAfter: retry}, nil
	}
	var active int64
	for _, receipt := range state.receipts {
		if receipt.status == receiptReserved && receipt.expiresAt.After(at) {
			active++
		}
	}
	if active >= limits.ActiveRuns {
		return StoreDecision{Reason: ReasonActiveRuns, RetryAfter: time.Second}, nil
	}
	if tokens > limits.PeriodTokens-state.usedTokens {
		return StoreDecision{Reason: ReasonTokens}, nil
	}
	if cost > limits.PeriodCostNanos-state.usedCost {
		return StoreDecision{Reason: ReasonCost}, nil
	}

	state.available = available - 1
	state.last = at
	state.usedTokens += tokens
	state.usedCost += cost
	state.receipts[id] = &memoryReceipt{
		tokens:      tokens,
		cost:        cost,
		burst:       limits.Burst,
		periodStart: boundary,
		expiresAt:   at.Add(limits.ReservationTTL),
		status:      receiptReserved,
	}
	return StoreDecision{Allowed: true}, nil
}

func (m *MemoryStore) Settle(
	ctx context.Context,
	scope string,
	id string,
	tokens int64,
	cost int64,
	at time.Time,
) error {
	if ctx == nil || ctx.Err() != nil || scope == "" || !reservationPattern.MatchString(id) ||
		tokens < 0 || cost < 0 || at.IsZero() {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.scopes[scope]
	if state == nil || state.receipts[id] == nil {
		return ErrNotFound
	}
	receipt := state.receipts[id]
	if tokens > receipt.tokens || cost > receipt.cost {
		return ErrInvalid
	}
	if receipt.status == receiptSettled {
		if receipt.actualTokens == tokens && receipt.actualCost == cost {
			return nil
		}
		return ErrConflict
	}
	if receipt.status != receiptReserved {
		return ErrConflict
	}
	if receipt.periodStart.Equal(state.periodStart) {
		state.usedTokens -= receipt.tokens - tokens
		state.usedCost -= receipt.cost - cost
	}
	receipt.status = receiptSettled
	receipt.actualTokens = tokens
	receipt.actualCost = cost
	return nil
}

func (m *MemoryStore) Compensate(ctx context.Context, scope string, id string, at time.Time) error {
	if ctx == nil || ctx.Err() != nil || scope == "" || !reservationPattern.MatchString(id) || at.IsZero() {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.scopes[scope]
	if state == nil || state.receipts[id] == nil {
		return ErrNotFound
	}
	receipt := state.receipts[id]
	if receipt.status == receiptCompensated {
		return nil
	}
	if receipt.status != receiptReserved {
		return ErrConflict
	}
	if receipt.periodStart.Equal(state.periodStart) {
		state.usedTokens -= receipt.tokens
		state.usedCost -= receipt.cost
	}
	state.available = math.Min(float64(receipt.burst), state.available+1)
	receipt.status = receiptCompensated
	return nil
}

func periodBoundary(at time.Time, period time.Duration) time.Time {
	nanos := at.UTC().UnixNano()
	return time.Unix(0, nanos-(nanos%period.Nanoseconds())).UTC()
}

var _ Store = (*MemoryStore)(nil)
