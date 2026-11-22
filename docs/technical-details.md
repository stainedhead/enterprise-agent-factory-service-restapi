# Technical details

## Toolchain

Go 1.27.4, golangci-lint v2.15.0, govulncheck v1.8.0, PostgreSQL 17.

## Package dependency rule

`cmd/*` → `internal/adapter/*` → `internal/usecase` → `internal/domain`; `internal/authz` is imported by
`usecase` and `adapter/httpapi`. `domain` imports only the standard library.

## Configuration

Environment variables, prefixed `AFS_`. `afs-api`: `AFS_API_ADDR` (default `:8080`), `AFS_DATABASE_URL`,
`AFS_DATABASE_OWNER_URL`, `AFS_DB_IAM_AUTH` (RDS IAM tokens; needs `AFS_AWS_REGION`), `AFS_METRICS_ADDR`,
`AFS_API_REPLICAS`, `AFS_TRUSTED_PROXY_HOPS`. `afs-reconciler` adds `AFS_EVENT_BUS_NAME`, `AFS_CLUSTER_NAME`
and `AFS_RECONCILER_USERS`; see `user-docs/reconciler.md`.

## Request pipeline and idempotency

Outer: metrics (RED). Then authentication, rate limit, CORS, If-Match, authorization and the suspended-team
guard. Idempotency is not middleware: each mutating use case claims and completes its `Idempotency-Key` inside
its own transaction (`acting.Idem`), so a crash cannot leave a mutation without its stored response.

## Reconciler

Engine (queue, backoff, leader-only jobs) with team environment, admission, lifecycle finalizer
(`team.Hooks.Deleting`) and housekeeping (outbox publisher, partitions, idempotency purge). Status goes to
`resource_status` through `StatusWriter`; `ConditionStore` adapts it for the finalizer.

## Database

Schema and migrations come from `enterprise-agent-factory-service-db` (Go module, tagged releases).
Integration tests use `AFS_TEST_DATABASE_URL`. The module is pinned to -db a6ecc3c (migrations 000001-000017);
`afs.schema_version()` is checked at startup. Envtest suites run with `-tags envtest` and `KUBEBUILDER_ASSETS`.

## Gateway (`afs-gateway`, M3)

Terminates TLS (cert-manager certificate, reloaded on change) and serves the Bedrock Runtime paths
(`converse`, `converse-stream`, `invoke`, `invoke-with-response-stream`) and `/anthropic/v1/messages`. The caller is
its source IP: a pod cache (pods, namespaces, Cilium endpoints, kept by watches) maps IP to pod to team, agent or
pool and worker pod; an unknown IP is `401`. `X-Afs-Gateway-Token` is optional and checked by TokenReview only when
sent (audience `afs-gateway`, cached 5 minutes); a TokenReview outage with a token present is `503`. SigV4 and
`x-api-key` are ignored and never logged. The gateway assumes the team's Bedrock role and calls the team's inference
profile.

Spend: `ensure_spend_counter`, then `reserve_spend_held(holder = pod)`; a refusal is `402 BudgetExceeded`; every
reservation is settled with the reserved budget and period (`settle_reservation`) on success, error or abort. A team
with no budget is `402`; a `notify` budget never blocks. The gateway fails closed: database or reservation trouble
is `503` within 2 s and nothing reaches Bedrock. `budget.threshold_crossed` and `budget.exhausted` are inserted once
in `budget_threshold_events` (exhausted is threshold 101). Caches (team, models, prices, budget, tag set) expire in
60 s and are dropped by `LISTEN afs_desired_state`.

Usage rows (`usage_events`) are written in batches (one `INSERT ... SELECT FROM unnest`, flushed within 500 ms;
`COPY` is refused under row-level security). Every row carries the team's current `tag_set_id`, and `pool_id` and
`worker_pod` for pool workers. A call is a row whose outcome is `ok` or `error`; `rejected_budget` and
`rejected_model` are not calls. Reports, the activity collector and (M4) `cost_daily.calls` all count calls by
this rule. No prompt or completion content is stored or logged.

Environment: `AFS_GATEWAY_ADDR`, `AFS_GATEWAY_TLS_CERT`, `AFS_GATEWAY_TLS_KEY`, `AFS_GATEWAY_ROLE_ARN` (required),
`AFS_BEDROCK_VPCE_URL`, `AFS_GATEWAY_MAX_BODY`, `AFS_GATEWAY_CACHE_TTL`, `AFS_GATEWAY_CILIUM_CHECK` (default on).
The chart grants its ServiceAccount a ClusterRole: get/list/watch on pods, namespaces and
`cilium.io/ciliumendpoints`, and create on `authentication.k8s.io/tokenreviews`.

## Budgets, notifications, reports (M3)

