# Technical details

## Toolchain

Go 1.27.4, golangci-lint v2.15.0, govulncheck v1.8.0, PostgreSQL 17.

## Package dependency rule

`cmd/*` → `internal/adapter/*` → `internal/usecase` → `internal/domain`; `internal/authz` is imported by
`usecase` and `adapter/httpapi`. `domain` imports only the standard library.

## Configuration

Environment variables, prefixed `AFS_`. `afs-api`: `AFS_API_ADDR` (default `:8080`), `AFS_DATABASE_URL`,
`AFS_DATABASE_OWNER_URL`, `AFS_DB_IAM_AUTH` (RDS IAM tokens; needs `AFS_AWS_REGION`), `AFS_METRICS_ADDR`,
`AFS_API_REPLICAS`, `AFS_TRUSTED_PROXY_HOPS`, `AFS_PAGE_TOKEN_KEY` and `AFS_IDEMPOTENCY_HMAC_KEY` (both at least 32 bytes
and identical on every replica; with `AFS_API_REPLICAS` above 1 a missing or short key stops startup, with one replica a
random key is drawn). `afs-reconciler` adds `AFS_EVENT_BUS_NAME`, `AFS_CLUSTER_NAME`
and `AFS_RECONCILER_USERS`; see `user-docs/reconciler.md`.

## Request pipeline and idempotency

Outer: metrics (RED). Then authentication, rate limit, CORS, If-Match, authorization and the suspended-team
guard. Idempotency is not middleware: each mutating use case claims and completes its `Idempotency-Key` inside
its own transaction (`acting.Idem`), so a crash cannot leave a mutation without its stored response. Idempotency
fingerprints of secret values are HMAC-SHA-256 under `AFS_IDEMPOTENCY_HMAC_KEY`, never a bare hash. The per-route body
limit is fixed in the contract; there is no `AFS_MAX_BODY_BYTES` (the chart no longer sets it). The public `/readyz`
answers `unavailable` for any failing check and logs the cause with the request id; the operations port keeps the detail.
The listener sets `ReadHeaderTimeout` 10 s, `ReadTimeout` 60 s and `IdleTimeout` 120 s and no `WriteTimeout`;
`CreateImport` extends its own read deadline to 10 minutes. Settings are read once in `settingsFrom`; `os.Getenv` appears
only there and in `main.go` (a test enforces it). Denied authorizations are audited with `context.WithoutCancel`, and the
audit source IP comes from the metadata middleware, never from a client header.

## Reconciler

Engine (queue, backoff, leader-only jobs) with team environment, admission, lifecycle finalizer
(`team.Hooks.Deleting`) and housekeeping (outbox publisher, partitions, idempotency purge). Status goes to
`resource_status` through `StatusWriter`; `ConditionStore` adapts it for the finalizer.

Observability: reconcile duration, errors, lag and drift are recorded by the engine through `engine.Recorder`;
notification failures through `notify.FailureRecorder`; gateway settle failures and dropped usage rows through
`gateway.SettleRecorder` and `gateway.UsageDropRecorder`, bound to `observability.Metrics` in `cmd/*`
(`afs_gateway_settle_failures_total`, `afs_gateway_usage_rows_dropped_total{reason}`). A test fails the build when a metric
named in the alert rules or dashboards has no writer. Condition messages are sanitised by `condmsg` (ARNs become `<arn>`,
12-digit ids `<account-id>`, scrubbers applied, 512 bytes) at the point they are stored; full errors go to the log.

## Database

Schema and migrations come from `enterprise-agent-factory-service-db` (Go module, tagged releases).
Integration tests use `AFS_TEST_DATABASE_URL`. The module is pinned to -db 83f0345 (pseudo-version `v0.0.0-20261122071732-83f034507b16`, migrations 000001-000019; untagged: re-pin to the release tag);
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
scraped from a CronJob. `afs_billing` reads `service_settings` and `team_models` (-db 000018): price sync follows
`pricing.price_list_sync` and Bedrock lines are attributed by inference profile with a model id. The env override
`AFS_PRICING_PRICE_LIST_SYNC` was removed.

The export bundle builder runs in the reconciler as a leader-only job and is registered only when `AFS_EXPORT_BUCKET`
is set; it then also requires `AFS_EXPORT_KMS_KEY_ID` (SSE-KMS) and uses the shared S3 adapter. `afs-api` never links
S3. `CreateImport` (service admins, up to 100 MB, `400` over it) checks `schemaVersion` and the SHA-256 before any
write, applies one transaction per team and reports each team; `RestoreRevision` locks the team row and applies the
plan in one transaction. Restore reuses the agents guardrails: each agent and binding step passes `agents.Guard`
(`template guard`, `template guard (digest)`, `model guard`, `storage class guard`, `binding guard`), and a refused step is
skipped whole and listed in `skipped` as `refused by the <guard>: <reason>`. `restore.Service.Guards` is required (a nil
source fails the run); guard reads are memoised per restore.

