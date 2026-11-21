package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// Config is what every binary passes to Start. The composition root fills it from AFS_LOG_LEVEL,
// AFS_METRICS_ADDR and AFS_OTLP_ENDPOINT (EnvConfig).
type Config struct {
	Service     string
	Version     string
	LogLevel    string
	MetricsAddr string
	OTLP        string
	// LogOutput defaults to os.Stderr.
	LogOutput io.Writer
	// RequestID extracts the request id from a context; may be nil.
	RequestID func(context.Context) string
	// Checks are the readiness checks served on /readyz.
	Checks []Check
	// Metrics is the registry to serve; nil builds one. A composition root that records metrics while
	// it wires (the outbox lag, RED middleware) builds it first with NewMetrics and passes it here.
	Metrics *Metrics
}

// EnvConfig reads the shared environment variables for service.
func EnvConfig(service, version string, env func(string) string) Config {
	addr := env("AFS_METRICS_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	return Config{Service: service, Version: version, LogLevel: env("AFS_LOG_LEVEL"), MetricsAddr: addr, OTLP: env("AFS_OTLP_ENDPOINT")}
}

// Runtime is a binary's telemetry.
type Runtime struct {
	Log     *slog.Logger
	Metrics *Metrics
	// Shutdown flushes OpenTelemetry and stops the operations server.
	Shutdown func(context.Context) error
}

// Start builds the logger, the metrics registry and OpenTelemetry, and serves /metrics, /healthz and
// /readyz on MetricsAddr in the background until Shutdown or ctx ends. The logger is also made the
// default so library code that calls slog.Default() is redacted too.
func Start(ctx context.Context, c Config) (*Runtime, error) {
	out := c.LogOutput
	if out == nil {
		out = os.Stderr
	}
	log := NewLogger(out, LogConfig{Level: c.LogLevel, Service: c.Service, RequestID: c.RequestID})
	slog.SetDefault(log)
	m := c.Metrics
	if m == nil {
		m = NewMetrics(c.Service, c.Version)
	}
	shutdownOTel, err := SetupOTel(ctx, OTelConfig{Endpoint: c.OTLP, Service: c.Service, Version: c.Version})
	if err != nil {
		return nil, err
	}
	srv := OpsServer(c.MetricsAddr, m, c.Checks...)
	opsCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := ServeOps(opsCtx, srv); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("operations server stopped", "err", err)
		}
	}()
	return &Runtime{Log: log, Metrics: m, Shutdown: func(ctx context.Context) error {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
		return shutdownOTel(ctx)
	}}, nil
}
