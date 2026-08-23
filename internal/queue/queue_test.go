package queue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/outbox"
)

var queueTestTime = time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)

func TestMessageAndTraceValidation(t *testing.T) {
	valid := testMessage()
	if err := validateMessage(valid); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Message){
		func(message *Message) { message.DeliveryID = "bad" },
		func(message *Message) { message.TenantID = "bad" },
		func(message *Message) { message.Payload = []byte(`[]`) },
		func(message *Message) { message.AvailableAt = message.CreatedAt.Add(-time.Second) },
		func(message *Message) {
			message.Trace.TraceParent = "00-00000000000000000000000000000000-1111111111111111-01"
		},
		func(message *Message) {
			message.Trace.TraceParent = "00-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA-2222222222222222-01"
		},
		func(message *Message) { message.Trace.TraceState = "vendor=value\n" },
	}
	for _, mutate := range mutations {
		message := valid
		mutate(&message)
		if err := validateMessage(message); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("validation error=%v for %+v", err, message)
		}
	}
}

type recordingEnqueuer struct {
	message Message
	err     error
}

func (r *recordingEnqueuer) Enqueue(_ context.Context, message Message) error {
	r.message = message
	return r.err
}

func TestRunQueuedAdapterPreservesPartitionIdentityAndTrace(t *testing.T) {
	target := &recordingEnqueuer{}
	adapter, _ := NewRunQueuedAdapter(target)
	payload := runQueuedPayload{SchemaVersion: 1, TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001",
		RunID: "run_0001", UserMessageID: "msg_0001", TurnSequence: 1, MaxSteps: 12,
		TraceParent: "00-11111111111111111111111111111111-2222222222222222-01", TraceState: "vendor=value"}
	data, _ := json.Marshal(payload)
	event := outbox.Event{TenantID: "ten_0001", ID: "evt_0001", AggregateType: "run", AggregateID: "run_0001",
		Type: "RunQueued.v1", Payload: data, Status: outbox.StatusPending, AvailableAt: queueTestTime, OccurredAt: queueTestTime}
	if err := adapter.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if target.message.TenantID != "ten_0001" || target.message.SessionID != "ses_0001" || target.message.RunID != "run_0001" ||
		target.message.Trace.TraceParent != payload.TraceParent || target.message.Trace.TraceState != payload.TraceState ||
		!strings.Contains(string(target.message.Payload), `"max_steps":12`) {
		t.Fatalf("message=%+v payload=%s", target.message, target.message.Payload)
	}
}

func TestRunQueuedAdapterRejectsForgedAndMalformedOutbox(t *testing.T) {
	target := &recordingEnqueuer{}
	adapter, _ := NewRunQueuedAdapter(target)
	valid := runQueuedPayload{SchemaVersion: 1, TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001",
		RunID: "run_0001", UserMessageID: "msg_0001", TurnSequence: 1, MaxSteps: 12}
	for _, mutate := range []func(*outbox.Event){
		func(event *outbox.Event) { event.TenantID = "ten_0002" },
		func(event *outbox.Event) { event.AggregateID = "run_9999" },
		func(event *outbox.Event) { event.Type = "Other.v1" },
		func(event *outbox.Event) { event.Status = outbox.StatusPublished },
		func(event *outbox.Event) { event.Payload = []byte(`{"unknown":true}`) },
	} {
		data, _ := json.Marshal(valid)
		event := outbox.Event{TenantID: "ten_0001", ID: "evt_0001", AggregateType: "run", AggregateID: "run_0001",
			Type: "RunQueued.v1", Payload: data, Status: outbox.StatusPending, AvailableAt: queueTestTime, OccurredAt: queueTestTime}
		mutate(&event)
		if err := adapter.Publish(context.Background(), event); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Publish error=%v event=%+v", err, event)
		}
	}
}

func TestPostgresAdapterConfigurationAndSQLContracts(t *testing.T) {
	if _, err := NewPostgresAdapter(nil, DefaultConfig()); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil transaction runner error=%v", err)
	}
	for name, sql := range map[string]string{"claim": claimSQL, "renew": renewSQL, "ack": ackSQL, "nack": nackSQL} {
		if !strings.Contains(sql, "lease_token") || name == "claim" && (!strings.Contains(sql, "FOR UPDATE SKIP LOCKED") || !strings.Contains(sql, "session_id")) {
			t.Errorf("%s SQL lost lease/partition contract: %s", name, sql)
		}
	}
}

func testMessage() Message {
	return Message{DeliveryID: "evt_0001", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001", RunID: "run_0001",
		Payload: []byte(`{"schema_version":1}`), Trace: TraceContext{TraceParent: "00-11111111111111111111111111111111-2222222222222222-01"},
		CreatedAt: queueTestTime, AvailableAt: queueTestTime}
}
