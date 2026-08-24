package sse

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	platformredis "github.com/agenticrag/agenticrag/internal/platform/redis"
)

type fakeOutputClient struct {
	key, after, firstAfter string
	data                   []byte
	ttl                    time.Duration
	entries                []platformredis.StreamEntry
}

func (f *fakeOutputClient) StreamAppendNext(_ context.Context, key platformredis.Key, data []byte, ttl time.Duration) (string, error) {
	f.key, f.data, f.ttl = key.String(), append([]byte(nil), data...), ttl
	return "7-0", nil
}

func (f *fakeOutputClient) StreamRead(ctx context.Context, key platformredis.Key, after string, _ int64, _ time.Duration) ([]platformredis.StreamEntry, error) {
	f.key, f.after = key.String(), after
	if f.firstAfter == "" {
		f.firstAfter = after
	}
	if len(f.entries) > 0 {
		entries := f.entries
		f.entries = nil
		return entries, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRedisOutputBrokerUsesIndependentTransientStream(t *testing.T) {
	client := &fakeOutputClient{}
	keys, _ := platformredis.NewKeyBuilder("test")
	broker, err := NewRedisOutputBroker(client, keys, time.Hour, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	delta := OutputDelta{RunID: "run_0001", Text: "hello", OccurredAt: time.Now().UTC()}
	id, err := broker.PublishOutput(context.Background(), "ten_0001", delta)
	if err != nil {
		t.Fatal(err)
	}
	if id != "7-0" || client.key != "ar:test:{ten_0001}:events:output:run_0001" || client.ttl != time.Hour {
		t.Fatalf("id=%q key=%q ttl=%s", id, client.key, client.ttl)
	}
	var stored OutputDelta
	if json.Unmarshal(client.data, &stored) != nil || stored.Text != delta.Text {
		t.Fatalf("stored=%+v", stored)
	}
}

func TestRedisOutputBrokerResumesAfterTransientCursor(t *testing.T) {
	delta := OutputDelta{RunID: "run_0001", Text: "later", OccurredAt: time.Now().UTC()}
	data, _ := json.Marshal(delta)
	client := &fakeOutputClient{entries: []platformredis.StreamEntry{{ID: "8-0", Data: data}}}
	keys, _ := platformredis.NewKeyBuilder("test")
	broker, _ := NewRedisOutputBroker(client, keys, time.Hour, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	values, _ := broker.SubscribeOutput(ctx, "ten_0001", "run_0001", "7-0")
	entry := <-values
	if entry.ID != "8-0" || entry.Delta.Text != "later" || client.firstAfter != "7-0" {
		t.Fatalf("entry=%+v firstAfter=%q", entry, client.firstAfter)
	}
}

func TestOutputCursorIsIndependentFromDurableSequence(t *testing.T) {
	for _, value := range []string{"0-0", "1-0", "42-0"} {
		if !validOutputCursor(value) {
			t.Errorf("valid cursor rejected: %s", value)
		}
	}
	for _, value := range []string{"", "1", "01-0", "1-2", "d:1"} {
		if validOutputCursor(value) {
			t.Errorf("invalid cursor accepted: %s", value)
		}
	}
}