Role links: the reconciler assumes the agent's own role (it trusts `AFS_RECONCILER_ROLE_ARN`) and that session assumes
the linked role with the external id; the agent role's policy grants `sts:AssumeRole` on each non-disabled link's exact
ARN. The reconciler's own role holds `sts:AssumeRole` and `sts:TagSession` on `role/afs/*` and
`organizations:ListAccounts` for the hourly account mirror (`AFS_ORG_MIRROR=off` disables it). The chart gives the
reconciler a ClusterRole for the kinds it renders and a Role for its leader Lease.

Tags: the effective set (system tags, service tags, team tags; cap 40) is applied to roles, Pod Identity associations,
inference profiles, snapshots and NodePools, and the keys AFS applied are recorded in `afs-tag-keys` so a removed tag
is removed and foreign tags are kept. Secrets and pool inbox queues are tagged at creation and then kept in step by
the `tag_sync` controller (M6-X2; `usecase/reconcile/tagsync`): engine kind and status kind `tag_sync`, id = team id, one
resource per active or suspended team (`postgres.NewTeamLister`). `postgres.TagSyncSource` reads the team's tags, its
secrets with their agent or pool owner and names, and its live pools; the controller computes `tags.Effective` per
resource, then `tags.Diff` and `tags.Stale`, and calls the `tagsync.Tagger` port: `secretsmanager.Tagger`
(`DescribeSecret`, `TagResource`, `UntagResource`; metadata only) and `sqs.QueueTagger` (`ListQueueTags`, `TagQueue`,
`UntagQueue`; URLs derived from `pool.QueueName`, nothing listed). Both map a missing resource (and a secret scheduled
for deletion) to `tagsync.ErrMissing`, which is skipped. Routes: `team_tags`, `secrets`, `agent_pools`, `agents` and
`service_tags` through the single `mergeRoutes`. The intake queue of work orders is platform-owned and not tagged here.

Viewer kubectl (M5-E): `GetTeamKubeconfig` renders a kubeconfig in `afs-api` from `AFS_CLUSTER_NAME`,
`AFS_CLUSTER_ENDPOINT`, `AFS_CLUSTER_CA_DATA` and `AFS_AWS_REGION` (`503` when unset); it uses the `aws eks get-token`
exec plugin and holds no token. Access is the reconciler's `kubectl` controller (kind `kubectl`, id = team id): per team
with `viewer_kubectl` on, a namespace `Role` and `RoleBinding` named `afs-viewer` (get, list and watch only), and one EKS
access entry per federated role (`AFS_VIEWER_KUBECTL_ROLES`, provider id or `default` to role ARN; unset means no access
entry), tagged `afs-managed-by=viewer-kubectl` and mapped to a group derived from the role ARN with no access policy.
The binding goes first on removal. An entry AFS did not create is never changed.

## Report rollups and M5 integration

Report rollups (-db 000018): the reconciler runs `afs.rollup_reports_recent(2)` every 10 minutes
(`report-rollup-recent`) and `(7)` nightly (`report-rollup-nightly`) as leader jobs, with
`afs_report_rollup_runs_total` and `afs_report_rollup_last_success_timestamp_seconds`. Activity and traffic reports
over more than two days read `afs.agent_activity_rollup_daily` and `afs.external_traffic_rollup_daily` at every level;
shorter ranges read the 5-minute tables, which are always current. Both paths give the same sums (a test compares
them). Service activity p99 is 28 ms and service traffic 54 ms at 20 teams x 10 agents over 90 days.

Pools: the pool controller re-renders on a pool row change (handoffs and secret bindings raise
`agent_pools.generation`) and on the change feed of the tables that have revision triggers (config values, role links,
opt-outs). Scaling changes replicas only. The reconciler may administer SQS queues named `afs-*`; team and agent roles
are bounded to `afs-*` queues. Restore rebuilds agent secret bindings (resource id `<team>/<agent>/<target>`).
The billing role reads `service_settings` and `team_models`; no env fallback remains.

## Work orders and workloads (M6)

- **Workload principal.** `afs-api` validates a projected ServiceAccount token offline against the cluster issuer's JWKS
  (`AFS_CLUSTER_OIDC_ISSUER`): audience exactly `afs-api`, life at most one hour, subject
  `system:serviceaccount:<ns>:<name>`. `afs.resolve_workload` maps it to team, agent or pool and worker pod. A workload
  holds only `work-orders:report`, bounded by `Principal.Workload.TeamID` equal to the assigned team; pool and pod
  are written to the audit record. It may omit `If-Match` on `UpdateWorkOrderRepo` (`middleware.RequireIfMatchFor`;
  the use case treats version 0 as the current version for a workload only), because it cannot read the version.
