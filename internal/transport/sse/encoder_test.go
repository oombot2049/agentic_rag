package sse

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/events"
)

func TestEncoderUsesExactSingleLineSSEFraming(t *testing.T) {
	event, err := events.New("ten_0001", "trn_0001", "run_0001", 2, events.AssistantDelta,
		events.AssistantDeltaPayload{Text: "first\nsecond"}, time.Date(2026, 8, 23, 0, 0, 2, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(event)
	var output bytes.Buffer
	if err := writeEvent(&output, event); err != nil {
		t.Fatal(err)
	}
	want := "id: 2\nevent: assistant.delta\ndata: " + string(data) + "\n\n"
	if output.String() != want || strings.Contains(output.String(), "first\nsecond") {
		t.Fatalf("encoded=%q want=%q", output.String(), want)
	}
}

func TestEncoderWritesNonDurableRetryAndHeartbeatFrames(t *testing.T) {
	var output bytes.Buffer
	if err := writeRetry(&output, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := writeHeartbeat(&output, time.Date(2026, 8, 23, 1, 2, 3, 0, time.FixedZone("offset", 8*60*60))); err != nil {
		t.Fatal(err)
	}
	if output.String() != "retry: 3000\n\n: heartbeat 2026-08-22T17:02:03Z\n\n" {
		t.Fatalf("control frames=%q", output.String())
	}
}
