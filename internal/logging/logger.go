// Package logging provides the structured logger and the request scoped
// correlation identifier used across HTTP handlers, services and workers.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// contextKey is the private key type of this package.
type contextKey struct{ name string }

var (
	requestIDKey = contextKey{name: "request_id"}
	loggerKey    = contextKey{name: "logger"}
)

// New builds a JSON logger for the given level.
func New(level string) *slog.Logger {
	var parsed slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parsed})
	return slog.New(handler)
}

// WithRequestID attaches a correlation identifier to the context.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, requestID)
}

// RequestID reads the correlation identifier of the context.
func RequestID(ctx context.Context) string {
	if value, ok := ctx.Value(requestIDKey).(string); ok {
		return value
	}
	return ""
}

// WithLogger stores a request scoped logger in the context.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerKey, logger)
}

// FromContext returns the request scoped logger, falling back to a discarding
// logger so library code never has to nil check.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.New(discardHandler{})
}

// discardHandler drops every record. It keeps tests silent by default.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
