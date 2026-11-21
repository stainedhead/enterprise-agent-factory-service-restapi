# Enterprise Agent Factory Service — Control plane PRD (`-restapi`)

| | |
|---|---|
| **Status** | Draft for review, feature-complete scope (2026-11-21); revised after the `review-prd` pass of 2026-11-21 (decisions D23–D46 of the root PRD §10.1) |
| **Author** | J (Enterprise Architecture), drafted with Claude |
| **Parent** | [Root PRD](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/enterprise-agent-factory-service-initial-PRD.md) (stories US-1..US-18 and the stories added after them); [solution design](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/solution-design.md) |
| **Provides** | [API contract](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/api-contract.md) → `api/openapi.yaml`; [gateway contract](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/gateway-contract.md) (`afs-gateway` ↔ agents, new, D28); EventBridge events (API contract §7) |
| **Consumes** | [Database contract](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/database-contract.md) at `-db` `v0.13.0` (schema 000013); the platform, AWS and third-party dependencies in §7 |

## 1. Purpose

The control plane: four Go binaries that turn people's intent into a running, isolated, metered
environment.

| Binary | Job |
|---|---|
| `afs-api` | The REST API and the security boundary: authentication, RBAC, validation, desired-state writes with audit and outbox, write-only secret values, reports, export and import requests, revision restore |
| `afs-reconciler` | Leader-elected. Applies desired state to EKS and AWS continuously, reverts drift, reports status; runs the activity and traffic collectors, the outbox publisher, partition maintenance and retention, spend periods and the reservation sweeper, notification delivery, log delivery and redaction, admission policies, volume snapshots, suspend, delete and ephemeral teardown, swarm pools and their queues, work order intake and tracking, export and import bundles |
| `afs-gateway` | The only path from agents to Bedrock (gateway contract). Authenticates the agent, reserves worst-case cost, invokes, settles, meters, tags and fails closed |
| `afs-billing` | Daily jobs: CUR ingestion and reconciliation with variance alerts, AWS Price List sync proposals, showback exports, cost allocation tag status; scheduled report delivery |

## 2. Scope

**In:** the four binaries, `api/openapi.yaml`, Helm chart and Terraform modules under `deploy/`,
runbooks in `user-docs/`. **Out:** the schema (owned by `-db`), clients (`-cli`, `-website`), the agent
runtime (Agentic Teammates), the shared named roles that account owners deploy in target accounts for
pools (for example by StackSet), and metering of model providers other than Bedrock (root EXT-6: their
calls bypass the gateway and are not metered, budgeted or capped). No search API in v1 (D44).

## 3. Architecture constraints

- Clean Architecture: `domain` ← `usecase` ← `adapter/*`, `cmd/*` is the composition root. `domain` and
  `usecase` import no HTTP, SQL, Kubernetes or AWS packages.
- RBAC is decided only in `internal/authz` (API contract §3). Every route declares its permission; a
  route without one fails a test.
- `afs-api` never calls the Kubernetes API and calls AWS only for Secrets Manager writes (create, put
  value, rotate, tag and delete secrets under `<service>/*`). It makes no STS, S3 or other AWS call
  (`AGENTS.md` is authoritative). Link verification, bundle storage and everything else is the
  reconciler's.
- Every mutation: row + audit + outbox in one transaction (database contract §5).
- Secrets never touch the database, logs, errors, traces or events.
- **One rule for 401, 403 and 404**, applied in this order by the authorization middleware:

| Caller and route | Result |
|---|---|
| Missing, expired or unknown credential | `401 Unauthenticated` |
| Team-scoped route (`/v1/teams/{teamId}/…`), or a team-owned resource reached by id (a work order), where the caller has no binding on that team; a team that does not exist gives the same answer | `404 NotFound` |
| Security operator (person or service-level key) on any team-scoped route, whether or not the team exists | `403 AccessDenied` |
| Bound to the team (including a workload principal on its own team) but lacking the permission | `403 AccessDenied` with `details.requiredPermission` |
| Service-scoped route (`/v1/admin/…`, `/v1/reports/…`, `/v1/work-orders`, `/v1/team-directory`) without the permission | `403 AccessDenied` with `details.requiredPermission` |

  Visibility is decided before permission, so a `403` never confirms that another team's resource exists.
  Security operators list a team's work orders with `ListWorkOrders?teamId=`, not `ListTeamWorkOrders`.
  Every `403` writes one audit record (actor, operation, team when known, required permission, outcome
  `denied`) in its own transaction; cross-team `404`s are counted in a security metric, not audited
  against the other team.

## 4. Requirements owned

Scope is feature complete (root ADR-014). Priorities P0 to P2 are build order, not optional scope.

