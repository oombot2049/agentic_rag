package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var eventTime = time.Date(2026, 8, 23, 0, 0, 3, 0, time.UTC)

func TestEveryKnownEventRoundTripsStrictContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		eventType Type
		payload   any
	}{
		{RunQueued, RunQueuedPayload{MaxSteps: 12}},
		{RunStarted, EmptyPayload{}},
		{StepStarted, StepStartedPayload{StepID: "stp_0001", StepNo: 1, Kind: domain.StepKindRetrieval}},
		{RetrievalCompleted, RetrievalCompletedPayload{StepID: "stp_0001", ResultCount: 5}},
		{ToolStarted, ToolStartedPayload{StepID: "stp_0001", ToolCallID: "tcl_0001", ToolName: "knowledge.search"}},
		{ToolCompleted, ToolCompletedPayload{StepID: "stp_0001", ToolCallID: "tcl_0001", Succeeded: true}},
		{AssistantDelta, AssistantDeltaPayload{Text: "Retention"}},
		{StepCompleted, StepCompletedPayload{StepID: "stp_0001", StepNo: 1, Status: domain.StepStatusSucceeded}},
		{RunCancelRequested, EmptyPayload{}},
		{RunCompleted, RunCompletedPayload{MessageID: "msg_0001"}},
		{RunFailed, RunReasonPayload{ReasonCode: "PROVIDER_UNAVAILABLE"}},
		{RunCancelled, EmptyPayload{}},
		{RunTimedOut, RunReasonPayload{ReasonCode: "RUN_DEADLINE_EXCEEDED"}},
	}
	for index, test := range cases {
		t.Run(test.eventType.String(), func(t *testing.T) {
			event, err := New("ten_0001", "trn_0001", "run_0001", uint64(index+1), test.eventType, test.payload, eventTime)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(raw, &keys); err != nil || len(keys) != 7 {
				t.Fatalf("wire keys=%v err=%v raw=%s", keys, err, raw)
			}
			for _, forbidden := range []string{"tenant_id", "prompt", "arguments", "provider_error", "chain_of_thought"} {
				if _, exists := keys[forbidden]; exists || strings.Contains(string(raw), forbidden) {
					t.Fatalf("wire contains forbidden field %q: %s", forbidden, raw)
				}
			}
			decoded, err := DecodeStrict("ten_0001", raw)
			if err != nil || !decoded.Known() || decoded.Type() != test.eventType || decoded.Sequence() != uint64(index+1) || decoded.SSEID() != fmt.Sprint(index+1) {
				t.Fatalf("decoded=%+v err=%v", decoded, err)
			}
			payload := decoded.Payload()
			payload[0] = '['
			if decoded.Payload()[0] != '{' {
				t.Fatal("Payload did not return a defensive copy")
			}
		})
	}
}

func TestEmissionRejectsWrongPayloadAndSensitiveShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		eventType Type
		payload   any
	}{
		{"unknown", "agent.thought", EmptyPayload{}},
		{"wrong type", RunQueued, EmptyPayload{}},
		{"steps", RunQueued, RunQueuedPayload{MaxSteps: 21}},
		{"step ID", StepStarted, StepStartedPayload{StepID: "bad", StepNo: 1, Kind: domain.StepKindTool}},
		{"step status", StepCompleted, StepCompletedPayload{StepID: "stp_0001", StepNo: 1, Status: domain.StepStatusRunning}},
		{"tool arguments shape", ToolStarted, map[string]any{"tool_name": "x", "arguments": map[string]any{"secret": true}}},
		{"delta empty", AssistantDelta, AssistantDeltaPayload{}},
		{"delta oversized", AssistantDelta, AssistantDeltaPayload{Text: strings.Repeat("x", maximumDeltaBytes+1)}},
		{"raw failure", RunFailed, map[string]string{"provider_error": "credential leaked"}},
		{"reason", RunFailed, RunReasonPayload{ReasonCode: "provider said secret"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New("ten_0001", "trn_0001", "run_0001", 1, test.eventType, test.payload, eventTime); err == nil {
				t.Fatal("New error = nil")
			}
		})
	}
	if _, err := New("ten_0001", "trn_0001", "run_0001", math.MaxInt64+1, RunStarted, EmptyPayload{}, eventTime); err == nil {
		t.Fatal("oversized sequence error = nil")
	}
}

