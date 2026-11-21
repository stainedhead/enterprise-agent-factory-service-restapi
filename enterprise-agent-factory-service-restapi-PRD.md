# Enterprise Agent Factory Service — Control plane PRD (`-restapi`)

| | |
|---|---|
| **Status** | Draft for review (2026-11-21) |
| **Author** | J (Enterprise Architecture), drafted with Claude |
| **Parent** | [Root PRD](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/enterprise-agent-factory-service-initial-PRD.md) (stories US-1..US-16); [solution design](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/solution-design.md) |
| **Provides** | [API contract](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/api-contract.md) → `api/openapi.yaml`; EventBridge events (contract §7) |
| **Consumes** | [Database contract](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/docs/contracts/database-contract.md) at a `-db` tag; Kubernetes (EKS), AWS (IAM, Pod Identity, STS, Secrets Manager, Bedrock, Organizations, Price List, CUR via Athena, EventBridge); Cilium/Hubble; External Secrets Operator; Agentic Teammates CRDs |

## 1. Purpose

The control plane: four Go binaries that turn people's intent into a running, isolated, metered
environment.

| Binary | Job |
|---|---|
| `afs-api` | The REST API and the security boundary: authentication, RBAC, validation, desired-state writes with audit and outbox, write-only secret values, reports |
| `afs-reconciler` | Leader-elected. Applies desired state to EKS and AWS continuously, reverts drift, reports status; runs the activity and traffic collectors, the outbox publisher and partition maintenance |
| `afs-gateway` | The only path from agents to Bedrock. Authenticates the agent, reserves worst-case cost, invokes, settles, meters, tags and fails closed |
| `afs-billing` | Daily jobs: CUR ingestion and reconciliation with variance alerts, AWS Price List sync proposals, showback exports |

## 2. Scope

**In:** the four binaries, `api/openapi.yaml`, Helm chart and Terraform modules under `deploy/`,
runbooks in `user-docs/`. **Out:** the schema (owned by `-db`), clients (`-cli`, `-website`), the agent
runtime (Agentic Teammates).

## 3. Architecture constraints

- Clean Architecture: `domain` ← `usecase` ← `adapter/*`, `cmd/*` is the composition root. `domain` and
  `usecase` import no HTTP, SQL, Kubernetes or AWS packages.
- RBAC is decided only in `internal/authz` (API contract §3). Every route declares its permission; a
  route without one fails a test.
- `afs-api` never calls Kubernetes and calls AWS only for Secrets Manager writes and STS reads it needs
  to validate input. Everything else is the reconciler's.
- Every mutation: row + audit + outbox in one transaction (database contract §5).
- Secrets never touch the database, logs, errors, traces or events.

## 4. Requirements owned

