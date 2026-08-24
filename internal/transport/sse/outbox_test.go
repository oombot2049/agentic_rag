package sse

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/events"
	"github.com/agenticrag/agenticrag/internal/outbox"
	platformredis "github.com/agenticrag/agenticrag/internal/platform/redis"
)

func TestOutboxPublisherValidatesAndPublishesRunEvent(t *testing.T) {
	client := &fakeStreamClient{}
	keys, _ := platformredis.NewKeyBuilder("test")
	broker, _ := NewRedisBroker(client, keys, time.Hour, time.Millisecond)
	publisher, _ := NewOutboxPublisher(broker)
	event, _ := events.New("ten_0001", "trn_0001", "run_0001", 4, events.RunStarted, events.EmptyPayload{}, time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC))
	payload, _ := json.Marshal(event)
	delivery := outbox.Event{TenantID: domain.TenantID("ten_0001"), Type: "RunEvent.v1", AggregateType: "run",
		AggregateID: "run_0001", Status: outbox.StatusPending, Payload: payload}
	if err := publisher.Publish(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	if client.id != "4-0" {
		t.Fatalf("stream ID=%s", client.id)
	}
	delivery.AggregateID = "run_9999"
	if err := publisher.Publish(context.Background(), delivery); err == nil {
		t.Fatal("aggregate mismatch accepted")
	}
}