Budget API: `GetBudget`, `PutBudget` (amount at most the effective ceiling; `onExhaust: notify` needs
`teams:set-guardrails`). The budget reconciler runs as leader-only jobs: opens each period's counter by its first
second (`budget.reset` once, marked by the audit action `budget.period_opened`), carries `amount_usd` into
`limit_usd`, sweeps expired reservations every minute, expires ceiling overrides and applies the backstop
(`BudgetExhausted=True` plus the `afs-budget-exhausted` deny policy on the team's Bedrock role; removed on reset,
increase or override). The reconciler never emits `budget.exhausted`; only the gateway does. Gateway and reconciler
compute the period start the same way (UTC month, or `starts_at` for lifetime) and both use the idempotent
`ensure_spend_counter`.

Notification channels: one EventBridge rule per channel (`afs-<channel id>`), an API destination and connection for
Slack, webhook and Jira, an SQS target drained and sent with SES for email. Team-supplied URLs are only called
through `notifyhttp` (https, public addresses, no redirects). `notification.test_requested` is an extension event
(`domain.ExtensionEventEmitters`).

Reports: ranges over two days read daily tables, shorter ranges the 5-minute tables. Report rows are `groupBy`
columns followed by measures: usage `calls, inputTokens, outputTokens, costUsd`; costs `estimatedUsd, billedUsd`;
activity and traffic as documented on `Report.rows` in `api/openapi.yaml`. `tag.<key>` query parameters filter the
report operations only. The activity collector samples every running agent and pool worker per 5-minute window.

## Showback and scheduled reports (M4-E)

`port.ObjectStore` is the reports bucket (put with SSE-KMS, pre-signed GET, list); its S3 adapter
(`adapter/aws/s3`) is linked by `afs-billing` and the export bundle builder, never by `afs-api` (archtest).
`afs-api` stores report schedules (`usecase/reportschedules`, own-team channel rule, audit only); `afs-billing`
runs them (`usecase/billing/schedules`): one transaction per due schedule claims the row (`FOR UPDATE SKIP
LOCKED`), computes the report with the on-demand report's query rules and the single CSV renderer
`httpapi.ReportCSV`, stores `reports/<team>/<schedule>/<report>-<due instant>.csv`, publishes `report.delivered`
(`channelId`, `downloadUrl`; OQ-S2) and records the outcome through the four columns `afs_billing` may update. A
failure publishes `report.failed`, records the error and leaves `next_run_at`. Each recorded run bumps the row's
`version` (the touch trigger). A pre-signed URL made from temporary credentials stops working when those do, so its
real lifetime is the shorter of 7 days and the job's credential lifetime. The monthly showback
(`usecase/billing/showback`) reads `cost_daily` joined to the stamped tag set and refuses to write a CSV whose
dimension does not add up to the month's total.

## Billing jobs, export, links and tags (M4 integration)

`afs-billing --job <name>` runs one job per CronJob; jobs register themselves with `register(name, fn)` in
`cmd/afs-billing`: `cur-ingest` (03:00), `reconcile` (04:00), `price-sync` (Mondays 05:00), `report-schedules` (every 15
minutes) and `showback` (daily 06:00 UTC). The reconcile job logs each day as one structured line with
`metric="afs_cost_cur_variance_ratio"` (WARN when a team-day is over `billing.variance_alert_pct`) for a CloudWatch
Logs metric filter, and publishes `cost.variance_detected` once per team-day transition; the Prometheus gauge is not
scraped from a CronJob. `afs_billing` cannot read `service_settings` or `team_models` yet (-db X-DB-10): price sync
falls back to `AFS_PRICING_PRICE_LIST_SYNC`. A scheduled report's `version` rises each time it runs (the run state is
on the same row); clients re-read before an update.

The export bundle builder runs in the reconciler as a leader-only job and is registered only when `AFS_EXPORT_BUCKET`
is set; it then also requires `AFS_EXPORT_KMS_KEY_ID` (SSE-KMS) and uses the shared S3 adapter. `afs-api` never links
S3. `CreateImport` (service admins, up to 100 MB, `400` over it) checks `schemaVersion` and the SHA-256 before any
write, applies one transaction per team and reports each team; `RestoreRevision` locks the team row and applies the
plan in one transaction.

Role links: the reconciler assumes the agent's own role (it trusts `AFS_RECONCILER_ROLE_ARN`) and that session assumes
the linked role with the external id; the agent role's policy grants `sts:AssumeRole` on each non-disabled link's exact
ARN. The reconciler's own role holds `sts:AssumeRole` and `sts:TagSession` on `role/afs/*` and
`organizations:ListAccounts` for the hourly account mirror (`AFS_ORG_MIRROR=off` disables it). The chart gives the
reconciler a ClusterRole for the kinds it renders and a Role for its leader Lease.

Tags: the effective set (system tags, service tags, team tags; cap 40) is applied to roles, Pod Identity associations,
inference profiles, snapshots and NodePools, and the keys AFS applied are recorded in `afs-tag-keys` so a removed tag
is removed and foreign tags are kept. Secrets and work-order queues are tagged at creation only (M6 task).
