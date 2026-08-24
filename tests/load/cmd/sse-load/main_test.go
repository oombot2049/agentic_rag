package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPercentileUsesNearestRank(t *testing.T) {
	values := []time.Duration{4 * time.Millisecond, time.Millisecond, 3 * time.Millisecond, 2 * time.Millisecond}
	if got := percentile(values, .95); got != 4 {
		t.Fatalf("p95=%v", got)
	}
}

func TestRunValidatesArgumentsBeforeCredentials(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestConsumeMeasuresResumeAndAssistantTTFT(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher := writer.(http.Flusher)
		_, _ = writer.Write([]byte("id: 1\nevent: run.started\ndata: {}\n\n"))
		flusher.Flush()
		_, _ = writer.Write([]byte("id: 2\nevent: assistant.delta\ndata: {}\n\n"))
		flusher.Flush()
	}))
	defer server.Close()
	var c collector
	c.consume(context.Background(), server.Client(), server.URL, "secret", false)
	result := c.result("connections")
	if result.Attempted != 1 || result.Connected != 1 || result.Errors != 0 || len(c.ttft) != 1 {
		t.Fatalf("unexpected summary: %+v", result)
	}
}