| Id | Binary | Requirement | Product reqs | Pri |
|---|---|---|---|---|
| RA-1 | api | Implement every operation in the API contract §4 with its permission, conventions (§1) and errors (§6); serve `api/openapi.yaml` at `/v1/openapi.yaml` | IF-1, ACC-3 | P0 |
| RA-2 | api | OIDC token validation for every enabled provider (JWKS cache 10 min, issuer and audience checks); team-scoped group bindings only from the team's provider; API key auth by SHA-256 lookup | ACC-1, ACC-2, ACC-4 | P0 |
| RA-3 | api | `GET /v1/me`: bindings and effective permissions per team | ACC-3 | P0 |
| RA-4 | api | Session settings on every transaction; optimistic concurrency; idempotency keys; request ids | CFG-1 | P0 |
| RA-5 | api | Secret create, put-value and rotate write to Secrets Manager under `afs.secret_path`, then store the ARN; the value is held only for the call | SEC-1, SEC-2, SEC-4, SEC-5 | P0 |
| RA-6 | api | Reports: `/reports/{usage\|costs\|activity\|traffic}` at service, team and agent level, groupBy incl. `tag:<key>`, CSV; scheduled reports | RPT-1..9, TAG-6 | P0/P1 |
| RA-7 | api | Price proposals: list, approve, reject (service admins), audit and events | BUD-9 | P1 |
| RC-1 | reconciler | Per team: namespace, Pod Security `restricted`, ResourceQuota, LimitRange, default-deny NetworkPolicies, Cilium FQDN egress per agent, team IAM role with permissions boundary, ESO SecretStore, Bedrock application inference profiles per enabled model | TEAM-2, TEAM-3, AGT-4 | P0 |
| RC-2 | reconciler | Per agent: Agentic Teammates custom resource rendering a StatefulSet (1 replica) with ServiceAccount, Pod Identity association, volume claim, ConfigMap (`envFrom` + `/etc/afs/config`), ExternalSecrets for bindings (env or file), `/etc/afs/aws/config`, `/etc/afs/tags.json` and `AFS_TAGS`, gateway token projection, pod-template hash of referenced versions | AGT-1..3, AGT-7, AGT-9, SEC-3, SEC-6, CFG-7, TAG-4 | P0 |
| RC-3 | reconciler | Dedicated agent roles and cross-account links: `sts:AssumeRole` statements, link verification on create, daily and on demand, status write-back | AGT-6, AGT-10, TEAM-8 | P1 |
| RC-4 | reconciler | Drift: watch owned objects, revert changes, audit `drift.corrected`; full resync every 10 minutes and at startup | CFG-2 | P0 |
| RC-5 | reconciler | Status: phase, conditions, observed generation per resource into `resource_status` | TEAM-5 | P0 |
| RC-6 | reconciler | Budget backstop: IAM explicit deny on the team's Bedrock role at exhaustion, removed on reset or increase | BUD-3 | P0 |
| RC-7 | reconciler | Collectors: activity every 5 minutes; Hubble flows to `external_traffic` with denied reasons; sign-in events; `egress.denied_spike` | RPT-5, RPT-6, OPS-4 | P0/P1 |
| RC-8 | reconciler | Outbox publisher to EventBridge, at-least-once, ordered per aggregate; partition maintenance; org account mirror hourly | OPS-2, TEAM-8 | P0 |
| RC-9 | reconciler | Tags: apply effective tags to every AWS resource it manages, add and remove on change | TAG-1, TAG-4, TAG-5 | P1 |
| RC-10 | reconciler | Ephemeral teams: teardown at cap or expiry per the team's rule, keep config and audit | TEAM-7, BUD-8 | P2 |
| GW-1 | gateway | Authenticate agents by projected ServiceAccount token (audience `afs-gateway`); resolve team, agent and allowed models | AGT-4 | P0 |
| GW-2 | gateway | Reserve worst-case cost (`max_tokens` × output price + input estimate) with `reserve_spend`; reject with `BudgetExceeded` when false; invoke through the team's inference profile; settle to actual; write `usage_events` | BUD-2, BUD-5 | P0 |
| GW-3 | gateway | Fail closed: database or reservation unavailable → reject; no call goes to Bedrock without a reservation | BUD-2, NFR | P0 |
| GW-4 | gateway | Thresholds: emit `budget.threshold_crossed` once per period per threshold | BUD-4 | P0 |
| GW-5 | gateway | Add the agent's effective tags as Bedrock `requestMetadata`; cache per agent, refresh on `team.tags_changed` | TAG-1, TAG-4 | P1 |
| GW-6 | gateway | Streaming responses supported; tokens counted from the final usage block | BUD-2 | P0 |
| BI-1 | billing | Daily CUR ingest: Bedrock by inference-profile tag, EKS split cost allocation by namespace, EBS by `afs-team-id`; write `cost_daily` | BUD-6 | P1 |
| BI-2 | billing | Reconcile gateway vs billed per team-day; flag above `billing.variance_alert_pct`; emit `cost.variance_detected` | BUD-6 | P1 |
| BI-3 | billing | Daily AWS Price List read when `pricing.price_list_sync` is on; supersede the old pending proposal and propose a changed price | BUD-9 | P1 |
| BI-4 | billing | Showback CSV by cost center and any tag | BUD-7, TAG-6 | P1 |

## 5. User stories and acceptance criteria

Stories refine the product stories (US-n). Each acceptance criterion maps to at least one automated
test; the test level is in brackets: [unit], [adapter] (real PostgreSQL or fakes), [envtest]
(Kubernetes API), [e2e] (dev cluster).

