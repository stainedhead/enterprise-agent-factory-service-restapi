# Implementation notes: review fixes

Each stream appends only inside its own section. Record, in order: decisions (with the Q or RD id), the
design review outcome per fix (checklist from `AGENTS.md`), gotchas, draft ADR or docs text for the
orchestrator to apply in Step 11, and follow-ups.

## Decisions taken at spec time (authorized by J)

| Id | Decision |
|---|---|
| Q1 | FR-R01 enforced in the use case only. A `-db` trigger for allowed digests is a recorded follow-up, not built |
| Q2 | FR-R05: bounded in-memory retry (10,000 entries, 10 minute window), counter and alert. No new table |
| Q3 | FR-R04: an Invoke family whose limit field is unknown is refused with 400 `ValidationError` |
| Q4 | FR-R16: the CD-7 mutations are listed as exempt in the contract (no new event types) |
| RD-1 | `AFS_MAX_BODY_BYTES` removed from the chart and docs |
| RD-2 | Idempotency key: `AFS_IDEMPOTENCY_HMAC_KEY`, `settings.IdempotencyHMACKey`, `httpapi.Config.IdempotencyHMACKey` |
| RD-3 | Metric names `afs_gateway_settle_failures_total` and `afs_gateway_usage_rows_dropped_total{reason}` |
| RD-4 | Recorder interfaces in `usecase`; `cmd/*` binds them to metrics |
| RD-5 | `acting.Deps.Logger` optional, nil falls back to `slog.Default()` |
| RD-6 | `wire.go` is D's; A edits `wire_restore.go`; E edits `wire_secrets.go` |
| Step 7 | The original spec was archived with M2-F8 and M6-F3 to M6-F6 open; see the archived dashboard |

## Stream A: Restore guardrails and audit identity

(append here)

## Stream B: Gateway spend integrity

(append here)

## Stream C: Observability

Worktree `/home/user/wt/restapi-rf-c`, branch `rf/c`, cut from 80db7e1. Authorized by J; no questions asked.

### Decisions

- **Recorder shape (RD-4).** `engine.Recorder` (new `engine/recorder.go`) has `ReconcileDone(kind, d, err)`, `ReconcileLag(kind, d)` and `DriftCorrected(kind)`. It is an optional `engine.Config.Recorder`. `notify.FailureRecorder` (`NotificationFailed(channelType)`) is an optional `Failures` field on `TestRunner`, `FailureDrainer` and `EmailDrainer`. `cmd/afs-reconciler/metrics_bind.go` (`reconcileMetrics`) binds both to `observability.Metrics`; `notifyDeps.Failures` carries it into `wireNotify`.
- **Lag definition.** Time from a desired-state change being queued (change feed or the stale-generation poll) to the first successful pass that applies it. The engine keeps `changedAt[key]` (first mark wins, so retries keep counting from the original change) and observes lag on success only. A full resync alone records no lag. A pass with `NoStatus` observes nothing.
- **Drift counter** increments once per corrected object, independent of whether a `DriftRecorder` is configured.
- **Sanitiser.** `condmsg.Sanitize` applies an injected scrubber, replaces ARNs with `<arn>` and bare 12-digit ids with `<account-id>`, and cuts to 512 bytes on a rune boundary. The scrubber is a package-level `atomic.Pointer` set by `SetScrubber` because usecase packages cannot import `observability`; `newReconcileMetrics` installs `observability.Scrub`. Call sites: `engine.writeFailure` (both the failing and the `Ready` condition), `tagsync` refused-tags condition, `lifecycle.next` (both the `InProgress` and error branches). The error itself is returned unchanged, so the engine's `reconcile failed` log line carries the full text through the redacting handler.
- **Dead-metric test** (`observability/deadmetrics_test.go`). Every `afs_*` name in `prometheusrule.yaml` and `dashboards/*.json` must be declared, be on the `writers` list, and have a selector use (`.Field.` or `.Method(`) in non-test source under `cmd/` or `internal/`. `deferredWriters` exempts the two gateway counters until stream B binds them; once a writer exists the test demands the entry be removed. Verified red on the tree before the fixes: four charted metrics had no writer (`afs_reconcile_errors_total`, `afs_reconcile_lag_seconds`, `afs_drift_corrected_total`, `afs_notification_failures_total`; `afs_reconcile_duration_seconds` is registered but not charted).
- **Stream B contract.** `Metrics.SettleFailed()` and `Metrics.UsageRowDropped(reason)` (or the fields `GatewaySettleFailures` and `GatewayUsageRowsDropped`) are the accessors. B-10 binds them in `cmd/afs-gateway` and deletes the two `deferredWriters` entries in `deadmetrics_test.go` (the allowed cross-stream edit; the `writers` entries already name both the accessors and the fields).
- **Chart.** New alert `AFSGatewaySettleFailures` (warning, `increase(...[15m]) > 0`) in group `afs-gateway`; dashboard panel 13 "Gateway settle failures and dropped usage rows" (`sum by (reason)` for drops).

### Design review (AGENTS.md checklist)

Dependency rule holds: engine and notify define ports, `observability` imports no usecase, `condmsg` imports only the standard library. No new routes. No secret reaches a condition or log: the scrubber runs before ARN removal. No mutations added. Context passed through. Exported identifiers documented. No account ids beyond the test constant 111122223333. No `api/openapi.yaml` change.

### Edits outside Stream C's Owns list (for the integrator)

- `deploy/helm/afs/tests/render_test.go` (added `AFSGatewaySettleFailures` to the expected alert list) and `deploy/helm/afs/tests/testdata/default.golden.yaml` (regenerated with `-update`): unavoidable consequence of the new alert and panel. Expect a trivial golden conflict with D's chart changes (`api-deployment.yaml` env); regenerate with `go test -update` after rebase.

### Docs text for the orchestrator (Step 11)

- `docs/technical-details.md`: Observability: reconcile duration, errors, lag and drift are recorded by the engine through `engine.Recorder`; notification failures through `notify.FailureRecorder`; a test fails the build when a charted metric has no writer. Condition messages are sanitised by `condmsg` (ARNs, 12-digit ids, scrubbers, 512 bytes); full errors go to the log.
- ADR line: condition text is untrusted upstream output; it is sanitised at the point it is stored, not at the API edge.

### Follow-ups

- Other `err.Error()` writes into conditions in controllers outside the three FR-R13 call sites (for example `notify` and per-kind controllers returning messages through `engine.WithCondition`) pass through `engine.writeFailure` and so are covered; controllers that build `domain.Condition` messages directly were not audited here.

## Stream D: afs-api runtime and configuration

(append here)

## Stream E: Secrets fingerprint, imports and contract

(append here)

## Orchestrator

(cross-stream requests and their resolution; merge log)

## Lessons learned

(append at completion)
