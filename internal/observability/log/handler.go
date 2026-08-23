package log

import (
	"context"
	"log/slog"
	"strings"

	"github.com/agenticrag/agenticrag/internal/redaction"
)

const redacted = redaction.Replacement

var processRedactor = mustDefaultRedactor()

type contextHandler struct {
	next     slog.Handler
	redactor *redaction.Service
}

// NewContextHandler wraps a handler with correlation injection, reserved-field
// protection and sensitive-attribute redaction.
func NewContextHandler(next slog.Handler) slog.Handler {
	return NewContextHandlerWithRedactor(next, defaultRedactor())
}

// NewContextHandlerWithRedactor installs a tenant-aware redaction service.
func NewContextHandlerWithRedactor(next slog.Handler, redactorService *redaction.Service) slog.Handler {
	if next == nil {
		panic("log: nil handler")
	}
	if redactorService == nil {
		redactorService = defaultRedactor()
	}
	return &contextHandler{next: next, redactor: redactorService}
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, record slog.Record) error {
	correlation := CorrelationFromContext(ctx)
	view := h.redactor.View(ctx, correlation.TenantID)
	message := view.Redact(redaction.BoundaryLog, record.Message).Value
	clean := slog.NewRecord(record.Time, record.Level, message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		if !isReserved(attr.Key) {
			clean.AddAttrs(h.sanitizeAttr(view, attr))
		}
		return true
	})
	clean.AddAttrs(correlationAttrs(correlation)...)
	return h.next.Handle(ctx, clean)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attrs))
	view := h.redactor.View(context.Background(), "")
	for _, attr := range attrs {
		if !isReserved(attr.Key) {
			clean = append(clean, h.sanitizeAttr(view, attr))
		}
	}
	return &contextHandler{next: h.next.WithAttrs(clean), redactor: h.redactor}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	view := h.redactor.View(context.Background(), "")
	return &contextHandler{next: h.next.WithGroup(safeKey(view, name)), redactor: h.redactor}
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

func (h *contextHandler) sanitizeAttr(view redaction.View, attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if view.SensitiveKey(attr.Key) {
		return slog.String(attr.Key, redacted)
	}
	attr.Key = safeKey(view, attr.Key)
	switch attr.Value.Kind() {
	case slog.KindString:
		result := view.RedactField(redaction.BoundaryLog, attr.Key, attr.Value.String())
		return slog.String(attr.Key, result.Value)
	case slog.KindGroup:
		children := attr.Value.Group()
		clean := make([]slog.Attr, 0, len(children))
		for _, child := range children {
			if !isReserved(child.Key) {
				clean = append(clean, h.sanitizeAttr(view, child))
			}
		}
		attr.Value = slog.GroupValue(clean...)
		return attr
	case slog.KindAny:
		return slog.String(attr.Key, redacted)
	default:
		return attr
	}
}

func safeKey(view redaction.View, key string) string {
	result := view.Redact(redaction.BoundaryLog, key)
	if result.Value != key {
		return "redacted_field"
	}
	return key
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
	return defaultRedactor().SensitiveKey(context.Background(), "", key)
}

func defaultRedactor() *redaction.Service {
	return processRedactor
}

func mustDefaultRedactor() *redaction.Service {
	service, err := redaction.NewStatic(redaction.StrictPolicy())
	if err != nil {
		panic(err)
	}
	return service
}

func normalizeKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.NewReplacer("-", "_", ".", "_").Replace(key)
	return key
}