### afs-api

**RA-US-1 · One enforcement point.** *(US-2, US-3)* As security, I want every route to declare a
permission and pass one authorization function.
- A table-driven test enumerates every route in the router and fails if one has no permission [unit].
- For every operation and each of the three roles plus an unbound user, the allowed role gets 2xx and
  each denied role gets `403 AccessDenied` with `details.requiredPermission` [adapter].
- A team-scoped route takes `teamId` from the path only; a body `teamId` that differs → `ValidationError`
  [unit].
- A resource in a team the caller is not bound to returns `404`, never `403` [adapter].

**RA-US-2 · Sign-in through the right provider.** *(US-1, US-2)* As a team member, I want to sign in
through my team's provider and get my roles.
- A token from an enabled provider with a valid signature, issuer, audience and expiry is accepted; a
  disabled provider's token → `401` [unit].
- A group binding on team A applies only when the token's issuer is team A's provider (or the default
  when A has none) [unit].
- `GET /v1/identity-providers` works without a token and returns no client secret ARN [adapter].

**RA-US-3 · Create a team end to end.** *(US-1)*
- `CreateTeam` with required tags, models, provider and first team-admin writes all rows, one audit
  record and one `team.created` event in one transaction; any failure writes none [adapter].
- Missing required tags → `400 ValidationError` with `details.missingTags` [adapter].
- Repeating the request with the same `Idempotency-Key` returns the first response; a different body
  → `409` [adapter].

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

**RA-US-6 · Optimistic concurrency.** *(US-13)*
- Update without `If-Match` → `428`; with a stale version → `412`; with the current version → 200 and
  `ETag` advanced by one [adapter].

**RA-US-7 · Reports.** *(US-11, US-12)*
- Each report kind at each level returns the same totals as summing the underlying table for the range
  [adapter].
- Ranges over two days read daily views; shorter ranges read 5-minute tables; p99 < 1 s for 90 days at
  the initial scale [load].
- `Accept: text/csv` returns the same rows as JSON [adapter].
- `groupBy=tag:business-unit` returns one row per value plus an untagged row [adapter].

**RA-US-8 · Approve prices.** *(US-10)*
- Only service admins can approve or reject; approval publishes `pricing.proposal_decided` and the
  gateway uses the new price for calls after its effective time [adapter, e2e].

### afs-reconciler

**RC-US-1 · Converge a team.** *(US-1, US-3)*
- Given a new team row, the namespace, quotas, Pod Security labels, default-deny policies, team IAM role
  with boundary, SecretStore and inference profiles exist within 60 s (p95) and `resource_status` is
  `Ready` with `observedGeneration = generation` [envtest, e2e].
- Admission refuses NetworkPolicy, LoadBalancer or NodePort Services, hostNetwork and privileged pods in
  team namespaces [e2e].

**RC-US-2 · Zero-impact agent pods.** *(US-4, US-5)*
- The rendered pod has: env vars from the ConfigMap and from Secret refs for env bindings; files for
  file bindings at their exact targets; `/etc/afs/config/<NAME>`; `/etc/afs/aws/config` when links exist;
  `/etc/afs/tags.json` and `AFS_TAGS`; the gateway token; no AWS keys [envtest].
- Rotating a secret or changing a config value changes the pod-template hash and rolls only the agents
  that reference it [envtest].
- A container image with no AFS-specific code receives all of the above (verified with `busybox env`
  and `cat` in e2e).

**RC-US-3 · Revert drift.** *(US-13)*
- Editing or deleting an owned object restores it within 60 s and writes one `drift.corrected` audit
  record [envtest].
- Starting against a fresh cluster and a restored database converges everything with no manual step
  [e2e restore drill].

**RC-US-4 · Cross-account links.** *(US-7)*
- A new link gets an `sts:AssumeRole` statement on its exact role ARN, a profile in the pod's AWS config,
  status *Pending trust*, then *Verified* after a successful assume with the external ID and session
  tags, or *Failed* with the AWS error code [adapter with fake STS].
- Narrowing the allowed-accounts filter disables links outside it on the next pass [adapter].