| Id | Binary | Requirement | Product reqs | Pri |
|---|---|---|---|---|
| RA-1 | api | Implement every operation in the API contract §4 with its permission, conventions (§1) and errors (§6), including `ListTeamDirectory` (`GET /v1/team-directory`: id, slug and display name only, for security operators and service admins, D38); serve `api/openapi.yaml` at `/v1/openapi.yaml` | IF-1, ACC-3, AGT-5, EXT-1..4, EXT-7 | P0 |
| RA-1a | api | Replace the scaffold `internal/authz` with the API contract §3 catalog exactly: every permission and no other (the scaffold's `budgets:set-ceiling` goes; the ceiling is under `teams:set-guardrails`), roles `team_viewer`, `team_admin`, `service_admin`, `security_operator` (`work-orders:read`, `work-orders:write`, `swarm-templates:read`, D37) and the workload principal (D29). `Authorize(principal, permission, resource)` takes resource attributes so conditional grants are decided in one place: `teams:set-aws-accounts` when `mayManageAwsAccounts`, `teams:get-kubeconfig` when `viewerKubectl`, declaring external systems when `mayAddExternalSystems`, `work-orders:read` for work orders assigned to or touching a repository of the caller's team, `work-orders:report` on the assigned team only | ACC-2, ACC-3 | P0 |
| RA-2 | api | OIDC token validation for every enabled provider (JWKS cached 10 minutes and refetched on an unknown `kid`; issuer, audience and expiry checks); team-scoped group bindings only from the team's provider; API key auth by SHA-256 lookup, refusing expired keys and updating `lastUsedAt` at most once a minute | ACC-1, ACC-2, ACC-4 | P0 |
| RA-2b | api | Workload principal (D29): accept the projected ServiceAccount token with audience `afs-api` (mounted at `/var/run/secrets/afs/api/token`). `afs-api` validates it offline against the EKS cluster's OIDC issuer JWKS (issuer, audience, expiry at most 1 h), with no Kubernetes call, and maps namespace and ServiceAccount to the agent or pool through the database. The principal (`type: workload`, id = agent or pool id) holds only `work-orders:report` on work orders assigned to its team, and `GetMe` | WO-3 | P2 |
| RA-3 | api | `GET /v1/me`: principal (`type` `user`, `api_key` or `workload`), `serviceAdmin`, `securityOperator`, and per bound team the role and the effective `permissions[]` as decided by `authz`, including flag-dependent rights (D42) | ACC-3 | P0 |
| RA-4 | api | Session settings on every transaction (`afs.actor`, `afs.service_admin`, `afs.team_ids`, `afs.security_operator`); optimistic concurrency (`If-Match` on every PUT, PATCH and DELETE); idempotency keys for `api.idempotency_key_ttl_seconds` per the database contract §7.1, with the `CreateApiKey` response stored without `secretKey` (D35); request ids echoed in `X-Request-Id`; a change of team `kind` is `ValidationError` | CFG-1, IF-1 | P0 |
| RA-5 | api | Secret create, put-value and rotate write to Secrets Manager under `afs.secret_path`, then store the ARN; the value is held only for the call | SEC-1, SEC-2, SEC-4, SEC-5 | P0 |
| RA-6 | api | Reports: `/reports/{usage\|costs\|activity\|traffic}` at service, team and agent level, groupBy incl. `tag:<key>` (tags as of the record) and `pool`, CSV, forecast and `projectedCapAt` on budget positions; report schedules on the team's own channels, created only by team-admins and service admins (D23) | RPT-1..9, TAG-6 | P0/P1 |
| RA-7 | api | Price proposals: list, approve, reject (service admins), audit and events; an approved price is effective from `max(effectiveFrom, approvedAt)`, never earlier | BUD-9 | P1 |
| RA-8 | api | Ephemeral teams: create with expiry, teardown rule and lifetime budget in one request; stages, pools, handoffs, pool secrets, configuration, external systems and shared role links (API contract §4.4a). A pool role link must be inside the team's allowed accounts and use the pool's one role name | TEAM-7, AGT-8, BUD-8 | P2 |
| RA-9 | api | Security operators: service-level bindings and keys (service admins only; team-admins cannot add them); work orders and repositories (§4.10a); swarm templates (security operators list and describe them); team-scoped work order reads | ACC-2, ACC-4, WO-1..6 | P2 |
| RA-10 | api | Viewer kubectl: `GetTeamKubeconfig` when the team has it on; isolation tier and viewer kubectl as guardrails | ACC-5, TEAM-6 | P1/P2 |
| RA-11 | api | Cost allocation tag status for service admins (`ListCostTagStatus`); the activation banner is acknowledged through setting `billing.tag_activation_acknowledged` | TAG-7 | P2 |
| RA-12 | api | Export and import (`CreateExport`, `GetExport`, `CreateImport`): `afs-api` records the request in `export_bundles` and returns it `pending`; the reconciler builds or stages the bundle (RC-20). A dry-run import returns a create, update or skip plan and writes no desired state; an applied import writes the staged desired state through the normal transaction rules (row, audit, outbox per resource), maps account ids and leaves every role link `pending_trust` for re-verification | CFG-5 | P1 |
| RA-13 | api | `RestoreRevision` with `dryRun`: restore the team's desired state (team settings, agents, bindings, configuration values, external system switches, tags, budget) to a revision in one transaction. Dry run returns the diff and writes nothing. Secret values are never in revisions; a binding to a secret that no longer exists is skipped and reported | CFG-6 | P1 |
| RA-14 | api | Budget rules: amount at most the effective ceiling (`maxBudgetUsd`, or `ceilingOverrideUsd` while `ceilingOverrideExpiresAt` is in the future, set by service admins only, D34); `onExhaust: notify` only on persistent teams and only by service admins, ephemeral teams always block (D33); a lifetime budget cannot be removed and an ephemeral team cannot become persistent | BUD-1, BUD-3, BUD-8, TEAM-7 | P0 |
| RA-15 | api | Rate limits per principal: 20 req/s, burst 40, `429 Throttled` with `Retry-After`; each replica enforces its share of the limit. CORS only from the origins in setting `api.cors_origins` (D43) | IF-1, NFR | P0 |
| RA-16 | api | Settings (D45): `GetSettings` / `PutSetting` accept only the keys in the root PRD §6.13 settings table and the database contract, with type and range checks; an unknown key or bad value → `ValidationError`. §4.1 lists the keys this repository reads | CFG-1 | P0 |
| RC-1 | reconciler | Per team: namespace, Pod Security `restricted`, ResourceQuota, LimitRange, default-deny NetworkPolicies, Cilium FQDN egress per agent, team IAM role with permissions boundary, the per-team Bedrock role the gateway assumes (trusting only the gateway's role, allowed only to invoke the team's inference profiles), ESO SecretStore, Bedrock application inference profiles per enabled model. AWS names are by id (D25): IAM roles `afs-<ulid>` (the team's Bedrock role `afs-<team ulid>-bedrock`) under path `/afs/<namespace>/`; human names go in tags (`afs-team-slug`, `afs-agent-name`, `afs-pool-name`) | TEAM-2, TEAM-3, AGT-4, EXT-5 | P0 |
| RC-2 | reconciler | Per agent (and per dedicated node pool when the team's isolation tier is `dedicated`: a Karpenter NodePool with tainted, labelled and tagged nodes, and matching tolerations and selectors on the team's pods): Agentic Teammates custom resource rendering a StatefulSet (1 replica) with ServiceAccount, Pod Identity association, volume claim, ConfigMap (`envFrom` + `/etc/afs/config`), ExternalSecrets for bindings (env or file; a secret naming an external system that is off for the agent is not delivered), `/etc/afs/aws/config`, `/etc/afs/tags.json` and `AFS_TAGS`, the gateway token projection at `/var/run/secrets/afs/gateway/token` and env `AFS_GATEWAY_URL`, `AWS_ENDPOINT_URL_BEDROCK_RUNTIME` and `ANTHROPIC_BASE_URL` (D28), the `afs-api` token projection in teams with an assigned work order (D29), pod-template hash of referenced versions. Roles carry `afs-team-id` (and `afs-agent-id` or `afs-pool-id` for dedicated roles) as role tags, and IAM conditions use the built-in `aws:PrincipalTag/kubernetes-namespace` session tag (D26, OQ-1) | AGT-1..3, AGT-7, AGT-9, SEC-3, SEC-6, CFG-7, TAG-4, TEAM-6, EXT-7 | P0 |
| RC-3 | reconciler | Dedicated agent roles and cross-account links: `sts:AssumeRole` statements, link verification on create, daily and on demand, status write-back | AGT-6, AGT-10, TEAM-8 | P1 |
| RC-4 | reconciler | Change detection and drift (D30): `LISTEN afs_desired_state` (payload `<table>:<team_id>:<id>`, sent by the `-db` 000013 trigger) and reconcile the named resource; poll `generation <> observed_generation` every 30 s as a backstop and after any lost connection; full resync every 10 minutes and at startup. Watch owned objects, revert changes, audit `drift.corrected` | CFG-2 | P0 |
| RC-5 | reconciler | Status: phase, conditions, observed generation per resource into `resource_status`, using only the contract vocabulary (D24): `phase` Pending, Reconciling, Ready, Degraded, Failed, Deleting; budget exhaustion is condition `BudgetExhausted=True` on a Ready resource | TEAM-5 | P0 |
| RC-6 | reconciler | Budget backstop: IAM explicit deny on the team's Bedrock role within 60 s of exhaustion and condition `BudgetExhausted=True`; both removed within 60 s of a period reset, a budget increase or a ceiling override that makes room | BUD-3 | P0 |
| RC-7 | reconciler | Collectors: activity every 5 minutes; Hubble flows to `external_traffic` with denied reasons (connections and bytes; HTTP requests and errors only where an egress proxy exists, OQ-8); every row stamped with the team's current tag set and, for pools, `pool_id` and the worker pod; sign-ins to the team's log delivery only (RC-18; no table, no event); `egress.denied_spike` when an agent's denied flows exceed `egress.denied_spike_threshold` in 5 minutes | RPT-5, RPT-6, OPS-4, TAG-6 | P0/P1 |
| RC-8 | reconciler | Outbox publisher to EventBridge, at-least-once, ordered per aggregate, retrying with `attempts` and backoff; partitions created at least one month ahead; retention (D36): partitions of `usage_events`, `agent_activity` and `external_traffic` older than 13 months dropped with `afs.drop_partitions_older_than(13)`, `audit_log` kept online with no deletion (OQ-4); org account mirror hourly; idempotency key purge every 5 minutes | OPS-2, TEAM-8, IF-1 | P0 |
| RC-9 | reconciler | Tags: apply effective tags to every AWS resource it manages, add and remove on change; the effective set (system + service + team) is capped at 40 keys (D27, OQ-2) | TAG-1, TAG-4, TAG-5 | P1 |
| RC-10 | reconciler | Ephemeral teams: teardown when `teams_due_for_teardown` lists the team (expiry, cap, work order complete), through the RC-17 delete flow: stop agents and pools, revoke IAM, delete queues and namespace, record `torn_down_at` and reason, keep config, usage and audit; `team.teardown_started`, `team.torn_down` | TEAM-7, BUD-8 | P2 |
| RC-11 | reconciler | Pools: one Deployment per pool (workers 0–200) with the pool's ServiceAccount, Pod Identity role, secrets, ConfigMap, egress policy, scratch volume and optional EFS; shared named role in each linked account with verification. Queues (D32): one SQS queue per receiving pool (its inbox, named `afs-<team ulid>-<short hash>`, D25); the receiver gets `AFS_INBOX_QUEUE_URL`, each sender `AFS_HANDOFF_<TO_POOL_NAME_UPPER_SNAKE>_QUEUE_URL`; only the roles of pools with a handoff to it may send, only the receiver may receive | AGT-8 | P2 |
| RC-12 | reconciler | Work orders: drain the EventBridge intake queue (dedupe by event id); create the remediation team from the template (expiry from TTL, lifetime budget from the cap, rule `at_work_complete`); read PR state from GitHub every 10 minutes with a GitHub App installation token (OQ-5), using conditional requests within 5,000 requests per hour per installation; mark done, failed or cancelled; notify owners once per event and recipient through RC-15 (teams on their channels, outside contacts through `work_orders.notification_channel_id`); SLA warning `work_orders.sla_warning_hours` before due, and breach | WO-1..6 | P2 |
| RC-13 | reconciler | Viewer kubectl: EKS access entry for the team's provider role (OQ-9) mapped to a namespace-scoped read-only Role (no secrets, exec, attach, port-forward) when on, removed when off | ACC-5 | P2 |
| RC-14 | reconciler | Spend periods and reservations (D31, D34): at each period start call `afs.ensure_spend_counter(budget_id, period_start)` for every budget and emit `budget.reset`; carry a budget change into the counter's `limit_usd` within 60 s; every minute call `afs.release_expired_reservations()` (expiry `gateway.reservation_ttl_seconds`, default 900) and audit each release count; when a ceiling override expires, clear it and, if a budget exceeds the base ceiling, lower the budget to the ceiling and emit `budget.ceiling_override_expired` | BUD-1..4, BUD-8 | P0 |
| RC-15 | reconciler | Notification delivery (D46): reconcile each `notification_channels` row into an EventBridge rule filtered on the team id and the channel's event types, with an API destination (Slack, webhook, Jira; credentials in an EventBridge connection backed by Secrets Manager) or, for email, an SQS target that the reconciler drains and sends with SES. Removed with the channel. Delivery failures are retried and visible on the channel's status | OPS-3, BUD-4, WO-4 | P1 |
| RC-16 | reconciler | Agent volumes and snapshots: StatefulSet PVC retention `Retain` for scale-down and delete; online expansion when `sizeGiB` grows (patching the PVC); EBS snapshots through CSI `VolumeSnapshot` on the agent's schedule (daily or weekly) and on `CreateAgentSnapshot` requests, pruned to `snapshotRetain`, recorded in `agent_volume_snapshots`; a missing PVC (cluster rebuild) is re-created from the latest ready snapshot; snapshots carry the agent's tags | AGT-9, BUD-6, TAG-4 | P0 |
| RC-17 | reconciler | Lifecycle, following the root TEAM-9 lifecycle: agent pause (replicas 0, volume kept), resume and restart (pod-template annotation); pool pause and resume (replicas 0 and back to `workers`); team suspend (every agent and pool to 0 replicas, the gateway refuses the team's calls, volumes, IAM, secrets and data kept; `team.suspended`) and resume (each agent and pool back to its own prior state; `team.resumed`); the ordered delete finalizer for `DeleteTeam` and `DeleteAgent`: scale to zero, apply each agent's on-delete rule (final snapshot then delete, delete, or retain), delete pool volumes and queues, revoke IAM (Pod Identity associations, roles, link grants), delete inference profiles and the access entry, delete the namespace, mark the row `deleted`. Each step is idempotent and named in a condition while `Deleting`; a failed step is retried | TEAM-1, AGT-1, AGT-8, AGT-9 | P0 |
| RC-18 | reconciler | Log delivery and redaction: from `log_destinations`, render a per-team Fluent Bit aggregator (fed by the node DaemonSet, routed by namespace) with one output per destination (CloudWatch Logs, S3, OTLP, Splunk HEC, Datadog) and only that team's destination credentials. A redaction filter runs before every output and before traffic records are stored: credential-like values (passwords, bearer tokens, AWS keys, private keys, JWTs) and the values of the team's bound secrets become `[redacted:<first 8 hex of HMAC-SHA-256 with a per-team key>]`. The filter gets the bound secrets through ESO into a Secret only the aggregator reads; the reconciler renders references and never reads a value. Sign-in events (RC-7) go to the same outputs | OPS-1, OPS-4 | P1 |
| RC-19 | reconciler | Admission (TEAM-4): install and own ValidatingAdmissionPolicies and bindings (default engine, OQ-6) for `afs-t-*` namespaces: refuse NetworkPolicy and CiliumNetworkPolicy changes by anyone but the reconciler, LoadBalancer and NodePort Services, hostNetwork, hostPID, hostIPC, hostPath and privileged pods, changes to AFS-managed labels, and tolerations or node selectors for a dedicated node pool other than the namespace's own team's. Drift on the policies is reverted like any owned object | TEAM-4, TEAM-6 | P0 |
| RC-20 | reconciler | Bundles for RA-12: export writes a versioned, KMS-encrypted bundle to the export S3 bucket with its SHA-256 (all desired state per root CFG-5: configuration values, external systems catalog and switches, allowed-accounts filters, identities and role links with their external ids, latest snapshot references; no secret value, key hash or client secret, only ARNs and metadata); import reads the bundle, checks checksum and schema version, applies the account map and stores the plan and the staged content for `afs-api` to apply (RQ-1) | CFG-5 | P1 |
| GW-1 | gateway | Serve the gateway contract (D28): Bedrock Runtime–compatible `POST /model/{modelId}/converse`, `/converse-stream`, `/invoke`, `/invoke-with-response-stream` and Anthropic Messages–compatible `POST /anthropic/v1/messages`. Authenticate agents and pool workers by the projected ServiceAccount token (audience `afs-gateway`, Kubernetes TokenReview, result cached at most 60 s); resolve team, agent or pool and worker pod, and allowed models; a model not enabled → `403 AccessDenied`; a suspended team → `403 AccessDenied` | AGT-4, AGT-8 | P0 |
| GW-2 | gateway | Reserve worst-case cost before every call (D28, D31): `maxTokens` from the request, else the model's `max_output_tokens`, times the output price, plus input tokens estimated as request bytes ÷ 3 times the input price; `afs.ensure_spend_counter`, then `afs.reserve_spend_held(...)`; NULL → `402` `BudgetExceeded` (not retryable). Invoke through the team's inference profile with the team's Bedrock role; settle with `afs.settle_reservation(id, actual)`; write `usage_events` stamped with the team's current tag set (and pool and worker pod for pool workers), including rejected calls with their reason and the call's latency | BUD-2, BUD-5, TAG-6, RPT-3 | P0 |
| GW-3 | gateway | Fail closed: database or reservation unavailable → `503 Unavailable` within 2 s; no call goes to Bedrock without a reservation. Bedrock is reached over TLS through the VPC endpoint only | BUD-2, NFR | P0 |
| GW-4 | gateway | Thresholds: emit `budget.threshold_crossed` once per period per threshold (one row in `budget_threshold_events` decides), and `budget.exhausted` once per period on the first refusal at 100% | BUD-4 | P0 |
| GW-5 | gateway | Add the agent's effective tags as Bedrock `requestMetadata`, at most 16 pairs (D27): system tags first, then service fixed and required tags, then team tags in key order; dropped keys are recorded for the T13 page. Cache per agent; refresh on the `afs_desired_state` notification for the team's tags and at most 60 s after any change | TAG-1, TAG-4 | P1 |
| GW-6 | gateway | Streaming responses (ConverseStream, InvokeModelWithResponseStream, Messages SSE): tokens counted from the final usage block; a client abort or upstream error mid-stream settles on the tokens seen so far (output tokens counted from the streamed content when no usage block arrived) | BUD-2 | P0 |
| GW-7 | gateway | Reservations are held rows (`afs.spend_reservations`: id, budget, period, amount, holder pod, `expires_at` = now + `gateway.reservation_ttl_seconds`); every reservation is settled on success, error or abort; one a crashed replica leaves behind is released by the RC-14 sweeper | BUD-2 | P0 |
| BI-1 | billing | Daily CUR ingest: Bedrock by inference-profile tag, EKS split cost allocation by namespace, EBS volumes and snapshots by `afs-team-id`; write `cost_daily` | BUD-6 | P1 |
| BI-2 | billing | Reconcile gateway vs billed per team-day; flag above `billing.variance_alert_pct` (default 5); emit `cost.variance_detected` | BUD-6 | P1 |
| BI-3 | billing | Daily AWS Price List read when `pricing.price_list_sync` is on; supersede the old pending proposal and propose a changed price | BUD-9 | P1 |
| BI-4 | billing | Showback CSV by cost center and any tag, using the tags in effect when each cost happened | BUD-7, TAG-6 | P1 |
| BI-5 | billing | Report schedules: run due schedules, send CSV to the team's channel through the channel adapters of RC-15, record run status; `report.delivered` / `report.failed` | RPT-8 | P1 |
| BI-6 | billing | Cost allocation tag status: daily, record which keys CUR has seen and their AWS activation status; flag a key not seen 3 days after activation (`cost_tags.not_seen`) | TAG-7 | P2 |
| BI-7 | billing | `cost_daily` rows stamped with the tag set AWS billed with (CUR tags mapped to a tag set) and the pool for pool workers; a day with a tag change is split by the CUR line items' own tags | TAG-6, AGT-8 | P1 |
| DP-1 | deploy | Terraform: RDS PostgreSQL 17 with PITR (RPO 5 min), AWS Backup with cross-region and cross-account copies under KMS keys, the export bucket, the gateway's and reconciler's IAM roles; Helm: the four binaries with Pod Identity, at least 3 replicas of `afs-api` and `afs-gateway` across AZs | CFG-4, NFR | P0 |

### 4.1 Settings this repository reads

The root PRD §6.13 and the database contract hold the full settings table (D45); `-db` 000013 seeds the
new keys. This list names the reader of each key used here.

| Key | Default | Read by |
|---|---|---|
| `api.idempotency_key_ttl_seconds` | 3600 | api (RA-4), reconciler purge (RC-8) |
| `api.cors_origins` | `[]` | api (RA-15) |
| `teams.ephemeral_max_days` | 90 | api (RA-8) |
| `reports.idle_cpu_millicores` | 50 | reconciler activity collector (RC-7) |
| `egress.denied_spike_threshold` | 20 per 5 minutes | reconciler traffic collector (RC-7) |
| `gateway.reservation_ttl_seconds` | 900 | gateway (GW-7), reconciler sweeper (RC-14) |
| `billing.variance_alert_pct` | 5 | billing (BI-2) |
| `pricing.price_list_sync` | on | billing (BI-3) |
| `billing.tag_activation_acknowledged` | off | api (RA-11) |
| `work_orders.sla_warning_hours` | 48 | reconciler (RC-12) |
| `work_orders.notification_channel_id` | unset | reconciler (RC-12) |

## 5. User stories and acceptance criteria

Stories refine the product stories (US-n). Each acceptance criterion maps to at least one automated
test; the test level is in brackets: [unit]; [adapter] (real PostgreSQL or fakes); [envtest]
(Kubernetes API); [e2e] (dev cluster); [e2e restore drill] (a fresh dev cluster and a restored database);
[load] (the initial scale of §6 against the dev cluster with a fake Bedrock); [contract test] (the
OpenAPI document and examples checked against the API contract and replayed against the server).

### afs-api

**RA-US-1 · One enforcement point.** *(US-2, US-3)* As security, I want every route to declare a
permission and pass one authorization function.
- A table-driven test enumerates every route in the router and fails if one has no permission [unit].
- A test asserts that the `authz` permission catalog and role grants equal the API contract §3 exactly
  (generated from the contract table); `-website` runs the same check against `permission.ts` [unit].
- For every operation and each caller type (viewer, team-admin, service admin, security operator,
  team API key with each role, service-level key, workload principal, unbound user), the allowed caller
  gets 2xx and each denied caller gets the answer of the §3 rule: `404` when not bound to the team,
  `403 AccessDenied` with `details.requiredPermission` when bound or on a service route [adapter].
- A security operator gets `403` on every `/v1/teams/{teamId}/…` route, for an existing team and for one
  that does not exist alike [adapter].
- Conditional grants: `teams:set-aws-accounts` allowed for a team-admin only when `mayManageAwsAccounts`,
  `teams:get-kubeconfig` only when `viewerKubectl`, `DeclareTeamExternalSystem` only when
  `mayAddExternalSystems`; each has an allowed and a denied case [unit].
- A team-scoped route takes `teamId` from the path only; a body `teamId` that differs → `ValidationError`
  [unit].
- Every `403` writes one audit record with outcome `denied` and the required permission; the request's
  own transaction writes nothing [adapter].

**RA-US-2 · Sign-in through the right provider.** *(US-1, US-2)* As a team member, I want to sign in
through my team's provider and get my roles.
- A token from an enabled provider with a valid signature, issuer, audience and expiry is accepted; a
  disabled provider's token → `401` [unit].
- A group binding on team A applies only when the token's issuer is team A's provider (or the default
  when A has none) [unit].
- JWKS are cached for 10 minutes; a token signed with a new key (unknown `kid`) triggers one refetch and
  is accepted; a key removed from the JWKS stops validating after the refetch [unit with fake JWKS].
- An expired or deleted API key → `401`; a valid key's `lastUsedAt` advances [adapter].
- `GET /v1/identity-providers` works without a token and returns no client secret ARN [adapter].

**RA-US-2b · Workloads report progress.** *(US-16)*
- A pool's `afs-api` token from team A, assigned a work order, can `UpdateWorkOrderRepo` on that work
  order and call `GetMe` [adapter].
- The same token gets `404` on a work order not assigned to team A and on every route of team B, and
  `403` on every other route of team A and on service routes [adapter].
- A token with audience `afs-gateway`, an expired token or one from another cluster's issuer → `401`
  [unit].

**RA-US-3 · Create a team end to end.** *(US-1)*
- `CreateTeam` with required tags, models, provider and first team-admin writes all rows, one audit
  record and one `team.created` event in one transaction; any failure writes none [adapter].
- Missing required tags → `400 ValidationError` with `details.missingTags` [adapter].
- Repeating the request with the same `Idempotency-Key` returns the first response; a different body
  → `409` [adapter].
- A team-admin or viewer calling `CreateTeam` → `403` and one denied audit record [adapter].

**RA-US-3a · Who am I.** *(US-2, ACC-3)*
- `GetMe` for a viewer, a team-admin, a service admin, a security operator, a team API key and a
  workload returns the principal type, `serviceAdmin`, `securityOperator` and, per bound team, exactly
  the permissions `authz` grants, including `teams:get-kubeconfig` only on teams with `viewerKubectl`
  and `teams:set-aws-accounts` only with `mayManageAwsAccounts` [adapter].
- A service admin gets `serviceAdmin: true` and the full list; a security operator gets
  `securityOperator: true` with `work-orders:read`, `work-orders:write` and `swarm-templates:read` and no
  team [adapter].

**RA-US-4 · Agents.** *(US-4)*
- `CreateAgent` refuses an image without `@sha256:` [unit], a model the team has not enabled, a storage
  class not offered, and a binding of another agent's secret [adapter].
- `UpdateAgent` refuses shrinking the volume or changing its storage class [unit].
- No request or response field is a replica count [contract test].

**RA-US-5 · Write-only secrets.** *(US-5, US-6)*
- `CreateSecret` calls Secrets Manager `CreateSecret` with the name from `afs.secret_path` and tags
  `afs-team-id` (and `afs-agent-id` for agent scope), then stores only the ARN [adapter with fake AWS].
- If Secrets Manager fails, no row is written and the error is `503 Unavailable` without the value
  [adapter].
- A fuzz test sends values through every secret route and asserts the value never appears in any
  response body, header, log line, trace attribute, audit record or event [adapter].
- The `afs-api` binary's AWS client is built with Secrets Manager only; a test fails if any other AWS
  service client is constructed in `cmd/afs-api` [unit].

**RA-US-6 · Optimistic concurrency and sessions.** *(US-13)*
- Update without `If-Match` → `428`; with a stale version → `412`; with the current version → 200 and
  `ETag` advanced by one [adapter].
- A transaction opened without the session settings sees no team rows [adapter].
- Every response carries `X-Request-Id`; a client-sent id of at most 64 characters is echoed and appears
  in the error body and the audit record [adapter].
- Changing a team's `kind` → `400 ValidationError` [adapter].

**RA-US-7 · Reports.** *(US-11, US-12)*
- Each report kind at each level returns the same totals as summing the underlying table for the range
  [adapter].
- Ranges over two days read daily views; shorter ranges read 5-minute tables; p99 < 1 s for 90 days at
  the initial scale [load].
- `Accept: text/csv` returns the same rows as JSON [adapter].
- `groupBy=tag:business-unit` returns one row per value plus an untagged row [adapter].
- The budget position returns `forecastUsd` (linear on the period's spend so far) and `projectedCapAt`
  (when the forecast reaches the budget, absent when it does not within the period) [unit].
- A viewer of team A gets `404` for team B's reports and `403` for service reports [adapter].

**RA-US-7a · Report schedules.** *(US-11, RPT-8)*
- A team-admin creates a weekly schedule on the team's own Slack channel; a service-level channel or
  another team's channel → `ValidationError` [adapter].
- A viewer creating or changing a schedule → `403` (D23) [adapter].

**RA-US-8 · Approve prices.** *(US-10)*
- Only service admins can approve or reject; approval publishes `pricing.proposal_decided` and the
  gateway uses the new price for calls after its effective time [adapter, e2e].
- The effective time is `max(effectiveFrom, approvedAt)`: approving a proposal whose `effectiveFrom` is in
  the past applies it from the approval, and no usage row before it is repriced [adapter].

**RA-US-9 · Retries are safe.** *(IF-1)*
- A create repeated with the same key, operation and body within the TTL returns the stored response with
  `Idempotent-Replayed: true` and writes nothing new; a different body → `409` with
  `details.reason = idempotency_mismatch`; a concurrent duplicate waits or gets `409
  idempotency_in_progress`; a 5xx is not stored [adapter].
- Keys are per principal: the same key from another caller is a new request [adapter].
- A replayed `CreateApiKey` returns the key's metadata without `secretKey`, and no stored response
  contains `secretKey` [adapter].

**RA-US-10 · Ephemeral teams and pools.** *(US-15, US-17)*
- `CreateTeam` with `kind: ephemeral` and no expiry, rule or lifetime budget → `400`; an expiry beyond
  `teams.ephemeral_max_days` → `400`; with them, one transaction writes team and budget [adapter].
- Making an ephemeral team persistent or removing its lifetime budget → `400 ValidationError` [adapter].
- Pool operations on a persistent team → `409`; a pool named like an existing agent → `409`; `ScalePool`
  outside 0–200 → `400` [adapter].
- A pool role link in an account outside the team's allowed accounts, or with a role name other than the
  pool's, → `400 ValidationError` [adapter].
- Viewers can read pools and get `403` on every pool mutation [adapter].

**RA-US-11 · Work orders for security.** *(US-16)*
- A security operator (person or service-level key) can create, update, cancel and list all work
  orders, filter them by `teamId`, list and describe swarm templates and use `ListTeamDirectory`; it gets
  `403` on creating or changing a template and on any team route [adapter].
- A second open work order for the same CVE → `409`; a repository without an owner → `400` [adapter].
- A team-admin sees work orders assigned to the team or touching its repositories, read-only, through
  `ListWorkOrders` and `ListTeamWorkOrders`; an unrelated team gets `404` [adapter].
- The assigned team's agents and pools can update repository status with `work-orders:report`; setting
  `pr_opened` without a PR link → `400` [adapter].
- A team-admin adding a security operator binding → `403`; only service admins can [adapter].

**RA-US-12 · Guardrails stay with service admins.** *(US-18, ACC-5, TEAM-6)*
- A team-admin changing `isolationTier` or `viewerKubectl` → `403`; `GetTeamKubeconfig` with it off →
  `403`, with it on returns a kubeconfig that uses an exec credential plugin and contains no token
  [adapter].

**RA-US-13 · Budgets within the ceiling.** *(US-9, BUD-1, D33, D34)*
- A team-admin setting a budget above the ceiling → `400 ValidationError` [adapter].
- A service admin sets `ceilingOverrideUsd` with an expiry; while it is unexpired a team-admin can set a
  budget up to the override; a team-admin setting the override → `403` [adapter].
- `onExhaust: notify` on an ephemeral team → `400`; on a persistent team by a team-admin → `403`; by a
  service admin → 200 [adapter].

**RA-US-14 · Export, import and restore.** *(US-13, CFG-5, CFG-6)*
- An export bundle contains no secret value, API key hash or client secret, and its SHA-256 matches its
  content [adapter].
- A dry-run import returns a create, update or skip plan, lists secrets whose values must be put again,
  and writes no desired-state row [adapter].
- An applied import maps account ids by the account map, writes one audit record per resource, and every
  imported role link is `pending_trust` [adapter].
- A bundle whose checksum does not match → the import `failed` and nothing is written [adapter].
- `RestoreRevision` with `dryRun` returns the diff and writes nothing; without it, the team's resources
  match the revision, `generation` is the previous value + 1, and one audit record names the source
  revision [adapter].
- A viewer calling `RestoreRevision` → `403`; a non-service-admin calling `CreateExport` → `403`
  [adapter].

**RA-US-15 · Rate limits and settings.** *(IF-1)*
- One principal sending 60 requests within one second gets at least 40 successes and `429 Throttled`
  with `Retry-After` for the rest; another principal in the same second is not throttled [adapter].
- A browser request from an origin not in `api.cors_origins` gets no CORS allow headers; one from a
  listed origin does [adapter].
- `PutSetting` with an unknown key or a value of the wrong type or range → `400 ValidationError`; a
  non-service-admin → `403` [adapter].

### afs-reconciler

**RC-US-1 · Converge a team.** *(US-1, US-3)*
- Given a new team row, the namespace, quotas, Pod Security labels, default-deny policies, team IAM role
  with boundary, the team's Bedrock role trusting only the gateway, SecretStore and inference profiles
  exist within 60 s (p95) and `resource_status` is `Ready` with `observedGeneration = generation`
  [envtest, e2e].
- A desired-state change is picked up from `afs_desired_state` within 5 s; with notifications blocked it
  is picked up by the 30 s poll [adapter].
- Two teams can each have an agent named `reviewer`; their IAM roles are named by id and carry the human
  names as tags [adapter with fake AWS].

**RC-US-1a · Admission.** *(US-3, US-18, TEAM-4)*
- Admission refuses NetworkPolicy, CiliumNetworkPolicy, LoadBalancer or NodePort Services, hostNetwork
  and privileged pods in team namespaces from any subject except the reconciler [e2e].
- A pod in team B tolerating team A's dedicated taint or selecting its nodes is refused [envtest].
- Deleting an admission policy or binding restores it within 60 s with a `drift.corrected` record
  [envtest].

**RC-US-2 · Zero-impact agent pods.** *(US-4, US-5)*
- The rendered pod has: env vars from the ConfigMap and from Secret refs for env bindings; files for
  file bindings at their exact targets; `/etc/afs/config/<NAME>`; `/etc/afs/aws/config` when links exist;
  `/etc/afs/tags.json` and `AFS_TAGS`; the gateway token and `AFS_GATEWAY_URL`,
  `AWS_ENDPOINT_URL_BEDROCK_RUNTIME` and `ANTHROPIC_BASE_URL`; no AWS keys [envtest].
- Rotating a secret or changing a config value changes the pod-template hash and rolls only the agents
  that reference it [envtest].
- A secret naming an external system that is off for an agent is not delivered to that agent, and is
  delivered again within 60 s of the system being switched on [envtest].
- The `afs-api` token is projected only in teams with an assigned work order [envtest].
- A container image with no AFS-specific code receives all of the above (verified with `busybox env`
  and `cat` in e2e).
- With isolation tier `dedicated`, the team's pods carry the toleration and node selector of its own
  NodePool, land only on its nodes, and no other team's pod lands there [e2e].

**RC-US-3 · Revert drift and recover.** *(US-13)*
- Editing or deleting an owned object restores it within 60 s and writes one `drift.corrected` audit
  record [envtest].
- Starting against a fresh cluster and a database restored to a point in time converges everything,
  agent volumes re-created from their latest snapshots, with no manual step within 1 hour at the
  initial scale [e2e restore drill].

**RC-US-4 · Cross-account links.** *(US-7)*
- A new link gets an `sts:AssumeRole` statement on its exact role ARN, a profile in the pod's AWS config,
  status *Pending trust*, then *Verified* after a successful assume with the external ID and session
  tags, or *Failed* with the AWS error code [adapter with fake STS].
- Narrowing the allowed-accounts filter disables links outside it on the next pass [adapter].

**RC-US-5 · Egress follows external systems.** *(US-8)*
- Switching a system off at service, team or agent level removes its FQDNs from the affected agents'
  Cilium policies within 60 s, without restarting pods [envtest].
- Denied flows are written with the right `denied_reason` [adapter with recorded Hubble flows].
- An agent whose denied flows exceed `egress.denied_spike_threshold` in 5 minutes produces one
  `egress.denied_spike` event for that window [unit with fakes].

**RC-US-6 · Activity.** *(US-11)*
- Each running agent gets one sample per 5-minute window; working when it made a model call, reported
  busy, or used CPU above `reports.idle_cpu_millicores` [unit with fakes].

**RC-US-7 · Tags everywhere.** *(US-12)*
- Every AWS resource the reconciler creates carries the agent's or team's effective tags; a change is
  applied within 10 minutes; `afs-team-id` and `afs-agent-id` are always present and never removed
  [adapter with fake AWS].
- An effective tag set above 40 keys is refused before any AWS call and the resource is `Degraded` with
  a condition naming the limit [unit].

**RC-US-8 · Ephemeral teardown.** *(US-15)*
- When a team's rule fires, within 5 minutes its agents and pools stop, IAM roles and queues are
  deleted, the namespace is removed, `torn_down_at` and the reason are recorded and `team.torn_down` is
  published; config revisions, usage and audit remain [envtest, adapter].

**RC-US-9 · Swarm pools.** *(US-17)*
- A pool renders one Deployment with `workers` replicas, its own ServiceAccount and role, scratch volume
  of the requested size and, when chosen, an EFS mount; scaling changes replicas only [envtest].
- Each receiving pool has one inbox queue; it gets `AFS_INBOX_QUEUE_URL`; a pool handing off to two pools
  gets one `AFS_HANDOFF_<TO_POOL>_QUEUE_URL` per receiver; a pool receiving from two pools has one inbox
  that both may send to; no other role can send, and only the receiver can receive [adapter with fake
  AWS].
- A shared named role is linked and verified in each listed account, like an agent role link [adapter].

**RC-US-10 · Work orders flow.** *(US-16)*
- An EventBridge request is turned into one work order even when delivered twice [adapter].
- A work order gets a remediation team from its template within 2 minutes, with the cap as lifetime
  budget, the template TTL as expiry and rule `at_work_complete` [envtest].
- PR state read from GitHub moves repositories to `pr_opened` and `merged`; when all are merged, not
  affected or skipped, the work order is done and the team is torn down [adapter with recorded GitHub].
- Polling 500 repositories every 10 minutes stays under 5,000 requests per hour per installation, using
  conditional requests [adapter with recorded GitHub].
- Each owner gets one notification per event (assigned, PR opened, merged, SLA warning, SLA breached,
  completed); outside contacts are emailed through the configured service channel, and nothing is sent
  to them when it is unset [adapter].

**RC-US-11 · Read-only kubectl.** *(US-18, ACC-5)*
- With viewer kubectl on, a viewer can `get`, `list` and `watch` workloads, events and logs in the
  namespace and is refused secrets, `exec`, `attach`, `port-forward` and any change; off removes the
  access entry within 60 s [e2e].

**RC-US-12 · Budget periods, backstop and stuck reservations.** *(US-9, BUD-3)*
- Each budget's counter for a period exists by the first second of the period, created by the reconciler
  or by the gateway's first call, and `budget.reset` is published once [adapter].
- Raising a budget carries into `limit_usd` and lifts the IAM deny and the `BudgetExhausted` condition
  within 60 s [adapter with fake IAM].
- At exhaustion the team's Bedrock role gets the explicit deny and the team the `BudgetExhausted=True`
  condition within 60 s [adapter with fake IAM].
- A gateway killed mid-call leaves a reservation that is released within `gateway.reservation_ttl_seconds`
  plus 2 minutes; reserved spend returns to zero and one audit record notes the release [adapter].
- When a ceiling override expires with the budget above the base ceiling, the budget is lowered to the
  ceiling and `budget.ceiling_override_expired` is published once [adapter].

**RC-US-13 · Notifications.** *(US-9, US-16, OPS-3)*
- A Slack channel row becomes one EventBridge rule filtered on the team id with an API destination; one
  `budget.threshold_crossed` produces exactly one Slack message [adapter with fake EventBridge and Slack].
- An email channel is delivered through SES with one message per event id, even when the event arrives
  twice [adapter with fake SES].
- Deleting a channel removes its rule and target within 60 s; a team's rule never matches another team's
  events [adapter].

**RC-US-14 · Volumes and snapshots.** *(US-4, AGT-9)*
- Pausing an agent sets replicas to 0 and keeps the PVC; resuming brings back the same volume with its
  files [envtest, e2e].
- Deleting an agent with the default rule takes a final snapshot, waits for it to be ready, then deletes
  the volume; `retain` keeps the volume and records it; `delete` deletes it with no snapshot [envtest].
- A daily schedule keeps exactly `snapshotRetain` ready snapshots [envtest with a fake clock].
- `CreateAgentSnapshot` produces a ready snapshot recorded in `agent_volume_snapshots` [envtest].
- Growing `sizeGiB` expands the volume without recreating the pod's data [e2e].

**RC-US-15 · Suspend, resume and delete.** *(US-1, TEAM-1)*
- Suspending a team brings every agent and pool to 0 replicas within 60 s, keeps volumes, IAM and
  secrets, and the gateway refuses the team's calls; resuming restores each agent and pool to its prior
  state, and an agent paused before the suspend stays paused [envtest].
- Deleting a team runs the finalizer steps in order (recorded by fakes), shows `Deleting` with the
  current step as a condition, resumes after a failed step on the next pass, and ends with the row
  `deleted` while its audit and usage stay readable [envtest with fake AWS].

**RC-US-16 · Log delivery and redaction.** *(US-14, OPS-1, OPS-4)*
- A log line containing the value of one of the agent's bound secrets is delivered as
  `[redacted:<hash prefix>]`, and the same value gives the same token each time [e2e].
- Strings that look like passwords, bearer tokens, AWS keys, private keys or JWTs are redacted [unit].
- A team's logs never reach another team's destination, and its aggregator holds no other team's
  credentials [e2e].
- Adding or deleting a destination changes the outputs within 60 s [envtest].
- An agent's connection to a system's sign-in endpoint appears as a sign-in event (agent or pool worker,
  system, time) in the team's log delivery and in no database table or event [adapter].

**RC-US-17 · Export and import bundles.** *(US-13, CFG-5)*
- An export request produces one KMS-encrypted bundle in S3 whose checksum is recorded, with the
  request `succeeded` [adapter with fake S3].
- An import of a bundle with an unknown schema version fails and stages nothing [adapter].

### afs-gateway

**GW-US-1 · Hard cap under concurrency.** *(US-9)*
- Load test: 200 agents at 500 req/s against a nearly exhausted budget; total spend exceeds the budget
  by at most one in-flight request per agent, and every rejected call gets `402 BudgetExceeded` [load].
- With the database unreachable, every call is rejected with `503 Unavailable` within 2 s and none
  reaches Bedrock [adapter].
- Added latency p99 < 25 ms excluding Bedrock time [load].
- A request without `maxTokens` reserves the model's `max_output_tokens` times the output price plus the
  input estimate [unit].

**GW-US-2 · Attributed calls.** *(US-12, US-14)*
- Every Bedrock call uses the team's inference profile and carries `requestMetadata` with the agent's
  effective tags including `afs-team-id` and `afs-agent-id`, at most 16 pairs in the D27 order, with the
  dropped keys recorded [adapter with fake Bedrock].
- A token from a pod of team A can never invoke through team B's profile [unit].
- A model the team has not enabled → `403 AccessDenied`, recorded as a rejected call with reason
  `model` [adapter].
- A pool worker's call records the pool and the worker pod name [adapter].
- Every call, accepted or rejected, records its latency, and rejected calls record the reason (`budget`,
  `model`) [adapter].

**GW-US-3 · Spend under the tags of its time.** *(US-12)*
- Every `usage_events` row carries the team's tag set current at the call; after a tag change, new rows
  carry the new set within 60 s, and old rows keep theirs [adapter].

**GW-US-4 · Thresholds once.** *(US-9, BUD-4)*
- With 200 concurrent callers crossing 50%, 80% and 100% in the same second, exactly one
  `budget.threshold_crossed` per threshold and one `budget.exhausted` are published for the period
  [adapter].

**GW-US-5 · Streaming.** *(US-9, BUD-2)*
- A streamed response settles from its final usage block [adapter with fake Bedrock].
- A client that aborts mid-stream, or an upstream error mid-stream, settles on the tokens seen so far and
  releases the rest of the reservation [adapter with fake Bedrock].

### afs-billing

**BI-US-1 · Reconciliation and variance.** *(US-10)*
- Given CUR rows for a day, `cost_daily` holds Bedrock, EKS and storage (volumes and snapshots) costs per
  team, and `cost_reconciliations` is `over_threshold` when the difference exceeds the setting (default
  5%), with one `cost.variance_detected` event [adapter with fixture CUR].

**BI-US-2 · Price sync.** *(US-10)*
- With sync on and a changed price in the Price List fixture, exactly one pending proposal exists per
  model after the run; an older pending proposal is superseded; with sync off, nothing is proposed
  [adapter].

**BI-US-3 · Scheduled reports.** *(RPT-8)*
- A due schedule produces one CSV equal to the on-demand report for the same query, sent to the
  schedule's channel; `next_run_at` advances; a delivery failure records `failed` with the error and
  retries next cycle [adapter].

**BI-US-4 · Cost allocation tag status.** *(TAG-7)*
- Given CUR fixtures and a fake Billing API, each AFS tag key has `seen_in_cur` and `aws_status`; a key
  active for 3 days but not seen raises one `cost_tags.not_seen` event [adapter].

**BI-US-5 · Showback.** *(US-12, BUD-7)*
- The showback CSV for a month has one row per cost center (and per value of a chosen tag key) whose
  totals equal `cost_daily` for the month, with an `(untagged)` row for records missing the key [adapter].

**BI-US-6 · Costs under the tags they were billed with.** *(US-12)*
- Given a team tagged `business-unit = payments` until Tuesday 12:00 UTC and `platform` after, with CUR
  fixtures carrying those tags per line item, Monday's cost is under *payments*, Wednesday's under
  *platform*, and Tuesday's split by the line items' tags; pool worker lines carry the pool [adapter with
  fixture CUR].

## 6. Non-functional requirements

| Area | Requirement |
|---|---|
| Availability | `afs-api` and `afs-gateway` 99.9% monthly, at least 3 replicas across AZs; reconciler leader election with < 30 s failover |
| Performance | API p99 < 300 ms for reads, except reports (RA-US-7: p99 < 1 s for 90-day ranges); gateway p99 overhead < 25 ms; reconcile lag p95 < 60 s |
| Scale | 200 teams, 2,000 agents, 500 gateway req/s (initial) |
| Recovery | RPO 5 minutes (RDS PITR, DP-1); RTO 1 hour for a full rebuild from a restored database onto a fresh cluster, proven by the RC-US-3 restore drill |
| Retention | Partitioned facts (`usage_events`, `agent_activity`, `external_traffic`) 13 months, then dropped (RC-8); `audit_log` online with no deletion, which meets the root's 400 days (partitioning tracked in OQ-4) |
| Rate limiting | 20 req/s per principal, burst 40, `429 Throttled` with `Retry-After` (RA-15, RA-US-15) |
| Security | No long-lived AWS keys (Pod Identity for all binaries); OWASP ASVS L2; `Authorization` and tokens redacted in logs; agents' roles never get `bedrock:*`; the gateway checks the token audience and reaches Bedrock over TLS through the VPC endpoint only |
| Observability | OpenTelemetry traces and metrics; structured JSON logs with request id; RED metrics per route; every binary serves `/healthz` (process alive), `/readyz` (database reachable, schema version current, leader state for the reconciler) and Prometheus `/metrics`. SLIs with dashboards and alerts: API p99 latency, reconcile lag, drift corrections per hour, gateway reservation failures and added latency, expired reservations released, outbox publish lag (oldest unpublished event), notification delivery failures, CUR variance |
| Quality | `domain`, `usecase`, `authz` ≥ 90% coverage; allowed and denied test per route per role; `make gate` before every commit |

## 7. Dependencies

| Dependency | Used by | Note |
|---|---|---|
| `-db` `v0.13.0` (migrations 000001–000013) | all | `-db` has no tags yet; `v0.13.0` is tagged after the `-db` PR with 000013 merges. 000013 brings `pg_notify` triggers (D30), `ensure_spend_counter`, `spend_reservations` and its functions (D31), `models.max_output_tokens` (D28), ceiling override columns (D34), the D33 and D35 guards, `drop_partitions_older_than` (D36) and the D45 settings. RC-20 also needs the reconciler to update `export_bundles` status, checksum and summary (RQ-1) |
| EKS, Karpenter | reconciler | Dedicated NodePools (TEAM-6) |
| EBS CSI driver, VolumeSnapshot CRDs and snapshot controller | reconciler | Agent volumes and snapshots (AGT-9, RC-16) |
| EFS and the EFS CSI driver | reconciler | Optional shared pool volume (AGT-8) |
| Kubernetes TokenReview API | gateway | Agent and pool authentication (GW-1). `afs-api` validates its workload tokens against the cluster OIDC issuer JWKS instead |
| metrics-server | reconciler | CPU for the activity collector (RC-7) |
| Cilium and Hubble | reconciler | FQDN egress (EXT-5) and traffic flows (RC-7) |
| Fluent Bit | reconciler | Per-team log delivery and redaction (RC-18) |
| ValidatingAdmissionPolicy (built in; Kyverno only if OQ-6 decides so) | reconciler | TEAM-4 (RC-19) |
| External Secrets Operator | reconciler | Secret delivery to agents, pools and log aggregators |
| Agentic Teammates CRDs and status endpoint | reconciler | Pinned API group and version per OQ-7 (root R6) |
| AWS IAM, Pod Identity, STS, Organizations, EKS access entries | reconciler | Roles, links and their verification, account mirror, viewer kubectl |
| Per-team Bedrock role `afs-<team ulid>-bedrock` | gateway assumes, reconciler creates | RC-1 creates it with a trust on the gateway's role; RC-6 puts the deny on it |
| Bedrock (VPC endpoint, application inference profiles) | gateway, reconciler | Model calls and attribution |
| Secrets Manager | api (writes), ESO (reads) | Managed path `<service>/*` |
| EventBridge (bus, rules, API destinations, connections), SQS | reconciler | Outbox, notifications (RC-15), work order intake, pool inbox queues |
| SES | reconciler | Email notifications (RC-15) |
| S3 and KMS | billing (CUR), reconciler (bundles) | CUR via Athena; export bundles encrypted with a KMS key |
| AWS Backup, RDS PITR | `deploy/terraform` (DP-1) | CFG-4 |
| AWS Price List, Billing cost allocation tags, CUR via Athena | billing | BI-1..6 |
| GitHub App per organization (GHES by its own app per host) | reconciler | PR state for work orders, per OQ-5 |
| Shared named roles in target accounts | outside AFS | Deployed by each account owner (for example by StackSet) for pools |
| Root `docs/contracts/gateway-contract.md` | gateway, agents | New with D28; this repository provides it |

## 8. Delivery

| Milestone | This repository |
|---|---|
| M0 | OpenAPI from the API contract for `/v1/me`, teams and members; auth middleware; RA-1a (`authz` replaced, catalog-equality test); adopt `-db` `v0.13.0`; OQ-1 and OQ-2 spikes |
| M1 | RA-1..4, RA-15, RA-16 for teams, members, keys; RC-1, RC-4, RC-5, RC-19; RC-17 team suspend, resume and delete; DP-1 |
| M2 | Agents, secrets, config, external systems (RA-1, RA-5); RC-2, RC-16, RC-17 agent lifecycle, RC-7 traffic, RC-18 |
| M3 | GW-1..4, GW-6, GW-7; RA-14, RC-6, RC-14, RC-15, RC-7 activity; RA-6 reports |
| M4 | RC-3, RC-9, GW-5, BI-1..5, BI-7, RA-7, scheduled reports, RA-12, RA-13, RC-20 |
| M5 Budgeted workloads and swarms | RA-8, RA-10, RA-11, RC-10, RC-11, RC-13, BI-6 |
| M6 CVE remediation | RA-9, RA-2b, RC-12; feature complete |

## 9. Open questions

Each has an owner, a target milestone and the default this PRD builds to if it is not answered by then.
OQ numbers are shared across the PRDs (root PRD §10.1); RQ items are local to this repository.

| Id | Question | Affects | Owner | By | Default if unanswered |
|---|---|---|---|---|---|
| OQ-1 | Which built-in session tags does EKS Pod Identity set in the target EKS version? | RC-2, RC-3, IAM conditions | restapi | M0 spike | Conditions on `aws:PrincipalTag/kubernetes-namespace`; AFS ids as role tags (D26) |
| OQ-2 | Confirm the AWS limits of 50 tags per resource (EBS CSI adds up to 5) and 16 Bedrock `requestMetadata` pairs | RC-9, GW-5 | restapi | M0 spike | Cap the effective tag set at 40 keys; send 16 pairs in the D27 order |
| OQ-3 | Do third-party agent images need an env-only gateway credential, since they cannot read the token file? | GW-1, RC-2 | J / platform | M3 | Token file only, plus the Agentic Teammates runtime helper |
| OQ-4 | When to partition `audit_log` | RC-8 | db | 12 months or 50 GB | Kept online, unpartitioned, no deletion |
| OQ-5 | GitHub access for PR tracking: one GitHub App per organization or GHES tokens; the rate-limit budget | RC-12 | J (enterprise GitHub setup) | M6 | One GitHub App per organization, GHES by its own app per host; 5,000 requests per hour per installation with conditional requests |
| OQ-6 | Admission engine: ValidatingAdmissionPolicy or Kyverno | RC-19 | platform | M1 | ValidatingAdmissionPolicy (built in) |
| OQ-7 | Agentic Teammates CRD API group and version, and its status endpoint shape (root R6) | RC-2, RC-7 | J | M2 | Pin the version current at M2; activity falls back to model calls and CPU when the status endpoint is missing |
| OQ-8 | Egress HTTP request and error counts need an egress proxy | RC-7, RA-6 traffic reports | platform | M4 | Connections and bytes from Hubble only; requests and errors empty |
| OQ-9 | Which IAM role does the IdP federate to for viewer kubectl? | RC-13 | J | M5 | One role per provider, mapped by the access entry to the team's namespace Role |
| RQ-1 | How an import is applied without `afs-api` calling S3 or the reconciler writing desired state | RA-12, RC-20, API contract, `-db` | restapi with db | M4 | Two steps: the reconciler stages the bundle and plan in `export_bundles` (needs a `-db` grant on status, checksum, summary and `finished_at`, and a checksum that may be empty until done); `afs-api` applies the staged content when `CreateImport` with `dryRun: false` names a succeeded dry run (a contract addition) |

## 10. Definition of done (every change)

- [ ] API change in `api/openapi.yaml` (and the API contract if it is new) in the same PR; consumers told
- [ ] Gateway change in the gateway contract in the same PR
- [ ] Route declares a permission; allowed and denied tests per role; the catalog-equality test is green
- [ ] Mutation writes row, audit and outbox in one transaction
- [ ] No secret value, token or key in responses, logs, traces, errors or events
- [ ] `make gate` green; acceptance criteria above for the story covered by tests
