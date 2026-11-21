# Logs, metrics, traces and health

Every binary (`afs-api`, `afs-reconciler`, `afs-gateway`, `afs-billing`) behaves the same way.

| Setting | Meaning |
|---|---|
| `AFS_LOG_LEVEL` | `debug`, `info` (default), `warn` or `error` |
| `AFS_METRICS_ADDR` | Address of the operations server, default `:9090` |
| `AFS_OTLP_ENDPOINT` | OTLP/HTTP base URL (for example `http://collector:4318`); empty turns trace and metric export off |

## Logs

One JSON object per line on stderr, with `service`, `requestId` for API requests, and `traceId` and
`spanId` inside a span. Secrets are redacted before they are written: the `Authorization` header and any
attribute named like a token, password or key; bearer tokens, `afsk_` API keys, `X-Afs-Gateway-Token`
values, JWT-shaped strings, AWS access key ids and private keys wherever they appear, including inside
error text.

## Operations server

Served on `AFS_METRICS_ADDR`, separate from the public listener:

- `GET /metrics`: Prometheus format.
- `GET /healthz`: liveness. Always `200` while the process runs; it checks no dependency.
- `GET /readyz`: readiness. `200`, or `503` listing the failing checks: `database`, `schema` (the database
  schema is not older than the one the binary was built for) and, for the reconciler, `leader`, which is
  informational: a standby replica is ready.

## Metrics, alerts and dashboards

The metric names are fixed (`afs_api_requests_total`, `afs_api_request_duration_seconds`,
`afs_reconcile_lag_seconds`, `afs_gateway_added_latency_seconds`, `afs_outbox_oldest_age_seconds`, and so on).
The Helm chart installs a `PrometheusRule` with one alert per service-level indicator and a Grafana
dashboard (`ConfigMap` labelled `grafana_dashboard`). Thresholds are under `prometheusRule.thresholds` in
the chart values; turn either off with `prometheusRule.enabled` and `dashboards.enabled`.