**RC-US-5 · Egress follows external systems.** *(US-8)*
- Switching a system off at service, team or agent level removes its FQDNs from the affected agents'
  Cilium policies within 60 s, without restarting pods [envtest].
- Denied flows are written with the right `denied_reason` [adapter with recorded Hubble flows].

**RC-US-6 · Activity.** *(US-11)*
- Each running agent gets one sample per 5-minute window; working when it made a model call, reported
  busy, or used CPU above `reports.idle_cpu_millicores` [unit with fakes].

**RC-US-7 · Tags everywhere.** *(US-12)*
- Every AWS resource the reconciler creates carries the agent's or team's effective tags; a change is
  applied within 10 minutes; `afs-team-id` and `afs-agent-id` are always present and never removed
  [adapter with fake AWS].

### afs-gateway

**GW-US-1 · Hard cap under concurrency.** *(US-9)*
- Load test: 200 agents at 500 req/s against a nearly exhausted budget; total spend exceeds the budget
  by at most one in-flight request per agent, and every rejected call gets `BudgetExceeded` [load].
- With the database unreachable, every call is rejected within 2 s and none reaches Bedrock [adapter].
- Added latency p99 < 25 ms excluding Bedrock time [load].

**GW-US-2 · Attributed calls.** *(US-12, US-14)*
- Every Bedrock call uses the team's inference profile and carries `requestMetadata` with the agent's
  effective tags including `afs-team-id` and `afs-agent-id` [adapter with fake Bedrock].
- A token from a pod of team A can never invoke through team B's profile [unit].

### afs-billing

**BI-US-1 · Reconciliation and variance.** *(US-10)*
- Given CUR rows for a day, `cost_daily` holds Bedrock, EKS and storage costs per team, and
  `cost_reconciliations` is `over_threshold` when the difference exceeds the setting, with one
  `cost.variance_detected` event [adapter with fixture CUR].

**BI-US-2 · Price sync.** *(US-10)*
- With sync on and a changed price in the Price List fixture, exactly one pending proposal exists per
  model after the run; an older pending proposal is superseded; with sync off, nothing is proposed
  [adapter].

## 6. Non-functional requirements

| Area | Requirement |
|---|---|
| Availability | `afs-api` and `afs-gateway` 99.9% monthly, at least 3 replicas across AZs; reconciler leader election with < 30 s failover |
| Performance | API p99 < 300 ms for reads; gateway p99 overhead < 25 ms; reconcile lag p95 < 60 s |
| Scale | 200 teams, 2,000 agents, 500 gateway req/s (initial) |
| Security | No long-lived AWS keys (Pod Identity for all binaries); OWASP ASVS L2; `Authorization` and tokens redacted in logs; agents' roles never get `bedrock:*` |
| Observability | OpenTelemetry traces and metrics; structured JSON logs with request id; RED metrics per route; reconcile and gateway SLO dashboards |
| Quality | `domain`, `usecase`, `authz` ≥ 90% coverage; allowed and denied test per route per role; `make gate` before every commit |

## 7. Delivery

| Milestone | This repository |
|---|---|
| M0 | OpenAPI from the API contract for `/v1/me`, teams and members; auth middleware; adopt `-db` `v0.10.0` |
| M1 | RA-1..4 for teams, members, keys; RC-1, RC-4, RC-5 |
| M2 | Agents, secrets, config, external systems (RA-1, RA-5); RC-2, RC-7 traffic |
| M3 | GW-1..4, GW-6; RC-6, RC-7 activity; RA-6 reports |
| M4 | RC-3, RC-9, GW-5, BI-1..4, RA-7, scheduled reports, export and import |
| Phase 2/3 | RC-10, work orders |

## 8. Definition of done (every change)

- [ ] API change in `api/openapi.yaml` (and the API contract if it is new) in the same PR; consumers told
- [ ] Route declares a permission; allowed and denied tests per role
- [ ] Mutation writes row, audit and outbox in one transaction
- [ ] No secret value, token or key in responses, logs, traces, errors or events
- [ ] `make gate` green; acceptance criteria above for the story covered by tests
