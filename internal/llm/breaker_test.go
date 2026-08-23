package llm

import (
	"errors"
	"testing"
	"time"
)

func TestBreakerReopensAndAbandonedProbeDoesNotPoisonHealth(t *testing.T) {
	value := newBreaker(2, time.Minute)
	first, _ := value.allow(llmTestTime)
	value.fail(llmTestTime, true, first)
	second, _ := value.allow(llmTestTime)
	value.fail(llmTestTime, true, second)
	if _, err := value.allow(llmTestTime); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open error=%v", err)
	}

	probe, err := value.allow(llmTestTime.Add(time.Minute))
	if err != nil || !probe.probe {
		t.Fatalf("probe=%+v error=%v", probe, err)
	}
	value.fail(llmTestTime.Add(time.Minute), true, probe)
	snapshot := value.snapshot()
	if snapshot.State != BreakerOpen || !snapshot.OpenUntil.Equal(llmTestTime.Add(2*time.Minute)) {
		t.Fatalf("reopened=%+v", snapshot)
	}

	probe, err = value.allow(llmTestTime.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	value.abandon(probe)
	probe, err = value.allow(llmTestTime.Add(2 * time.Minute))
	if err != nil || !probe.probe {
		t.Fatalf("probe after abandonment=%+v error=%v", probe, err)
	}
	value.fail(llmTestTime.Add(2*time.Minute), false, probe)
	if snapshot = value.snapshot(); snapshot.State != BreakerClosed || snapshot.ConsecutiveFailures != 0 {
		t.Fatalf("non-relevant outcome=%+v", snapshot)
	}
}
