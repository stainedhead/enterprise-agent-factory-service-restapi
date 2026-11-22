package observability

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// LogConfig configures NewLogger.
type LogConfig struct {
	// Level is AFS_LOG_LEVEL: debug, info (default), warn or error.
	Level string
	// Service names the binary (afs-api, ...); it is added to every record.
	Service string
	// RequestID extracts the request id from a context (httpapi.RequestIDFrom); may be nil.
	RequestID func(context.Context) string
}

// ParseLevel maps AFS_LOG_LEVEL to a slog level; an unknown value is info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger returns a JSON logger on w whose every record passes through the redacting handler and
// carries the request id and trace ids of its context.
func NewLogger(w io.Writer, c LogConfig) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(c.Level)})
	l := slog.New(NewRedactingHandler(base, c.RequestID))
	if c.Service != "" {
		l = l.With("service", c.Service)
	}
	return l
}

func spanIDs(ctx context.Context) (string, string) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}
