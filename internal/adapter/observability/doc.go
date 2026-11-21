// Package observability is the telemetry of every AFS binary: JSON logs with a request id and a
// redacting handler, OpenTelemetry traces and metrics over OTLP, a Prometheus registry with the
// metrics the SLI dashboards and alerts are written against (names.go), and the operations server
// that serves /metrics, /healthz and /readyz.
//
// It imports nothing from the other adapters: the request id comes from a function the composition
// root passes in (httpapi.RequestIDFrom), and readiness checks are plain functions.
package observability
