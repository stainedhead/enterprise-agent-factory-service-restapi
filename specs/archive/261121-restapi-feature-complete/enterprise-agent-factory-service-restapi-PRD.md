# Enterprise Agent Factory Service — Control plane PRD (`-restapi`)

| | |
|---|---|
| **Status** | Draft for review, feature-complete scope (2026-11-21); revised after the `review-prd` pass of 2026-11-21 (decisions D23–D57 of the root PRD §10.1). **Extended 2026-11-22 for [Paperclip team collaboration](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/paperclip-team-collaboration-PRD.md)** (stories RA-US-19..19e, RC-US-19..20c, EDGE-US-1..2, INF-US-1; requirements RA-17..22, RC-21..30, EDGE-1..7, DP-2..4; milestones C1 to C4 in §8; decisions RD-1..23 and gaps G1..G10, all resolved by v1.3.1, in §9; review record in §11). The extension has had its own `review-prd` pass (§11). **Aligned to contract v1.3.1 (root `main` 1f60ecd, PC-D20..PC-D31) the same day** (§11): server-side edge sessions, apex callback, no SNI check, log routing, key functions, bootstrap event, swarm collaboration. Not yet built |
| **Author** | J (Enterprise Architecture), drafted with Claude |
| **Parent** | [Root PRD](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/enterprise-agent-factory-service-initial-PRD.md) (stories US-1..US-26); [solution design](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/solution-design.md) |
| **Provides** | [API contract](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/api-contract.md) → `api/openapi.yaml`; [gateway contract](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/gateway-contract.md) (`afs-gateway` ↔ agents, new, D28); EventBridge events (API contract §7); from contract v1.3.0 also the externally visible behaviour of the public collaboration endpoint, [API contract §4.4c](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/api-contract.md), served by the new fifth binary `afs-edge` |
| **Consumes** | [Database contract](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/database-contract.md) at `-db` `v0.13.0` (schema 000014); for the Paperclip change, the `-db` release that contains migration 000020 ([§7.2](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/database-contract.md); until it is tagged, a pseudo-version as for 000019); the platform, AWS and third-party dependencies in §7. **Pinned contract extracts must be resynced:** `api/contract/*.json`, `internal/authz/catalog_gen.go` and `api/src/**` were cut from contract 1.2.0 and must be regenerated from 1.3.0 (`make contract`, then `make openapi`, ADR-022) before any story below is implemented; the `-cli` and `-website` snapshots of `api/openapi.yaml` follow in their own PRs. This PRD change regenerates nothing |

## 1. Purpose

The control plane: five Go binaries that turn people's intent into a running, isolated, metered
environment (the fifth, `afs-edge`, is the only one reachable from the internet).

| Binary | Job |
|---|---|
| `afs-api` | The REST API and the security boundary: authentication, RBAC, validation, desired-state writes with audit and outbox, write-only secret values, reports, export requests, synchronous import, revision restore |
| `afs-reconciler` | Leader-elected. Applies desired state to EKS and AWS continuously, reverts drift, reports status; runs the activity and traffic collectors, the outbox publisher, partition maintenance and retention, spend periods and the reservation sweeper, notification delivery, log delivery and redaction, admission policies, volume snapshots, suspend, delete and ephemeral teardown, swarm pools and their queues, work order intake and tracking, export bundles |
| `afs-gateway` | The only path from agents to Bedrock (gateway contract). Identifies the calling pod (D57), reserves worst-case cost, invokes, settles, meters, tags and fails closed |
| `afs-edge` | The governed public entry to each team's Paperclip (Paperclip change, COL-15). Multi-AZ, at least 3 replicas in `afs-system` behind one internet-facing ALB and WAF: routes `<team-slug>.<collab.base_domain>` to the team's `paperclip` Service, signs people in with the team's provider (OIDC code + PKCE), authorizes every request with `internal/authz`, scans, limits and records, and forwards to nothing else. Reads the database only through ten `afs_edge` functions |
| `afs-billing` | Daily jobs: CUR ingestion and reconciliation with variance alerts, AWS Price List sync proposals, showback exports, cost allocation tag status; scheduled report delivery |

## 2. Scope

**In:** the five binaries, `api/openapi.yaml`, Helm chart and Terraform modules under `deploy/`,
runbooks in `user-docs/`, and for the Paperclip change the public ALB, WAF, certificate and DNS Terraform, the `afs-edge` chart and the rendering of the collaboration agent. **Out:** Paperclip, Hermes and the `agentic-team-w-paperclip` images (configured, never built or forked here), the console and CLI screens and commands for collaboration, the schema of migration 000020 (owned by `-db`), the schema (owned by `-db`), clients (`-cli`, `-website`), the agent
runtime (Agentic Teammates), the shared named roles that account owners deploy in target accounts for
pools (for example by StackSet), and metering of model providers other than Bedrock (root EXT-6: their
calls bypass the gateway and are not metered, budgeted or capped). No search API in v1 (D44). Also out for the Paperclip change: Paperclip pushing work into agents, swarm pools connecting to Paperclip, inbound webhooks from the internet, an external database for Paperclip, more than one Paperclip per team (change PRD non-goals), and storing, logging or exporting any Paperclip content.

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
- **`afs-edge` is the one public component and the narrowest.** It reads the database only through the six
  `SECURITY DEFINER` functions of the `afs_edge` role (routes, settings, principals, access counts, the secret-detection event, server-side sessions and the single-use handoff; no table access, database contract §7.2.5 and §7.2.8, PC-D12, PC-D22), has no Kubernetes
  client, calls AWS only for Secrets Manager reads of its `state` sealing key and the collaboration client secrets, and
  forwards only to the `paperclip` Service named by the route table. It decides access with `internal/authz`, never with
  its own role table; it never stores or logs Paperclip content (COL-26); a decision that cannot be made fails closed.
  Public ingress exists nowhere else: no team-owned load balancer, `Ingress` or route (COL-22, RC-23).

## 4. Requirements owned

Scope is feature complete (root ADR-014). Priorities P0 to P2 are build order, not optional scope.

