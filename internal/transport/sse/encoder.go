// Package sse exposes a resumable Server-Sent Events gateway for Run events.
package sse

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/agenticrag/agenticrag/internal/events"
)

func writeEvent(writer io.Writer, event events.Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal SSE event: %w", err)
	}
	if _, err := fmt.Fprintf(writer, "id: %s\nevent: %s\ndata: %s\n\n", event.SSEID(), event.Type(), data); err != nil {
		return fmt.Errorf("write SSE event: %w", err)
	}
	return nil
}

func writeHeartbeat(writer io.Writer, now time.Time) error {
	_, err := fmt.Fprintf(writer, ": heartbeat %s\n\n", now.UTC().Format(time.RFC3339))
	return err
}

func writeRetry(writer io.Writer, delay time.Duration) error {
	_, err := fmt.Fprintf(writer, "retry: %d\n\n", delay.Milliseconds())
	return err
}