func TestStrictAndCompatibleEvolution(t *testing.T) {
	t.Parallel()
	unknownType := []byte(`{"schema_version":1,"type":"citation.added","turn_id":"trn_0001","run_id":"run_0001","sequence":2,"occurred_at":"2026-08-23T00:00:03Z","payload":{"artifact_id":"art_0001"}}`)
	compatible, err := DecodeCompatible("ten_0001", unknownType)
	if err != nil || compatible.Known() || compatible.Type() != "citation.added" {
		t.Fatalf("compatible=%+v err=%v", compatible, err)
	}
	if _, err := DecodeStrict("ten_0001", unknownType); err == nil {
		t.Fatal("strict unknown type error = nil")
	}
	future := []byte(`{"schema_version":2,"type":"run.started","turn_id":"trn_0001","run_id":"run_0001","sequence":3,"occurred_at":"2026-08-23T00:00:03Z","payload":{"new_field":true},"optional":"future"}`)
	compatible, err = DecodeCompatible("ten_0001", future)
	if err != nil || compatible.Known() || compatible.SchemaVersion() != 2 {
		t.Fatalf("future=%+v err=%v", compatible, err)
	}
	if _, err := DecodeStrict("ten_0001", future); err == nil {
		t.Fatal("strict future error = nil")
	}
}

func TestDecodeRejectsDuplicateUnknownPrivateAndNoncanonicalFields(t *testing.T) {
	t.Parallel()
	valid := `"schema_version":1,"type":"assistant.delta","turn_id":"trn_0001","run_id":"run_0001","sequence":1,"occurred_at":"2026-08-23T00:00:03Z","payload":{"text":"x"}`
	cases := []string{
		`{` + valid + `,"sequence":2}`,
		`{"schema_version":1,"type":"assistant.delta","turn_id":"trn_0001","run_id":"run_0001","sequence":1,"occurred_at":"2026-08-23T00:00:03Z","payload":{"text":"x","text":"y"}}`,
		`{` + valid + `,"tenant_id":"ten_0001"}`,
		`{"schema_version":1,"type":"assistant.delta","turn_id":"trn_0001","run_id":"run_0001","sequence":1,"occurred_at":"2026-08-23T08:00:03+08:00","payload":{"text":"x"}}`,
		`{"schema_version":1,"type":"assistant.delta","turn_id":"trn_0001","run_id":"run_0001","sequence":1,"occurred_at":"2026-08-23T00:00:03Z","payload":{"text":"x","raw_prompt":"secret"}}`,
	}
	for _, raw := range cases {
		if _, err := DecodeStrict("ten_0001", []byte(raw)); err == nil {
			t.Errorf("DecodeStrict accepted %s", raw)
		}
	}
}

func TestSequenceTrackerGapsIdentityAndConcurrency(t *testing.T) {
	t.Parallel()
	tracker := NewSequenceTracker()
	first := mustEvent(t, "ten_0001", "trn_0001", "run_0001", 1)
	gap := mustEvent(t, "ten_0001", "trn_0001", "run_0001", 3)
	if err := tracker.Accept(first); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Accept(gap); err != nil {
		t.Fatalf("gap rejected: %v", err)
	}
	if err := tracker.Accept(gap); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("duplicate=%v", err)
	}
	backward := mustEvent(t, "ten_0001", "trn_0001", "run_0001", 2)
	if err := tracker.Accept(backward); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("backward=%v", err)
	}
	mixed := mustEvent(t, "ten_0002", "trn_0002", "run_0001", 4)
	if err := tracker.Accept(mixed); !errors.Is(err, ErrStreamIdentity) {
		t.Fatalf("mixed identity=%v", err)
	}
	tracker.Forget("run_0001")
	if err := tracker.Accept(mixed); err != nil {
		t.Fatalf("Accept after Forget=%v", err)
	}

	var group sync.WaitGroup
	for index := 0; index < 100; index++ {
		runID := domain.RunID(fmt.Sprintf("run_%04d", index+1000))
		event := mustEvent(t, "ten_0001", "trn_0001", runID, 1)
		group.Add(1)
		go func(event Event) {
			defer group.Done()
			if err := tracker.Accept(event); err != nil {
				t.Errorf("concurrent Accept=%v", err)
			}
		}(event)
	}
	group.Wait()
}

func mustEvent(t *testing.T, tenant domain.TenantID, turn domain.TurnID, run domain.RunID, sequence uint64) Event {
	t.Helper()
	event, err := New(tenant, turn, run, sequence, RunStarted, EmptyPayload{}, eventTime)
	if err != nil {
		t.Fatal(err)
	}
	return event
}
