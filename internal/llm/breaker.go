package llm

import (
	"sync"
	"time"
)

type BreakerState string

const (
	BreakerClosed   BreakerState = "CLOSED"
	BreakerOpen     BreakerState = "OPEN"
	BreakerHalfOpen BreakerState = "HALF_OPEN"
)

type BreakerSnapshot struct {
	State               BreakerState
	ConsecutiveFailures int
	OpenUntil           time.Time
	ProbeInFlight       bool
}

type breaker struct {
	mu                  sync.Mutex
	state               BreakerState
	consecutiveFailures int
	openUntil           time.Time
	probeInFlight       bool
	failureThreshold    int
	openDuration        time.Duration
}

type permit struct{ probe bool }

func newBreaker(threshold int, duration time.Duration) *breaker {
	return &breaker{state: BreakerClosed, failureThreshold: threshold, openDuration: duration}
}

func (b *breaker) allow(now time.Time) (permit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerClosed:
		return permit{}, nil
	case BreakerOpen:
		if now.Before(b.openUntil) {
			return permit{}, ErrCircuitOpen
		}
		b.state, b.probeInFlight = BreakerHalfOpen, true
		return permit{probe: true}, nil
	case BreakerHalfOpen:
		return permit{}, ErrCircuitOpen
	default:
		return permit{}, ErrCircuitOpen
	}
}

func (b *breaker) succeed() {
	b.mu.Lock()
	b.state, b.consecutiveFailures, b.openUntil, b.probeInFlight = BreakerClosed, 0, time.Time{}, false
	b.mu.Unlock()
}

func (b *breaker) fail(now time.Time, relevant bool, value permit) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !relevant {
		b.state, b.consecutiveFailures, b.openUntil, b.probeInFlight = BreakerClosed, 0, time.Time{}, false
		return
	}
	if value.probe || b.state == BreakerHalfOpen {
		b.state, b.openUntil, b.probeInFlight = BreakerOpen, now.Add(b.openDuration), false
		return
	}
	b.consecutiveFailures++
	if b.consecutiveFailures >= b.failureThreshold {
		b.state, b.openUntil, b.probeInFlight = BreakerOpen, now.Add(b.openDuration), false
	}
}

func (b *breaker) abandon(value permit) {
	if !value.probe {
		return
	}
	b.mu.Lock()
	if b.state == BreakerHalfOpen {
		b.state, b.openUntil, b.probeInFlight = BreakerOpen, time.Time{}, false
	}
	b.mu.Unlock()
}

func (b *breaker) snapshot() BreakerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BreakerSnapshot{State: b.state, ConsecutiveFailures: b.consecutiveFailures,
		OpenUntil: b.openUntil, ProbeInFlight: b.probeInFlight}
}
