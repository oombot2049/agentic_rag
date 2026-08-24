package sse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/events"
	platformredis "github.com/agenticrag/agenticrag/internal/platform/redis"
)

type fakeStreamClient struct {
	key   string
	id    string
	data  []byte
	ttl   time.Duration
	reads []platformredis.StreamEntry
	err   error
}

func (f *fakeStreamClient) StreamAppend(_ context.Context, key platformredis.Key, id string, data []byte, ttl time.Duration) error {
	f.key, f.id, f.data, f.ttl = key.String(), id, append([]byte(nil), data...), ttl
	return f.err
}

func (f *fakeStreamClient) StreamRead(context.Context, platformredis.Key, string, int64, time.Duration) ([]platformredis.StreamEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	result := f.reads
	f.reads = nil
	return result, nil
}

func TestRedisBrokerPublishesTenantScopedExactSequence(t *testing.T) {
	client := &fakeStreamClient{}
	keys, _ := platformredis.NewKeyBuilder("test")
	broker, err := NewRedisBroker(client, keys, time.Hour, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	event := testEvent(t, 7, events.RunStarted)
	if err := broker.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if client.key != "ar:test:{ten_0001}:events:run:run_0001" || client.id != "7-0" || client.ttl != time.Hour {
		t.Fatalf("key=%q id=%q ttl=%s", client.key, client.id, client.ttl)
	}
}

func TestRedisBrokerRejectsCorruptLiveIdentityAndSurfacesFailure(t *testing.T) {
	client := &fakeStreamClient{reads: []platformredis.StreamEntry{{ID: "8-0", Data: mustMarshalEvent(t, testEvent(t, 7, events.RunStarted))}}}
	keys, _ := platformredis.NewKeyBuilder("test")
	broker, _ := NewRedisBroker(client, keys, time.Hour, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, failures := broker.Subscribe(ctx, "ten_0001", "run_0001", 6)
	select {
	case err := <-failures:
		if err == nil {
			t.Fatal("expected corrupt stream identity failure")
		}
	case <-time.After(time.Second):
		t.Fatal("broker did not surface corruption")
	}

	client.err = errors.New("Redis down")
	_, failures = broker.Subscribe(ctx, "ten_0001", "run_0001", 6)
	if err := <-failures; err == nil {
		t.Fatal("expected Redis read failure")
	}
}

func mustMarshalEvent(t *testing.T, event events.Event) []byte {
	t.Helper()
	data, err := event.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return data
}
