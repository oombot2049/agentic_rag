package log

import (
	"context"
	"log/slog"
	"strings"
)

const redacted = "[REDACTED]"

type contextHandler struct {
	next slog.Handler
}

// NewContextHandler wraps a handler with correlation injection, reserved-field
// protection and sensitive-attribute redaction.
func NewContextHandler(next slog.Handler) slog.Handler {
	if next == nil {
		panic("log: nil handler")
	}
	return &contextHandler{next: next}
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		if !isReserved(attr.Key) {
			clean.AddAttrs(sanitizeAttr(attr))
		}
		return true
	})
	clean.AddAttrs(correlationAttrs(CorrelationFromContext(ctx))...)
	return h.next.Handle(ctx, clean)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		if !isReserved(attr.Key) {
			clean = append(clean, sanitizeAttr(attr))
		}
	}
	return &contextHandler{next: h.next.WithAttrs(clean)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{next: h.next.WithGroup(name)}
}

func correlationAttrs(c Correlation) []slog.Attr {
	pairs := [...]struct {
		key   string
		value string
	}{
		{FieldTenantID, c.TenantID},
		{FieldRequestID, c.RequestID},
		{FieldTraceID, c.TraceID},
		{FieldSessionID, c.SessionID},
		{FieldTurnID, c.TurnID},
		{FieldRunID, c.RunID},
	}
	attrs := make([]slog.Attr, 0, len(pairs))
	for _, pair := range pairs {
		if pair.value != "" {
			attrs = append(attrs, slog.String(pair.key, pair.value))
		}
	}
	return attrs
}

func sanitizeAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if isSensitive(attr.Key) {
		return slog.String(attr.Key, redacted)
	}
	if attr.Value.Kind() != slog.KindGroup {
		return attr
	}
	children := attr.Value.Group()
	clean := make([]slog.Attr, 0, len(children))
	for _, child := range children {
		if !isReserved(child.Key) {
			clean = append(clean, sanitizeAttr(child))
		}
	}
	attr.Value = slog.GroupValue(clean...)
	return attr
}

func isReserved(key string) bool {
	switch normalizeKey(key) {
	case FieldTenantID, FieldRequestID, FieldTraceID, FieldSessionID, FieldTurnID, FieldRunID:
		return true
	default:
		return false
	}
}

func isSensitive(key string) bool {
	normalized := normalizeKey(key)
	switch normalized {
	case "authorization", "cookie", "set_cookie", "password", "passwd",
		"secret", "api_key", "access_token", "refresh_token", "dsn",
		"prompt", "prompt_text", "prompt_content", "content", "message_content",
		"tool_arguments", "retrieval_content":
		return true
	}
	for _, suffix := range []string{"_password", "_passwd", "_secret", "_api_key", "_authorization", "_cookie", "_dsn"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func normalizeKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.NewReplacer("-", "_", ".", "_").Replace(key)
	return key
}
