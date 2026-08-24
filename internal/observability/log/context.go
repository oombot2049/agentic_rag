// Package log provides context-aware structured logging for AgenticRAG.
package log

import (
	"context"
	"strings"
)

const (
	FieldTenantID  = "tenant_id"
	FieldRequestID = "request_id"
	FieldTraceID   = "trace_id"
	FieldSessionID = "session_id"
	FieldTurnID    = "turn_id"
	FieldRunID     = "run_id"
)

// Correlation identifies the request and domain execution represented by a log
// record. Empty fields are omitted.
type Correlation struct {
	TenantID  string
	RequestID string
	TraceID   string
	SessionID string
	TurnID    string
	RunID     string
}

type correlationKey struct{}

// WithCorrelation returns a context whose non-empty values override existing
// correlation values. It does not mutate either input.
func WithCorrelation(ctx context.Context, update Correlation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	merged := CorrelationFromContext(ctx)
	merge := func(target *string, value string) {
		if value = strings.TrimSpace(value); value != "" {
			*target = value
		}
	}
	merge(&merged.TenantID, update.TenantID)
	merge(&merged.RequestID, update.RequestID)
	merge(&merged.TraceID, update.TraceID)
	merge(&merged.SessionID, update.SessionID)
	merge(&merged.TurnID, update.TurnID)
	merge(&merged.RunID, update.RunID)
	return context.WithValue(ctx, correlationKey{}, merged)
}

// CorrelationFromContext returns a copy of correlation state or the zero value.
func CorrelationFromContext(ctx context.Context) Correlation {
	if ctx == nil {
		return Correlation{}
	}
	correlation, _ := ctx.Value(correlationKey{}).(Correlation)
	return correlation
}
