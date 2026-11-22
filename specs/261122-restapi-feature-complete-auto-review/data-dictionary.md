# Data dictionary: review fixes

No table, column or migration changes (`-db` is untouched). These are the new or changed types, names and
settings the streams introduce. Names are fixed so streams agree without talking.

## Settings and environment (stream D defines; others consume)

| Name | Type | Default | Rule | FR |
|---|---|---|---|---|
| `AFS_PAGE_TOKEN_KEY` | secret, at least 32 bytes | none | Required when `AFS_API_REPLICAS > 1`; from a Kubernetes Secret via `secretKeyRef` | FR-R07 |
| `AFS_API_REPLICAS` | int | 1 | Read in `settingsFrom`; the chart sets it from `replicaCount` | FR-R07 |
| `AFS_HTTP_READ_TIMEOUT` | duration | 60s | afs-api `ReadTimeout` | FR-R09 |
| `AFS_HTTP_IDLE_TIMEOUT` | duration | 120s | afs-api `IdleTimeout` | FR-R09 |
| `AFS_IDEMPOTENCY_HMAC_KEY` | secret, at least 32 bytes | none | Required for secret idempotency; shared by replicas | FR-R14 |
| `AFS_AWS_ACCOUNT_ID`, `AFS_CLUSTER_*` | string | existing | Moved into `settings` and validated | FR-R11 |
| `AFS_MAX_BODY_BYTES` | removed | | Dead variable removed from chart and docs (RD-1) | FR-R11 |

## Go types and fields

| Name | Package | Owner | Purpose |
|---|---|---|---|
| `agents.Guard` (name may vary; one exported validator) | `internal/usecase/agents` | A | Template, digest, model, storage class and bindings checks shared by agents and restore |
| `restore.Skipped{Kind, ID, Reason}` | `internal/usecase/restore` | A | A planned agent step refused by a guard, listed in the response `skipped` |
| `acting.Deps.Logger *slog.Logger` | `internal/usecase/acting` | A | Optional; logs a failed denial write |
| `condmsg.Sanitize(msg string, scrub func(string) string) string` | `internal/usecase/reconcile/condmsg` | C | Strips ARNs and 12-digit account ids, applies scrubber, cuts to 512 bytes |
| `engine.Recorder` interface | `internal/usecase/reconcile/engine` | C | Duration and errors per kind, lag, drift corrected |
| `notify` failure recorder | `internal/usecase/reconcile/notify` | C | `afs_notification_failures_total{channel_type}` |
| `observability.Metrics.SettleFailures`, `.UsageRowsDropped{reason}` | `internal/adapter/observability` | C | RD-3 metrics |
| `gateway.SettleRecorder`, `gateway.settleQueue` | `internal/usecase/gateway` | B | Bounded retry queue, 10,000 entries, 10 minute window |
| `postgres.UsageWriter.Run(ctx)` and a drop recorder | `internal/adapter/postgres` | B | FR-R08 |
| `httpapi.Config.Areas []Area`, `AllowUnbound bool` | `internal/adapter/httpapi` | D | FR-R15 replaces the package-level `Areas` |
| `httpapi.Config.IdempotencyHMACKey []byte` | `internal/adapter/httpapi` | D | RD-2 |

## Metrics

| Metric | Labels | Writer | Reader |
|---|---|---|---|
| `afs_reconcile_duration_seconds` | `kind` | C engine | `prometheusrule.yaml:55`, dashboard |
| `afs_reconcile_errors_total` | `kind` | C engine | `prometheusrule.yaml:61`, dashboard |
| `afs_reconcile_lag_seconds` | `kind` | C engine | `prometheusrule.yaml:99`, dashboard |
| `afs_drift_corrected_total` | `kind` | C drift | dashboard |
| `afs_notification_failures_total` | `channel_type` | C notify | alert (new or existing) |
| `afs_gateway_settle_failures_total` | none | B settle queue | new alert in `prometheusrule.yaml` (C) |
| `afs_gateway_usage_rows_dropped_total` | `reason` = `queue_full`, `write_failed` | B usage writer | dashboard (C) |

## Fixed response bodies

- `/readyz` failing check on the API listener: `503` `{"status":"not ready","checks":{"database":"unavailable"}}`.
- Gateway uncappable Invoke family: `400` `ValidationError` naming the missing limit.
