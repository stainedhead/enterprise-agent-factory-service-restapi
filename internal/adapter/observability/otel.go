package observability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// OTelConfig configures SetupOTel.
type OTelConfig struct {
	// Endpoint is AFS_OTLP_ENDPOINT, an OTLP/HTTP base URL such as http://collector:4318. Empty
	// disables export: the global providers stay no-ops.
	Endpoint string
	// Insecure sends over plain HTTP; the endpoint's scheme decides when it is false.
	Insecure bool
	Service  string
	Version  string
	// MetricInterval is the export interval; zero means 30 s.
	MetricInterval time.Duration
}

// SetupOTel installs the global tracer and meter providers exporting over OTLP/HTTP and returns a
// shutdown that flushes both. With no endpoint it installs nothing and the shutdown is a no-op.
func SetupOTel(ctx context.Context, c OTelConfig) (shutdown func(context.Context) error, err error) {
	if c.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", c.Service), attribute.String("service.version", c.Version)))
	if err != nil {
		return nil, fmt.Errorf("observability: otel resource: %w", err)
	}
	topts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(c.Endpoint + "/v1/traces")}
	mopts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(c.Endpoint + "/v1/metrics")}
	if c.Insecure {
		topts = append(topts, otlptracehttp.WithInsecure())
		mopts = append(mopts, otlpmetrichttp.WithInsecure())
	}
	te, err := otlptracehttp.New(ctx, topts...)
	if err != nil {
		return nil, fmt.Errorf("observability: trace exporter: %w", err)
	}
	me, err := otlpmetrichttp.New(ctx, mopts...)
	if err != nil {
		return nil, fmt.Errorf("observability: metric exporter: %w", err)
	}
	interval := c.MetricInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(te), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me, sdkmetric.WithInterval(interval))), sdkmetric.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}
