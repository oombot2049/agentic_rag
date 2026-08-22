package outbox

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingPublisher struct {
	mu     sync.Mutex
	events []Event
	fail   map[string]error
}

func (p *recordingPublisher) Publish(_ context.Context, event Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, cloneEvent(event))
	return p.fail[event.ID]
}

func TestRelayPublishesRetriesAndDeadLetters(t *testing.T) {
	t.Parallel()
	script := &sqlScript{
		queries: []scriptQuery{{rows: [][]driver.Value{
			eventRow("evt_0001", "PENDING", 1, "relay-a", int64(1), testTime.Add(time.Minute), nil, nil),
			eventRow("evt_0002", "PENDING", 2, "relay-a", int64(1), testTime.Add(time.Minute), nil, nil),
			eventRow("evt_0003", "PENDING", 3, "relay-a", int64(1), testTime.Add(time.Minute), nil, nil),
		}}},
		execs: []scriptExec{{affected: 1}, {affected: 1}, {affected: 1}},
	}
	publisher := &recordingPublisher{fail: map[string]error{"evt_0002": errors.New("temporary"), "evt_0003": errors.New("poison")}}
	relay := newTestRelay(t, openRepository(t, script), publisher)
	result, err := relay.ProcessBatch(context.Background(), "ten_0001", "relay-a")
	if err != nil || result != (RelayResult{Claimed: 3, Published: 1, Retried: 1, Dead: 1}) {
		t.Fatalf("ProcessBatch = %+v/%v", result, err)
	}
	if len(publisher.events) != 3 || publisher.events[0].ID != "evt_0001" {
		t.Fatalf("published events = %+v", publisher.events)
	}
	script.mu.Lock()
	defer script.mu.Unlock()
	if script.calls[2].args[4].Value != StatusPending || script.calls[3].args[4].Value != StatusDead {
		t.Fatalf("failure routing args = %+v / %+v", script.calls[2].args, script.calls[3].args)
	}
}

func TestRelayAckFailureMakesStableEventRedeliverable(t *testing.T) {
	t.Parallel()
	publisher := &recordingPublisher{fail: map[string]error{}}
	first := &sqlScript{queries: []scriptQuery{{rows: [][]driver.Value{
		eventRow("evt_0001", "PENDING", 1, "relay-a", int64(1), testTime.Add(time.Minute), nil, nil),
	}}}, execs: []scriptExec{{err: errors.New("database unavailable")}}}
	relay := newTestRelay(t, openRepository(t, first), publisher)
	if _, err := relay.ProcessBatch(context.Background(), "ten_0001", "relay-a"); err == nil {
		t.Fatal("acknowledgement failure was hidden")
	}
	second := &sqlScript{queries: []scriptQuery{{rows: [][]driver.Value{
		eventRow("evt_0001", "PENDING", 2, "relay-b", int64(2), testTime.Add(2*time.Minute), nil, nil),
	}}}, execs: []scriptExec{{affected: 1}}}
	relay = newTestRelay(t, openRepository(t, second), publisher)
	if _, err := relay.ProcessBatch(context.Background(), "ten_0001", "relay-b"); err != nil {
		t.Fatal(err)
	}
	if len(publisher.events) != 2 || publisher.events[0].ID != publisher.events[1].ID {
		t.Fatalf("stable duplicate identity lost: %+v", publisher.events)
	}
}

func TestRelayConfigurationBackoffAndErrorNormalization(t *testing.T) {
	t.Parallel()
	repository := openRepository(t, &sqlScript{})
	if _, err := NewRelay(nil, &recordingPublisher{}, RelayConfig{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil relay error = %v", err)
	}
	if _, err := NewRelay(repository, nil, RelayConfig{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil publisher error = %v", err)
	}
	if _, err := NewRelay(repository, &recordingPublisher{}, RelayConfig{BatchSize: 101}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad config error = %v", err)
	}
	relay := newTestRelay(t, repository, &recordingPublisher{})
	if got := relay.retryDelay(1); got != time.Second {
		t.Fatalf("first retry = %s", got)
	}
	if got := relay.retryDelay(20); got != time.Minute {
		t.Fatalf("capped retry = %s", got)
	}
	message := normalizeError(errors.New(strings.Repeat("界", 1000)))
	if len(message) > MaximumErrorLength || !strings.HasSuffix(message, "界") {
		t.Fatalf("normalized UTF-8 bytes = %d", len(message))
	}
}

func newTestRelay(t *testing.T, repository *Repository, publisher Publisher) *Relay {
	t.Helper()
	relay, err := NewRelay(repository, publisher, RelayConfig{
		BatchSize: 10, ClaimDuration: time.Minute, MaxAttempts: 3,
		RetryBase: time.Second, RetryMaximum: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return relay
}