| Id | Binary | Requirement | Product reqs | Pri |
|---|---|---|---|---|
| RA-1 | api | Implement every operation in the API contract §4 with its permission, conventions (§1) and errors (§6), including `ListTeamDirectory` (`GET /v1/team-directory`: id, slug and display name only, for security operators and service admins, D38); serve `api/openapi.yaml` at `/v1/openapi.yaml`. The agent template operations are included: `ListAgentTemplates` (`models:read`, enabled templates only) and, for service admins, `ListAdminAgentTemplates`, `PutAgentTemplate` and `DeleteAgentTemplate` (`admin:agent-templates`, D54). Every response, errors and `401` included, carries `X-Afs-Contract-Version: <semver>` (D47) | IF-1, ACC-3, AGT-5, AGT-11, EXT-1..4, EXT-7 | P0 |
| RA-1a | api | Replace the scaffold `internal/authz` with the API contract §3 catalog exactly: every permission and no other (the scaffold's `budgets:set-ceiling` goes; the ceiling is under `teams:set-guardrails`), roles `team_viewer`, `team_admin`, `service_admin`, `security_operator` (`work-orders:read`, `work-orders:write`, `swarm-templates:read`, D37) and the workload principal (D29). `Authorize(principal, permission, resource)` takes resource attributes so conditional grants are decided in one place: `teams:set-aws-accounts` when `mayManageAwsAccounts`, `teams:get-kubeconfig` when `viewerKubectl`, declaring external systems when `mayAddExternalSystems`, `work-orders:read` for work orders assigned to or touching a repository of the caller's team, `work-orders:report` on the assigned team only | ACC-2, ACC-3 | P0 |
| RA-2 | api | OIDC token validation for every enabled provider (JWKS cached 10 minutes and refetched on an unknown `kid`; issuer, audience and expiry checks); team-scoped group bindings only from the team's provider; API key auth by SHA-256 lookup, refusing expired keys and updating `lastUsedAt` at most once a minute | ACC-1, ACC-2, ACC-4 | P0 |
| RA-2b | api | Workload principal (D29): accept the projected ServiceAccount token with audience `afs-api` (mounted at `/var/run/secrets/afs/api/token`). `afs-api` validates it offline against the EKS cluster's OIDC issuer JWKS (issuer, audience, expiry at most 1 h), with no Kubernetes call (the JWKS is fetched from the issuer URL and cached like the IdP keys), and maps namespace and ServiceAccount to the agent or pool through the database. The principal (`type: workload`, id = agent or pool id) holds only `work-orders:report` on work orders assigned to its team, and `GetMe` | WO-3 | P2 |
| RA-3 | api | `GET /v1/me`: principal (`type` `user`, `api_key` or `workload`), `serviceAdmin`, `securityOperator`, and per bound team the role and the effective `permissions[]` as decided by `authz`, including flag-dependent rights (D42) | ACC-3 | P0 |
| RA-4 | api | Session settings on every transaction (`afs.actor`, `afs.service_admin`, `afs.team_ids`, `afs.security_operator`); optimistic concurrency (`If-Match` on every PUT, PATCH and DELETE); idempotency keys for `api.idempotency_key_ttl_seconds` per the database contract §7.1, with the `CreateApiKey` and `CreateServiceApiKey` responses stored without `secretKey` (D52; the database refuses a body that still contains it, D35); request ids echoed in `X-Request-Id`; a change of team `kind` is `ValidationError` | CFG-1, IF-1 | P0 |
| RA-5 | api | Secret create, put-value and rotate write to Secrets Manager under `afs.secret_path`, then store the ARN; the value is held only for the call | SEC-1, SEC-2, SEC-4, SEC-5 | P0 |
| RA-6 | api | Reports: `/reports/{usage\|costs\|activity\|traffic}` at service, team and agent level, groupBy incl. `tag:<key>` (tags as of the record) and `pool`, CSV, forecast and `projectedCapAt` on budget positions; report schedules on the team's own channels, created only by team-admins and service admins (D23) | RPT-1..9, TAG-6 | P0/P1 |
| RA-7 | api | Price proposals: list, approve, reject (service admins), audit and events; an approved price is effective from `max(effectiveFrom, approvedAt)`, never earlier | BUD-9 | P1 |
| RA-8 | api | Ephemeral teams: create with expiry, teardown rule and lifetime budget in one request; stages, pools, handoffs, pool secrets, configuration, external systems and shared role links (API contract §4.4a). A pool role link must be inside the team's allowed accounts and use the pool's one role name | TEAM-7, AGT-8, BUD-8 | P2 |
| RA-9 | api | Security operators: service-level bindings and keys (service admins only; team-admins cannot add them); work orders and repositories (§4.10a), where `CreateWorkOrder` lets a repository name its owner (team id and/or email) or name none, in which case `afs-api` resolves it through `afs.resolve_repo_owners(host, repo)` (`-db` 000014) from the teams' `AFS_OWNED_REPOS` configuration value: an exact entry beats a wildcard, deleted teams are skipped, an explicit owner always wins, more than one team at the best match → `400` with `details.ambiguousRepos`, none → `400` with `details.unownedRepos` (D38); swarm templates (security operators list and describe them); team-scoped work order reads | ACC-2, ACC-4, WO-1..6 | P2 |
| RA-10 | api | Viewer kubectl: `GetTeamKubeconfig` when the team has it on; isolation tier and viewer kubectl as guardrails | ACC-5, TEAM-6 | P1/P2 |
| RA-11 | api | Cost allocation tag status for service admins (`ListCostTagStatus`); the activation banner is acknowledged through setting `billing.tag_activation_acknowledged` (`PutSetting`) | TAG-7 | P2 |
| RA-12 | api | Export and import (D49). `CreateExport` records the request in `export_bundles` and returns it `pending`; the reconciler builds the bundle (RC-20); `GetExport` returns status, `sizeBytes`, `sha256` and `downloadUrl` when `succeeded`. `CreateImport` (`?dryRun=true\|false`, default `true`) applies the request-body bundle synchronously, one transaction per team, returning `{dryRun, teams[…]}`; it verifies the checksum and `schemaVersion`, writes through the normal transaction rules (row, audit, outbox per resource), maps account ids and leaves every role link `pending_trust`. With `dryRun=true` nothing is written. A bundle over 100 MB → `400 ValidationError`. `afs-api` never reads or writes S3 | CFG-5 | P1 |
| RA-13 | api | `RestoreRevision` with `dryRun`: restore the team's desired state (team settings, agents, bindings, configuration values, external system switches, tags, budget) to a revision in one transaction. Dry run returns the diff and writes nothing. Secret values are never in revisions; a binding to a secret that no longer exists is skipped and reported | CFG-6 | P1 |
| RA-14 | api | Budget rules: amount at most the effective ceiling (`maxBudgetUsd`, or `ceilingOverrideUsd` while `ceilingOverrideExpiresAt` is in the future, set by service admins only, D34); `onExhaust: notify` only on persistent teams and only by service admins, ephemeral teams always block (D33); a lifetime budget cannot be removed and an ephemeral team cannot become persistent | BUD-1, BUD-3, BUD-8, BUD-10, TEAM-7 | P0 |
| RA-15 | api | Rate limits per principal: 20 req/s, burst 40, `429 Throttled` with `Retry-After`; each replica enforces its share of the limit. CORS only from the origins in setting `api.cors_origins` (D43) | IF-1, NFR | P0 |
| RA-16 | api | Settings (D45): `GetSettings` / `PutSetting` accept only the keys in the root PRD §6.13 settings table and the database contract, with type and range checks; an unknown key or bad value → `ValidationError`. §4.1 lists the keys this repository reads | CFG-1 | P0 |
| RC-1 | reconciler | Per team: namespace, Pod Security `restricted`, ResourceQuota, LimitRange, default-deny NetworkPolicies, Cilium FQDN egress per agent, team IAM role with permissions boundary, the per-team Bedrock role the gateway assumes (trusting only the gateway's role, allowed only to invoke the team's inference profiles), ESO SecretStore, Bedrock application inference profiles per enabled model, and the Cilium policy that lets only team-namespace pods reach `afs-gateway` (D57; WireGuard transparent encryption between nodes is a platform setting, §7). AWS names are by id (D25): IAM roles `afs-<ulid>` (the team's Bedrock role `afs-<team ulid>-bedrock`) under path `/afs/<namespace>/`; human names go in tags (`afs-team-slug`, `afs-agent-name`, `afs-pool-name`) | TEAM-2, TEAM-3, AGT-4, EXT-5 | P0 |
| RC-2 | reconciler | Per agent (and per dedicated node pool when the team's isolation tier is `dedicated`: a Karpenter NodePool with tainted, labelled and tagged nodes, and matching tolerations and selectors on the team's pods): Agentic Teammates custom resource rendering a StatefulSet (1 replica) with ServiceAccount, Pod Identity association, volume claim, ConfigMap (`envFrom` + `/etc/afs/config`), ExternalSecrets for bindings (env or file; a secret naming an external system that is off for the agent is not delivered), `/etc/afs/aws/config` (every profile sets `role_session_name` to the agent's id, or the pool's id for a pool, D51), `/etc/afs/tags.json` and `AFS_TAGS`, the gateway environment of the gateway contract §3 (D28, D57): `AFS_GATEWAY_URL`, `AWS_ENDPOINT_URL_BEDROCK_RUNTIME`, `ANTHROPIC_BASE_URL`, the placeholder `ANTHROPIC_API_KEY=afs-gateway`, the gateway CA bundle at `/var/run/secrets/afs/gateway/ca.crt` named in `AWS_CA_BUNDLE`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and `NODE_EXTRA_CA_CERTS`, and the optional token projection at `/var/run/secrets/afs/gateway/token` (`AFS_GATEWAY_TOKEN_FILE`), the `afs-api` token projection in teams with an assigned work order (D29), pod-template hash of referenced versions. Roles carry `afs-team-id` (and `afs-agent-id` or `afs-pool-id` for dedicated roles) as role tags, and IAM conditions use the built-in `aws:PrincipalTag/kubernetes-namespace` session tag (D26, OQ-1) | AGT-1..3, AGT-7, AGT-9, AGT-10, SEC-3, SEC-6, CFG-7, TAG-4, TEAM-6, EXT-7 | P0 |
| RC-3 | reconciler | Dedicated agent roles and cross-account links: `sts:AssumeRole` statements and the `role_session_name` of each profile (the agent or pool id, D51), so the target account's CloudTrail names the agent without relying on transitive session tags; link verification on create, daily and on demand, status write-back | AGT-6, AGT-10, TEAM-8 | P1 |
| RC-4 | reconciler | Change detection and drift (D30): `LISTEN afs_desired_state` (payload `<table>:<team_id>:<id>`, sent by the `-db` 000013 trigger) and reconcile the named resource; poll `generation <> observed_generation` every 30 s as a backstop and after any lost connection; full resync every 10 minutes and at startup. Watch owned objects, revert changes, audit `drift.corrected` | CFG-2 | P0 |
| RC-5 | reconciler | Status: phase, conditions, observed generation per resource into `resource_status`, using only the contract vocabulary (D24): `phase` Pending, Reconciling, Ready, Degraded, Failed, Deleting; budget exhaustion is condition `BudgetExhausted=True` on a Ready resource; an agent or pool whose template is disabled or whose image digest is no longer allowed gets condition `TemplateRetired=True` and keeps running (D54, AGT-11) | TEAM-5, AGT-11 | P0 |
| RC-6 | reconciler | Budget backstop: IAM explicit deny on the team's Bedrock role within 60 s of exhaustion and condition `BudgetExhausted=True`; both removed within 60 s of a period reset, a budget increase or a ceiling override that makes room | BUD-3 | P0 |
| RC-7 | reconciler | Collectors: activity every 5 minutes; Hubble flows to `external_traffic` with denied reasons (connections and bytes; HTTP requests and errors only where an egress proxy exists, OQ-8); every row stamped with the team's current tag set and, for pools, `pool_id` and the worker pod; sign-ins to the team's log delivery only (RC-18; no table, no event); `egress.denied_spike` when an agent's denied flows exceed `egress.denied_spike_threshold` in 5 minutes | RPT-5, RPT-6, OPS-4, TAG-6 | P0/P1 |
| RC-8 | reconciler | Outbox publisher to EventBridge, at-least-once, ordered per aggregate, retrying with `attempts` and backoff; partitions created at least one month ahead; retention (D36): partitions of `usage_events`, `agent_activity` and `external_traffic` older than 13 months dropped with `afs.drop_partitions_older_than(13)`, `audit_log` kept online with no deletion (OQ-4); org account mirror hourly; idempotency key purge every 5 minutes | OPS-2, TEAM-8, IF-1 | P0 |
| RC-9 | reconciler | Tags: apply effective tags to every AWS resource it manages, add and remove on change; the effective set (system + service + team) is capped at 40 keys (D27, OQ-2) | TAG-1, TAG-4, TAG-5 | P1 |
| RC-10 | reconciler | Ephemeral teams: teardown when `teams_due_for_teardown` lists the team (expiry, cap, work order complete), through the RC-17 delete flow: stop agents and pools, revoke IAM, delete queues and namespace, record `torn_down_at` and reason, keep config, usage and audit; `team.teardown_started`, `team.torn_down` | TEAM-7, BUD-8 | P2 |
| RC-11 | reconciler | Pools: one Deployment per pool (workers 0–200) with the pool's ServiceAccount, Pod Identity role, secrets, ConfigMap, egress policy, scratch volume and optional EFS; shared named role in each linked account with verification. Queues (D32): one SQS queue per receiving pool (its inbox, named `afs-<team ulid>-<short hash>`, D25); the receiver gets `AFS_INBOX_QUEUE_URL`, each sender `AFS_HANDOFF_<TO_POOL_NAME_UPPER_SNAKE>_QUEUE_URL`; only the roles of pools with a handoff to it may send, only the receiver may receive | AGT-8 | P2 |
| RC-12 | reconciler | Work orders: drain the EventBridge intake queue (dedupe by event id); create the remediation team from the template (expiry from TTL, lifetime budget from the cap, rule `at_work_complete`); read PR state from GitHub every 10 minutes with a GitHub App installation token (OQ-5), using conditional requests within 5,000 requests per hour per installation; mark done, failed or cancelled; notify owners once per event and recipient through RC-15 (teams on their channels, outside contacts through `work_orders.notification_channel_id`); SLA warning `work_orders.sla_warning_hours` before due, and breach | WO-1..6 | P2 |
| RC-13 | reconciler | Viewer kubectl: EKS access entry for the team's provider role (OQ-9) mapped to a namespace-scoped read-only Role (no secrets, exec, attach, port-forward) when on, removed when off; no access entry is created for a provider without a federated role (OQ-9) | ACC-5 | P2 |
| RC-14 | reconciler | Spend periods and reservations (D31, D34): at each period start call `afs.ensure_spend_counter(budget_id, period_start)` for every budget and emit `budget.reset`; carry a budget change into the counter's `limit_usd` within 60 s; every minute call `afs.release_expired_reservations()` (expiry `gateway.reservation_ttl_seconds`, default 900) and audit each release count; when a ceiling override expires, clear it and, if a budget exceeds the base ceiling, lower the budget to the ceiling and emit `budget.ceiling_override_expired` | BUD-1..4, BUD-8 | P0 |
| RC-15 | reconciler | Notification delivery (D46): reconcile each `notification_channels` row into an EventBridge rule filtered on the team id and the channel's event types, with an API destination (Slack, webhook, Jira; credentials in an EventBridge connection backed by Secrets Manager) or, for email, an SQS target that the reconciler drains and sends with SES. Removed with the channel. Delivery failures are retried and visible on the channel's status | OPS-3, BUD-4, WO-4 | P1 |
| RC-16 | reconciler | Agent volumes and snapshots: StatefulSet PVC retention `Retain` for scale-down and delete; online expansion when `sizeGiB` grows (patching the PVC); EBS snapshots through CSI `VolumeSnapshot` on the agent's schedule (daily or weekly) and on `CreateAgentSnapshot` requests, pruned to `snapshotRetain`, recorded in `agent_volume_snapshots`; a missing PVC (cluster rebuild) is re-created from the latest ready snapshot; snapshots carry the agent's tags | AGT-9, BUD-6, TAG-4 | P0 |
| RC-17 | reconciler | Lifecycle, following the root TEAM-9 lifecycle: agent pause (replicas 0, volume kept), resume and restart (pod-template annotation); pool pause and resume (replicas 0 and back to `workers`); team suspend (every agent and pool to 0 replicas, the gateway refuses the team's calls, volumes, IAM, secrets and data kept; `team.suspended`) and resume (each agent and pool back to its own prior state; `team.resumed`); the ordered delete finalizer for `DeleteTeam` and `DeleteAgent`: scale to zero, apply each agent's on-delete rule (final snapshot then delete, delete, or retain), delete pool volumes and queues, delete the team's secrets in Secrets Manager with a 30-day recovery window (D53), revoke IAM (Pod Identity associations, roles, link grants), delete inference profiles and the access entry, delete the namespace, mark the row `deleted`. Each step is idempotent and named in a condition while `Deleting`; a failed step is retried | TEAM-1, AGT-1, AGT-8, AGT-9 | P0 |
| RC-18 | reconciler | Log delivery and redaction: from `log_destinations`, render a per-team Fluent Bit aggregator (fed by the node DaemonSet, routed by namespace) with one output per destination (CloudWatch Logs, S3, OTLP, Splunk HEC, Datadog) and only that team's destination credentials. A redaction filter runs before every output and before traffic records are stored: credential-like values (passwords, bearer tokens, AWS keys, private keys, JWTs) and the values of the team's bound secrets become `[redacted:<first 8 hex of HMAC-SHA-256 with a per-team key>]`. The filter gets the bound secrets through ESO into a Secret only the aggregator reads; the reconciler renders references and never reads a value. Sign-in events (RC-7) go to the same outputs | OPS-1, OPS-4 | P1 |
| RC-19 | reconciler | Admission (TEAM-4): install and own ValidatingAdmissionPolicies and bindings (default engine, OQ-6) for `afs-t-*` namespaces: refuse NetworkPolicy and CiliumNetworkPolicy changes by anyone but the reconciler, LoadBalancer and NodePort Services, hostNetwork, hostPID, hostIPC, hostPath and privileged pods, changes to AFS-managed labels, and tolerations or node selectors for a dedicated node pool other than the namespace's own team's. Drift on the policies is reverted like any owned object | TEAM-4, TEAM-6 | P0 |
| RC-20 | reconciler | Export bundles for RA-12 (export only; import is applied by `afs-api` from the request body): write a versioned, KMS-encrypted bundle to the export S3 bucket (all desired state per root CFG-5: configuration values, external systems catalog and switches, allowed-accounts filters, identities and role links with their external ids, latest snapshot references; no secret value, key hash or client secret, only ARNs and metadata), then record `sha256`, `size_bytes`, `s3_uri`, a pre-signed `download_url` valid 7 days and its `download_expires_at` on the `export_bundles` row, and mark it `succeeded` (`failed` with a summary otherwise). After expiry a new export is needed | CFG-5 | P1 |
| GW-1 | gateway | Serve the gateway contract (D28, D57): Bedrock Runtime–compatible `POST /model/{modelId}/converse`, `/converse-stream`, `/invoke`, `/invoke-with-response-stream` and Anthropic Messages–compatible `POST /anthropic/v1/messages`. Identify the caller by its source pod IP (Kubernetes pod watch, cached, and Cilium endpoint identity → namespace, ServiceAccount, agent or pool, worker pod); an IP with no current team pod → `401 Unauthenticated`. The SigV4 `Authorization` header and `x-api-key` are ignored and never logged. The projected token (audience `afs-gateway`, header `X-Afs-Gateway-Token`) is optional: when sent, Kubernetes TokenReview validates it (result cached at most 5 minutes) and it must name the same pod as the source IP, else `401`. Resolve allowed models; a model not enabled → `403 AccessDenied`; a suspended team → `403 AccessDenied` with `details.reason = team_suspended` | AGT-4, AGT-8, SEC-6, IF-5 | P0 |
| GW-2 | gateway | Reserve worst-case cost before every call (D28, D31): `maxTokens` from the request, else the model's `max_output_tokens`, times the output price, plus input tokens estimated as request bytes ÷ 3 times the input price; `afs.ensure_spend_counter`, then `afs.reserve_spend_held(...)`; NULL → `402` `BudgetExceeded` (not retryable). Invoke through the team's inference profile with the team's Bedrock role; settle with `afs.settle_reservation(id, budget_id, period_start, actual)`, always with the budget and period reserved against; write `usage_events` stamped with the team's current tag set (and pool and worker pod for pool workers), including rejected calls (`outcome` `rejected_budget` or `rejected_model`) and the call's latency | BUD-2, BUD-5, TAG-6, RPT-3 | P0 |
| GW-3 | gateway | Fail closed: database or reservation unavailable → `503 Unavailable` within 2 s; no call goes to Bedrock without a reservation. Bedrock is reached over TLS through the VPC endpoint only | BUD-2, NFR | P0 |
| GW-4 | gateway | Thresholds: emit `budget.threshold_crossed` once per period per threshold (one row in `budget_threshold_events` decides), and `budget.exhausted` once per period on the first refusal at 100% | BUD-4 | P0 |
| GW-5 | gateway | Add the agent's effective tags as Bedrock `requestMetadata`, at most 16 pairs (D27): system tags first, then service fixed and required tags, then team tags in key order; dropped keys are recorded for the T13 page. Cache per agent; refresh on the `afs_desired_state` notification for the team's tags and at most 60 s after any change | TAG-1, TAG-4 | P1 |
| GW-6 | gateway | Streaming responses (ConverseStream, InvokeModelWithResponseStream, Messages SSE): tokens counted from the final usage block; a client abort or upstream error mid-stream settles on the tokens the usage reported so far, and when no usage was reported at all settles to the reservation (gateway contract §5) | BUD-2 | P0 |
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
| `collab.base_domain` | unset (feature off) | api (RA-17 hostnames and `effective`, RA-21), edge (EDGE-2), reconciler (RC-21 `PAPERCLIP_PUBLIC_URL`, RC-24) |
| `collab.public_endpoints` | `true` | api (RA-17), edge (EDGE-2) |
| `collab.allowed_source_cidrs` | `[]` | reconciler (RC-28, into the WAF IP sets) |
| `collab.session_ttl_minutes` | `480` | edge (EDGE-3) |
| `collab.content_scan` | `block` | edge (EDGE-6, EDGE-5 `unsupported_encoding`) |
| `collab.max_body_mb` | `25` | edge (EDGE-5) |
| `collab.denied_spike_threshold` | `50` per team per 5 minutes | reconciler (RC-27) |

### 4.2 Requirements owned for Paperclip team collaboration

Added by the change PRD [`paperclip-team-collaboration-PRD.md`](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/paperclip-team-collaboration-PRD.md)
(2026-11-22; milestones C1 to C4, §8). Build order follows its priorities: P0 collaboration agent, in-namespace connection and
web UI endpoint; P1 REST API endpoint, managed registration, scanning; P2 member sync and reports. Product requirement ids
(COL-n, SEC-7) are those of the change PRD; the contracts are authoritative for names and shapes (API contract v1.3.0
§2.1, §3, §4.4b, §4.4c, §7; database contract §7.2, migration 000020; gateway contract: no change).

| Id | Binary | Requirement | Product reqs | Pri |
|---|---|---|---|---|
| RA-17 | api | The seven collaboration operations of API contract §4.4b: `GetTeamCollaboration`, `PutTeamCollaboration` (`If-Match` optional, `412` on mismatch), `ListCollaborationLinks`, `ConnectAgentToCollaboration` and `DisconnectAgentFromCollaboration` (both `Idempotency-Key`, row plus audit, no outbox row; added to the CD-7 exemption list and its test, 71 operations), `RotateCollaborationKeys`, and `ListCollaborationEndpoints` (`admin:settings`, CSV, filters `teamId` and `state`). Errors: a team with no collaboration agent → `404` (reads) or `409` (mutations) with `details.reason = collaboration_agent_missing`; a suspended team → `409 team_suspended` on every mutation; `PutTeamCollaboration` field reasons `collab_base_domain_unset`, `collab_public_endpoints_off`, `collaboration_not_allowed_for_team`, `provider_without_collab_client`, `collaboration_agent_not_ready`. `PutTeamCollaboration` emits `collaboration.updated` with the `changes` object of §7, in the same transaction as the row and audit. `effective` and `reasons[]` are computed by `afs-api` from the stored choices, `teams.may_expose_collaboration`, the settings `collab.base_domain` and `collab.public_endpoints`, and the team provider's `collab_client_id` (the same rule as the `web_ui_effective` and `rest_api_effective` columns of `afs.collab_routes()`; `afs_app` cannot call that function). `hostname`, `webUiUrl` and `restApiUrl` are `null` while the base domain is unset. `unmanagedPaperclipAgents` is read from the `UnmanagedPaperclipAgents` condition in `resource_status` | COL-20, COL-23, COL-34, COL-35 | P0: `GetTeamCollaboration`, `PutTeamCollaboration`, `ListCollaborationLinks`, connect and disconnect in C1 (manual registration needs them, PC-D28); `ListCollaborationEndpoints` in C2 with the S10 tab; `RotateCollaborationKeys` P1 |
| RA-18 | api | `authz` gains role `team_collaborator` and permissions `collaboration:read`, `collaboration:manage`, `collaboration:access`, `collaboration:contribute`, `collaboration:api` exactly as API contract §3 (collaborator = every viewer permission, including the conditional ones (`teams:get-kubeconfig` when the team has `viewerKubectl`, `work-orders:read` for assigned work orders or own repositories, PC-D30), plus `collaboration:contribute` and `collaboration:api`; viewer, collaborator, team-admin and service admin hold `read` and `access`; team-admin and service admin hold `manage`; security operator and workload principal hold none). `teams:set-guardrails` also covers `mayExposeCollaboration`. `team_collaborator` is accepted on team bindings and team API keys only; a service-level binding or key with it → `ValidationError`; team-admins grant it like the other team roles (`CanGrant`) and still cannot grant `service_admin` or `security_operator`. `GET /v1/me` lists the new permissions per team. `tools/contractgen` and `internal/authz/catalog_gen.go` are resynced from contract 1.3.0, and the `-website` `permission.ts` drift test is told | ACC-2, COL-27, PC-D6 | P0 |
| RA-19 | api | `CreateAgent` with `role: collaboration` (API contract §4.4): role immutable (`UpdateAgent` change → `ValidationError`); template must be enabled with capability `paperclip` (`ValidationError`, `field: template`), a `paperclip` template is refused for a worker; `iamIdentity` defaults to and must be `dedicated`; `volume.snapshotSchedule` defaults to `daily`, `snapshotRetain` 14, `off` refused; `volume.onDelete` defaults to `snapshot_then_delete`; `collaboration` must be omitted; a second live collaboration agent → `409 Conflict`, `details.reason = collaboration_agent_exists` (the `agents_team_collaboration_uq` violation `23505` maps to it). In the same request and transaction scope as the agent row, audit and `agent.created`, `afs-api` generates the three boot secrets (`paperclip-better-auth-secret`, `paperclip-tool-action-signing-secret`, `paperclip-secrets-master-key`; 32 random bytes from `crypto/rand`, hex), writes each to Secrets Manager under the managed path, stores only the ARN with `managed_by = 'afs'`, binds them as `BETTER_AUTH_SECRET`, `PAPERCLIP_TOOL_ACTION_SIGNING_SECRET` and `PAPERCLIP_SECRETS_MASTER_KEY` (`delivery: env`), inserts the team's `team_collaboration` row (web UI on, REST API off, `manual`) and emits one `secret.created` per boot secret. If any Secrets Manager write fails: `503 Unavailable`, no database row, and the secrets already written are deleted (force, no recovery window) best effort and then by the reconciler's orphan sweep (RC-27). Worker agents: `collaboration.connect` defaults to `true` when the team has a live collaboration agent, else `false`; `connect: true` with none → `ValidationError` (`field: collaboration.connect`); `connect` and, in `manual` mode, `paperclipAgentId` are writable by `UpdateAgent` with the effect of `ConnectAgentToCollaboration`. `ListAgents` accepts `?role=`; `GetAgent` returns `role` and `collaboration`. `DeleteAgent` of the collaboration agent returns `202` with the `Deleting` status; the database trigger removes the route and disconnects every agent (COL-8) | COL-1..3, COL-5, COL-7, COL-10, COL-34 | P0 |
| RA-20 | api | Managed secrets (API contract §4.6): `Secret.managedBy` (`user`, `afs`, `paperclip`) on every read and the `?managedBy=` filter; `UpdateSecret`, `DeleteSecret` and `PutSecretValue` on `afs` and `paperclip` secrets → `409 Conflict` (`details.reason = managed_secret`); `RotateSecret` on an `afs` secret takes no body (AFS generates the value) and is allowed except `paperclip-secrets-master-key` (`ValidationError`, `reason: master_key_not_rotatable`); on a `paperclip` secret it is `409 Conflict` (`managed_secret`) (PC-D30); binding a `paperclip` secret → `ValidationError`; `CreateSecret` refuses the name `paperclip` and any name starting `paperclip-` except `paperclip-agent-api-key` on a worker agent (`reserved_name`). `RotateCollaborationKeys` writes a new 32-byte hex value to the two signing secrets (default) with `PutSecretValue`, returns `{rotated[], signsOutEveryone: true, agentRolloutPending: true}`, emits `secret.rotated` per secret, bumps the agent's generation so the pod rolls, and refuses `paperclip-secrets-master-key` and any other name with `400 ValidationError` (`field: names`). No secret value in any response, log, audit, event or error | SEC-1, SEC-2, SEC-7, COL-7 | P0 (rotate P1) |
| RA-21 | api | Catalog and settings additions: `AgentTemplate.capabilities[]` (create, update, list, `?capability=`; known value `paperclip`; removing it from a template a live collaboration agent uses, or adding it to one a live worker uses → `409`); `Team.mayExposeCollaboration` on `CreateTeam`, `UpdateTeam` and reads, changeable only with `teams:set-guardrails` (a team-admin sending it → `403` with `details.requiredPermission`); identity providers' optional `collabClientId` and `collabClientSecretArn` (reference only; needs the id; never returned by `ListSignInProviders`; `admin:identity-providers`); the seven `collab.*` settings of change PRD §6.9 with their types and ranges in `GetSettings` and `PutSetting` (`collab.base_domain` unset reads `null`); the collaboration kind of `GetTeamReport` and `GetServiceReport` (RA-6 machinery over `collaboration_access`, usage only; not schedulable); the seven new events of API contract §7 where `afs-api` is the writer (`collaboration.updated`; `secret.created`, `secret.rotated`) `CreateTeam` with a slug whose collaboration hostname is held (the `23505` named `collab_hostname_held`, database contract §7.2.5) → `409 Conflict`, `details.reason = hostname_held`, nothing written (PC-D30). | COL-23, COL-34, COL-35, COL-36, TEAM-9 | P0 (report P2) |
| RA-22 | api | Swarm templates with a collaboration agent (API contract §4.10a, PC-D29): `CreateSwarmTemplate` and `UpdateSwarmTemplate` accept the optional `spec.collaboration {templateId}`; the id must name an enabled agent template with capability `paperclip` (`ValidationError`, `field: spec.collaboration.templateId`); the field is stored and returned in `SwarmTemplate.spec`; disabling or deleting that agent template while a swarm template references it is `409`; pools never connect to Paperclip. `afs-api` does not instantiate the team or create secrets for it (RC-30) | COL-9 | P1 (C4) |
| RC-21 | reconciler | Render the collaboration agent (role `collaboration`) and nothing of this for a worker: the ClusterIP Service `paperclip` (port 3100, selector the agent's pod only, no other Service selects it, the Hermes API port is exposed by none); Paperclip's environment (`PAPERCLIP_DEPLOYMENT_MODE=authenticated`, `PAPERCLIP_DEPLOYMENT_EXPOSURE=public`, `PAPERCLIP_PUBLIC_URL=https://<hostname>` when a hostname exists, `PAPERCLIP_ALLOWED_HOSTNAMES` with the hostname and `paperclip.<namespace>.svc.cluster.local`, `HOST=0.0.0.0`, `PAPERCLIP_HOME=/var/lib/agent/paperclip`, and `AFS_PAPERCLIP_BOOTSTRAP=manual|managed` copied from `registration_mode`, which makes the pod's entrypoint perform Paperclip's bootstrap, PC-D26); the SEC-7 provider settings (`PAPERCLIP_SECRETS_PROVIDER=aws_secrets_manager`, prefix `<service>/<namespace>/<agent name>/paperclip/` built with the same path function as `afs.secret_path`, the environment KMS key id, the team's tags); the three boot secrets as ExternalSecrets with the env names of RA-19; one ServiceAccount, one Pod Identity association, one dedicated IAM role, one PVC (COL-4); the volume claim and snapshot schedule rules of RC-16. A change of any value rolls the pod through the pod-template hash. A hostname-less agent (base domain unset) still renders the Service and the environment without `PAPERCLIP_PUBLIC_URL` | COL-4, COL-5, COL-6, COL-7, SEC-7 | P0 |
| RC-22 | reconciler | Network and connection. For each connected worker (row `connect = true`, `link_state <> 'disabled'`): its three environment values (`PAPERCLIP_API_URL=http://paperclip.<namespace>.svc.cluster.local:3100`, `PAPERCLIP_AGENT_ID` from `paperclip_agent_id`, `PAPERCLIP_AGENT_API_KEY` as an ExternalSecret of its `paperclip-agent-api-key` secret) and a CiliumNetworkPolicy egress rule to the Paperclip pod on TCP 3100, same namespace only. On the Paperclip pod: one ingress policy admitting only connected agents of the namespace, pods labelled `app.kubernetes.io/name=afs-edge` in `afs-system`, and the reconciler pods in `afs-system`, on 3100; every other ingress stays denied (default deny of RC-1 unchanged). The collaboration agent's own egress is the external systems catalog's (RC-1 FQDN policy), no new rule. A connected worker whose key secret has no value yet (manual mode, key not created) starts without `PAPERCLIP_AGENT_API_KEY` and gets condition `CollaborationKeyMissing=True`; it never fails the pod. Changing `connect`, `paperclip_agent_id` or the key secret rolls the agent. Only the reconciler writes these policies (RC-19) | COL-10, COL-11, COL-28 | P0 |
| RC-23 | reconciler | Admission additions to RC-19 for `afs-t-*` namespaces: refuse `Ingress`; Gateway API `Gateway`, `HTTPRoute`, `GRPCRoute`, `TCPRoute`, `TLSRoute`, `UDPRoute` (the kinds present in the cluster's installed CRDs; the policy matches the API group `gateway.networking.k8s.io` as a whole); a Service with `externalIPs` or `loadBalancerSourceRanges`; any Service created or updated by a subject other than the reconciler whose selector is empty or contains a key of the collaboration pod's label set (RD-9); a Service named `paperclip` from anyone but the reconciler. LoadBalancer and NodePort stay refused (RC-19). Drift on the new policies is reverted like the others | COL-22, TEAM-4 | P0 |
| RC-24 | reconciler | Collaboration status and lifecycle. Condition `EndpointReady` (and phase of `team_collaboration` in `resource_status`, `resource_kind = 'team_collaboration'`, `resource_id = team_id`) is `True` only when the pod is ready, the Service has ready endpoints, and the route is publishable (base domain set and a hostname computed); on every change it emits `collaboration.endpoint_state_changed` with the reason vocabulary of §7 (`Ready`, `Starting`, `Paused`, `TeamSuspended`, `ServiceOff`, `GuardrailOff`, `BaseDomainUnset`, `RouteNotPublished`). `PaperclipReady` is `True` when Paperclip's health endpoint answers from inside the cluster (and in `managed` mode sign-up is closed, COL-27). Lifecycle follows RC-17: pause and team suspend keep data and the row; a deleting collaboration agent removes the Service and policies, the endpoint record disappears through the database trigger, and each connected agent rolls without its Paperclip values. Hostname holds: `collab_hostname_holds` rows past `held_until` are deleted daily; no early release | COL-8, COL-27, TEAM-9 | P0 |
| RC-25 | reconciler | Managed registration (`registration_mode = 'managed'`): for each connected agent create the Paperclip agent through Paperclip's API (name, AFS agent id in metadata, remote adapter, persona from the AFS agent), write the key Paperclip returns straight to Secrets Manager at the agent's `paperclip-agent-api-key` path and record the secret row and its `PAPERCLIP_AGENT_API_KEY` binding through `afs.record_collab_agent_key(team_id, agent_id, secret_arn)` (the reconciler cannot insert `afs`-managed rows itself, PC-D25), record `paperclip_agent_id`, set `link_state = 'linked'` and `last_synced_at`; on failure `failed` with a short sanitized `last_error` (no key, no content) and retry with backoff (1 s doubling to 5 min). Pausing the AFS agent pauses the Paperclip agent; disconnect or delete revokes the key in Paperclip, deletes the secret in Secrets Manager (30-day recovery window, D53), removes the row and binding with `afs.remove_collab_agent_key(team_id, agent_id)` and sets `disabled`. Both functions write their own audit record and no outbox row; the link events below are the reconciler's own outbox writes. The reconciler authenticates with the board key from the AFS-managed secret `paperclip-board-api-key` of the collaboration agent (written by the pod's bootstrap, RC-29); its role reads only secrets whose name is exactly that (IAM condition, RC-28). Events `agent.collaboration_linked` and `agent.collaboration_unlinked` (reasons `disconnected`, `agent_deleted`, `collaboration_agent_deleted`, `failed`) on the transition into and out of `linked` only. The Paperclip adapter is a port with a fake server for tests and is pinned to the Paperclip version of the template (PC-R5); on a contract mismatch `RegistrationHealthy=False` and agents keep working | COL-12, COL-14, COL-32 | P1 |
| RC-26 | reconciler | Roster guard and scanning (`managed` mode, every 10 minutes): read Paperclip's roster; any agent that is neither the orchestrator nor linked to an AFS agent, and any local-adapter agent other than the orchestrator, sets `UnmanagedPaperclipAgents=True` (names in the condition message, sanitized and cut to 500 characters) and emits `collaboration.unmanaged_agents_found` once per new name (reason `not_linked` or `local_adapter`); the set clears when gone. Scan new and changed issues and comments with the edge's pattern set (the shared `internal/domain/collab` scanner) and emit `collaboration.secret_detected` (`source: reconciler`, `action: detected`, `paperclipItemId`, `authorAgentId`, no value, no fragment); the cursor is the last scanned `updatedAt` per team, kept in memory and rebuilt from the last 24 hours on leader change; Paperclip content is never copied to the database, logs, traces or events. Sign-up check (`OpenSignUp`) and the Paperclip-created secret listing: hourly list of Secrets Manager metadata under `<service>/<namespace>/<agent>/paperclip/`, upserting `secrets` rows with `managed_by = 'paperclip'` (name rule of database contract §7.2.4), removing rows whose secret is gone; the reconciler never calls `GetSecretValue` on them | COL-13, COL-25, COL-27, SEC-7 | P1 |
| RC-27 | reconciler | Collaboration housekeeping: `afs.ensure_collaboration_access_partition(month)` monthly, at least one month ahead, and `drop_partitions_older_than(13)` already in RC-8; `collaboration.denied_spike` every minute from `collaboration_access` over the last 5 minutes per team (outcomes `denied_unauthenticated`, `denied_forbidden`, `denied_surface_disabled`, `denied_secret_detected` summed; strictly more than `collab.denied_spike_threshold`; payload of §7; at most one per team per 15 minutes, state kept in memory and rebuilt from the table on leader change so a failover can at worst repeat one); `afs.purge_collab_edge_sessions()` hourly (expired and revoked sessions, expired handoffs, database contract §7.2.8); the orphan sweep for boot secrets of a failed `CreateAgent` (secret under the managed path of an agent that does not exist, older than 10 minutes); the WAF allow-list sync (RC-28) | COL-30, COL-35, TEAM-9 | P0 (spike P0, sweep P1) |
| RC-28 | reconciler | IAM and WAF it owns for the feature: the collaboration agent's role policy (read its own secret path and `_shared`; `secretsmanager:CreateSecret`, `PutSecretValue`, `UpdateSecret`, `DeleteSecret`, `TagResource`, `GetSecretValue`, `DescribeSecret` only below `<service>/<namespace>/<agent>/paperclip/`, and `kms` use of the environment key through the Secrets Manager `kms:ViaService` condition; `secretsmanager:PutSecretValue` on the one exact ARN `<service>/<namespace>/<agent>/paperclip-board-api-key` and nothing else outside the prefix (PC-D26, COL-3); no other agent's path, no `bedrock:*`, SEC-7, COL-3); the reconciler's own read of secrets named exactly `paperclip-board-api-key` (policy in `deploy/terraform`, DP-4); and the sync of `collab.allowed_source_cidrs` into the two WAF IP sets Terraform creates (empty list means `0.0.0.0/0` and `::/0`, so the allow rule always holds), within 60 s of a change, with `wafv2:GetIPSet` and `wafv2:UpdateIPSet` on those two ARNs only | COL-3, COL-23, SEC-7 | P0 |
| RC-29 | reconciler | First admin and member sync. Bootstrap (COL-32, PC-D26, PC-D27): the pod's entrypoint performs Paperclip's bootstrap when `AFS_PAPERCLIP_BOOTSTRAP` is `manual` or `managed` (RC-21). In `manual` mode it prints the first-admin instructions to the pod log and the reconciler only surfaces `PaperclipReady` condition text naming the log command (no invite, no key). In `managed` mode the entrypoint writes the board key to `<service>/<namespace>/<collab agent>/paperclip-board-api-key` (the role's one exact-ARN `PutSecretValue`, RC-28) and the reconciler, which reads only that name, records it with `afs.record_collab_board_key(team_id, collab_agent_id, secret_arn)` (never bound to any agent), emits `collaboration.bootstrap_completed` `{teamId, agentId, mode}` once per bootstrap (no URL) and sends the first-admin invite to the creating team-admin as a direct email notification through the reconciler's notification delivery (RC-15); the invite URL is a credential and never reaches an event, audit record, log line or span. Member sync (`managed`, P2): for each user binding on the team create a Paperclip invite (collaborators and team-admins as members, viewers as viewers where Paperclip supports it) and deactivate the account when the binding is removed; group-only members are invited at their first edge sign-in through a reconciler work queue fed by `collaboration_access` `signed_in` rows with no invite; access is still decided by the edge | COL-32, COL-33 | P0 manual, P1 managed, P2 sync |
| RC-30 | reconciler | Swarm collaboration (PC-D29, database contract §7.2.9; P1, C4): for a work order team that `afs.instantiate_work_order_team` created with a collaboration agent (web UI on, REST API off, `manual`), the reconciler renders it exactly as RC-21 and, because `afs-api` is not in that flow, generates the three boot secrets itself (`paperclip-better-auth-secret`, `paperclip-tool-action-signing-secret`, `paperclip-secrets-master-key`: 32 `crypto/rand` bytes each, hex, created in Secrets Manager with the team's tags and the environment KMS key) and records each with `afs.record_work_order_secret`, passing the collaboration agent's `agt_` id in the pool argument (agent scope, `managed_by = 'afs'`, bound as `BETTER_AUTH_SECRET`, `PAPERCLIP_TOOL_ACTION_SIGNING_SECRET`, `PAPERCLIP_SECRETS_MASTER_KEY`; idempotent per name, so a retry after a crash neither duplicates nor regenerates a recorded secret). No value is logged. Teardown follows RC-10 | COL-9, COL-7 | P1 |
| EDGE-1 | edge | New binary `afs-edge` (`cmd/afs-edge`), the only internet-reachable component (COL-15). Clean Architecture layout: `internal/domain/collab` (routes, surfaces, decisions, scan patterns, access buckets; standard library only), `internal/usecase/edge` (use cases and ports; imports domain and authz only), `internal/adapter/edgehttp` (listener, reverse proxy, reserved paths, error pages), `internal/adapter/edgedb` (the ten `afs_edge` functions of database contract §7.2.5 and §7.2.8 and `LISTEN afs_desired_state`), `internal/adapter/edgesession` (the sealed `state` parameter, session id generation and the cookie), `internal/adapter/oidc` (reused; gains the code and PKCE client), `internal/adapter/edgelog` (access events). It connects as `afs_edge` (database contract §4.1), runs `/healthz`, `/readyz` (database reachable, schema version at least the 000020 build, route table loaded in the last 2 minutes) and `/metrics`, uses Pod Identity for its AWS calls (Secrets Manager `GetSecretValue` on the `state` sealing key and the collaboration client secrets only), and refuses to start when `afs.schema_version()` is lower than the version it was built against | COL-15, COL-16 | P0 |
| EDGE-2 | edge | Routing: map the host to a team through the route table (`afs.collab_routes()`), refreshed at start, on `afs_desired_state` notifications and every 30 s (a failed refresh keeps the last table for up to 5 minutes, then answers `503 upstream_unavailable`); settings from `afs.collab_settings()` on the same schedule. `404` with the §6 envelope and no detail for an unknown host, a team without a collaboration agent, or neither surface effective; `503 Unavailable` (`paused`, `team_suspended`, `starting`, `upstream_unavailable`, `Retry-After: 30`) when `ready = false`; forward only to `http://<service_name>.<namespace>.svc.cluster.local:<port>` from the table and to nothing else; never route by path; the edge does not check SNI (the ALB terminates TLS): it refuses with `404` any `Host` that is neither the apex nor a known team host, and the apex serves only `GET /oauth2/callback` (every other apex path is `404`); the `host_mismatch` reason no longer exists (PC-D21, PC-D23); with exactly one surface not effective, a request for it from a caller holding a binding on the team is `403 surface_disabled`, never `404` (PC-D31) | COL-16 | P0 |
| EDGE-3 | edge | Web UI sign-in and server-side sessions (COL-17, PC-D21, PC-D22): OIDC authorization code with PKCE (S256) against the team's provider with its `collab_client_id` and the client secret read once from the ARN in the route table (cached in memory 10 minutes, never logged); reserved paths `GET /_afs/signin`, `GET /_afs/handoff`, `POST /_afs/signout` on team hosts and `GET /oauth2/callback` on the apex only. The PKCE verifier, nonce, team and return path ride in an AEAD-sealed `state` parameter (10 minutes; key from a platform Secrets Manager secret with two active versions, rotated every 30 days) so the apex callback needs no cookie of a team host. After validating the ID token (issuer, audience = collab client id, expiry, nonce, signature through the shared JWKS cache) the callback generates a random 256-bit handoff code and calls `afs.collab_handoff_create(code_hash, team_id, host, issuer, subject, provider_id, groups)` with the SHA-256 of the code, then redirects to `https://<slug>.<base>/_afs/handoff?code=…`. The handoff on the team host generates a random 256-bit session id and calls `afs.collab_handoff_consume(code_hash, host, session_id_hash)`, which atomically consumes the code (single use across all replicas, 60 s, bound to the host) and creates the session row; no row means `401 token_invalid`. There is no `collab_session_create`. The cookie `__Host-afs-collab` (Secure, HttpOnly, SameSite=Lax, no Domain, Path=/) holds only the opaque session id; the plain id is never stored, only its hash. Each request resolves it with `afs.collab_session_get(id_hash)` (a live session; `last_seen_at` touched at most once a minute), checks the session's team against the host's team and the web UI's effectiveness (turning the web UI off refuses the team's sessions at once, no row is deleted), and uses the row's `groups` for `collab_principal` (no groups in the cookie, so no size limit to design around). Sign-out calls `afs.collab_session_revoke(id_hash)`, clears the cookie and records `signed_out`. Session lifetime is `collab.session_ttl_minutes`, applied by the database at consume. The reconciler purges old rows (RC-27) | COL-17, COL-20 | P0 |
| EDGE-4 | edge | Authorization (COL-18, COL-19): `collab_principal(team, subject, provider, groups)` or `collab_principal_key(team, sha256)` per decision, with a session's `groups` from `collab_session_get`, cached at most 60 s per session (or API key) and team (a negative result cached 10 s); roles map to permissions through `internal/authz`; `GET` and `HEAD` need `collaboration:access`; every other method and every client WebSocket frame need `collaboration:contribute`; REST API traffic also needs `collaboration:api`; WebSocket and SSE by a caller with only `collaboration:access` may watch and a client frame closes with code 1008 (PC-D17); workload and security-operator principals are never admitted; `X-Afs-Authorization` tokens use the issuer, audience, expiry and JWKS rules of RA-2, with the audience being the team provider's collaboration client id or console client id (RD-6), and team API keys (`afsk_…`) are hashed with SHA-256 and checked with `collab_principal_key` (state `active`, role holding `collaboration:api`); the traffic classes and the status table of API contract §4.4c (`400 ambiguous_credentials`, `malformed_credential`; `401 token_invalid`, `token_expired`, `key_expired`, `key_revoked`, `session_expired`; `403 no_team_role`, `permission_required`, `surface_disabled`) are returned exactly; a service admin entering a team is flagged `serviceAdminEntry` when no team role would have granted the access | COL-18, COL-19, COL-27 | P0 (REST API surface P1) |
| EDGE-5 | edge | Forwarding, limits and streams (COL-21, COL-30): strip the AFS cookies, `X-Afs-Authorization`, `X-Amzn-*` and client `X-Forwarded-*`; set `X-Forwarded-For` (the client address the ALB reports), `X-Forwarded-Proto` and `X-Request-Id`; add `Strict-Transport-Security: max-age=31536000; includeSubDomains`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `Content-Security-Policy: frame-ancestors 'none'` merged with the upstream policy; no CORS headers added; body limit `collab.max_body_mb` (`400 body_too_large`, as in §1 there is no `413`); `Content-Encoding` other than identity on a request body while scanning is not `off` → `400 unsupported_encoding`; per-principal limit 20 requests per second, burst 40 (token bucket per replica, each replica enforcing its share like RA-15), `429 Throttled` with `Retry-After`; WebSocket and server-sent events proxied with a 10-minute idle timeout and no response buffering; a response that carries a once-only Paperclip key is passed through unchanged and the edge never logs response or request bodies (PC-D31); graceful shutdown drains streams for up to 30 s | COL-21, COL-30 | P0 |
| EDGE-6 | edge | Credential scanning (COL-24): text bodies (JSON, `text/*`, form, text multipart parts; binary skipped) and text WebSocket frames from callers with `collaboration:contribute` are scanned for the eight pattern types of §4.4c with the shared scanner; `block` answers `422 SecretDetected` with `details.patterns[]` and a `message` that names no value, `warn` forwards and records `secret_warned`, `off` skips; bodies are scanned while streamed through a bounded window (64 KiB with a 256-byte overlap) so a match spanning chunks is still found and memory stays bounded; each detection is counted and `afs.emit_collab_secret_detected` is called at most once per team, person and pattern per 5 minutes with the accumulated `count`; matched text exists only inside the scanner and is never copied to a log line, error, span, metric label, event or the `422` body | COL-24, SEC-7 | P1 |
| EDGE-7 | edge | Records and telemetry (COL-26, COL-31, COL-36): five-minute counts per team, surface, principal kind and id, outcome and `service_admin_entry` flushed to `afs.record_collab_access` every 30 s and on shutdown (at most 1000 rows per call; a failed flush is retried and the buffer capped at 100,000 keys with a dropped-count metric); sign-in, sign-out and denied events as one JSON line each on stdout with the field `afs.team_id` (PC-D24), principal id or key prefix, method, route template, status, bytes, duration and `serviceAdminEntry`, routed to the team's log delivery by the node log collector's rule on `afs.team_id` (RD-8, DP-2) and to the platform's CloudWatch group kept 13 months; route templates come from a maintained list and an unknown path is recorded as its first segment only (PC-OQ-4); no body, header value, query string or raw path with ids; OpenTelemetry traces and RED metrics (requests, latency, denials by reason, sessions, scan detections by pattern, throttles, route table age) | COL-26, COL-31, COL-36 | P0 (counts for reports P2) |
| DP-2 | deploy | Helm: `afs-edge` Deployment (at least 3 replicas, topology spread across AZs, PodDisruptionBudget `minAvailable: 2`, Pod Identity ServiceAccount, `restricted` security context, read-only root filesystem, resources and HPA values), Service, `TargetGroupBinding` to the Terraform target group, NetworkPolicy (ingress 8443 only from the ALB subnets and kubelet probes; egress to the database, DNS, the IdP issuers and Secrets Manager endpoint, and to team namespaces' `paperclip` pods on 3100 only), ServiceMonitor, dashboards and burn-rate alert rules for edge availability and latency; a values file `values/edge.yaml`; the node log collector's (Fluent Bit) routing rule that sends the records of `afs-system/afs-edge` carrying `afs.team_id` to that team's aggregator with the OPS-4 redaction, plus the platform CloudWatch output kept 13 months (COL-31, PC-D24; no new network path); chart tests assert each object and that no `Ingress`, `LoadBalancer` or `NodePort` is rendered for it | COL-15, COL-16, NFR | P0 |
| DP-3 | deploy | Terraform module `collab-edge` (public entry): internet-facing ALB in public subnets; HTTP listener redirecting to HTTPS; HTTPS listener with an ACM certificate covering `<collab.base_domain>` and `*.<collab.base_domain>` (PC-D21), TLS policy `ELBSecurityPolicy-TLS13-1-2-2021-06`; target group (`ip`, HTTPS to port 8443, health check `/readyz`) of `afs-edge` pods only; a listener rule that forwards only hosts matching the base domain and its subdomains and answers a fixed `404` otherwise; Route 53 alias records for the apex and the wildcard; WAFv2 regional web ACL with the managed groups `AWSManagedRulesCommonRuleSet`, `AWSManagedRulesKnownBadInputsRuleSet`, `AWSManagedRulesAmazonIpReputationList` and `AWSManagedRulesAnonymousIpList`, a rate-based rule of 2,000 requests per 5 minutes per source IP, and the allow-list rule over two IP sets whose addresses are ignored by Terraform (`lifecycle.ignore_changes`) and owned by the reconciler; ALB access logs and WAF logs to S3 and CloudWatch with 13-month retention; the ALB security group allows 443 from `0.0.0.0/0` and `::/0` and egress to the edge pods only; the module is created only when `collab_base_domain` is set | COL-15, COL-23, COL-30 | P0 |
| DP-4 | deploy | Terraform IAM and secrets: the `afs-edge` Pod Identity role (Secrets Manager read on the `state` sealing-key secret and the collaboration client secrets by name pattern, no other service), the `afs_edge` database login credentials reference, the session-key secret with 30-day rotation, the collaboration agent role's added `PutSecretValue` on the exact `…/paperclip-board-api-key` ARN (RC-28), and the reconciler's added statements (read of secrets named `paperclip-board-api-key` under `<service>/*` by exact-name condition; `wafv2` on the two IP sets; Secrets Manager listing and tagging below `…/paperclip/`). `TestIAMLeastPrivilege` gains the cases; no wildcard resource or `*` action is introduced | COL-12, COL-23, SEC-7 | P0 |

**Traceability, change PRD to this repository.** COL-1: RA-19. COL-2: RA-19, RA-21. COL-3: RC-28. COL-4..6: RC-21.
COL-7: RA-19, RA-20, RC-21. COL-8: RC-24. COL-9: RA-US-19b (ephemeral teams; swarm templates are gap G10). COL-10, COL-11:
RC-22, RA-19. COL-12: RC-25, RC-29. COL-13, COL-14: RC-26, RC-25. COL-15: EDGE-1, DP-2, DP-3. COL-16: EDGE-2. COL-17: EDGE-3.
COL-18, COL-19: EDGE-4. COL-20: RA-17, EDGE-2, EDGE-4. COL-21: EDGE-5. COL-22: RC-23. COL-23: RA-21, RC-28, DP-3.
COL-24: EDGE-6. COL-25: RC-26. COL-26: EDGE-7 and the no-content tests of every story. COL-27: EDGE-4, RC-26. COL-28: RC-22,
RC-28. COL-29: not code; the `user-docs/` runbook (§10). COL-30: EDGE-5, RC-27, DP-3. COL-31: EDGE-7. COL-32, COL-33: RC-29.
COL-34, COL-35: RA-17, RA-19..21, RC-24..27. COL-36: RA-21, EDGE-7. SEC-7: RC-21, RC-26, RC-28. TEAM-3, TEAM-4, TEAM-9
changes (change PRD §6.8): RC-22, RC-23, RC-24. ACC-2 role: RA-18.


### 4.3 Requirements owned for the team model gateway (1.2.0)

Added by the change PRD [`model-endpoint-gateway-PRD.md`](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/model-endpoint-gateway-PRD.md)
(approved by J, 2026-11-25; Appendix D.3). The requirement ids MEG-GW-1..8, MEG-RC-1..6, MEG-BL-1..3 and MEG-API-1..5 and
their tests are listed in [`specs/261125-model-endpoint-gateway/spec.md`](../../261125-model-endpoint-gateway/spec.md); the
contracts are authoritative for names and shapes (gateway contract v1.2.0 §12, API contract v1.5.0 §3 and §4.13, database
contract v1.5.0 §7.4, migration 000023). The stories below refine the change PRD's US-60..US-80.

**MEG-US-1 · A service route, priced and switched on.** *(MEG-API-1, 2, 4)*
- Given a service admin, `CreateModelEndpoint` creates a disabled route with its provider taken from the host;
  `CreateEndpointModelPrice` appends an effective-dated price; enabling a route on a provider that is off is
  `provider_disabled`. Every mutation writes row, audit and outbox in one transaction; updates need `If-Match`. A team-admin
  is denied every `/v1/admin/model-endpoints` operation [handler, repository against the schema].

**MEG-US-2 · Credentials are references.** *(MEG-API-3)*
- `PutModelEndpointCredential` and `RotateModelEndpointCredential` write the value to Secrets Manager at the database's
  path and return `secretArn`, `status` and `rotatedAt`; no response, log, audit, event or error carries the value
  [handler with fake Secrets Manager].

**MEG-US-3 · A team on its own gateway.** *(MEG-RC-1, 2, 4)*
- Given `PutTeamGatewaySettings {deployment: per_namespace}`, the reconciler renders `afs-gateway` (2 replicas,
  PodDisruptionBudget, Service on 8443), ExternalSecrets for the route credentials and the database URL, a serving
  certificate, and a CiliumNetworkPolicy whose egress is DNS, the route hosts, Bedrock while it is on, the federation token
  hosts and the database; agents' gateway egress goes to it. Back to `shared`, the objects are removed [render tests].

**MEG-US-4 · Agents see their routes.** *(MEG-RC-3)*
- An agent of a per_namespace team with routes gets `AFS_LLM_ROUTES`, `AFS_LLM_<ROUTE>_BASE_URL`, `OPENAI_BASE_URL` on
  `$(AFS_GATEWAY_URL)`, a route default as `AFS_LLM_DEFAULT_ROUTE`/`MODEL`, and `/etc/afs/llm-routes.json`; an agent on
  the shared gateway gets none of them and its pod template is unchanged from 1.1.0 [render and source tests].

**MEG-US-5 · Route health.** *(MEG-RC-6)*
- A route without its credential is `Unavailable` (`credential_missing`) and publishes one `model_endpoint.degraded`; the
  same health again publishes nothing; the credential arriving makes it `Ready` and publishes one
  `model_endpoint.recovered` [against the schema].

**MEG-US-6 · Calls through a route are metered and capped.** *(MEG-GW-1..8)*
- `POST /r/<route>/v1/chat/completions` reserves spend, writes the output cap into the request, injects and strips
  `include_usage`, settles to the larger of the gateway's and the provider's cost, and records `usage_events` with the
  route and timing columns; a database loss fails closed [gateway tests with fake upstreams].

**MEG-US-7 · Provider costs are reconciled.** *(MEG-BL-1, 2)*
- Given recorded OpenAI, OpenRouter, Azure and Google responses, each team's provider cost of a route is compared with the
  gateway's settled cost and a variance above `billing.variance_alert_pct` publishes `cost.variance_detected`; unnamed
  cost is reported as unattributed; a Nous or OpenCode route without a statement for the month is not reconciled
  [adapter with fixtures, against the schema].

**MEG-US-8 · Capacity and prices.** *(MEG-BL-3)*
- A capacity route's day cost is split across the team's agents and pools by token share and the parts sum to the
  amount; a listed price that differs from the price in force gives one pending proposal per model, none when equal, and
  a newer one supersedes it [use case, against the schema].

**MEG-US-9 · Reports by route.** *(MEG-API-5)*
- Usage and costs group by `route` and filter on a route slug or `bedrock`; `GetInferenceReport` gives per-route calls,
  errors, tokens, cost and latency percentiles in JSON and CSV [against the schema].

## 5. User stories and acceptance criteria

Stories refine the product stories (US-n). Each acceptance criterion maps to at least one automated
test; the test level is in brackets: [unit]; [adapter] (real PostgreSQL or fakes); [envtest]
(Kubernetes API); [e2e] (dev cluster); [e2e restore drill] (a fresh dev cluster and a restored database);
[load] (the initial scale of §6 against the dev cluster with a fake Bedrock); [contract test] (the
OpenAPI document and examples checked against the API contract and replayed against the server).
The Paperclip change adds [terraform test] (`terraform test` with `command = plan` and mock providers, plus the HCL
structure tests of `make deploy-test`; no AWS account) and [parked] (needs a real cluster or AWS account, listed in §8.1).
A line "Proven by" names the Go test, `terraform test` file or chart test that proves the criterion above it. Names
follow the existing style: `Test<Subject>` with `/<case>` subtests for tables.

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
- `GET /v1/identity-providers` works without a token, returns `consoleClientId` and `cliClientId` for every enabled provider (D48) and no client secret ARN [adapter].

**RA-US-2b · Workloads report progress.** *(US-16)*
- A pool's `afs-api` token from team A, assigned a work order, can `UpdateWorkOrderRepo` on that work
  order and call `GetMe` [adapter].
- The same token gets `404` on a work order not assigned to team A and on every route of team B, and
  `403` on every other route of team A and on service routes [adapter].
- A token with audience `afs-gateway`, an expired token or one from another cluster's issuer → `401`
  [unit].
- Validation is offline against the cluster issuer's JWKS: with the Kubernetes API unreachable a valid
  token still works, and no Kubernetes client is constructed in `cmd/afs-api` [unit with fake JWKS].

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

**RA-US-4 · Agents.** *(US-4, US-21, AGT-11, D54)*
- `CreateAgent` refuses an image without `@sha256:` [unit], a model the team has not enabled, a storage
  class not offered, and a binding of another agent's secret [adapter].
- `UpdateAgent` refuses shrinking the volume or changing its storage class [unit].
- Given a disabled template or an image digest not on the template's allowed list, when `CreateAgent` (or
  `CreatePool`) names it, then `400 ValidationError` naming the template or digest; an unknown template →
  `400` [adapter].
- `ListAgentTemplates` returns only enabled templates; a team-admin calling `PutAgentTemplate` or
  `DeleteAgentTemplate` → `403`; a service admin deleting a template an agent or pool uses → `409 Conflict`
  (disable it instead) [adapter].
- Given an existing agent whose template is then disabled, then it is not changed by the API and the
  reconciler flags it `TemplateRetired` (RC-5) [adapter].
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

**RA-US-6 · Optimistic concurrency, sessions and versions.** *(US-13, US-26, D47)*
- Update without `If-Match` → `428`; with a stale version → `412`; with the current version → 200 and
  `ETag` advanced by one [adapter].
- A transaction opened without the session settings sees no team rows [adapter].
- Every response carries `X-Request-Id`; a client-sent id of at most 64 characters is echoed and appears
  in the error body and the audit record [adapter].
- Changing a team's `kind` → `400 ValidationError` [adapter].
- Every response, including `401`, `403`, `404` and `429`, carries `X-Afs-Contract-Version` equal to the
  contract's version [contract test].

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
- A replayed `CreateApiKey` or `CreateServiceApiKey` returns the key's metadata without `secretKey` and
  with `Idempotent-Replayed: true`, and no stored response contains `secretKey` (D52) [adapter].

**RA-US-10 · Ephemeral teams and pools.** *(US-15, US-17)*
- `CreateTeam` with `kind: ephemeral` and no expiry, rule or lifetime budget → `400`; an expiry beyond
  `teams.ephemeral_max_days` → `400`; with them, one transaction writes team and budget [adapter].
- Making an ephemeral team persistent or removing its lifetime budget → `400 ValidationError` [adapter].
- Pool operations on a persistent team → `409`: `afs-api` checks the team's `kind` before the write, so the
  database's `23514` is only a backstop; a pool named like an existing agent → `409`; `ScalePool`
  outside 0–200 → `400` [adapter].
- A pool role link in an account outside the team's allowed accounts, or with a role name other than the
  pool's, → `400 ValidationError` [adapter].
- Viewers can read pools and get `403` on every pool mutation [adapter].

**RA-US-11 · Work orders for security.** *(US-16, US-22, D38)*
- A security operator (person or service-level key) can create, update, cancel and list all work
  orders, filter them by `teamId`, list and describe swarm templates and use `ListTeamDirectory`; it gets
  `403` on creating or changing a template and on any team route [adapter].
- Owner resolution (D38): given team A listing `github.com/acme/*` and team B listing
  `github.com/acme/payments-api` in `AFS_OWNED_REPOS`, when `CreateWorkOrder` names the repository
  `acme/payments-api` with no owner, then its owner is team B (an exact entry beats a wildcard) and the
  response shows `ownerSource: team_config` [adapter].
- Given two teams that both list `github.com/acme/*`, when a repository `acme/other` is given with no owner,
  then `400 ValidationError` with `details.ambiguousRepos` naming both team ids, and no work order exists
  [adapter].
- Given no team lists a repository and none is given, then `400 ValidationError` with
  `details.unownedRepos`, and no work order exists [adapter].
- Given a repository that team B lists, when the request names team A (or an email) explicitly, then the
  explicit owner wins (`ownerSource: explicit`); a deleted team's entry is skipped [adapter].
- A second open work order for the same CVE → `409`; a repository with neither an explicit owner nor a resolved one → `400` (see above) [adapter].
- Given a swarm template that a service admin has disabled, when a security operator calls `CreateWorkOrder`
  naming it, then `400 ValidationError`; the template's revisions stay readable and existing remediation
  teams are unchanged [adapter].
- A team-admin sees work orders assigned to the team or touching its repositories, read-only, through
  `ListWorkOrders` and `ListTeamWorkOrders`; an unrelated team gets `404` [adapter].
- The assigned team's agents and pools can update repository status with `work-orders:report`; setting
  `pr_opened` without a PR link → `400` [adapter].
- A team-admin adding a security operator binding → `403`; only service admins can [adapter].

**RA-US-12 · Guardrails stay with service admins.** *(US-18, ACC-5, TEAM-6)*
- A team-admin changing `isolationTier` or `viewerKubectl` → `403`; `GetTeamKubeconfig` with it off →
  `403`, with it on returns a kubeconfig that uses an exec credential plugin and contains no token
  [adapter].

**RA-US-13 · Budgets within the ceiling.** *(US-9, US-24, BUD-1, BUD-10, D33, D34)*
- A team-admin setting a budget above the ceiling → `400 ValidationError` [adapter].
- A service admin sets `ceilingOverrideUsd` with an expiry; while it is unexpired a team-admin can set a
  budget up to the override; a team-admin setting the override → `403` [adapter].
- `onExhaust: notify` on an ephemeral team → `400`; on a persistent team by a team-admin → `403`; by a
  service admin → 200 [adapter].

**RA-US-14 · Export, import and restore.** *(US-13, US-20, CFG-5, CFG-6, D49)*
- An export bundle contains no secret value, API key hash or client secret, and its SHA-256 matches its
  content [adapter].
- `CreateImport` with `?dryRun=true` (the default) returns `{dryRun: true, teams[…]}` with a create, update
  or skip result per team, lists secrets whose values must be put again, and writes no desired-state row
  [adapter].
- With `?dryRun=false` the request-body bundle is applied synchronously, one transaction per team: account
  ids are mapped by the account map, one audit record is written per resource, every imported role link is
  `pending_trust`, and a team that fails is reported `failed` with nothing written for it [adapter].
- A bundle whose checksum does not match → `400 ValidationError` and nothing is written; a bundle over
  100 MB → `400 ValidationError` [adapter].
- `afs-api` makes no S3 call for an import or an export [unit].
- (US-20) `RestoreRevision` with `dryRun` returns the diff and writes nothing; without it, the team's resources
  match the revision, `generation` is the previous value + 1, and one audit record names the source
  revision [adapter].
- A viewer calling `RestoreRevision` → `403`; a non-service-admin calling `CreateExport`, `GetExport` or
  `CreateImport` → `403` [adapter].

**RA-US-15 · Rate limits, settings and service-level access.** *(US-21, US-26, IF-1)*
- One principal sending 60 requests within one second gets at least 40 successes and `429 Throttled`
  with `Retry-After` for the rest; another principal in the same second is not throttled [adapter].
- A browser request from an origin not in `api.cors_origins` gets no CORS allow headers; one from a
  listed origin does [adapter].
- `PutSetting` with an unknown key or a value of the wrong type or range → `400 ValidationError`; a
  non-service-admin → `403` [adapter].
- Disabling an identity provider that a team uses → `409 Conflict`; adding a provider and marking it the
  default is audited, and `ListSignInProviders` then lists both with the default flagged [adapter].
- A service-level API key is created only by a service admin, holds only the `security_operator` role and
  shows its secret once; it can call the work order operations, `ListSwarmTemplates`, `GetSwarmTemplate`,
  `ListTeamDirectory` and `GetMe`, and gets `403` on everything else, every team-scoped route included
  [adapter].
- Deleting a service-level notification channel named in `work_orders.notification_channel_id` → `409`; a
  team-admin choosing a service-level channel for a report schedule → `400 ValidationError` [adapter].

**RA-US-16 · Suspend and delete a team.** *(US-23, TEAM-1, TEAM-9)*
- Given a suspended team, when a person or API key calls any mutation except `ResumeTeam` and `DeleteTeam`,
  then `409 Conflict` with `details.reason = team_suspended`; reads still succeed [adapter].
- A team-admin calling `SuspendTeam` succeeds; a team-admin calling `DeleteTeam` → `403` and one denied
  audit record; a service admin's `DeleteTeam` returns the team with `state = deleting` and
  `status.phase = Deleting` [adapter].
- After deletion `GET` of the team, its usage, costs, revisions and audit still return for service admins,
  with `state = deleted` [adapter].
- The secrets of a deleted team are scheduled for deletion with a 30-day recovery window (RC-17, RC-US-15)
  [adapter with fake AWS].

**RA-US-17 · Log destinations and notification channels.** *(US-19, OPS-1, OPS-3)*
- A team-admin creates a CloudWatch, S3, OTLP, Splunk HEC or Datadog destination whose credentials are a
  secret reference; a request carrying a credential value inline → `400 ValidationError` [adapter].
- A viewer calling `CreateLogDestination` or `CreateNotificationChannel` → `403` and one denied audit
  record; the same viewer can list and describe both [adapter].
- `TestNotificationChannel` on a channel whose delivery fails returns the failure and records it on the
  channel [adapter with fake Slack].

**RA-US-18 · Cost allocation tag status.** *(TAG-7, US-21)*
- `ListCostTagStatus` returns each AFS tag key with `seenInCur` and `awsStatus` to a service admin; a
  team-admin → `403` [adapter].
- `PutSetting` of `billing.tag_activation_acknowledged = true` by a service admin acknowledges the banner
  and is audited; a non-boolean value → `400 ValidationError` [adapter].

**RA-US-19 · Collaboration API.** *(US-27, US-30, US-33; COL-20, COL-34, COL-35)* As a team-admin, I want to read and set the team's collaboration settings and connect agents, so that I publish only the surfaces we use. (RA-17)
- Each of the seven operations is in `api/openapi.yaml` with its permission (`collaboration:read` for `GetTeamCollaboration` and `ListCollaborationLinks`; `collaboration:manage` for `PutTeamCollaboration`, connect, disconnect and `RotateCollaborationKeys`; `admin:settings` for `ListCollaborationEndpoints`), its CLI and SDK names and its examples; the contract test finds no operation missing and none extra against `api/contract/*.json` resynced from API contract 1.3.0 [contract test]. Proven by: `TestContractOperations`, `TestEveryRouteDeclaresAPermission`.
- For each of viewer, collaborator, team-admin, service admin, security operator, team API key of each team role, service-level key and workload principal, and an unbound user, every operation returns the §3 answer: `2xx` when allowed; `404` for an unbound user on a team route; `403 AccessDenied` with `details.requiredPermission` for a bound caller without the permission (a viewer or collaborator calling `PutTeamCollaboration` gets `403` and one denied audit record); `403` for a security operator on every team route; `403` for every non-service-admin on `ListCollaborationEndpoints` [adapter]. Proven by: `TestCollaborationAuthzMatrix/<operation>/<caller>`.
- Given a team with no collaboration agent, when `GetTeamCollaboration` or `ListCollaborationLinks` is called, then `404 NotFound` with `details.reason = collaboration_agent_missing`; when `PutTeamCollaboration`, connect, disconnect or rotate is called, then `409 Conflict` with the same reason [adapter]. Proven by: `TestCollaborationMissingAgent`.
- Given a suspended team, every mutation of this story returns `409 Conflict` with `details.reason = team_suspended` and writes no row, audit or outbox record; reads still work [adapter]. Proven by: `TestCollaborationSuspendedTeam`.
- Given `collab.base_domain` unset, when a surface is switched on, then `400 ValidationError` with `details.fields[0]` `{field: "webUiEnabled", reason: "collab_base_domain_unset"}` (or `restApiEnabled`); likewise `collab_public_endpoints_off`, `collaboration_not_allowed_for_team` and, for the web UI only, `provider_without_collab_client`; `registrationMode: managed` while the agent is not `Ready` → `collaboration_agent_not_ready`; switching a surface off is always accepted; several failed reasons are all listed [adapter]. Proven by: `TestPutTeamCollaborationValidation/<reason>`.
- Given a valid `PutTeamCollaboration` with `If-Match: 4`, then the row, one audit record and one `collaboration.updated` outbox event whose `data.changes` lists only the fields that changed (`from`, `to`) are written in one transaction, the response has `status.phase = Pending` and `version` 5, and a stale `If-Match` → `412 PreconditionFailed` with nothing written; omitting `If-Match` is accepted; omitting `registrationMode` keeps the current value [adapter]. Proven by: `TestPutTeamCollaborationWritesRowAuditOutbox`, `TestPutTeamCollaborationIfMatch`.
- Given the stored choice web UI on, `mayExposeCollaboration` off, then `GetTeamCollaboration` returns `webUiEnabled: true`, `effective.webUi: false` and `effective.reasons` containing `team_not_allowed`; each of `service_endpoints_off`, `base_domain_unset` and `provider_without_collab_client` appears when its condition fails; `hostname`, `webUiUrl` and `restApiUrl` are `null` while the base domain is unset, else `<slug>.<base>`, `https://<hostname>/` and `https://<hostname>/api` [adapter]. Proven by: `TestCollaborationEffectiveMatchesRouteFunction` (compares with `afs.collab_routes()` for the same fixtures, so the two implementations cannot drift) .
- `ConnectAgentToCollaboration` with `{paperclipAgentId}` in `manual` mode upserts `agent_collaboration` (`connect = true`, `link_state = pending`), writes one audit record and no outbox row, returns the `CollaborationLink` `200`, and is idempotent under the same `Idempotency-Key`; the same call with `paperclipAgentId` in `managed` mode → `ValidationError` (`field: paperclipAgentId`, `reason: managed_registration`); naming the collaboration agent or a pool → `ValidationError` (`field: agentId`); an unknown agent → `404`. `DisconnectAgentFromCollaboration` sets `connect = false` and writes audit only. Both are on the CD-7 exemption list and `outbox_exemptions_test.go` passes with 71 entries [adapter]. Proven by: `TestConnectAgent/*`, `TestDisconnectAgent`, `TestOutboxExemptionList`.
- `ListCollaborationLinks` returns one item per live worker agent (never the collaboration agent), a never-connected agent as `connect: false`, `linkState: disabled`, filters `linkState` and `connect`, paginates with `maxResults` and `nextToken`, and `lastError` never contains a Paperclip key [adapter]. Proven by: `TestListCollaborationLinks`.
- `ListCollaborationEndpoints` returns every team that has a collaboration row (hostname, stored and effective surfaces, `mayExposeCollaboration`, `phase`, `lastAccessAt` = newest `signed_in` or `allowed` bucket), filters by `teamId` and `state`, renders CSV with formula cells prefixed (`'`) like the other CSV exports, and a team caller or security operator gets `403` [adapter]. Proven by: `TestListCollaborationEndpoints`.
- Priorities (PC-D28): `GetTeamCollaboration`, `PutTeamCollaboration`, `ListCollaborationLinks`, `ConnectAgentToCollaboration` and `DisconnectAgentFromCollaboration` ship in C1, because `manual` registration needs the links, connect and `paperclipAgentId` before any edge exists; `ListCollaborationEndpoints` ships in C2 with the S10 tab; the OpenAPI `x-priority` of each equals the contract table [contract test]. Proven by: `TestCollaborationOperationPriorities`.

**RA-US-19a · The collaborator role in the authz matrix.** *(US-29, US-30; COL-18, COL-27, ACC-2, PC-D6)* As security, I want one catalog that knows `team_collaborator` and the five `collaboration:*` permissions. (RA-18)
- A test regenerates the permission and role tables from API contract §3 and asserts that `internal/authz` equals them exactly, including the five new permissions and the role `team_collaborator`; a permission or grant present in one and not the other fails the test (extends the catalog-equality test of RA-US-1) [unit]. Proven by: `TestCatalogEqualsContract`.
- The grant table is, per role: `team_viewer` holds `collaboration:read` and `collaboration:access`; `team_collaborator` holds every `team_viewer` permission plus `collaboration:contribute` and `collaboration:api`; `team_admin` and `service_admin` hold all five (`service_admin` also `admin:settings`); `security_operator` and the workload principal hold none of the five. Each cell has an allowed and a denied case, so a missing denial fails [unit]. Proven by: `TestCollaborationPermissionGrants/<role>/<permission>`.
- `CanGrant`: a team-admin may grant `team_collaborator` and still cannot grant `service_admin` or `security_operator`; a `team_collaborator` cannot grant any role; `AddMember` and `CreateApiKey` accept `team_collaborator` for a team and refuse it for service-level bindings and keys with `400 ValidationError` before the database's `23514` is reached [unit, adapter]. Proven by: `TestCanGrantCollaborator`, `TestAddMemberCollaborator`, `TestServiceKeyRejectsCollaborator`.
- `GET /v1/me` for a collaborator bound to team A returns, for A, every viewer permission plus `collaboration:contribute` and `collaboration:api`; for a team-admin all five; for a viewer `read` and `access` only; for a workload and a security operator none; `teams:set-guardrails` carries `mayExposeCollaboration` only for service admins [adapter]. Proven by: `TestGetMeCollaborationPermissions`.
- A collaborator inherits the conditional viewer grants (PC-D30): on a team with `viewerKubectl` on, `GetTeamKubeconfig` is allowed and `GET /v1/me` lists `teams:get-kubeconfig`; with it off, denied; a collaborator also holds `work-orders:read` for work orders assigned to, or touching a repository of, the team and not for others [unit, adapter]. Proven by: `TestCollaboratorConditionalViewerGrants`.
- A viewer-role and a collaborator-role team API key each get `403` with `details.requiredPermission = collaboration:manage` on `PutTeamCollaboration`, and `200` on `GetTeamCollaboration` [adapter]. Proven by: `TestCollaborationAuthzMatrix/api_key`.
- A team-admin sending `mayExposeCollaboration` in `UpdateTeam` gets `403` with `details.requiredPermission = teams:set-guardrails` and nothing written; a service admin succeeds, audit and `team.updated` are written, and the team's `effective` flags change on the next read [adapter]. Proven by: `TestUpdateTeamMayExposeCollaboration`.
- Re-sync: `make contract-check` fails when `api/contract/*.json` or `internal/authz/catalog_gen.go` was cut from a contract version older than 1.3.0 (ADR-022) [unit]. Proven by: `TestContractExtractVersion`.

**RA-US-19b · Create the collaboration agent with its boot secrets.** *(US-27; COL-1..3, COL-5, COL-7)* As a team-admin, I want one request that creates the Paperclip agent with its secrets and never shows me a key. (RA-19)
- Given a team with no collaboration agent and an enabled template with capability `paperclip`, when `CreateAgent` is called with `role: collaboration`, then within the one request: the agent row (`role = collaboration`, `iamIdentity = dedicated`, `volume.snapshotSchedule = daily`, `snapshotRetain = 14`, `onDelete = snapshot_then_delete`), three `secrets` rows with `managed_by = 'afs'` and the managed-path ARNs, three `agent_secret_bindings` (`env`, targets `BETTER_AUTH_SECRET`, `PAPERCLIP_TOOL_ACTION_SIGNING_SECRET`, `PAPERCLIP_SECRETS_MASTER_KEY`), the `team_collaboration` row (web UI on, REST API off, `manual`), one audit record, one `agent.created` and three `secret.created` outbox rows; the response is `202` with the agent in `Pending` and contains no secret value [adapter with fake Secrets Manager]. Proven by: `TestCreateCollaborationAgent`.
- The three values are 32 bytes from `crypto/rand` hex-encoded (64 characters), distinct per secret and per call; the fake Secrets Manager received exactly three `CreateSecret` calls with the team's tags and the environment KMS key and no other call; a log capture of the whole request holds none of the values, their ARNs' values or any prefix of them [adapter, unit]. Proven by: `TestBootSecretsAreRandomAndNeverLogged`.
- Given a second `CreateAgent` with `role: collaboration` for the team, then `409 Conflict` with `details.reason = collaboration_agent_exists`, and no Secrets Manager call was made (the API checks first; the `23505` of `agents_team_collaboration_uq` is the backstop and maps to the same answer under a race) [adapter]. Proven by: `TestSecondCollaborationAgentRefused`, `TestCollaborationAgentRaceMapsUniqueViolation`.
- Given a template without `paperclip`, then `400 ValidationError` with `field: template` naming it; a `paperclip` template for a worker → `400 ValidationError` (`field: template`); `iamIdentity: team_shared` → `400` (`field: iamIdentity`); `volume.snapshotSchedule: off` → `400` (`field: volume.snapshotSchedule`); a `collaboration` object in the body → `400` (`field: collaboration`) [unit]. Proven by: `TestCreateCollaborationAgentValidation/*`.
- Given Secrets Manager fails on the second of three writes, then the response is `503 Unavailable` (`Retry-After` set), no `agents`, `secrets`, binding, `team_collaboration`, audit or outbox row exists, and the one secret already written is deleted without recovery window by the compensating call (a failed compensation is logged by name only and left to the orphan sweep, RC-27) [adapter with fake Secrets Manager]. Proven by: `TestCreateCollaborationAgentSecretsFailureCreatesNothing`.
- Given a database failure after the three secrets were written, then the compensating deletes run and the response is `500` with the standard envelope [adapter]. Proven by: `TestCreateCollaborationAgentRollsBackSecrets`.
- `UpdateAgent` with a different `role` → `400 ValidationError` (`field: role`); a worker created while the team has a collaboration agent has `collaboration.connect = true`, and without one `false`, and `connect: true` with none → `400` (`field: collaboration.connect`); `UpdateAgent` with `collaboration.connect` is equivalent to connect and rolls the agent (generation bumped once) [adapter]. Proven by: `TestAgentRoleImmutable`, `TestWorkerConnectDefaults`, `TestUpdateAgentConnect`.
- `DeleteAgent` of the collaboration agent returns `202`, writes audit and `agent.deleted`, and (database trigger) `GetTeamCollaboration` then returns `404 collaboration_agent_missing` and every worker's link reads `connect: false` [adapter]. Proven by: `TestDeleteCollaborationAgent`.
- A collaboration agent may be created in a persistent or an ephemeral team (COL-9): in an ephemeral team the same request succeeds, the team's teardown (RC-10) deletes the agent through the ordinary finalizer and removes the route; the persistent-only rules (RA-14 `onExhaust: notify`) are unchanged [adapter]. Proven by: `TestCreateCollaborationAgentEphemeralTeam`.
- `GetAgent` and `ListAgents?role=collaboration` return `role` and, on workers, `collaboration{connect, paperclipAgentId, linkState, lastSyncedAt, lastError}`; the object is absent on the collaboration agent [adapter]. Proven by: `TestAgentRoleAndCollaborationFields`.

**RA-US-19c · Managed secrets and key rotation.** *(US-32; SEC-7, COL-7)* As a team-admin, I want AFS-generated and Paperclip-created secrets shown and protected, and the signing secrets rotatable. (RA-20)
- `ListSecrets` returns `managedBy` on every row (`user`, `afs`, `paperclip`) and `?managedBy=paperclip` filters; for `afs` and `paperclip` rows no value, no version id and no value-derived field is returned [adapter]. Proven by: `TestListSecretsManagedBy`.
- `UpdateSecret`, `DeleteSecret` and `PutSecretValue` on an `afs` or a `paperclip` secret → `409 Conflict`, `details.reason = managed_secret`, nothing written, Secrets Manager not called; `RotateSecret` on an `afs` secret takes no body (a body is `ValidationError`) and AFS generates the value: on `paperclip-better-auth-secret` it succeeds, on `paperclip-secrets-master-key` → `400` (`reason: master_key_not_rotatable`), on a `paperclip` secret → `409 Conflict` (`managed_secret`) (PC-D30) [adapter]. Proven by: `TestManagedSecretMutations/*`.
- `CreateSecret` with the name `paperclip`, `paperclip-anything`, or `paperclip-agent-api-key` on the collaboration agent or at team scope → `400` (`reason: reserved_name`); `paperclip-agent-api-key` on a worker agent succeeds with `managedBy = user`; a binding of a `paperclip` secret → `400` [adapter]. Proven by: `TestReservedSecretNames/*`, `TestBindPaperclipSecretRefused`.
- `RotateCollaborationKeys` with no body writes a fresh 32-byte hex value to both signing secrets with `PutSecretValue`, returns `200` `{rotated: [two items with secretId, name, versionId], signsOutEveryone: true, agentRolloutPending: true}`, emits two `secret.rotated` events (no value), bumps the agent generation once and writes one audit record; `{names: ["paperclip-secrets-master-key"]}` → `400` (`field: names`, `reason: master_key_not_rotatable`) and nothing is written; an unknown name → `400`; a viewer, collaborator, security operator or workload → `403` [adapter with fake Secrets Manager]. Proven by: `TestRotateCollaborationKeys/*`.
- No response, audit row, outbox payload, log line or trace span of this story contains a generated value (scan test over the captured outputs for the values the fake generated) [adapter]. Proven by: `TestNoSecretValueEscapes/collaboration`.

**RA-US-19d · Settings, guardrail, templates, providers and report kinds.** *(US-33; COL-23, COL-36)* As a service admin, I want the collaboration knobs in the API. (RA-21)
- `GetSettings` lists the seven `collab.*` keys with value, default and range (`collab.base_domain` unset reads `null`); `PutSetting` rejects an out-of-range value (`collab.session_ttl_minutes` 14 or 1441, `collab.max_body_mb` 0 or 101, `collab.denied_spike_threshold` 0, `collab.content_scan = "maybe"`, `collab.base_domain = "Not A Host"`, `collab.allowed_source_cidrs = ["10.0.0.1/33"]`) with `400 ValidationError` naming the key; a team-admin → `403` [adapter]. Proven by: `TestSettingsCollabKeys/*`.
- `ListAgentTemplates?capability=paperclip` returns only templates with it; `PutAgentTemplate` accepts `capabilities: ["paperclip"]` and refuses an unknown value; removing `paperclip` from a template a live collaboration agent uses → `409`; adding it to one a live worker uses → `409` [adapter]. Proven by: `TestAgentTemplateCapabilities/*`.
- `CreateIdentityProvider` and `UpdateIdentityProvider` accept `collabClientId` and `collabClientSecretArn` (an ARN, never a value; an ARN without an id → `400`; a non-ARN string → `400`), and `ListSignInProviders` never returns either field [adapter]. Proven by: `TestIdentityProviderCollabClient`.
- `GetTeamReport` and `GetServiceReport` with kind `collaboration` return `signIns`, `uniquePeople`, `requests`, `deniedRequests` and `secretDetections` from `collaboration_access`, grouped by `day`, `surface`, `reason` or `person`, only for the caller's teams, never content; the kind is not accepted by `CreateReportSchedule`; a person row carries `principal{id, type, name?}` and a `serviceAdminEntry` flag [adapter]. Proven by: `TestCollaborationReport/*`.
- The events `collaboration.updated`, `secret.created` and `secret.rotated` have the §7 envelope and `data` shapes (checked against the contract's examples) [contract test]. Proven by: `TestEventPayloadsCollaboration`.
- `CreateTeam` with a slug whose collaboration hostname is held (a deleted team that ever had a collaboration agent, within 30 days) → `409 Conflict`, `details.reason = hostname_held`, no row, audit or outbox record written; the database's `23505` with constraint name `collab_hostname_held` maps to it, while any other duplicate slug stays the ordinary `409`; after the hold expires (fake clock, hold row purged by RC-27) the same slug is accepted [adapter with PostgreSQL]. Proven by: `TestCreateTeamHostnameHeld`.

**RA-US-19e · Swarm templates with a collaboration agent.** *(US-27, US-33; COL-9, PC-D29; P1, C4)* As a service admin, I want a swarm template to give its remediation team a Paperclip for people to watch. (RA-22)
- `CreateSwarmTemplate` and `UpdateSwarmTemplate` accept `spec.collaboration {templateId}`, store it and return it in `SwarmTemplate.spec`; a `templateId` that is unknown, disabled or without capability `paperclip` → `400 ValidationError` (`field: spec.collaboration.templateId`); omitting it keeps current behaviour; a team-admin or security operator → `403` as for any template write [adapter]. Proven by: `TestSwarmTemplateCollaboration/*`.
- Disabling or deleting an agent template that an enabled swarm template references through `spec.collaboration` → `409 Conflict` listing the swarm templates; a swarm template with a `paperclip` agent template in a pool's `template` → `400` (pools never connect to Paperclip, COL-9 non-goal) [adapter]. Proven by: `TestAgentTemplateReferencedBySwarmTemplate`, `TestPoolCannotUsePaperclipTemplate`.
- `afs-api` neither instantiates the work order team nor writes a secret for it: a unit test over `CreateWorkOrder` asserts no Secrets Manager call (the reconciler's RC-US-20c does that) [adapter]. Proven by: `TestWorkOrderCreationDoesNotTouchSecretsManager`.

### afs-reconciler

**RC-US-1 · Converge a team.** *(US-1, US-3)*
- Given a new team row, the namespace, quotas, Pod Security labels, default-deny policies, team IAM role
  with boundary, the team's Bedrock role trusting only the gateway, SecretStore and inference profiles
  exist within 60 s (p95) and `resource_status` is `Ready` with `observedGeneration = generation`
  [envtest, e2e].
- A desired-state change is picked up from `afs_desired_state` within 5 s; with notifications blocked it
  is picked up by the 30 s poll [adapter].
- An agent whose template is disabled, or whose image digest is removed from the template's allowed list,
  keeps running and gets condition `TemplateRetired=True` within 60 s; re-enabling clears it (D54)
  [envtest].
- Two teams can each have an agent named `reviewer`; their IAM roles are named by id and carry the human
  names as tags [adapter with fake AWS].

**RC-US-1a · Admission.** *(US-3, US-18, TEAM-4)*
- Admission refuses NetworkPolicy, CiliumNetworkPolicy, LoadBalancer or NodePort Services, hostNetwork
  and privileged pods in team namespaces from any subject except the reconciler [e2e].
- A pod in team B tolerating team A's dedicated taint or selecting its nodes is refused [envtest].
- Deleting an admission policy or binding restores it within 60 s with a `drift.corrected` record
  [envtest].
- Given a pod in another team's namespace, or any pod outside team namespaces, when it connects to
  `afs-gateway`, then Cilium denies the connection; a team pod reaches it (US-25, D57) [e2e].

**RC-US-2 · Zero-impact agent pods.** *(US-4, US-5)*
- The rendered pod has: env vars from the ConfigMap and from Secret refs for env bindings; files for
  file bindings at their exact targets; `/etc/afs/config/<NAME>`; `/etc/afs/aws/config` when links exist;
  `/etc/afs/tags.json` and `AFS_TAGS`; `AFS_GATEWAY_URL`, `AWS_ENDPOINT_URL_BEDROCK_RUNTIME`,
  `ANTHROPIC_BASE_URL` and `ANTHROPIC_API_KEY=afs-gateway`; the gateway CA bundle at
  `/var/run/secrets/afs/gateway/ca.crt` named in `AWS_CA_BUNDLE`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and
  `NODE_EXTRA_CA_CERTS`; the optional gateway token file; no AWS keys (D57) [envtest].
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
- Every profile in the rendered `/etc/afs/aws/config` sets `role_session_name` to the agent's id (the
  pool's id for a pool), so a CloudTrail event in the linked account names the agent (D51) [adapter with
  fake STS].

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

**RC-US-13 · Notifications.** *(US-9, US-16, US-19, OPS-3)*
- A Slack channel row becomes one EventBridge rule filtered on the team id with an API destination; one
  `budget.threshold_crossed` produces exactly one Slack message [adapter with fake EventBridge and Slack].
- An email channel is delivered through SES with one message per event id, even when the event arrives
  twice [adapter with fake SES].
- A delivery failure (for example a Slack webhook returning an error, or a failed *Send test*) is retried and
  shown with its error on the channel's status [adapter with fake Slack].
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

**RC-US-15 · Suspend, resume and delete.** *(US-23, TEAM-1, TEAM-9)*
- Suspending a team brings every agent and pool to 0 replicas within 60 s, keeps volumes, IAM and
  secrets, and the gateway refuses the team's calls; resuming restores each agent and pool to its prior
  state, and an agent paused before the suspend stays paused [envtest].
- Deleting a team runs the finalizer steps in order (recorded by fakes), shows `Deleting` with the
  current step as a condition, resumes after a failed step on the next pass, and ends with the row
  `deleted` while its audit and usage stay readable [envtest with fake AWS].
- Deleting a persistent team schedules every one of its secrets for deletion in Secrets Manager with a
  30-day recovery window (not an immediate delete) before the namespace goes [adapter with fake AWS].

**RC-US-16 · Log delivery and redaction.** *(US-14, OPS-1, OPS-4)*
- A log line containing the value of one of the agent's bound secrets is delivered as
  `[redacted:<hash prefix>]`, and the same value gives the same token each time [e2e].
- Strings that look like passwords, bearer tokens, AWS keys, private keys or JWTs are redacted [unit].
- A team's logs never reach another team's destination, and its aggregator holds no other team's
  credentials [e2e].
- Adding or deleting a destination changes the outputs within 60 s [envtest].
- An agent's connection to a system's sign-in endpoint appears as a sign-in event (agent or pool worker,
  system, time) in the team's log delivery and in no database table or event [adapter].

**RC-US-17 · Export bundles.** *(US-13, CFG-5, D49)*
- An export request produces one KMS-encrypted bundle in S3 whose checksum and size are recorded, with the
  request `succeeded` [adapter with fake S3].
- `GetExport` on a succeeded export shows `downloadUrl`, valid 7 days, and `download_expires_at`; after
  expiry a new export is needed [adapter with fake S3].
- A failed build records `failed` with a summary and no checksum [adapter].

**RC-US-18 · Housekeeping jobs.** *(US-13, OPS-2, IF-1, D36)* (RC-8)
- The outbox publisher delivers every event to EventBridge at least once and in order per aggregate; a
  failed publish is retried with `attempts` and backoff and a duplicate is harmless to consumers
  [adapter with fake EventBridge].
- Partitions of `usage_events`, `agent_activity` and `external_traffic` exist at least one month ahead; a
  run on the last day of a month finds next month's partitions present [adapter].
- The organization account mirror runs hourly; an account removed from the organization shows in the next
  run [adapter with fake Organizations].
- Idempotency keys past their TTL are purged every 5 minutes [adapter].

**RC-US-19 · Render the collaboration agent.** *(US-27; COL-4..7, SEC-7)* As a team-admin, I want the Paperclip agent to run as one pod with one identity. (RC-21)
- Given a collaboration agent, the reconciler renders the Agentic Teammates resource and, beside it, the ClusterIP Service `paperclip` (port 3100, selector this pod only), the environment of COL-6 (`PAPERCLIP_DEPLOYMENT_MODE=authenticated`, `PAPERCLIP_DEPLOYMENT_EXPOSURE=public`, `PAPERCLIP_PUBLIC_URL=https://<hostname>`, `PAPERCLIP_ALLOWED_HOSTNAMES=<hostname>,paperclip.<namespace>.svc.cluster.local`, `HOST=0.0.0.0`, `PAPERCLIP_HOME=/var/lib/agent/paperclip`), the SEC-7 settings (`PAPERCLIP_SECRETS_PROVIDER=aws_secrets_manager`, prefix `<service>/<namespace>/<agent>/paperclip/`, the environment KMS key, the team's tags), and the three boot-secret ExternalSecrets bound as `BETTER_AUTH_SECRET`, `PAPERCLIP_TOOL_ACTION_SIGNING_SECRET` and `PAPERCLIP_SECRETS_MASTER_KEY`; the object set is compared with a golden manifest [envtest]. Proven by: `TestCollaborationAgentRender/golden`.
- Given a worker agent in the same namespace, then it gets none of: the `paperclip` Service, any `PAPERCLIP_*` setting other than the connected-agent values of RC-US-19a, the SEC-7 settings, the boot-secret ExternalSecrets [envtest]. Proven by: `TestWorkerGetsNoCollaborationObjects`.
- The pod has one ServiceAccount, one Pod Identity association and one dedicated IAM role (named by id, D25), one volume claim mounted at `/var/lib/agent` (so `/var/lib/agent/paperclip` is on it), the snapshot schedule `daily`, retain 14; no second container exposes a Service port, and no Service port other than 3100 exists for the pod (the Hermes API server port is exposed by none) [envtest]. Proven by: `TestCollaborationOneIdentityOnePod`.
- Given the base domain unset, then the Service and the environment are rendered without `PAPERCLIP_PUBLIC_URL`, the hostname entry is absent from `PAPERCLIP_ALLOWED_HOSTNAMES`, and `EndpointReady` is `False` with reason `BaseDomainUnset` [envtest]. Proven by: `TestCollaborationRenderWithoutBaseDomain`.
- Given the base domain is set later, then within 60 s the environment gains `PAPERCLIP_PUBLIC_URL` and the pod rolls once (a settings change is picked up by the 30 s poll, RC-4) [envtest]. Proven by: `TestCollaborationRenderBaseDomainChange`.
- Given someone deletes the `paperclip` Service, edits its selector or edits the Paperclip environment, then drift correction restores it within 60 s with a `drift.corrected` audit record [envtest]. Proven by: `TestCollaborationDriftCorrected`.
- A model call from the collaboration pod is attributed to the collaboration agent by the gateway like any agent (source-pod identity, GW-1), and a budget exhaustion leaves the pod and the Service running (`BudgetExhausted=True` only) [adapter with fake Bedrock, envtest]. Proven by: `TestCollaborationAgentBudgetExhaustedKeepsServing`.
- The rendered environment holds no secret value: a scan of the rendered objects for the values of the fake secrets finds none, and the three boot secrets appear only as ExternalSecret references [unit]. Proven by: `TestCollaborationRenderHasNoSecretValues`.

**RC-US-19a · Connect agents and isolate Paperclip.** *(US-28; COL-10, COL-11, COL-28)* As a team-admin, I want connected agents to reach Paperclip and nothing else to. (RC-22)
- Given a connected worker with `paperclip_agent_id` set and a key secret, then its pod has `PAPERCLIP_API_URL=http://paperclip.<namespace>.svc.cluster.local:3100`, `PAPERCLIP_AGENT_ID` and `PAPERCLIP_AGENT_API_KEY` (ExternalSecret reference), the CiliumNetworkPolicy has exactly one new egress rule for it (the Paperclip pod, TCP 3100, same namespace), and nothing else about the pod changes (diff against the unconnected render) [envtest]. Proven by: `TestConnectedAgentRender/diff`.
- Given `connect` changes from false to true, or `paperclip_agent_id` or the key secret's version changes, then the pod-template hash changes and the agent rolls exactly once; a change of an unrelated agent does not roll it [envtest]. Proven by: `TestConnectedAgentRolls`.
- The ingress policy on the Paperclip pod admits only: connected agents of the same namespace, pods labelled `app.kubernetes.io/name=afs-edge` in namespace `afs-system`, and reconciler pods in `afs-system`, on TCP 3100. A policy test renders the Cilium policy and a selector evaluator checks a matrix of source pods (connected worker, unconnected worker, pod in another team namespace, edge pod, reconciler pod, a pod in `afs-system` with another label, a pod in a namespace named like the edge's) and expects allow only for the first, fourth and fifth [unit]. Proven by: `TestPaperclipIngressPolicyMatrix`.
- Given a worker with *Connect* off, or a pod in another namespace, when it connects to the Paperclip pod, then the connection is denied and Hubble reports a denied flow with reason `policy` that the traffic collector records in `external_traffic` [e2e, parked until a real cluster, see §8.1]. Proven by: `TestPaperclipIsolationE2E` (parked) and, with envtest, `TestPaperclipPolicyHasNoOtherIngress`.
- The Paperclip pod has no egress rule to any worker agent and no Service of the worker is selected by an egress rule of it (pull only, PC-D5) [envtest]. Proven by: `TestPaperclipNeverInitiatesToAgents`.
- Given a connected worker in manual mode whose `paperclip-agent-api-key` has no value yet, then the pod starts without `PAPERCLIP_AGENT_API_KEY`, condition `CollaborationKeyMissing=True` appears on the agent, the pod is not failed, and the condition clears and the pod rolls when the secret is created [envtest]. Proven by: `TestConnectedAgentKeyMissing`.
- Connecting grants nothing else: a connected agent's egress, secret bindings, IAM role and budget are byte-identical to the same agent unconnected, except the one rule and three values above [envtest]. Proven by: `TestConnectGrantsNothingElse`.
- Disconnect (or deletion of the collaboration agent) removes the three values and the egress rule and rolls the agent; the key secret in `manual` mode is left to its owner [envtest]. Proven by: `TestDisconnectRemovesValuesAndRule`.

**RC-US-19b · Admission: no other way in.** *(US-33; COL-22, TEAM-4)* As security, I want a team to be unable to publish anything. (RC-23)
- In an `afs-t-*` namespace, each of these is refused by the ValidatingAdmissionPolicy when issued by a team-admin's subject or any subject other than the reconciler: an `Ingress`; a Gateway API `Gateway`, `HTTPRoute`, `GRPCRoute`, `TCPRoute`, `TLSRoute` and `UDPRoute`; a Service with `externalIPs`; a Service with `loadBalancerSourceRanges`; a Service of type `LoadBalancer` or `NodePort` (existing); a Service named `paperclip`; a Service whose selector is empty; a Service whose selector contains `afs.io/agent-id`, `afs.io/collaboration`, `statefulset.kubernetes.io/pod-name` or another key of the collaboration pod's label set (RD-9); each refusal names the rule [envtest]. Proven by: `TestAdmissionRefuses/<object>`.
- The reconciler's own `paperclip` Service is accepted, and a team's ordinary ClusterIP Service for its own pods (selector on its own labels) is accepted [envtest]. Proven by: `TestAdmissionAccepts/*`.
- A team cannot publish a second Paperclip: creating an agent's StatefulSet or a Deployment running the Paperclip image with a Service on 3100 is not refused by admission (images are not policy), but the Service has no route, no ingress admission and no public hostname, so the isolation test asserts the host of the second one answers `404` at the edge (the edge routes only the `paperclip` Service of the route table) [envtest plus edge unit]. Proven by: `TestSecondPaperclipUnreachable`.
- Deleting or editing the new policies or bindings restores them within 60 s with a `drift.corrected` record; the policy bindings cover only namespaces labelled as AFS team namespaces [envtest]. Proven by: `TestAdmissionPoliciesDriftCorrected/collaboration`.
- The CRD list the policy refers to is optional: in a cluster without the Gateway API CRDs the policy still installs and the Gateway rules are inert [envtest]. Proven by: `TestAdmissionWithoutGatewayAPICRDs`.

**RC-US-19c · Endpoint state, lifecycle and housekeeping.** *(US-27, US-33; COL-8, COL-30, TEAM-9)* As a service admin, I want the endpoint's state reported truthfully and the leftovers cleaned. (RC-24, RC-27)
- `EndpointReady` is `True` only when the pod is ready, the Service has a ready endpoint and a hostname exists; otherwise `False` with the reason vocabulary of §7; each change of `ready` emits exactly one `collaboration.endpoint_state_changed` with `previousReady`, `reason` and `effective` (a flapping pod produces one event per change, never duplicates on a resync) [envtest, adapter]. Proven by: `TestEndpointReadyConditions/*`, `TestEndpointStateChangedEmittedOnce`.
- Pause of the collaboration agent, and suspension of the team, keep the PVC, the row and the Service, set reasons `Paused` and `TeamSuspended`, and resume restores `Ready` without recreating the volume [envtest]. Proven by: `TestCollaborationPauseSuspendResume`.
- Deleting the collaboration agent runs the RC-17 finalizer: scale to zero, the on-delete rule (`snapshot_then_delete` creates a final snapshot first), delete the Service and policies; the database trigger has already removed the route; every connected worker rolls without its three values and its link reads `disabled` [envtest, adapter]. Proven by: `TestDeleteCollaborationAgentFinalizer`.
- Hostname holds: rows of `collab_hostname_holds` with `held_until` in the past are deleted by the daily job and unexpired ones are kept [adapter]. Proven by: `TestHostnameHoldPurge`.
- `afs.ensure_collaboration_access_partition` runs monthly; a run on the last day of a month finds next month's partition present [adapter]. Proven by: `TestCollaborationAccessPartitions`.
- Denied spike: given 51 denied rows (any mix of the four denied outcomes) for one team in the last 5 minutes and threshold 50, then one `collaboration.denied_spike` is published within 2 minutes with the §7 payload (`denied`, `threshold`, `byReason`), delivered through the team's channels and the service admins' channels; exactly 50 does not fire; a second spike inside 15 minutes publishes nothing; after a leader change the in-memory state is rebuilt from the table and at most one repeat is possible [adapter with fake clock]. Proven by: `TestDeniedSpike/*`.
- Orphan sweep: a secret under `<service>/<namespace-of-no-agent>/…/paperclip-*` older than 10 minutes with no agent row is deleted, a younger one is kept, and one belonging to a live agent is never touched [adapter with fake Secrets Manager]. Proven by: `TestBootSecretOrphanSweep`.
- Session purge: the hourly job calls `afs.purge_collab_edge_sessions()` and logs the count; with the fake database holding a session expired 61 minutes ago, one revoked 61 minutes ago, one live, and a handoff expired 61 minutes ago, the first, second and fourth are removed and the live one stays [adapter with PostgreSQL]. Proven by: `TestPurgeCollabEdgeSessions`.
- IAM: the rendered role policy of a collaboration agent allows the Secrets Manager actions of RC-28 only below `<service>/<namespace>/<agent>/paperclip/`, plus `secretsmanager:PutSecretValue` on the one exact ARN `…/<agent>/paperclip-board-api-key` and on no other path outside the prefix (PC-D26) and `GetSecretValue` on its own agent path and `_shared`, and denies another agent's path, another namespace's path and `bedrock:*` (policy evaluation with a local simulator over the rendered JSON, no AWS) [unit]. Proven by: `TestCollaborationRolePolicy/*`.
- WAF allow-list sync: a change of `collab.allowed_source_cidrs` is written into the two IP sets within 60 s (v4 and v6 split correctly); an empty list writes `0.0.0.0/0` and `::/0`; an invalid entry is skipped with a condition on the reconciler's health, never a partial write [adapter with fake WAFv2]. Proven by: `TestWAFAllowListSync/*`.

**RC-US-20 · Managed registration.** *(US-28; COL-12, COL-14)* As a team-admin, I want AFS to keep Paperclip's roster in step with my agents. (RC-25)
- Against a fake Paperclip API (`internal/adapter/paperclip/paperclipfake`, an `httptest` server implementing the pinned Paperclip operations: create agent, revoke key, pause agent, list roster, list issues and comments): given `managed` mode and a connected agent, within 2 minutes (fake clock steps) a Paperclip agent exists with the AFS agent id in its metadata, the key from the response is in the fake Secrets Manager at the agent's `paperclip-agent-api-key` path with `managed_by = 'afs'`, `paperclip_agent_id` is recorded, `link_state = linked`, `last_synced_at` is set, and one `agent.collaboration_linked` is emitted [adapter]. Proven by: `TestManagedRegistrationHappyPath`.
- The key goes straight from the Paperclip response to Secrets Manager: a log, trace and error capture over the whole run contains no part of it, `last_error` never holds it, and the database holds only the ARN [adapter]. Proven by: `TestManagedRegistrationNeverLogsKey`. The secret row and its binding are written only through `afs.record_collab_agent_key` (the fake database refuses a direct insert of the `afs`-managed row with `42501`), which also writes the audit record and no outbox row [adapter with PostgreSQL]. Proven by: `TestManagedRegistrationUsesKeyFunctions`.
- Pausing an AFS agent pauses its Paperclip agent; resuming resumes it; neither changes the link state [adapter]. Proven by: `TestManagedRegistrationPauseResume`.
- Disconnecting or deleting the agent revokes the key in the fake Paperclip (a later call with that key gets `401`), deletes the secret in Secrets Manager with a 30-day recovery window, removes the row and binding with `afs.remove_collab_agent_key`, sets `link_state = disabled`, and emits `agent.collaboration_unlinked` with reason `disconnected` or `agent_deleted`; deleting the collaboration agent emits it with `collaboration_agent_deleted` for every linked agent [adapter]. Proven by: `TestManagedRegistrationDisconnect`.
- A Paperclip failure (`500`, timeout, schema mismatch) sets `link_state = failed` with a sanitized `last_error` of at most 500 characters, retries with backoff 1 s doubling to 5 min, emits `agent.collaboration_unlinked` (`failed`) only if the agent was `linked`, and sets `RegistrationHealthy=False` on a contract mismatch while the other agents keep their state [adapter]. Proven by: `TestManagedRegistrationFailures/*`.
- The reconciler authenticates with the board key read from Secrets Manager (secret `paperclip-board-api-key` only); with that secret missing, `RegistrationHealthy=False` with reason `BoardKeyMissing` and no Paperclip call is made [adapter]. Proven by: `TestManagedRegistrationNeedsBoardKey`.
- Switching `registration_mode` from `manual` to `managed` adopts agents whose `paperclip_agent_id` is already set (it verifies them in the roster instead of creating duplicates), and from `managed` to `manual` leaves roster, keys and secrets as they are [adapter]. Proven by: `TestRegistrationModeSwitch`.
- Calls to Paperclip use only `http://paperclip.<namespace>.svc.cluster.local:3100` of the team being reconciled (a unit test asserts the adapter is constructed per team with that base URL, and refuses any other host) [unit]. Proven by: `TestPaperclipAdapterBaseURL`.

**RC-US-20a · Roster guard and first admin.** *(US-28, US-29; COL-13, COL-27, COL-32)* As a security reviewer, I want unmanaged Paperclip agents and open sign-up flagged. (RC-26, RC-29)
- Given a local-adapter agent added inside the fake Paperclip, then within 10 minutes `UnmanagedPaperclipAgents=True` names it, `GetTeamCollaboration.unmanagedPaperclipAgents` returns the name, and `collaboration.unmanaged_agents_found` (`reason: local_adapter`) is emitted once; a roster member that is neither the orchestrator nor linked emits with `reason: not_linked`; a second scan emits nothing for the same name; removing it clears the condition; a new name emits again [adapter]. Proven by: `TestRosterGuard/*`.
- The orchestrator (the `hermes_local` agent bound to the collaboration agent) is never reported [adapter]. Proven by: `TestRosterGuardIgnoresOrchestrator`.
- Given the fake Paperclip reports open sign-up, then `PaperclipReady=False` with reason `OpenSignUp` in `managed` mode; closed sign-up clears it [adapter]. Proven by: `TestOpenSignUpDetected`.
- Bootstrap (PC-D26, PC-D27; against the fake database, Secrets Manager and notification adapter): the rendered pod has `AFS_PAPERCLIP_BOOTSTRAP` equal to the team's `registration_mode`. In `managed` mode, once the board key secret `…/paperclip-board-api-key` exists in the fake Secrets Manager, the reconciler records it with `afs.record_collab_board_key` (no `agent_secret_bindings` row is created and no agent environment variable carries it), emits exactly one `collaboration.bootstrap_completed` `{teamId, agentId, mode}` and sends one email notification with the invite to the creating team-admin; the invite URL appears in no event, audit record, log line, trace span or error (captured-output scan), and a reconciler restart sends no second invite or event. In `manual` mode no board key is read, no event or invite is sent, and the `PaperclipReady` message names the log command only [adapter]. Proven by: `TestFirstAdminBootstrap/*`, `TestBootstrapInviteNeverLogged`.
- Member sync (P2): a new user binding creates one invite (collaborator and team-admin as member, viewer as viewer when the fake supports it), removing the binding deactivates the account, and a group-only member is invited at the first `signed_in` bucket with no invite; the edge's decision is unaffected by the Paperclip account [adapter]. Proven by: `TestMemberSync/*`.

**RC-US-20b · Scanning and Paperclip-created secrets.** *(US-32; COL-25, SEC-7)* As security, I want credentials written by agents detected and Paperclip's own secrets listed. (RC-26)
- Given the fake Paperclip holds an issue where agent `agt_X` wrote a GitHub token, then within 10 minutes `collaboration.secret_detected` is emitted with `source: reconciler`, `action: detected`, `pattern: github_token`, `paperclipItemId`, `authorAgentId = agt_X` and no value or fragment (a scan of the event, audit, logs and traces for the token finds nothing); the same item is not reported twice; an edited item is rescanned [adapter]. Proven by: `TestAgentWriteScan/*`.
- One table test per pattern type shares the vectors of the edge scanner (`aws_access_key`, `github_token`, `slack_token`, `private_key`, `jwt`, `okta_client_secret`, `entra_client_secret`, `generic_assignment`) and a set of near-misses that must not match [unit]. Proven by: `TestScannerVectors` in `internal/domain/collab`.
- No Paperclip issue, comment or chat text is stored in any AFS table, log, trace or event: a test runs the full scan with sentinel text and searches the fake database, the log capture and the outbox for it [adapter]. Proven by: `TestNoPaperclipContentPersisted`.
- Secret listing: given the fake Secrets Manager holds three secrets under `<service>/<namespace>/<agent>/paperclip/`, then after the hourly job `secrets` has three rows with `managed_by = 'paperclip'`, names `paperclip-<suffix>` by the §7.2.4 rule (collisions get `-<4 hex>`), the ARNs and metadata only; a deleted secret's row is removed; no `GetSecretValue` call was made on them; a row for another team is never touched [adapter]. Proven by: `TestPaperclipSecretListing/*`.

**RC-US-20c · Swarm collaboration agent.** *(US-27, US-33; COL-9, COL-7, PC-D29; P1, C4)* As a service admin, I want a remediation team's Paperclip to come up with its secrets without `afs-api`. (RC-30)
- Given a work order team created by `afs.instantiate_work_order_team` from a template with `spec.collaboration` (collaboration agent named `paperclip`, dedicated identity, daily snapshots, `team_collaboration` row web UI on, REST API off, `manual`), the reconciler renders it exactly as RC-US-19 [envtest]. Proven by: `TestSwarmCollaborationRender`.
- The reconciler generates three random 32-byte hex values, creates the three boot secrets in the fake Secrets Manager with the team's tags and KMS key, and calls `afs.record_work_order_secret` once per secret with the collaboration agent's `agt_` id in the pool argument; the database then holds three agent-scope `afs`-managed rows with their `env` bindings (`BETTER_AUTH_SECRET`, `PAPERCLIP_TOOL_ACTION_SIGNING_SECRET`, `PAPERCLIP_SECRETS_MASTER_KEY`) and the pod starts only after they exist (`CollaborationKeyMissing`-style condition `BootSecretsPending` until then) [adapter with PostgreSQL and fake Secrets Manager]. Proven by: `TestSwarmCollaborationBootSecrets`.
- A crash between creating a secret and recording it, then a restart, leaves exactly three secrets and three rows: an existing recorded secret is neither regenerated nor duplicated, and an unrecorded one is recorded with its existing value (never a second secret) [adapter]. Proven by: `TestSwarmCollaborationBootSecretsIdempotent`.
- No generated value appears in any log, trace, error, event or audit record [adapter]. Proven by: `TestNoSecretValueEscapes/swarmCollaboration`.
- When the work order completes, teardown (RC-10) removes the agent, its Service, policies and route like any collaboration agent, and the hostname hold applies [envtest, adapter]. Proven by: `TestSwarmCollaborationTeardown`.

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
- A model the team has not enabled → `403 AccessDenied`, recorded as a rejected call with `outcome`
  `rejected_model` [adapter].
- A pool worker's call records the pool and the worker pod name [adapter].
- Every call, accepted or rejected, records its latency, and rejected calls record the outcome
  (`rejected_budget`, `rejected_model`) [adapter].

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
- A stream aborted before any usage is reported settles to the reservation (the worst case stays
  charged) [adapter with fake Bedrock].

**GW-US-6 · Caller identity and suspended or deleted teams.** *(US-23, US-25, D57)*
- A call from an agent using the AWS SDK's Bedrock Runtime client or the Anthropic SDK with only the
  environment of the gateway contract §3 succeeds, is attributed to the agent, and its SigV4 signature or
  API key is ignored [conformance suite].
- A call whose source IP is not a current team pod → `401 Unauthenticated`; a call that sends the optional
  token of a different pod → `401`; a call with a valid token of the source pod succeeds [adapter].
- A suspended team's call → `403 AccessDenied` with `details.reason = team_suspended`; after the team is
  deleted its pods no longer exist and a call from its former IP → `401` [adapter].
- Settlement always passes the budget and period it reserved against
  (`afs.settle_reservation(id, budget_id, period_start, actual)`) [adapter].
- A cached TokenReview result is not reused after 5 minutes [unit with fake clock].

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

### afs-edge

The edge is a Go service tested like the others: table-driven unit tests for the domain and use cases with fake ports,
adapter tests against `httptest` servers (a fake Paperclip upstream, a fake IdP with JWKS, code and token endpoints), a real
PostgreSQL (`AFS_TEST_DATABASE_URL`) for `edgedb` against migration 000020, and a load test with a fake upstream.
`internal/domain/collab` and `internal/usecase/edge` hold at least 90% coverage (`make cover`), and `internal/archtest` is
extended so `domain/collab` imports the standard library only, `usecase/edge` imports only `domain` and `authz`, and
`cmd/afs-edge` is the only place adapters are wired.

**EDGE-US-1 · Route.** *(US-29; COL-15, COL-16)* As a team member, I want my team's hostname to reach my team's Paperclip and nothing else. (EDGE-1, EDGE-2)
- Given a route table with teams A and B, `a.<base>` reaches A's `paperclip` Service (`http://paperclip.<A namespace>.svc.cluster.local:3100`) and `b.<base>` reaches B's; the upstream host in every forwarded request comes from the table and never from the request [unit, adapter]. Proven by: `TestRouteByHost`, `TestUpstreamIsFromRouteTable`.
- An unknown host, the apex on any path other than `GET /oauth2/callback`, a team without a collaboration agent, a host whose surfaces are both not effective (each of `collab.public_endpoints` off, `mayExposeCollaboration` off, base domain unset, both switches off), and a deleted team's host all return `404 NotFound` in the §6 envelope (HTML page for navigation) with the same body for each, no team name and no reason [unit]. Proven by: `TestUnknownOrDisabledHost404/*`.
- A team with `ready = false` returns `503 Unavailable` with `Retry-After: 30` and `details.reason` equal to `not_ready_reason` (`paused`, `team_suspended`, `starting`); a route table older than 5 minutes after failed refreshes returns `503 upstream_unavailable`; the page names the reason and carries the `requestId` [unit]. Proven by: `TestNotReady503/*`, `TestStaleRouteTable`.
- The edge does not check SNI (the ALB terminates TLS, PC-D23): a request whose `Host` is neither the apex nor a known team host → `404` (the same body as an unknown team, never `400`); the reason `host_mismatch` is not produced anywhere (a grep test over the source and the OpenAPI document finds it nowhere); there is no path-based routing: `https://<base>/<slug>/…` is `404` [unit]. Proven by: `TestUnknownHost404NoSNICheck`, `TestNoPathRouting`, `TestNoHostMismatchReason`.
- A route change reaches the edge within 60 s (p95): a database change that produces an `afs_desired_state` notification is applied within 5 s; with notifications blocked the 30 s refresh applies it; a settings-only change (no notification, `service_settings` has no generation) applies within 30 s [adapter with PostgreSQL]. Proven by: `TestRouteRefreshOnNotify`, `TestRouteRefreshPoll`, `TestSettingsRefreshWithin30s`.
- Startup: the edge refuses to start when `afs.schema_version()` is lower than its build's; `/readyz` is `503` until the route table loaded once and after 2 minutes without a refresh [adapter]. Proven by: `TestEdgeStartupChecks`.
- The WebSocket and SSE upgrade requests are routed by the same table; a team switched off closes its open streams within 60 s [adapter]. Proven by: `TestStreamsClosedWhenTeamDisabled`.

**EDGE-US-1a · Sign in with the team's provider.** *(US-29; COL-17)* As a team member, I want to sign in once through my company provider. (EDGE-3)
- Given the web UI is effective and I hold no session, when I navigate to `https://<slug>.<base>/x`, then the response is `302` to `/_afs/signin?return=/x`, which redirects to the team provider's authorization endpoint with `response_type=code`, the collaboration client id, `redirect_uri=https://<base>/oauth2/callback`, `scope=openid profile email` plus the groups scope the provider needs, `code_challenge_method=S256`, a `nonce` and a sealed `state`; one redirect URI is used for every team [adapter with fake IdP]. Proven by: `TestSignInRedirect`.
- The callback exists only on the apex: `GET https://<base>/oauth2/callback` validates `state` (sealed, 10 minutes; tamper and expiry fail), exchanges the code with the PKCE verifier and the collaboration client secret (read from the ARN of the route table through the fake Secrets Manager, never logged), validates the ID token (signature through the shared JWKS cache, issuer of the team's provider, audience the collaboration client id, expiry, nonce), calls `afs.collab_handoff_create` with the SHA-256 of a fresh random code and the identity (issuer, subject, provider id, groups), and redirects to `https://<slug>.<base>/_afs/handoff?code=…`; a token for another team's provider, a wrong audience, an expired token or a bad nonce → `401 token_invalid` or `token_expired` and no handoff row; every other apex path, including `/_afs/handoff`, is `404` [adapter with fake IdP and PostgreSQL]. Proven by: `TestCallbackValidation/*`, `TestApexServesOnlyCallback`.
- Handoff and session creation: `GET /_afs/handoff?code=` generates a random 256-bit session id and calls `afs.collab_handoff_consume(code_hash, host, session_id_hash)`; on a row it sets `__Host-afs-collab` (`Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`) to the plain id and redirects to the original path; the database stores only the id's hash. With no row (unknown, expired after 60 s, already consumed, or presented on another team's host) → `401 token_invalid` and no cookie. With three edge replicas and 50 concurrent requests presenting the same code, exactly one succeeds (single use across replicas, PC-D22) [adapter with PostgreSQL]. Proven by: `TestHandoffConsumeCreatesSession`, `TestHandoffBindings/*`, `TestHandoffSingleUseAcrossReplicas`.
- Each request resolves the cookie with `afs.collab_session_get(id_hash)`; a session issued for team A is never accepted on `b.<base>` (the session's `team_id` differs from the host's → `401 session_expired` for non-navigation requests and a redirect to sign in for navigation); a forged or unknown id gives no row and the same answers; the plain id and its hash appear in no log [unit, adapter]. Proven by: `TestSessionNeverValidOnAnotherTeamHost`, `TestUnknownSessionRefused`.
- Session lifetime is `collab.session_ttl_minutes` (default 480), applied by the database at consume; at expiry a navigation is redirected to sign in again and a non-navigation request gets `401 session_expired`; a change of the setting applies to new sessions only [adapter with PostgreSQL, fake clock]. Proven by: `TestSessionExpiry`.
- Turning the web UI off (stored choice, a guardrail or the service setting) makes the edge refuse that team's sessions on the next request after the route refresh (within 60 s, p95) without deleting any session row, and turning it on again restores them while they are unexpired [adapter with PostgreSQL]. Proven by: `TestWebUiOffRefusesSessions`.
- Groups come from the session row: a person with 300 groups (the database allows 500) is decided by `collab_principal` with those groups and the decision is cached at most 60 s per session; there is no group data in the cookie, and a 501-group sign-in is refused at handoff creation with `400` rather than truncated [adapter]. Proven by: `TestSessionGroupsFromDatabase`, `TestTooManyGroupsRefused`.
- The `state` sealing key comes from the platform Secrets Manager secret with two active versions: a state sealed with the previous version still opens during the overlap; a tampered, truncated or foreign-key state fails closed; the key never appears in a log [unit]. Proven by: `TestStateSealKeyRotation`, `TestTamperedStateRefused`.
- Sign-out (`POST /_afs/signout`) calls `afs.collab_session_revoke(id_hash)`, clears the cookie and records `signed_out`; the revoked id is refused from then on on every replica; signing in again is required; the IdP session is not ended [adapter with PostgreSQL]. Proven by: `TestSignOut`.
- A team whose provider has no `collab_client_id` cannot serve the web UI: `web_ui_effective` is false and the host answers by the 404 rule unless the REST API is effective, in which case the browser navigation gets `403 surface_disabled` [unit]. Proven by: `TestProviderWithoutCollabClient`.
- Sign-in round trip adds less than 3 s (p95) excluding the IdP in the load test with a fake IdP [load]. Proven by: `TestLoadSignIn`.
- Against real Okta and Entra ID tenants: parked (§8.1). The fake IdP is a recorded-fixture conformance server covering both providers' discovery, JWKS rotation and groups claim shapes [adapter]. Proven by: `TestIdPConformanceFixtures/{okta,entra}`.

**EDGE-US-1b · REST API credentials.** *(US-31; COL-19)* As an automation author, I want my SSO token or team API key to work in `X-Afs-Authorization`. (EDGE-4)
- Given the REST API is effective, a request with `X-Afs-Authorization: Bearer <access token>` from the team's provider (issuer, audience, expiry, signature, `kid` refetch as RA-US-2) and a Paperclip key in `Authorization` reaches Paperclip with `Authorization` byte-identical and no `X-Afs-Authorization` header [adapter]. Proven by: `TestAPITrafficTokenForwarded`.
- A team API key `afsk_…` is hashed with SHA-256 and checked with `collab_principal_key(team, hash)`; state `active` with a role holding `collaboration:api` is admitted; `expired` → `401 key_expired`; `revoked` → `401 key_revoked`; an unknown key, another team's key and a service-level key all give `401 token_invalid` with the same body (a key never reveals other teams); a security-operator key and a workload token are never admitted [adapter with PostgreSQL]. Proven by: `TestAPIKeyStates/*`.
- A request carrying both the session cookie and `X-Afs-Authorization` → `400` `ambiguous_credentials`; a malformed header (`Basic …`, empty bearer, bearer longer than 8 KiB) → `400` `malformed_credential` [unit]. Proven by: `TestAmbiguousCredentials`, `TestMalformedCredential`.
- Traffic classification: cookie → web UI; header → API; a navigation request with neither → redirect to sign in; a non-navigation request with neither → `401 Unauthenticated` (not a redirect) [unit]. Proven by: `TestTrafficClasses`.
- With the REST API not effective and the web UI effective, header traffic → `403 surface_disabled`; with the web UI not effective and the REST API effective, cookie traffic → `403 surface_disabled`; both not effective → `404` [unit]. Proven by: `TestSurfaceDisabledMatrix`. With exactly one surface not effective, a caller who holds a binding on the team and asks for that surface gets `403 surface_disabled`, never `404`; a caller with no binding gets the `404` of an unknown host, so existence is not leaked (PC-D31) [unit]. Proven by: `TestSurface403Not404ForBoundCaller`, `TestSurface404ForUnboundCaller`.
- Token audience: the access token's audience may be the provider's collaboration client id or its console client id, so the CLI's SSO token works (RD-6); a token whose audience is neither → `401 token_invalid` [unit]. Proven by: `TestAPITokenAudience`.

**EDGE-US-2 · Authorize, limit, scan and record.** *(US-29, US-30, US-33; COL-18, COL-20, COL-21, COL-24, COL-26, COL-30, COL-31)* As security, I want every request decided by AFS before Paperclip sees it. (EDGE-4 to EDGE-7)
- Method rule: for each role (viewer, collaborator, team-admin, service admin) and each of `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, the table of API contract §4.4c holds: safe methods need `collaboration:access`, every other method `collaboration:contribute`, so a viewer's `POST` → `403 AccessDenied` (`details.reason = permission_required`, `requiredPermission = collaboration:contribute`) and a collaborator's reaches the upstream; on the REST API surface a viewer-role key's `GET` succeeds, its `POST` → `403`, and a key without `collaboration:api` → `403` (`requiredPermission = collaboration:api`) [unit]. Proven by: `TestAuthorizeMatrix/<surface>/<role>/<method>`.
- A caller with no binding on the team → `403` `no_team_role` and never any upstream call; a caller bound on team B only gets `403` on A's host after sign-in with no A content, and B's session is not sent to A [adapter]. Proven by: `TestNoTeamRole`, `TestCrossTeamHostDenied`.
- The edge calls `internal/authz.Authorize` and has no copy of the role table: a unit test generates the matrix from `authz` and fails if `usecase/edge` declares a role or permission literal [unit]. Proven by: `TestEdgeUsesAuthzOnly`.
- Decision cache: a removed binding is refused within 60 s (fake clock: allowed at t, binding removed at t+1 s, still allowed at t+59 s from the cached decision, refused at t+61 s); a negative decision is cached 10 s; a changed route (switch off) applies within 60 s whatever the cache holds because the route is checked first [unit with fake clock]. Proven by: `TestDecisionCacheTTL`, `TestSwitchOffBeatsCache`.
- Service admin entry: a person whose only grant is the service-admin role is admitted and counted with `service_admin_entry = true` and `serviceAdminEntry: true` in the event; a person with a team role is not flagged even if also a service admin [unit]. Proven by: `TestServiceAdminEntryFlag`.
- Forwarding: the upstream receives no `Cookie` with `__Host-afs-*`, no `X-Afs-Authorization`, no `X-Amzn-*`, no client `X-Forwarded-*`; it receives `X-Forwarded-For` (the client address the ALB reports, appended per ALB convention), `X-Forwarded-Proto: https` and `X-Request-Id` (generated if absent, echoed in the response); other cookies and headers pass unchanged; responses gain `Strict-Transport-Security`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `Content-Security-Policy` containing `frame-ancestors 'none'` (merged with an upstream policy, never weaker); no `Access-Control-*` header is added [adapter]. Proven by: `TestForwardedHeaders`, `TestResponseHeaders`. A response that carries a once-only Paperclip key (for example the response creating an agent API key) is passed through byte for byte, and no log line, span or event holds any request or response body [adapter]. Proven by: `TestOnceOnlyKeyPassesThrough`, `TestEdgeNeverLogsBodies`.
- Body limit: a body over `collab.max_body_mb` → `400` `body_too_large` (no `413`), with no upstream call when `Content-Length` is known and the stream cut (and the upstream request aborted) when chunked; `Content-Encoding: gzip` on a body while scanning is `block` or `warn` → `400` `unsupported_encoding`, accepted when scanning is `off` [adapter]. Proven by: `TestBodyLimit`, `TestUnsupportedEncoding`.
- Limits: 20 requests per second per principal with burst 40: 41 requests in one second from one key → the 41st gets `429 Throttled` with `Retry-After`; another principal is unaffected; each replica enforces its share (the configured replica count divides the rate) [unit with fake clock]. Proven by: `TestPerPrincipalLimit`.
- Streams: a WebSocket and an SSE stream are proxied end to end with an idle timeout of 10 minutes; a caller holding only `collaboration:access` may open them and the first client frame closes the connection with code 1008; a writer's text frames are scanned like bodies and binary frames pass; the 5,000-stream scale target is met in the load test with a fake upstream [adapter, load]. Proven by: `TestWebSocketReadOnlyClosed1008`, `TestSSEProxied`, `TestStreamIdleTimeout`, `TestLoadStreams`.
- Surface switches: with the REST API turned off the next header request → `403` `surface_disabled` within 60 s, with the web UI off new sign-ins and existing sessions are refused within 60 s, with both off the host is `404` [adapter with PostgreSQL, fake clock]. Proven by: `TestSurfaceSwitchWithin60s`.
- Scanning: one table test per pattern (`aws_access_key`, `github_token`, `slack_token`, `private_key`, `jwt`, `okta_client_secret`, `entra_client_secret`, `generic_assignment`) and per body shape (JSON, form, `text/plain`, multipart text part, a pattern split across two chunks, a pattern at a chunk boundary, a near-miss) for modes `block` (`422 SecretDetected`, `details.patterns` holds the pattern types, nothing reaches the upstream), `warn` (forwarded, outcome `secret_warned`) and `off` (forwarded, no scan); binary parts and bodies over the window are handled without buffering more than the window [unit, adapter]. Proven by: `TestScanBlock/*`, `TestScanWarn/*`, `TestScanOff`, `TestScanChunkBoundary`.
- The `422` body, the `message`, the log lines, the spans, the metric labels and the `collaboration.secret_detected` event never contain the matched text or any fragment of it: a test posts sentinel credentials and searches every captured output [adapter]. Proven by: `TestSecretNeverEchoed`.
- Detections are counted per team, person and pattern and `afs.emit_collab_secret_detected` is called at most once per team, person and pattern in 5 minutes with the accumulated `count`; the call carries the surface, principal kind and id, pattern and action (`blocked` or `warned`) and no value [adapter with PostgreSQL]. Proven by: `TestSecretDetectedEventThrottle`.
- Records: five-minute counts are buffered per key and flushed to `afs.record_collab_access` every 30 s (secret detections go to `afs.emit_collab_secret_detected`; these two calls are the only database writes the edge makes, and a test asserts no other `afs.*` function is called outside the read functions) and at shutdown, at most 1000 rows per call; a failed flush is retried without losing counts; each outcome of the `collaboration_access` enum is produced by its case (sign-in, sign-out, allowed, `secret_warned`, the four denied outcomes, `throttled`, `unavailable`); anonymous requests have `principal_id = ''` [adapter with PostgreSQL]. Proven by: `TestAccessRecordsFlush`, `TestAccessOutcomes/*`.
- Edge events: sign-ins, sign-outs and denied requests produce one JSON line each with the field `afs.team_id` (not `team_id`), principal id or key prefix, method, route template, status, bytes, duration and `serviceAdminEntry` and nothing else; no body, no header value, no query string, no raw path containing ids (an unknown path is recorded as its first segment); the redaction of OPS-4 is applied; a sentinel in a path id and in the query is not found in any output [unit, adapter]. Proven by: `TestEdgeEventsContainNoContent`, `TestRouteTemplates`.
- Denied-spike inputs: 51 denied requests across the four denied outcomes for one team within 5 minutes produce counts the RC-US-19c spike job sums to 51 (cross-test with the reconciler job over the same fixture) [adapter]. Proven by: `TestDeniedOutcomesFeedSpike`.
- Quality: `afs-edge` adds less than 20 ms p99 over a fake upstream at 100 requests per second per team for 50 concurrent people (200 teams in the route table) [load]; `internal/usecase/edge` and `internal/domain/collab` have at least 90% coverage [unit]; graceful shutdown drains in-flight requests and flushes the buffers within 30 s [adapter]. Proven by: `TestLoadEdgeOverhead`, `make cover`, `TestGracefulShutdown`.
- No log line, span or metric label of any edge test contains a token, API key, cookie value, client secret, sealing key or Paperclip credential (the captured-output scan of RA-US-5 applied to the edge) [adapter]. Proven by: `TestEdgeNoCredentialsInOutputs`.

### deploy

**INF-US-1 · The public entry in Terraform and Helm.** *(US-29, US-33; COL-15, COL-23, COL-30)* As the platform team, I want the one public path defined as code and checked without an AWS account. (DP-2, DP-3, DP-4)
- Real AWS and EKS runs are parked (§8.1). Everything below is checked at plan level: `terraform validate`, `tflint`, the HCL structure tests of `deploy/terraform/tests` (`make deploy-test`, which parse the files with no `terraform` binary and no credentials), and `terraform test` with `command = plan` and mock providers in `deploy/terraform/modules/collab-edge/tests/*.tftest.hcl`; Helm is checked by `helm template` golden tests in `deploy/helm/afs/tests` [adapter]. Proven by: `TestEveryFileParses`, `terraform test`, `TestEdgeChartRender`.
- The `collab-edge` module has an internet-facing ALB in public subnets, an HTTP listener whose only action is a redirect to HTTPS (`HTTP_301`), an HTTPS listener with `ELBSecurityPolicy-TLS13-1-2-2021-06` (TLS 1.2 minimum, 1.3 preferred) and an ACM certificate whose domain is `<base>` with the subject alternative name `*.<base>` (PC-D21), a default action of a fixed `404`, and one rule forwarding only when the host header matches `<base>` or `*.<base>` to a target group of target type `ip` that belongs to `afs-edge` [terraform test]. Proven by: `TestCollabEdgeListeners`.
- Route 53 has an alias record for the apex and one for the wildcard, both to the ALB; the module creates nothing when `collab_base_domain` is unset (plan shows zero resources) and nothing is hardcoded (account id, region, zone id and subnet ids are variables; `TestNoHardcodedIdentifiers` extended) [terraform test]. Proven by: `TestCollabEdgeDNS`, `TestCollabEdgeDisabledWhenNoDomain`.
- The WAF web ACL (scope `REGIONAL`, associated with the ALB) contains `AWSManagedRulesCommonRuleSet`, `AWSManagedRulesKnownBadInputsRuleSet`, `AWSManagedRulesAmazonIpReputationList` and `AWSManagedRulesAnonymousIpList`; a rate-based rule with limit 2000 over an evaluation window of 300 s aggregated by `IP`, action `block`; an allow-list rule that blocks any source not in the two IP sets (v4 and v6), whose `addresses` are in `lifecycle.ignore_changes` so the reconciler owns them; logging enabled to a CloudWatch log group named `aws-waf-logs-…` with 13-month retention and a redaction of the `authorization`, `cookie` and `x-afs-authorization` headers [terraform test]. Proven by: `TestCollabWAFRules`, `TestCollabWAFAllowListOwnedByReconciler`, `TestCollabWAFLogging`.
- ALB access logs go to an encrypted S3 bucket with a public-access block and a 13-month lifecycle (the hardened-bucket test of `TestBucketsAreHardened` extended); the ALB security group allows 443 from the internet and egress only to the edge pods' port; `drop_invalid_header_fields` is on; deletion protection is on [terraform test]. Proven by: `TestCollabAccessLogsBucketHardened`, `TestCollabSecurityGroups`.
- IAM: the `afs-edge` role may read the `state` sealing-key secret and `<service>/collab/*` client secrets only and has no `bedrock:*`, no `iam:*`, no `sts:*`, no wildcard resource; the collaboration agent role gains exactly one statement outside its prefix, `secretsmanager:PutSecretValue` on the one `…/paperclip-board-api-key` ARN; the reconciler's new statements are exactly: `secretsmanager:GetSecretValue` on `…/paperclip-board-api-key` by exact-name condition, `wafv2:GetIPSet` and `wafv2:UpdateIPSet` on the two IP-set ARNs, listing and tagging below `…/paperclip/` [adapter]. Proven by: `TestIAMLeastPrivilege/collab`.
- The `state` sealing-key secret has 30-day rotation configured, and the module outputs nothing secret [terraform test]. Proven by: `TestCollabSessionKeyRotation`.
- Helm renders `afs-edge` with `replicas >= 3`, `topologySpreadConstraints` over zones, a PodDisruptionBudget `minAvailable: 2`, Pod Identity ServiceAccount, `runAsNonRoot`, read-only root filesystem, dropped capabilities, resource requests, an `HPA`, `/readyz` and `/healthz` probes, a ServiceMonitor, a `TargetGroupBinding` naming the Terraform target group and a NetworkPolicy that admits ingress on 8443 only from the ALB subnets and the kubelet and egress only to DNS, the database, the IdP issuer hosts, the Secrets Manager endpoint and `paperclip` pods on 3100; no `Ingress`, no `LoadBalancer` or `NodePort` Service and no `hostNetwork` is rendered for it; the chart's existing tests still pass [adapter]. Proven by: `TestEdgeChartRender/*`, `TestEdgeChartHasNoPublicObjects`. The chart also renders the node log collector's Fluent Bit rule that routes records of `afs-system/afs-edge` by the field `afs.team_id` to that team's aggregator (OPS-4 redaction applied) and to the platform CloudWatch group, and the golden test shows a record with `afs.team_id = team_A` reaching only team A's output in a local Fluent Bit run of the rendered config (no network, `fluent-bit --dry-run` plus a file output) [adapter]. Proven by: `TestEdgeLogRoutingByTeamID`.
- Dashboards and burn-rate alert rules exist for edge availability (99.9% monthly) and latency (the 20 ms overhead and sign-in objectives) and for route-table age, 5xx by reason and denial spikes [adapter]. Proven by: `TestEdgeAlertRules`.
- Not provable without a real account: certificate issuance and DNS validation, WAF behaviour on real traffic, the ALB-to-pod path through the LB Controller, Okta and Entra ID sign-in, and the external penetration test; listed in §8.1.

## 6. Non-functional requirements

| Area | Requirement |
|---|---|
| Availability | `afs-api` and `afs-gateway` 99.9% monthly, at least 3 replicas across AZs; reconciler leader election with < 30 s failover |
| Performance | API p99 < 300 ms for reads, except reports (RA-US-7: p99 < 1 s for 90-day ranges); gateway p99 overhead < 25 ms; reconcile lag p95 < 60 s |
| Scale | 200 teams, 2,000 agents, 500 gateway req/s (initial) |
| Recovery | RPO 5 minutes (RDS PITR, DP-1); RTO 1 hour for a full rebuild from a restored database onto a fresh cluster, proven by the RC-US-3 restore drill |
| Retention | Partitioned facts (`usage_events`, `agent_activity`, `external_traffic`) 13 months, then dropped (RC-8); `audit_log` online with no deletion, which meets the root's 400 days (partitioning tracked in OQ-4) |
| Rate limiting | 20 req/s per principal, burst 40, `429 Throttled` with `Retry-After` (RA-15, RA-US-15) |
| Security | No long-lived AWS keys (Pod Identity for all binaries); OWASP ASVS L2; `Authorization` and tokens redacted in logs; agents' roles never get `bedrock:*`; the gateway identifies callers by source pod (Cilium), checks an optional token's audience and pod, and reaches Bedrock over TLS through the VPC endpoint only |
| Observability | OpenTelemetry traces and metrics; structured JSON logs with request id; RED metrics per route; every binary serves `/healthz` (process alive), `/readyz` (database reachable, schema version current, leader state for the reconciler) and Prometheus `/metrics`. SLIs with dashboards and alerts: API p99 latency, reconcile lag, drift corrections per hour, gateway reservation failures and added latency, expired reservations released, outbox publish lag (oldest unpublished event), notification delivery failures, CUR variance |
| Quality | `domain`, `usecase`, `authz` ≥ 90% coverage (this includes `internal/domain/collab` and `internal/usecase/edge`); allowed and denied test per route per role; `make gate` before every commit |
| Edge performance | `afs-edge` adds < 20 ms p99 to a request excluding Paperclip; sign-in round trip < 3 s (p95) excluding the IdP; a route or switch change applies within 60 s (p95), a notified change within 5 s (EDGE-US-1, EDGE-US-2 load tests, fake upstream and fake IdP) |
| Edge scale | 200 teams in the route table; 50 concurrent people and 100 requests per second per team; 5,000 concurrent WebSocket or SSE streams across the service; per-replica token buckets for 20 requests per second per principal |
| Edge availability | 99.9% monthly, at least 3 replicas across zones, PodDisruptionBudget `minAvailable: 2`, readiness on a loaded route table, graceful drain 30 s; a refresh outage keeps the last route table 5 minutes then fails closed with `503` (RD-19) |
| Edge security | OWASP ASVS L2 for `afs-edge`; no long-lived credentials (Pod Identity, `afs_edge` database login with a rotated secret); the `state` sealing key from Secrets Manager rotated every 30 days with two active versions; sessions and handoffs server-side with only SHA-256 hashes stored (a database read cannot replay a session); the cookie is `__Host-` prefixed, `Secure`, `HttpOnly`, `SameSite=Lax` and holds an opaque id; no token, key, cookie value or matched secret text in any log, span, metric label, event or error (captured-output scan tests); security review sign-off and an external penetration test before production (the sign-off is a C4 exit, the test is parked, §8.1) |
| Edge privacy | No Paperclip content, header value, query string or raw path with ids in any AFS storage, log, trace or event; records carry route templates and counts only (COL-26) |
| Edge observability | OpenTelemetry traces and metrics (requests, latency, denials by reason, sessions, scan detections by pattern, throttles, route table age, session-groups truncation); `/healthz`, `/readyz`, `/metrics`; dashboards and multi-window burn-rate alerts on edge availability and latency; reconciler metrics for registration, roster guard, scanning, denied-spike evaluation and the WAF sync |
| Edge recovery | The edge holds no session state: sessions and handoffs are in the database, so losing or replacing a replica signs nobody out and loses only that replica's buffered five-minute counts (flushed every 30 s); Paperclip's own recovery is the collaboration agent's snapshots (AGT-9, RC-16) and the boot secrets in Secrets Manager (change PRD §8 Recovery) |

## 7. Dependencies

| Dependency | Used by | Note |
|---|---|---|
| `-db` `v0.13.0` (migrations 000001–000014) | all | `-db` has no tags yet; `v0.13.0` is tagged after the `-db` PR with 000014 merges. 000013 brings `pg_notify` triggers (D30), `ensure_spend_counter`, `spend_reservations` and its functions (D31), `models.max_output_tokens` (D28), ceiling override columns (D34), the D33 and D35 guards, `drop_partitions_older_than` (D36) and the D45 settings. 000014 brings `agent_templates` and `template_id` on agents and pools (D54), `export_bundles` `download_url`, `download_expires_at` and `size_bytes` with service-admin-only RLS and reconciler column grants (D49), and `afs.resolve_repo_owners` (D38) |
| EKS, Karpenter | reconciler | Dedicated NodePools (TEAM-6) |
| EBS CSI driver, VolumeSnapshot CRDs and snapshot controller | reconciler | Agent volumes and snapshots (AGT-9, RC-16) |
| EFS and the EFS CSI driver | reconciler | Optional shared pool volume (AGT-8) |
| Kubernetes pod watch and TokenReview API | gateway | Source-pod identification and the optional token check (GW-1, D57). `afs-api` validates its workload tokens offline against the cluster OIDC issuer JWKS instead (RA-2b) |
| Cluster OIDC issuer JWKS endpoint | api | Workload token keys (RA-2b), cached and refetched on an unknown `kid` |
| metrics-server | reconciler | CPU for the activity collector (RC-7) |
| Cilium and Hubble, WireGuard transparent encryption | reconciler, gateway | FQDN egress (EXT-5), traffic flows (RC-7), and the source-pod identity and reach policy of the gateway (D57) |
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
| `-db` release with migration 000020 (`afs_edge` role and ten functions including server-side sessions and handoffs, the reconciler's key and purge functions, swarm collaboration in `instantiate_work_order_team` and `record_work_order_secret`, `agents.role`, `team_collaboration`, `agent_collaboration`, `secrets.managed_by`, `collaboration_access`, `collab_hostname_holds`, `teams.may_expose_collaboration`, `identity_providers.collab_*`, `agent_templates.capabilities`, the seven `collab.*` settings, `team_collaborator`) | api, reconciler, edge | Needed from C1; operators run `afs-migrate bootstrap-roles` before deploying it. Until tagged, a pseudo-version, as for 000019 |
| `agentic-team-w-paperclip` image with Paperclip and Hermes, by digest; reading `PAPERCLIP_API_URL`, `PAPERCLIP_AGENT_ID` and `PAPERCLIP_AGENT_API_KEY` from env and not generating boot secrets when supplied (change PRD §11) | reconciler | Agentic Teammates project; a template with capability `paperclip` |
| Paperclip (version pinned in the template): `authenticated` + `public` mode, AWS Secrets Manager provider, agent and board API keys, roster, issue and comment read, invite APIs | reconciler (RC-25, RC-26, RC-29) | Upstream; the adapter is pinned and contract-tested against the fake server (PC-R5) |
| Okta and Entra ID collaboration client per provider, one redirect URI per environment, groups claim | edge (EDGE-3) | Enterprise identity team; fixtures stand in until C2 dev sign-in |
| Public DNS zone for `collab.base_domain`, ACM certificate validation, public subnets | deploy (DP-3) | Platform, network team |
| AWS WAFv2, internet-facing ALB, AWS Load Balancer Controller (`TargetGroupBinding`) | deploy (DP-2, DP-3), reconciler (WAF sync) | Platform |
| Terraform 1.7 or later (`mock_provider` in `terraform test`) and `tflint` in CI | deploy tests (RD-16) | Platform CI |
| Gateway API CRDs | reconciler (RC-23) | Optional: the admission rules are inert without them |
| Corporate approval for an internet-facing endpoint, or the CIDR allow list | edge, deploy | Security (change PRD §11) |

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

Paperclip team collaboration (change PRD §10), starting from the feature-complete M6 state and after the `-db` 000020 release:

| Milestone | This repository | Exit |
|---|---|---|
| C1 Collaboration agent | Contract resync (1.3.0); RA-18; RA-19; RA-17 (`GetTeamCollaboration`, `PutTeamCollaboration`, `ListCollaborationLinks`, connect, disconnect: all P0 in C1, PC-D28); RA-20 (managed secrets, reserved names); RA-21 (templates, guardrail, identity provider fields, settings); RC-21; RC-22; RC-23; RC-24; RC-27 (partitions, orphan sweep); RC-28 (IAM, including the board-key `PutSecretValue`); RC-29 manual bootstrap (`AFS_PAPERCLIP_BOOTSTRAP=manual`) | Envtest: a collaboration agent renders, two connected workers get env and egress, no new reachability, admission refuses every publish path; `make gate` green; the image's `AFS_PAPERCLIP_BOOTSTRAP` support confirmed with Agentic Teammates |
| C2 Public web UI | EDGE-1, EDGE-2, EDGE-3, EDGE-4 (web UI and method rules), EDGE-5, EDGE-7 (records and events); RC-27 denied spike; RC-28 WAF sync; RA-17 `ListCollaborationEndpoints` (PC-D28); DP-2, DP-3, DP-4 | Edge tests green against the fake IdP and PostgreSQL; `terraform test` and chart tests green; load tests meet the NFRs; removal within 60 s proven; sign-in with Okta and Entra in dev and the pen test booking are parked (§8.1) |
| C3 REST API and managed mode | EDGE-4 (`X-Afs-Authorization`), EDGE-6; RA-17 (`RotateCollaborationKeys`); RC-25; RC-26; RC-29 managed bootstrap, `collaboration.bootstrap_completed`; the `invoke-api` target of the CLI | Fake-Paperclip tests for registration, roster guard, scanning and listing; a pipeline test with a team API key through the edge |
| C4 People and reports | RC-29 member sync; RA-21 collaboration report kind; EDGE-7 counts feeding reports; COL-9 (ephemeral team; RA-22 and RC-30, swarm collaboration, PC-D29); security review sign-off | Member sync and reports against the fake; every item of §10 for this change ticked |

### 8.1 Parked: not provable with envtest, fakes, `terraform test` or chart tests

Real AWS and EKS testing is parked for the project. These criteria are implemented and proven as far as fakes go; the rest
is listed here so nobody reads a green fake test as the real thing.

| Item | Why parked | What stands in |
|---|---|---|
| Hubble-observed denied flow for a non-connected pod (RC-US-19a) and Cilium policy enforcement | Needs a real Cilium cluster | The policy-render matrix and the envtest object assertions |
| ACM issuance and DNS validation, Route 53 behaviour, ALB-to-pod path through the Load Balancer Controller, WAF behaviour on real traffic and managed rule groups | Needs an AWS account | `terraform test` plan assertions on every resource and attribute |
| Sign-in with real Okta and Entra ID, groups claim shapes in real tokens, redirect-URI registration | Needs tenants | Recorded-fixture fake IdP for both providers (`TestIdPConformanceFixtures`) |
| The real Paperclip and Hermes image, its health endpoint, API versions, bootstrap and invite flows (PC-OQ-2, PC-OQ-3) | Needs the image and a cluster | The fake Paperclip server pinned to the documented API |
| Secrets Manager provider of Paperclip writing under `…/paperclip/` with the team's KMS key; IAM denial of other paths | Needs AWS | A local policy evaluator over the rendered policy JSON |
| Edge throughput at the NFR scale on real nodes, WebSocket behaviour through an ALB (idle timeout 10 minutes, ALB idle timeout set to match) | Needs a cluster | Load tests against fakes on one machine |
| External penetration test and ASVS L2 verification of the public endpoint | Needs the test environment and a vendor | Unit, adapter and captured-output tests |
| End-to-end test of the product stories US-27..US-33 in the dev environment | Needs a dev cluster | The repository stories above |

## 9. Open questions

Each has an owner, a target milestone and the default this PRD builds to if it is not answered by then.
OQ numbers are shared across the PRDs (root PRD §10.3).

| Id | Question | Affects | Owner | By | Default if unanswered |
|---|---|---|---|---|---|
| OQ-1 | Which built-in session tags does EKS Pod Identity set in the target EKS version? | RC-2, RC-3, IAM conditions | restapi | M0 spike | Conditions on `aws:PrincipalTag/kubernetes-namespace`; AFS ids as role tags (D26) |
| OQ-2 | Confirm the AWS limits of 50 tags per resource (EBS CSI adds up to 5) and 16 Bedrock `requestMetadata` pairs | RC-9, GW-5 | restapi | M0 spike | Cap the effective tag set at 40 keys; send 16 pairs in the D27 order |
| OQ-3 | Does any agent runtime need the token path at the gateway, rather than pod identity alone? | GW-1, RC-2 | J / platform | M3 | Pod identity by source IP for every runtime; the token is optional and checked only when sent (D57) |
| OQ-4 | When to partition `audit_log` | RC-8 | db | 12 months or 50 GB | Kept online, unpartitioned, no deletion |
| OQ-5 | GitHub access for PR tracking: one GitHub App per organization or GHES tokens; the rate-limit budget | RC-12 | J (enterprise GitHub setup) | M6 | One GitHub App per organization, GHES by its own app per host; 5,000 requests per hour per installation with conditional requests |
| OQ-6 | Admission engine: ValidatingAdmissionPolicy or Kyverno | RC-19 | platform | M1 | ValidatingAdmissionPolicy (built in) |
| OQ-7 | Agentic Teammates CRD API group and version, and its status endpoint shape (root R6) | RC-2, RC-7 | J | M2 | Pin the version current at M2; activity falls back to model calls and CPU when the status endpoint is missing |
| OQ-8 | Egress HTTP request and error counts need an egress proxy | RC-7, RA-6 traffic reports | platform | M4 | Connections and bytes from Hubble only; requests and errors empty |
| OQ-9 | Which IAM role does the IdP federate to for viewer kubectl? | RC-13 | J | M5 | ACC-5 waits for the federation; no access entry is created without a federated role |
| RA-OQ-1 | Which Paperclip API operations, versions and error shapes does the pinned template version expose for create agent, revoke key, pause, roster, issue and comment listing, and invites? | RC-25, RC-26, RC-29 | `-restapi`, Paperclip upstream | C3 spike | The fake server implements the operations named in COL-12, COL-13, COL-25 and COL-33; the adapter degrades to `RegistrationHealthy=False` on a mismatch (PC-R5) |
| RA-OQ-2 | Board key and first-admin bootstrap | RC-29, RC-25 | `-restapi`, Agentic Teammates | **Closed:** PC-D26 and PC-D27 (RD-11, RD-17). Remaining dependency: the image implements `AFS_PAPERCLIP_BOOTSTRAP` (change PRD §11) | The contract default |
| RA-OQ-3 | Database path for the reconciler's `afs`-managed secret rows | RC-25, RC-29 | `-db` | **Closed:** PC-D25 (the three key functions, RD-10) | The contract default |
| RA-OQ-4 | Shared state for single-use handoff and for session groups | EDGE-3, EDGE-4 | `-db` | **Closed:** PC-D22 (server-side sessions and handoffs, RD-4, RD-5) | The contract default |
| RA-OQ-5 | Path templates: which Paperclip path segments carry content or ids (PC-OQ-4) | EDGE-7 | `-restapi` | C2 | A maintained list; unknown paths recorded as their first segment only |
| RA-OQ-6 | Does the ALB idle timeout (default 60 s, max 4,000 s) get set to above the edge's 10-minute stream idle timeout in Terraform? | DP-3, EDGE-5 | platform | C2 | Yes: 660 s on the listener's load balancer attribute |

The Paperclip change also inherits the open questions of the change PRD §12.3 (PC-OQ-1 to PC-OQ-8; this repository is the owner of PC-OQ-2 and PC-OQ-4 and a party to PC-OQ-3, PC-OQ-6 and PC-OQ-8) at the milestones given there.

### 9.1 Decisions for the Paperclip change (`-restapi`, RD-n)

Decided here, in the `review-prd` pass of 2026-11-22, for J's review. Where a decision depends on another repository it says
so and the matching gap is listed in §9.2.

| # | Question | Decision |
|---|---|---|
| RD-1 | Apex callback and certificate | *Resolved by PC-D21.* The ACM certificate has the domain `<base>` and the SAN `*.<base>`, Route 53 has an apex alias and the wildcard, the callback is served on the apex only and every other apex path is `404` (DP-3, EDGE-2, EDGE-3). Gap G1 closed |
| RD-2 | Who owns the target group and the WAF allow list? | Terraform owns the ALB, listeners, WAF and an `ip` target group; Helm attaches the pods through a `TargetGroupBinding` (AWS Load Balancer Controller). `collab.allowed_source_cidrs` is a runtime service setting, so Terraform creates two IP sets with `ignore_changes` on `addresses` and the reconciler syncs them (RC-28); an empty list means any (`0.0.0.0/0`, `::/0`) |
| RD-3 | Where does the edge live in the layout? | `cmd/afs-edge`; `internal/domain/collab`; `internal/usecase/edge`; adapters `edgehttp`, `edgedb`, `edgesession`, `edgelog`; the existing `adapter/oidc` is extended, not copied. The pattern scanner lives in `domain/collab` so the reconciler's scan (RC-26) shares its vectors. `usecase/edge` imports `authz` for every decision, which the existing dependency rule allows |
| RD-4 | Single-use handoff across replicas | *Superseded by PC-D22.* Server-side: `collab_handoff_create` at the apex callback, `collab_handoff_consume(code_hash, host, session_id_hash)` at the team host, which consumes the code and creates the session atomically (no `collab_session_create`). The cookie-nonce and per-replica cache design is dropped. Gap G2 closed |
| RD-5 | Session groups | *Superseded by PC-D22.* The session row holds `groups` (at most 500); the cookie holds only the opaque id; `collab_session_get` returns the groups for `collab_principal`; no cookie splitting or truncation. A 501-group sign-in is refused at handoff creation (the table CHECK) with `403 no_team_role`-style denial and a metric, never truncated (RD-20). Gap G3 closed |
| RD-6 | Which audience does the edge accept on `X-Afs-Authorization`? | The provider's collaboration client id or its console client id, so the CLI's SSO token (console or CLI public client) works; `afs-api`'s own audience rules stay as they are |
| RD-7 | Host and SNI | *Superseded by PC-D23.* No SNI check; `Host` that is neither the apex nor a known team host is `404`; `host_mismatch` is removed. Gap G4 closed |
| RD-8 | Edge events to the team's log delivery | *Confirmed by PC-D24, with the field named `afs.team_id`* (this PRD first wrote `team_id`). The edge writes one JSON line per sign-in, sign-out and denied request to stdout; the node log collector routes `afs-system/afs-edge` records by `afs.team_id` to that team's aggregator with OPS-4 redaction (rule in the `-restapi` Helm chart and Fluent Bit configuration, DP-2) and CloudWatch keeps all lines 13 months. Gap G5 closed |
| RD-9 | How can admission recognise "a Service that selects the collaboration pod" without looking up pods? | The collaboration pod carries only AFS-managed labels (`afs.io/…`, protected by RC-19) and the StatefulSet controller's labels. The policy refuses a non-reconciler Service whose selector is empty or contains any of those keys, and any Service named `paperclip`. The Cilium ingress policy on the pod (RC-22) is the second layer for any shape CEL cannot decide |
| RD-10 | The reconciler's `afs`-managed secret rows | *Superseded by PC-D25.* `afs.record_collab_agent_key`, `afs.remove_collab_agent_key` and `afs.record_collab_board_key` (`afs_reconciler` only; audit, no outbox row). No direct grants. Gap G6 closed |
| RD-11 | Board key and first-admin bootstrap (PC-OQ-2) | *Resolved by PC-D26 (the PC-OQ-2 default).* The pod's entrypoint bootstraps when `AFS_PAPERCLIP_BOOTSTRAP=manual|managed` is set; in `managed` mode it writes the board key to `<svc>/<ns>/<collab-agent>/paperclip-board-api-key` (the role's one exact-ARN `PutSecretValue`); the reconciler reads only that name and records it with `record_collab_board_key`. The `Bootstrapper` port stays only as the test seam. Gap G7 closed |
| RD-12 | Where do the names of unmanaged Paperclip agents live? | In the `UnmanagedPaperclipAgents` condition message, sanitized and cut to 500 characters by `condmsg.Sanitize`, parsed by `afs-api` into `unmanagedPaperclipAgents`; no new column |
| RD-13 | Boot secrets are written before the database transaction commits | Secrets Manager first, with a compensating delete when the transaction fails, then an orphan sweep by the reconciler (RC-27). The agent row exists only if all three secrets exist; the opposite order would leave an agent that cannot start |
| RD-14 | `effective` and `reasons[]` in `GetTeamCollaboration` | Computed in `afs-api` from tables `afs_app` can read; `TestCollaborationEffectiveMatchesRouteFunction` compares it with `afs.collab_routes()` so the two cannot drift |
| RD-15 | State for the denied-spike job | In reconciler memory, rebuilt from `collaboration_access` on leader change; a failover may repeat one event, never lose the condition |
| RD-16 | How are Terraform and Helm acceptance criteria proven without AWS or a cluster? | `terraform validate`, `tflint`, the existing HCL structure tests (`make deploy-test`), a new `terraform test` with `command = plan` and `mock_provider` blocks per module (Terraform 1.7 or later), and `helm template` golden tests. Anything that needs a real account or cluster is listed in §8.1 |
| RD-17 | First admin invite | *Resolved by PC-D27.* A direct email notification to the creating team-admin through the reconciler's notification delivery, never an event, audit record or log line; the event `collaboration.bootstrap_completed` `{teamId, agentId, mode}` carries no URL. Gap G8 closed |
| RD-18 | `ListCollaborationEndpoints` priority | *Resolved by PC-D28.* P0 in C2 with the S10 tab; `ListCollaborationLinks`, connect and disconnect are P0 in C1. Gap G9 closed |
| RD-19 | Edge decisions when the route table cannot be refreshed | Keep the last table for 5 minutes, then answer `503 upstream_unavailable`; never fail open (a team switched off during an outage stays reachable for at most 5 minutes, recorded as such in the runbook) |
| RD-20 | A person with more than 500 groups | The database caps `groups` at 500 (`collab_edge_handoffs` and sessions CHECK, `collab_principal` `22023`). The edge does not truncate (a truncated list could silently drop the group that grants access, or worse, cache a partial decision): the sign-in is refused with the `403 no_team_role` page, the metric `afs_edge_groups_over_limit_total` increments, and the operator runbook says to narrow the provider's groups claim |
| RD-21 | Session ids | The edge generates the 256-bit random session id and the handoff code with `crypto/rand` and only ever sends SHA-256 hashes to the database; the plain values exist in the cookie and the redirect URL only. A `23505` on `collab_handoff_create` (hash collision) is retried once with a new code |
| RD-22 | `hostname_held` mapping | `afs-api` maps only the `23505` whose constraint name is `collab_hostname_held` to `409 hostname_held`; any other duplicate slug stays the ordinary duplicate `409` |
| RD-23 | Swarm collaboration boot secrets | Generated by the reconciler (RC-30) through the existing `record_work_order_secret` path with the collaboration agent's `agt_` id (PC-D29), never by `afs-api`; the three names are idempotent, so a crash between creating and recording neither duplicates nor regenerates a recorded secret |

### 9.2 Contract gaps and inconsistencies found (all resolved by contract v1.3.1)

These are for the coordinator and the owners of the contracts; none is silently worked around beyond the default in 9.1.

| # | Where | Gap |
|---|---|---|
| G1 | Change PRD COL-15 and API contract §4.4c | The callback is on the apex `<collab.base_domain>`, which a `*.<collab.base_domain>` certificate and a wildcard record do not cover (RD-1) **Resolved in contract v1.3.1 by PC-D21.** |
| G2 | Change PRD COL-17, database contract §7.2.5 | One-time handoff code "single use" across three or more edge replicas needs shared state; `afs_edge` has no table or function for it (RD-4) **Resolved in contract v1.3.1 by PC-D22.** |
| G3 | Database contract §7.2.5 `collab_principal` | It needs the full groups list on every decision, which cannot ride in a cookie for people with hundreds of groups; no function returns the group names bound on a provider (RD-5) **Resolved in contract v1.3.1 by PC-D22.** |
| G4 | Change PRD COL-16 | "Host and SNI differ" cannot be checked behind an ALB that terminates TLS (RD-7) **Resolved in contract v1.3.1 by PC-D23.** |
| G5 | Change PRD COL-31 | No path is defined for edge events to reach a team's log delivery (RD-8) **Resolved in contract v1.3.1 by PC-D24.** |
| G6 | Database contract §7.2.4 and §7.2.6 | `afs_reconciler` can write only `managed_by = 'paperclip'` secret rows, yet COL-12 and COL-32 have the reconciler create `afs`-managed `paperclip-agent-api-key` and `paperclip-board-api-key` and bind the key (RD-10) **Resolved in contract v1.3.1 by PC-D25.** |
| G7 | Change PRD COL-3 and PC-OQ-2 | The role may write only below `paperclip/`, but the board key's managed path is outside it, and the first-admin bootstrap has no defined actor (RD-11) **Resolved in contract v1.3.1 by PC-D26.** |
| G8 | API contract §7 | No event or message type for the first admin invite (RD-17) **Resolved in contract v1.3.1 by PC-D27.** |
| G9 | API contract §4.4b, change PRD §10 | `ListCollaborationEndpoints` is P2 but its screen (S10 tab) is in C2 (RD-18) **Resolved in contract v1.3.1 by PC-D28.** |
| G10 | API contract §4.4, change PRD COL-9 | Swarm templates "may include" a collaboration agent but no template `spec` field or `instantiate_work_order_team` behaviour is defined; this repository builds nothing for it until a contract adds it **Resolved in contract v1.3.1 by PC-D29.** |
## 10. Definition of done (every change)

- [ ] API change in `api/openapi.yaml` (and the API contract if it is new) in the same PR; consumers told
- [ ] Gateway change in the gateway contract in the same PR
- [ ] Route declares a permission; allowed and denied tests per role; the catalog-equality test is green
- [ ] Mutation writes row, audit and outbox in one transaction
- [ ] No secret value, token or key in responses, logs, traces, errors or events
- [ ] `make gate` green; acceptance criteria above for the story covered by tests

Added for the Paperclip change (every story in §5 for it):

- [ ] Contract extracts resynced (`make contract`, `make openapi`) in the same PR as the first story that needs them; `make contract-check` and `make openapi-check` green; `-cli` and `-website` told to regenerate
- [ ] `afs-edge` change: `internal/archtest` green (domain stdlib only, usecase imports domain and authz only), `internal/usecase/edge` and `internal/domain/collab` at or above 90% coverage, captured-output scan test green (no token, key, cookie, matched text or Paperclip content anywhere)
- [ ] Every edge status code and `details.reason` of API contract §4.4c has a test; every `collaboration_access` outcome has a test
- [ ] Terraform: `make deploy-test`, `terraform validate`, `tflint` and `terraform test` green; no hardcoded account, region, zone or subnet id
- [ ] Helm: chart tests green; nothing in the chart creates an `Ingress`, `LoadBalancer`, `NodePort` or Gateway object for any team
- [ ] A new route, role or permission updates `internal/authz`, `permission.ts` (told to `-website`) and the catalog-equality test in the same cycle
- [ ] `user-docs/` runbook for operators: enabling `collab.base_domain`, provider collaboration clients and redirect URI, DNS and certificate, the allow list, what the edge records, the low-trust review preset advice (COL-29), kill switches, the RD-19 outage behaviour; it does not link into `specs/`
- [ ] Anything in §8.1 that a story touched is still listed there, honestly, until it has run against the real thing

## 11. Review record (Paperclip change)

`review-prd` pass of 2026-11-22 (dev-flow `commands/review-prd.md`, run by hand, with the author acting as the user for
step 5), over the whole file with attention to the additions of §4.2, §5 (RA-US-19..19d, RC-US-19..20b, EDGE-US-1..2,
INF-US-1), §6 to §9 and §10. Verdict after fixes: **Ready for spec**.

| Dimension | Status | Note |
|---|---|---|
| Problem clarity | ✅ Pass | §1 and §2 name the fifth binary, the public entry and what stays out; the problem is the change PRD's §2, cited, not repeated |
| Scope definition | ✅ Pass | In and out lists extended; the change PRD's non-goals restated for this repository |
| Functional completeness | ✅ Pass | Every COL-n and SEC-7 that touches this repository maps to at least one RA, RC, EDGE or DP requirement (traceability paragraph of §4.2); COL-29 is documentation only and is in §10 |
| NFR coverage | ✅ Pass | Edge performance, scale, availability, security, privacy, observability and recovery added to §6 |
| Acceptance criteria | ✅ Pass | Given / When / Then with exact names, codes and timings; every story ends in test names; criteria that need a real account or cluster are tagged [parked] and listed in §8.1 |
| Dependency identification | ✅ Pass | `-db` 000020, image, Paperclip, IdP clients, DNS and certificate, WAF and ALB, Load Balancer Controller, Terraform 1.7 added to §7 |
| Open questions | ✅ Pass | RA-OQ-1..6 with owner, milestone and default; PC-OQ inherited; decisions RD-1..19 and gaps G1..G10 recorded |

Fixed in this pass: a decision id used for two different decisions (RD-5) and a reference to a section that did not exist
(§9a, now §8.1); the board-key decision (RD-11) rewritten to a testable default; one gap raised in a first draft that the
contract in fact resolves was removed; COL-32 and COL-33 had no owning requirement (added RC-29); COL-9 had no criterion
(added to RA-US-19b); the decision-cache criterion was ambiguous about the 60 s bound (now explicit at t+59 s and
t+61 s); parked work was scattered through stories and is now one table (§8.1); the contract-extract resync note was
missing from the header (added).

Decisions logged: RD-1 to RD-23 (§9.1). Contract gaps found in the first pass: G1 to G10 (§9.2), all since resolved.

### 11.1 Aligned to v1.3.1 (2026-11-22)

Re-read against API contract v1.3.1, database contract §7.2 (including §7.2.8 and §7.2.9) and PC-D20 to PC-D31 (root `main` 1f60ecd). Every dimension stays Pass. Changed: edge sessions and handoffs are server-side (ten `afs_edge` functions, no `collab_session_create`: the handoff consume creates the session under a hash the edge generates), so the cookie-nonce, per-replica cache and cookie-splitting designs were removed (EDGE-3, EDGE-US-1a, RD-4, RD-5); apex-only callback and certificate (PC-D21); no SNI check and no `host_mismatch` (PC-D23); the log field is `afs.team_id` and the Helm chart carries the Fluent Bit routing rule (PC-D24, DP-2); the three key functions replace direct secret writes (PC-D25); the pod entrypoint bootstraps and the role's exact-ARN `PutSecretValue`, `collaboration.bootstrap_completed` and the direct invite (PC-D26, PC-D27); C1 priorities for links, connect and disconnect and C2 for endpoints (PC-D28); swarm collaboration added (RA-22, RC-30, RA-US-19e, RC-US-20c; PC-D29); `hostname_held`, collaborator conditional grants and `RotateSecret` without a body (PC-D30); 403 versus 404 and once-only key passthrough (PC-D31); the hourly session purge (RC-27). Gaps G1 to G10 and RA-OQ-2 to RA-OQ-4 closed with their PC-D ids. Remaining inconsistency: none found in the contracts; the image's support for `AFS_PAPERCLIP_BOOTSTRAP` is an upstream dependency, not a contract question.
