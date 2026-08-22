package log

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func decodeLine(t *testing.T, output string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &result); err != nil {
		t.Fatalf("decode log %q: %v", output, err)
	}
	return result
}

func TestCorrelationMerge(t *testing.T) {
	base := WithCorrelation(context.Background(), Correlation{
		TenantID: "tenant-a", RequestID: "request-a", TraceID: "trace-a",
	})
	ctx := WithCorrelation(base, Correlation{
		TenantID: " ", SessionID: " session-a ", TurnID: "turn-a", RunID: "run-a",
	})
	got := CorrelationFromContext(ctx)
	want := Correlation{
		TenantID: "tenant-a", RequestID: "request-a", TraceID: "trace-a",
		SessionID: "session-a", TurnID: "turn-a", RunID: "run-a",
	}
	if got != want {
		t.Fatalf("CorrelationFromContext() = %+v, want %+v", got, want)
	}
	if zero := CorrelationFromContext(nil); zero != (Correlation{}) {
		t.Fatalf("CorrelationFromContext(nil) = %+v", zero)
	}
	if got := CorrelationFromContext(WithCorrelation(nil, Correlation{RequestID: "request-b"})); got.RequestID != "request-b" {
		t.Fatalf("WithCorrelation(nil) = %+v", got)
	}
}

func TestJSONLoggerAddsCorrelationAndStaticFields(t *testing.T) {
	var output bytes.Buffer
	logger := NewJSON(&output, Options{Service: "agenticrag", Environment: "test", Version: "v1"})
	ctx := WithCorrelation(context.Background(), Correlation{
		TenantID: "ten_1", RequestID: "req_1", TraceID: "trace_1",
		SessionID: "ses_1", TurnID: "trn_1", RunID: "run_1",
	})
	logger.InfoContext(ctx, "run_started", "step_count", 2)
	entry := decodeLine(t, output.String())
	want := map[string]string{
		"msg": "run_started", "service": "agenticrag", "environment": "test", "version": "v1",
		FieldTenantID: "ten_1", FieldRequestID: "req_1", FieldTraceID: "trace_1",
		FieldSessionID: "ses_1", FieldTurnID: "trn_1", FieldRunID: "run_1",
	}
	for key, value := range want {
		if entry[key] != value {
			t.Errorf("entry[%q] = %v, want %q", key, entry[key], value)
		}
	}
	if entry["step_count"] != float64(2) {
		t.Errorf("step_count = %v", entry["step_count"])
	}
}

func TestReservedFieldsCannotBeSpoofed(t *testing.T) {
	var output bytes.Buffer
	logger := NewJSON(&output, Options{}).With(FieldTenantID, "logger-spoof")
	ctx := WithCorrelation(context.Background(), Correlation{TenantID: "trusted-tenant"})
	logger.InfoContext(ctx, "request_received", FieldTenantID, "record-spoof")
	entry := decodeLine(t, output.String())
	if entry[FieldTenantID] != "trusted-tenant" {
		t.Fatalf("tenant_id = %v", entry[FieldTenantID])
	}
	if count := strings.Count(output.String(), `"tenant_id"`); count != 1 {
		t.Fatalf("tenant_id key count = %d in %s", count, output.String())
	}
}

func TestSensitiveAttributesAreRedacted(t *testing.T) {
	var output bytes.Buffer
	logger := NewJSON(&output, Options{}).With("provider_api_key", "key-leak")
	logger.InfoContext(context.Background(), "model_called",
		"prompt", "prompt-leak",
		"prompt_hash", "safe-hash",
		slog.Group("tool", slog.String("arguments", "safe-name"), slog.String("password", "nested-leak")),
		"retrieval_content", "retrieval-leak",
	)
	raw := output.String()
	for _, leaked := range []string{"key-leak", "prompt-leak", "nested-leak", "retrieval-leak"} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("log leaked %q: %s", leaked, raw)
		}
	}
	entry := decodeLine(t, raw)
	if entry["provider_api_key"] != redacted || entry["prompt"] != redacted || entry["retrieval_content"] != redacted {
		t.Fatalf("top-level values were not redacted: %v", entry)
	}
	if entry["prompt_hash"] != "safe-hash" {
		t.Fatalf("safe metadata was redacted: %v", entry["prompt_hash"])
	}
	tool := entry["tool"].(map[string]any)
	if tool["password"] != redacted || tool["arguments"] != "safe-name" {
		t.Fatalf("nested redaction mismatch: %v", tool)
	}
}

func TestEmptyCorrelationOmittedAndLevelFiltered(t *testing.T) {
	var output bytes.Buffer
	logger := NewJSON(&output, Options{Level: slog.LevelWarn})
	logger.InfoContext(context.Background(), "filtered")
	logger.WarnContext(context.Background(), "visible")
	entry := decodeLine(t, output.String())
	if entry["msg"] != "visible" {
		t.Fatalf("msg = %v", entry["msg"])
	}
	for _, key := range []string{FieldTenantID, FieldRequestID, FieldTraceID, FieldSessionID, FieldTurnID, FieldRunID} {
		if _, exists := entry[key]; exists {
			t.Errorf("empty correlation field %q was emitted", key)
		}
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestConcurrentContextsDoNotLeak(t *testing.T) {
	const count = 100
	var output lockedBuffer
	logger := NewJSON(&output, Options{})
	var wait sync.WaitGroup
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			id := "req_" + strconv.Itoa(i)
			ctx := WithCorrelation(context.Background(), Correlation{RequestID: id})
			logger.InfoContext(ctx, "request_observed", "ordinal", i)
		}(i)
	}
	wait.Wait()

	seen := make(map[string]int, count)
	scanner := bufio.NewScanner(strings.NewReader(output.String()))
	for scanner.Scan() {
		entry := decodeLine(t, scanner.Text())
		requestID, ok := entry[FieldRequestID].(string)
		if !ok {
			t.Fatalf("missing request ID: %v", entry)
		}
		ordinal := int(entry["ordinal"].(float64))
		if requestID != "req_"+strconv.Itoa(ordinal) {
			t.Fatalf("cross-context log: request=%s ordinal=%d", requestID, ordinal)
		}
		seen[requestID]++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != count {
		t.Fatalf("observed %d unique requests, want %d; output bytes=%d", len(seen), count, len(output.String()))
	}
	for key, occurrences := range seen {
		if occurrences != 1 {
			t.Errorf("%s occurred %d times", key, occurrences)
		}
	}
}

func TestSensitiveKeyClassification(t *testing.T) {
	tests := []struct {
		key       string
		sensitive bool
	}{
		{"Authorization", true}, {"db.password", true}, {"provider-api-key", true},
		{"prompt_content", true}, {"tool_arguments", true}, {"content", true},
		{"prompt_hash", false}, {"prompt_bytes", false}, {"token_count", false}, {"artifact_id", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%t", tt.key, tt.sensitive), func(t *testing.T) {
			if got := isSensitive(tt.key); got != tt.sensitive {
				t.Fatalf("isSensitive(%q) = %t, want %t", tt.key, got, tt.sensitive)
			}
		})
	}
}