- **Work order reconciler** (`reconcile/workorder`): intake from the EventBridge queue (`AFS_WORK_ORDER_QUEUE_URL`,
  dedupe on `sourceEventId`), `Assigner` (`afs.instantiate_work_order_team`), `Secrets`, `Tracker` (GitHub via
  `AFS_GITHUB_APPS_FILE`) and `Notifier`.
- **Template secrets.** `spec.secrets` entries are `{pool, name, description?, delivery?, target?}` (validated in
  `domain.ValidateSwarmSpec`, resolved by `domain.TemplateSecretBinding`). The `Secrets` job creates each secret empty
  in Secrets Manager at `afs.secret_path(team, NULL, pool, name)` and records the returned ARN with -db 000019
  `afs.record_work_order_secret` (idempotent on team and name; SQLSTATEs mapped in `postgres.recordSecretError`).
- **Outbox payloads.** `housekeeping.Envelope` publishes the section 7 envelope `{id, occurredAt, teamId, poolId?, actor,
  data}` as stored (the API and -db 000019 write it) and wraps the flat payloads that `instantiate_work_order_team`
  wrote before 000019 (id `evt_` plus the outbox id zero-padded, stable across redelivery).
- **Handler registration.** Every `httpapi.Bind*` returns a `Registrar` and mutates nothing; `cmd/afs-api` collects them
  in `Config.Registrars`. `httpapi.New` returns an error naming any operation without a handler unless
  `Config.AllowUnbound`, which is set only without a database (health, readiness and the OpenAPI document) and in tests.
- **Not covered here.** Checks that need a cluster or an account (end-to-end run, restore drill, full-scale load,
  the ASVS checklist, `govulncheck` where the toolchain cannot be downloaded) run in CI and on the development
  cluster. `-db` is pinned to a pseudo-version until it is tagged.

## Collaboration edge (`afs-edge`, Paperclip)

- **Shape.** `cmd/afs-edge` is the composition root. `internal/usecase/edge` decides one request at a time (`Service.Handle`
  returns Forward, Respond or Redirect); `adapter/edgehttp` forwards, answers and tunnels WebSockets; `adapter/edgedb` calls
  the ten `afs_edge` functions; `adapter/edgesession` seals the sign-in state; `adapter/oidc.CodeFlow` runs code + PKCE and
  validates ID tokens and bearer tokens with the same key sets as `afs-api`; `adapter/edgelog` writes the records.
- **Decision order** (API contract 4.4c): host (apex or team, else 404) -> reserved paths -> class (cookie = web, header =
  API, both = 400) -> surface effective -> identify (session, API key, bearer token) -> `collab.Decide` over an `authz`
  principal -> limiter (20 rps, burst 40) -> body limits and scan. Positive decisions are cached 60 s, negative 10 s.
- **Route table.** Loaded from `afs.collab_routes()` and `afs.collab_settings()` at start, every 30 s and on each database
  notification; a table older than 5 minutes answers `503 upstream_unavailable` for every host.
- **Access counts.** Five-minute buckets keyed by team, surface, principal kind and id, outcome and service-admin entry are
  held in memory (at most 100,000 keys) and flushed every 30 s and at shutdown, 1,000 rows per call.
- **Reconciler side.** Jobs `collab-endpoint`, `collab-links`, `collab-denied-spike`, `collab-bootstrap`, `collab-partitions`,
  `collab-sessions` and `collab-waf` (`cmd/afs-reconciler/wire_collab.go`); the collaboration pod gets a Service named
  `paperclip` on 3100, ingress and egress policies keyed by the label `afs.stainedhead.io/paperclip-connected`, and a role
  policy limited to its own `/paperclip/` path plus one exact board key name. Admission policy
  `afs-protect-paperclip-service` keeps teams from publishing that port themselves.

- **Managed registration.** Jobs `collab-register` (1 min), `collab-probe` (1 min), `collab-guard` (10 min: roster guard and
  agent-write scan) and `collab-members` (5 min, only when `AFS_NOTIFY_EMAIL_FROM` is set), `cmd/afs-reconciler/wire_collab_managed.go`.
  The Paperclip client (`internal/adapter/paperclip`) calls the team's `paperclip.<namespace>.svc.cluster.local:3100` with the
  board key as a bearer token: `GET /api/health`, `GET /api/agents`, `POST /api/agents`, `POST /api/agents/{id}/pause|resume|revoke`,
  `GET /api/activity`, `GET|POST /api/invites`, `DELETE /api/invites/{id}`, `POST /api/members/deactivate`. The PRD does not
  specify this surface, so it follows the fake in `papertest`; it needs a check against a live Paperclip. A new agent's key goes
  straight to Secrets Manager (`<prefix>/<ns>/<agent>/paperclip-agent-api-key`) and `afs.record_collab_agent_key` records it;
  keys, the board key and invite URLs are the redacting `collab.Key` type and never reach a log, an event or an error.
  A connected worker whose key has no value starts without `PAPERCLIP_AGENT_API_KEY` and gets `CollaborationKeyMissing=True`.
