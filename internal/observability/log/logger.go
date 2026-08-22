package log

import (
	"io"
	"log/slog"
)

// Options configures process-level JSON logging. Empty static values are
// omitted. A nil Level defaults to info.
type Options struct {
	Level       slog.Leveler
	AddSource   bool
	Service     string
	Environment string
	Version     string
}

// NewJSON constructs a context-aware JSON logger.
func NewJSON(writer io.Writer, options Options) *slog.Logger {
	level := options.Level
	if level == nil {
		level = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
		AddSource: options.AddSource,
		Level:     level,
	})
	logger := slog.New(NewContextHandler(handler))
	attrs := make([]any, 0, 6)
	if options.Service != "" {
		attrs = append(attrs, "service", options.Service)
	}
	if options.Environment != "" {
		attrs = append(attrs, "environment", options.Environment)
	}
	if options.Version != "" {
		attrs = append(attrs, "version", options.Version)
	}
	return logger.With(attrs...)
}
