# Spec: `-restapi` feature complete

| | |
|---|---|
| **Spec** | `specs/261121-restapi-feature-complete/` (created 2026-11-21 by `create-spec`, step 1 of `implm-frm-prd`) |
| **Source PRD** | [`enterprise-agent-factory-service-restapi-PRD.md`](enterprise-agent-factory-service-restapi-PRD.md) (moved here; the repo root keeps a stub that points to it) |
| **Branch** | `feat/restapi-feature-complete` |
| **Scope** | Feature complete: every requirement of the PRD §4 and every story of §5. Milestones M0–M6 are build order only; all are in scope |
| **Contracts (followed exactly)** | Root `docs/contracts/api-contract.md` v1.1.0, `database-contract.md` (schema 000014), `gateway-contract.md` v1.0.0 |
| **Schema** | `enterprise-agent-factory-service-db` migrations 000001–000014 at commit `b0d8754` (no tag yet), adopted as Go pseudo-version `v0.0.0-20261121202720-b0d8754e4659` |
| **Status** | Draft (step 1); review in step 2 |

## 1. Executive summary

The control plane of the Enterprise Agent Factory Service is four Go binaries in one module. `afs-api` is
the REST API and the only security boundary: it authenticates people, API keys and workloads, decides
every access in `internal/authz`, validates input, and writes desired state with an audit record and an
outbox event in one transaction. `afs-reconciler` turns that desired state into isolated team namespaces,
IAM roles, agent and pool workloads, egress policy, log delivery, notifications and export bundles, and
reverts drift. `afs-gateway` is the only path from agents to Bedrock: it identifies the calling pod,
reserves the worst-case cost against the team's budget, invokes the team's inference profile and fails
closed. `afs-billing` ingests the AWS Cost and Usage Report, reconciles it against gateway spend, proposes
price changes and delivers scheduled reports.

This spec turns the PRD into an implementation plan for a sequence of milestone agents (M0 to M6), each of
which may split its milestone into parallel worker streams in git worktrees. The full v1 OpenAPI document
is produced in M0 so that `-cli` and `-website` can generate clients while the handlers are built.

## 2. Problem statement

The repository is a scaffold: four binaries start and stop, `authz` holds an outdated three-role matrix
(`budgets:set-ceiling`, no security operator, no workload principal, no conditional grants), and
`api/openapi.yaml` holds two health routes. The root PRD, the three contracts and the `-db` schema
(000001–000014) now define the full product. Clients (`-cli`, `-website`) are blocked on the OpenAPI
document; agents are blocked on the gateway; every product capability is blocked on the control plane.

## 3. Goals and non-goals

### Goals

1. Every operation of API contract §4 (182 operationIds in §4.1–§4.11, `GetHealth` and `GetReadiness` included) with its permission, conventions
   (§1), errors (§6) and events (§7), and `api/openapi.yaml` describing all of them from M0.
2. One enforcement point: `internal/authz` equals contract §3 exactly; every route declares a permission;
   the 401/404/403 rule of PRD §3 is applied by one middleware.
3. A reconciler that converges teams, agents and pools to desired state within 60 s (p95), reverts drift
   and rebuilds everything from a restored database in 1 hour.
4. A gateway that enforces hard budget caps under concurrency, fails closed, and adds < 25 ms p99.
5. Billing that reconciles gateway spend against CUR, proposes prices and delivers reports.
6. Deployment (Helm, Terraform) with RPO 5 min and HA across AZs.

### Non-goals

- The schema (owned by `-db`; this repository never creates or alters tables).
- Clients (`-cli`, `-website`), the agent runtime (Agentic Teammates), shared named roles in target
  accounts (deployed by account owners), metering of non-Bedrock model providers (root EXT-6).
- A search API (D44). An agent replica count (AGT-7). `audit_log` partitioning (OQ-4).
- Reading or storing any secret value in `afs-api` beyond holding a write-only value for one call.

## 4. User requirements and functional requirements

Functional requirements keep the PRD ids, which every task, test and commit uses. FR numbers are the
`create-spec` index. Full text: PRD §4 (authoritative); the summaries below are for navigation.

| FR | PRD id | Binary | Summary | Pri | Milestone |
|---|---|---|---|---|---|
| FR-001 | RA-1 | api | Every operation of contract §4; `ListTeamDirectory`; agent template ops; `/v1/openapi.yaml`; `X-Afs-Contract-Version` on every response | P0 | M0 (OpenAPI, routes) → M1–M6 (handlers) |
| FR-002 | RA-1a | api | `authz` = contract §3 exactly; roles incl. `security_operator` and workload; `Authorize(principal, permission, resource)` with conditional grants | P0 | M0 |
| FR-003 | RA-2 | api | OIDC validation per enabled provider (JWKS 10 min, refetch on unknown `kid`); team group bindings only from the team's provider; API keys by SHA-256, `lastUsedAt` ≤ 1/min | P0 | M1 |
| FR-004 | RA-2b | api | Workload principal from projected token (aud `afs-api`), validated offline against the cluster issuer JWKS | P2 | M6 |
| FR-005 | RA-3 | api | `GET /v1/me` with effective permissions per team, flag-dependent | P0 | M1 |
| FR-006 | RA-4 | api | Session settings per transaction; `If-Match`; idempotency (D52); `X-Request-Id`; `kind` immutable | P0 | M0 (store), M1 (HTTP) |
| FR-007 | RA-5 | api | Write-only secrets via Secrets Manager under `afs.secret_path` | P0 | M2 |
| FR-008 | RA-6 | api | Reports (usage, costs, activity, traffic) at service, team, agent level; groupBy incl. `tag:<key>`, `pool`; CSV; forecast; report schedules (D23) | P0/P1 | M3, M4 |
| FR-009 | RA-7 | api | Price proposals list/approve/reject; effective `max(effectiveFrom, approvedAt)` | P1 | M4 |
| FR-010 | RA-8 | api | Ephemeral teams, stages, pools, handoffs, pool secrets/config/external systems/role links | P2 | M5 |
| FR-011 | RA-9 | api | Security operators, service keys, work orders with D38 owner resolution, swarm templates | P2 | M6 |
| FR-012 | RA-10 | api | `GetTeamKubeconfig`; isolation tier and viewer kubectl as guardrails | P1/P2 | M5 (kubeconfig); M1 (guardrail fields) |
| FR-013 | RA-11 | api | `ListCostTagStatus`; banner via `PutSetting` | P2 | M5 |
| FR-014 | RA-12 | api | `CreateExport` (async), `GetExport`, `ListExports`, `CreateImport` (sync, dry run default) | P1 | M4 |
| FR-015 | RA-13 | api | `RestoreRevision` with dry run | P1 | M4 |
| FR-016 | RA-14 | api | Budget rules: effective ceiling, override (D34), `notify` (D33), lifetime budgets | P0 | M3 (budget), M1 (override fields on team) |
| FR-017 | RA-15 | api | Rate limit 20 req/s burst 40 per principal (replica share); CORS from `api.cors_origins` | P0 | M1 |
| FR-018 | RA-16 | api | `GetSettings`/`PutSetting` with type and range checks | P0 | M1 |
| FR-019 | RC-1 | reconciler | Team environment: namespace, PSS, quota, LimitRange, default-deny, Cilium, IAM roles (team, Bedrock), ESO SecretStore, inference profiles, gateway reach policy, names by id (D25) | P0 | M1 |
| FR-020 | RC-2 | reconciler | Agent workload: Agentic Teammates CR → StatefulSet, SA, Pod Identity, PVC, ConfigMap, ExternalSecrets, AWS config (D51), tags, gateway env (D57), token projections, template hash; dedicated NodePools | P0 | M2 |
| FR-021 | RC-3 | reconciler | Dedicated roles, cross-account links, verification | P1 | M4 |
| FR-022 | RC-4 | reconciler | Change feed `LISTEN afs_desired_state`, 30 s poll, 10 min resync, drift revert (D30) | P0 | M1 |
| FR-023 | RC-5 | reconciler | Status vocabulary (D24), `TemplateRetired` (D54) | P0 | M1 (engine), M2 (agents) |
| FR-024 | RC-6 | reconciler | Budget backstop IAM deny ≤ 60 s | P0 | M3 |
| FR-025 | RC-7 | reconciler | Activity and traffic collectors, sign-ins to log delivery, `egress.denied_spike` | P0/P1 | M2 (traffic), M3 (activity) |
| FR-026 | RC-8 | reconciler | Outbox publisher, partitions, retention (D36), org account mirror, idempotency purge | P0 | M1 (org mirror M4) |
| FR-027 | RC-9 | reconciler | Effective tags on every AWS resource, cap 40 (D27) | P1 | M4 |
| FR-028 | RC-10 | reconciler | Ephemeral teardown through the RC-17 delete flow | P2 | M5 |
| FR-029 | RC-11 | reconciler | Pools: Deployment, identity, queues (D32), shared roles | P2 | M5 |
| FR-030 | RC-12 | reconciler | Work orders: intake, remediation team, GitHub PR tracking (OQ-5), notifications, SLA | P2 | M6 |
| FR-031 | RC-13 | reconciler | Viewer kubectl access entry (OQ-9) | P2 | M5 |
| FR-032 | RC-14 | reconciler | Spend periods, reservation sweeper, override expiry (D31, D34) | P0 | M3 |
| FR-033 | RC-15 | reconciler | Notification delivery via EventBridge rules, API destinations, SES (D46) | P1 | M3 |
| FR-034 | RC-16 | reconciler | Agent volumes, expansion, snapshots, re-create from snapshot | P0 | M2 |
| FR-035 | RC-17 | reconciler | Lifecycle: pause/resume/restart, suspend/resume, ordered delete finalizer (D53) | P0 | M1 (team), M2 (agent), M5 (pool) |
| FR-036 | RC-18 | reconciler | Per-team Fluent Bit aggregator, outputs, redaction | P1 | M2 |
| FR-037 | RC-19 | reconciler | ValidatingAdmissionPolicies (OQ-6) | P0 | M1 |
| FR-038 | RC-20 | reconciler | Export bundles to S3 (KMS), pre-signed link 7 days | P1 | M4 |
| FR-039 | GW-1 | gateway | Bedrock- and Anthropic-compatible paths; caller by source pod IP (D57); optional token | P0 | M3 |
| FR-040 | GW-2 | gateway | Worst-case reservation, invoke via team profile/role, settle, usage rows | P0 | M3 |
| FR-041 | GW-3 | gateway | Fail closed in ≤ 2 s; Bedrock via VPC endpoint only | P0 | M3 |
| FR-042 | GW-4 | gateway | Thresholds once per period; `budget.exhausted` once | P0 | M3 |
| FR-043 | GW-5 | gateway | `requestMetadata` ≤ 16 pairs in D27 order | P1 | M4 |
| FR-044 | GW-6 | gateway | Streaming settlement | P0 | M3 |
| FR-045 | GW-7 | gateway | Held reservations, settle always, sweeper releases | P0 | M3 |
| FR-046 | BI-1 | billing | Daily CUR ingest to `cost_daily` | P1 | M4 |
| FR-047 | BI-2 | billing | Variance alerts | P1 | M4 |
| FR-048 | BI-3 | billing | Price List sync proposals | P1 | M4 |
| FR-049 | BI-4 | billing | Showback CSV | P1 | M4 |
| FR-050 | BI-5 | billing | Report schedule delivery | P1 | M4 |
| FR-051 | BI-6 | billing | Cost allocation tag status | P2 | M5 |
| FR-052 | BI-7 | billing | Costs under billed tag sets, pool | P1 | M4 |
| FR-053 | DP-1 | deploy | Terraform (RDS PITR, Backup, export bucket, IAM) and Helm (4 binaries, HA) | P0 | M1 |

Settings read by this repository: PRD §4.1. `GetSettings`/`PutSetting` validate every key of root PRD §6.13
(also `secrets.path_prefix`, which the PRD's §4.1 does not list because only the database reads it).

### 4.1 Contract-derived requirements

The PRD defers to the contracts. These obligations come from the contracts directly and are tested like
PRD acceptance criteria (ids `CT-…`, listed in §9.2):

- API contract §1 conventions (pagination and token expiry, unknown query parameters, CSV with
  `X-Afs-Next-Token`, CORS headers), §5 shapes, §6 errors incl. `428`, §7 event catalog.
- Gateway contract §2 paths incl. `404` on unsupported operations, §4 `maxTokens` capping, §7 errors
  (`400`, `413` over 20 MB, `429` pass-through, `502 UpstreamError` releasing the reservation), §8 usage
  rows written in batches within 1 s, §9 `X-Afs-Gateway-Contract-Version` on every response.
- Database contract §1 (services other than `afs-api` refuse to start on an older schema), §5 (retry
  `40001`/`40P01` up to 3 times), §6 (SQLSTATE → API code mapping).

## 5. Non-functional requirements

PRD §6 is authoritative. Summary and how each is verified:

| Area | Requirement | Verified by |
|---|---|---|
| Availability | `afs-api`, `afs-gateway` 99.9%, ≥ 3 replicas across AZs; reconciler leader failover < 30 s | Helm chart tests (M1-K), leader election test (M1-F) |
| Performance | API reads p99 < 300 ms (reports p99 < 1 s at 90 days); gateway overhead p99 < 25 ms; reconcile lag p95 < 60 s | Load suite `test/load` (M3-D, M6-F) |
| Scale | 200 teams, 2,000 agents, 500 gateway req/s | Load suite |
| Recovery | RPO 5 min; RTO 1 h | Terraform (M1-K), restore drill `test/e2e/restore` (M6-F) |
| Retention | 13 months for partitioned facts; audit forever | RC-8 tasks |
| Rate limiting | 20 req/s, burst 40 | RA-US-15.1 |
| Security | No long-lived AWS keys; OWASP ASVS L2; redaction; agents never get `bedrock:*` | Secret fuzz test (RA-US-5.3), IAM policy unit tests, ASVS checklist (M6-F) |
| Observability | OTel traces/metrics; JSON logs with request id; RED per route; `/healthz`, `/readyz`, `/metrics` per binary; SLI dashboards and alerts | M1-L tasks, Helm `PrometheusRule` and dashboards |
| Quality | ≥ 90% coverage on `domain`, `usecase`, `authz`; allowed and denied test per route per role; `make gate` | `make cover`, `make gate`, route matrix test (M0-D) |

## 6. System architecture

See [architecture.md](architecture.md). Affected layers, all new unless stated:

- `api/`: `openapi.yaml` (bundled, modified), `src/` fragments (new), `contract/` machine extract (new),
  `embed.go` (new).
- `tools/contractgen`, `tools/openapibundle` (new).
- `internal/domain`, `internal/authz` (rewritten), `internal/usecase/*` (per area), `internal/usecase/reconcile/*`,
  `internal/usecase/gateway`, `internal/usecase/billing`.
- `internal/adapter/httpapi` (modified), `postgres`, `k8s`, `aws/*`, `oidc`, `github`, `gatewayhttp`.
- `cmd/afs-*` composition roots (modified), `deploy/helm/afs`, `deploy/terraform/modules/*` (new).
- `test/e2e`, `test/load`, `test/conformance` (new, build-tagged).

## 7. Scope of changes

| Kind | Items |
|---|---|
| Create | Everything listed in architecture.md §2 that does not exist today |
| Modify | `internal/authz/authz.go` (replaced by the contract catalog), `internal/adapter/httpapi/server.go`, `cmd/afs-*/main.go`, `api/openapi.yaml`, `Makefile` (targets `contract`, `openapi`, `envtest`, `test-db`, `e2e`, `load`, `conformance`), `.github/workflows/ci.yml` (envtest assets, OpenAPI drift check), `go.mod` |
| Dependencies added | `-db` module (pseudo-version above), `pgx/v5` v5.13.0, `go-jose/v4`, `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery`, `sigs.k8s.io/controller-runtime` (envtest and client), AWS SDK for Go v2 service modules, `pb33f/libopenapi` + `libopenapi-validator` (tests), `go.opentelemetry.io/otel`, `prometheus/client_golang`, `golang.org/x/time/rate`, `anthropic-sdk-go` (conformance tests only), `github.com/google/go-github` or a thin client (decided in research.md R-9) |
| Docs | `docs/technical-details.md`, `docs/architectural-decision-record.md` (ADR-002 accepted, ADR-003+ from architecture.md §8), `user-docs/` runbooks (Step 4) |

## 8. Breaking changes

| Area | Change |
|---|---|
| API | None for clients: no v1 operation was released. `api/openapi.yaml` grows from 2 to all operations; `info.version` = contract `1.1.0` |
| `authz` | The scaffold permission set changes (`budgets:set-ceiling` removed, about 40 permissions added); internal package only |
| Config | New environment variables per binary (architecture.md §7) |
| Schema | None (consumed from `-db`). A later `-db` 000015 is adopted by bumping the pseudo-version or tag (plan.md §6) |

## 9. Success criteria and acceptance criteria

The feature is complete when:

1. Every acceptance criterion in §9.1 and §9.2 has at least one automated test at the stated level, and
   tasks.md marks every task done.
2. `make gate` passes; `make cover` shows ≥ 90% for `domain`, `usecase`, `authz`.
3. The route test (every route has a permission), the catalog-equality test (authz = contract §3), the
   OpenAPI equality test (OpenAPI = contract §4) and the event catalog test (events = contract §7) pass.
4. `[envtest]`, `[adapter]` suites pass in CI; `[e2e]`, `[load]`, `[conformance suite]` and the restore drill
   pass on the dev cluster (run by the M6 milestone agent or a human with cluster access; results recorded
   in `implementation-notes.md`).
5. Each milestone's exit task (tasks.md) is done: gate green, status updated, docs updated, OpenAPI drift
   check green.

Quality gates per commit: `make gate` (AGENTS.md); per milestone: plus `make test-db`, `make envtest`.

### 9.1 PRD acceptance criteria

Id = story id + `.` + the criterion's position in the PRD story (`RA-US-3.2` is the second criterion of
RA-US-3). Text is the PRD's, unchanged; the bracket is the test level.

#### afs-api

**RA-US-1 · One enforcement point** *(US-2, US-3)* As security, I want every route to declare a

| AC | Criterion |
|---|---|
| RA-US-1.1 | A table-driven test enumerates every route in the router and fails if one has no permission [unit]. |
| RA-US-1.2 | A test asserts that the `authz` permission catalog and role grants equal the API contract §3 exactly (generated from the contract table); `-website` runs the same check against `permission.ts` [unit]. |
| RA-US-1.3 | For every operation and each caller type (viewer, team-admin, service admin, security operator, team API key with each role, service-level key, workload principal, unbound user), the allowed caller gets 2xx and each denied caller gets the answer of the §3 rule: `404` when not bound to the team, `403 AccessDenied` with `details.requiredPermission` when bound or on a service route [adapter]. |
| RA-US-1.4 | A security operator gets `403` on every `/v1/teams/{teamId}/…` route, for an existing team and for one that does not exist alike [adapter]. |
| RA-US-1.5 | Conditional grants: `teams:set-aws-accounts` allowed for a team-admin only when `mayManageAwsAccounts`, `teams:get-kubeconfig` only when `viewerKubectl`, `DeclareTeamExternalSystem` only when `mayAddExternalSystems`; each has an allowed and a denied case [unit]. |
| RA-US-1.6 | A team-scoped route takes `teamId` from the path only; a body `teamId` that differs → `ValidationError` [unit]. |
| RA-US-1.7 | Every `403` writes one audit record with outcome `denied` and the required permission; the request's own transaction writes nothing [adapter]. |

**RA-US-2 · Sign-in through the right provider** *(US-1, US-2)* As a team member, I want to sign in

| AC | Criterion |
|---|---|
| RA-US-2.1 | A token from an enabled provider with a valid signature, issuer, audience and expiry is accepted; a disabled provider's token → `401` [unit]. |
| RA-US-2.2 | A group binding on team A applies only when the token's issuer is team A's provider (or the default when A has none) [unit]. |
| RA-US-2.3 | JWKS are cached for 10 minutes; a token signed with a new key (unknown `kid`) triggers one refetch and is accepted; a key removed from the JWKS stops validating after the refetch [unit with fake JWKS]. |
| RA-US-2.4 | An expired or deleted API key → `401`; a valid key's `lastUsedAt` advances [adapter]. |
| RA-US-2.5 | `GET /v1/identity-providers` works without a token, returns `consoleClientId` and `cliClientId` for every enabled provider (D48) and no client secret ARN [adapter]. |

**RA-US-2b · Workloads report progress** *(US-16)*

| AC | Criterion |
|---|---|
| RA-US-2b.1 | A pool's `afs-api` token from team A, assigned a work order, can `UpdateWorkOrderRepo` on that work order and call `GetMe` [adapter]. |
| RA-US-2b.2 | The same token gets `404` on a work order not assigned to team A and on every route of team B, and `403` on every other route of team A and on service routes [adapter]. |
| RA-US-2b.3 | A token with audience `afs-gateway`, an expired token or one from another cluster's issuer → `401` [unit]. |
| RA-US-2b.4 | Validation is offline against the cluster issuer's JWKS: with the Kubernetes API unreachable a valid token still works, and no Kubernetes client is constructed in `cmd/afs-api` [unit with fake JWKS]. |

**RA-US-3 · Create a team end to end** *(US-1)*

| AC | Criterion |
|---|---|
| RA-US-3.1 | `CreateTeam` with required tags, models, provider and first team-admin writes all rows, one audit record and one `team.created` event in one transaction; any failure writes none [adapter]. |
| RA-US-3.2 | Missing required tags → `400 ValidationError` with `details.missingTags` [adapter]. |
| RA-US-3.3 | Repeating the request with the same `Idempotency-Key` returns the first response; a different body → `409` [adapter]. |
| RA-US-3.4 | A team-admin or viewer calling `CreateTeam` → `403` and one denied audit record [adapter]. |

**RA-US-3a · Who am I** *(US-2, ACC-3)*

| AC | Criterion |
|---|---|
| RA-US-3a.1 | `GetMe` for a viewer, a team-admin, a service admin, a security operator, a team API key and a workload returns the principal type, `serviceAdmin`, `securityOperator` and, per bound team, exactly the permissions `authz` grants, including `teams:get-kubeconfig` only on teams with `viewerKubectl` and `teams:set-aws-accounts` only with `mayManageAwsAccounts` [adapter]. |
| RA-US-3a.2 | A service admin gets `serviceAdmin: true` and the full list; a security operator gets `securityOperator: true` with `work-orders:read`, `work-orders:write` and `swarm-templates:read` and no team [adapter]. |

**RA-US-4 · Agents** *(US-4, US-21, AGT-11, D54)*

| AC | Criterion |
|---|---|
| RA-US-4.1 | `CreateAgent` refuses an image without `@sha256:` [unit], a model the team has not enabled, a storage class not offered, and a binding of another agent's secret [adapter]. |
| RA-US-4.2 | `UpdateAgent` refuses shrinking the volume or changing its storage class [unit]. |
| RA-US-4.3 | Given a disabled template or an image digest not on the template's allowed list, when `CreateAgent` (or `CreatePool`) names it, then `400 ValidationError` naming the template or digest; an unknown template → `400` [adapter]. |
| RA-US-4.4 | `ListAgentTemplates` returns only enabled templates; a team-admin calling `PutAgentTemplate` or `DeleteAgentTemplate` → `403`; a service admin deleting a template an agent or pool uses → `409 Conflict` (disable it instead) [adapter]. |
| RA-US-4.5 | Given an existing agent whose template is then disabled, then it is not changed by the API and the reconciler flags it `TemplateRetired` (RC-5) [adapter]. |
| RA-US-4.6 | No request or response field is a replica count [contract test]. |

**RA-US-5 · Write-only secrets** *(US-5, US-6)*

| AC | Criterion |
|---|---|
| RA-US-5.1 | `CreateSecret` calls Secrets Manager `CreateSecret` with the name from `afs.secret_path` and tags `afs-team-id` (and `afs-agent-id` for agent scope), then stores only the ARN [adapter with fake AWS]. |
| RA-US-5.2 | If Secrets Manager fails, no row is written and the error is `503 Unavailable` without the value [adapter]. |
| RA-US-5.3 | A fuzz test sends values through every secret route and asserts the value never appears in any response body, header, log line, trace attribute, audit record or event [adapter]. |
| RA-US-5.4 | The `afs-api` binary's AWS client is built with Secrets Manager only; a test fails if any other AWS service client is constructed in `cmd/afs-api` [unit]. |

**RA-US-6 · Optimistic concurrency, sessions and versions** *(US-13, US-26, D47)*

| AC | Criterion |
|---|---|
| RA-US-6.1 | Update without `If-Match` → `428`; with a stale version → `412`; with the current version → 200 and `ETag` advanced by one [adapter]. |
| RA-US-6.2 | A transaction opened without the session settings sees no team rows [adapter]. |
| RA-US-6.3 | Every response carries `X-Request-Id`; a client-sent id of at most 64 characters is echoed and appears in the error body and the audit record [adapter]. |
| RA-US-6.4 | Changing a team's `kind` → `400 ValidationError` [adapter]. |
| RA-US-6.5 | Every response, including `401`, `403`, `404` and `429`, carries `X-Afs-Contract-Version` equal to the contract's version [contract test]. |

**RA-US-7 · Reports** *(US-11, US-12)*

| AC | Criterion |
|---|---|
| RA-US-7.1 | Each report kind at each level returns the same totals as summing the underlying table for the range [adapter]. |
| RA-US-7.2 | Ranges over two days read daily views; shorter ranges read 5-minute tables; p99 < 1 s for 90 days at the initial scale [load]. |
| RA-US-7.3 | `Accept: text/csv` returns the same rows as JSON [adapter]. |
| RA-US-7.4 | `groupBy=tag:business-unit` returns one row per value plus an untagged row [adapter]. |
| RA-US-7.5 | The budget position returns `forecastUsd` (linear on the period's spend so far) and `projectedCapAt` (when the forecast reaches the budget, absent when it does not within the period) [unit]. |
| RA-US-7.6 | A viewer of team A gets `404` for team B's reports and `403` for service reports [adapter]. |

**RA-US-7a · Report schedules** *(US-11, RPT-8)*

| AC | Criterion |
|---|---|
| RA-US-7a.1 | A team-admin creates a weekly schedule on the team's own Slack channel; a service-level channel or another team's channel → `ValidationError` [adapter]. |
| RA-US-7a.2 | A viewer creating or changing a schedule → `403` (D23) [adapter]. |

**RA-US-8 · Approve prices** *(US-10)*

| AC | Criterion |
|---|---|
| RA-US-8.1 | Only service admins can approve or reject; approval publishes `pricing.proposal_decided` and the gateway uses the new price for calls after its effective time [adapter, e2e]. |
| RA-US-8.2 | The effective time is `max(effectiveFrom, approvedAt)`: approving a proposal whose `effectiveFrom` is in the past applies it from the approval, and no usage row before it is repriced [adapter]. |

**RA-US-9 · Retries are safe** *(IF-1)*

| AC | Criterion |
|---|---|
| RA-US-9.1 | A create repeated with the same key, operation and body within the TTL returns the stored response with `Idempotent-Replayed: true` and writes nothing new; a different body → `409` with `details.reason = idempotency_mismatch`; a concurrent duplicate waits or gets `409 idempotency_in_progress`; a 5xx is not stored [adapter]. |
| RA-US-9.2 | Keys are per principal: the same key from another caller is a new request [adapter]. |
| RA-US-9.3 | A replayed `CreateApiKey` or `CreateServiceApiKey` returns the key's metadata without `secretKey` and with `Idempotent-Replayed: true`, and no stored response contains `secretKey` (D52) [adapter]. |

**RA-US-10 · Ephemeral teams and pools** *(US-15, US-17)*

| AC | Criterion |
|---|---|
| RA-US-10.1 | `CreateTeam` with `kind: ephemeral` and no expiry, rule or lifetime budget → `400`; an expiry beyond `teams.ephemeral_max_days` → `400`; with them, one transaction writes team and budget [adapter]. |
| RA-US-10.2 | Making an ephemeral team persistent or removing its lifetime budget → `400 ValidationError` [adapter]. |
| RA-US-10.3 | Pool operations on a persistent team → `409`: `afs-api` checks the team's `kind` before the write, so the database's `23514` is only a backstop; a pool named like an existing agent → `409`; `ScalePool` outside 0–200 → `400` [adapter]. |
| RA-US-10.4 | A pool role link in an account outside the team's allowed accounts, or with a role name other than the pool's, → `400 ValidationError` [adapter]. |
| RA-US-10.5 | Viewers can read pools and get `403` on every pool mutation [adapter]. |

**RA-US-11 · Work orders for security** *(US-16, US-22, D38)*

| AC | Criterion |
|---|---|
| RA-US-11.1 | A security operator (person or service-level key) can create, update, cancel and list all work orders, filter them by `teamId`, list and describe swarm templates and use `ListTeamDirectory`; it gets `403` on creating or changing a template and on any team route [adapter]. |
| RA-US-11.2 | Owner resolution (D38): given team A listing `github.com/acme/*` and team B listing `github.com/acme/payments-api` in `AFS_OWNED_REPOS`, when `CreateWorkOrder` names the repository `acme/payments-api` with no owner, then its owner is team B (an exact entry beats a wildcard) and the response shows `ownerSource: team_config` [adapter]. |
| RA-US-11.3 | Given two teams that both list `github.com/acme/*`, when a repository `acme/other` is given with no owner, then `400 ValidationError` with `details.ambiguousRepos` naming both team ids, and no work order exists [adapter]. |
| RA-US-11.4 | Given no team lists a repository and none is given, then `400 ValidationError` with `details.unownedRepos`, and no work order exists [adapter]. |
| RA-US-11.5 | Given a repository that team B lists, when the request names team A (or an email) explicitly, then the explicit owner wins (`ownerSource: explicit`); a deleted team's entry is skipped [adapter]. |
| RA-US-11.6 | A second open work order for the same CVE → `409`; a repository with neither an explicit owner nor a resolved one → `400` (see above) [adapter]. |
| RA-US-11.7 | Given a swarm template that a service admin has disabled, when a security operator calls `CreateWorkOrder` naming it, then `400 ValidationError`; the template's revisions stay readable and existing remediation teams are unchanged [adapter]. |
| RA-US-11.8 | A team-admin sees work orders assigned to the team or touching its repositories, read-only, through `ListWorkOrders` and `ListTeamWorkOrders`; an unrelated team gets `404` [adapter]. |
| RA-US-11.9 | The assigned team's agents and pools can update repository status with `work-orders:report`; setting `pr_opened` without a PR link → `400` [adapter]. |
| RA-US-11.10 | A team-admin adding a security operator binding → `403`; only service admins can [adapter]. |

**RA-US-12 · Guardrails stay with service admins** *(US-18, ACC-5, TEAM-6)*

| AC | Criterion |
|---|---|
| RA-US-12.1 | A team-admin changing `isolationTier` or `viewerKubectl` → `403`; `GetTeamKubeconfig` with it off → `403`, with it on returns a kubeconfig that uses an exec credential plugin and contains no token [adapter]. |

**RA-US-13 · Budgets within the ceiling** *(US-9, US-24, BUD-1, BUD-10, D33, D34)*

| AC | Criterion |
|---|---|
| RA-US-13.1 | A team-admin setting a budget above the ceiling → `400 ValidationError` [adapter]. |
| RA-US-13.2 | A service admin sets `ceilingOverrideUsd` with an expiry; while it is unexpired a team-admin can set a budget up to the override; a team-admin setting the override → `403` [adapter]. |
| RA-US-13.3 | `onExhaust: notify` on an ephemeral team → `400`; on a persistent team by a team-admin → `403`; by a service admin → 200 [adapter]. |

**RA-US-14 · Export, import and restore** *(US-13, US-20, CFG-5, CFG-6, D49)*

| AC | Criterion |
|---|---|
| RA-US-14.1 | An export bundle contains no secret value, API key hash or client secret, and its SHA-256 matches its content [adapter]. |
| RA-US-14.2 | `CreateImport` with `?dryRun=true` (the default) returns `{dryRun: true, teams[…]}` with a create, update or skip result per team, lists secrets whose values must be put again, and writes no desired-state row [adapter]. |
| RA-US-14.3 | With `?dryRun=false` the request-body bundle is applied synchronously, one transaction per team: account ids are mapped by the account map, one audit record is written per resource, every imported role link is `pending_trust`, and a team that fails is reported `failed` with nothing written for it [adapter]. |
| RA-US-14.4 | A bundle whose checksum does not match → `400 ValidationError` and nothing is written; a bundle over 100 MB → `400 ValidationError` [adapter]. |
| RA-US-14.5 | `afs-api` makes no S3 call for an import or an export [unit]. |
| RA-US-14.6 | (US-20) `RestoreRevision` with `dryRun` returns the diff and writes nothing; without it, the team's resources match the revision, `generation` is the previous value + 1, and one audit record names the source revision [adapter]. |
| RA-US-14.7 | A viewer calling `RestoreRevision` → `403`; a non-service-admin calling `CreateExport`, `GetExport` or `CreateImport` → `403` [adapter]. |

**RA-US-15 · Rate limits, settings and service-level access** *(US-21, US-26, IF-1)*

| AC | Criterion |
|---|---|
| RA-US-15.1 | One principal sending 60 requests within one second gets at least 40 successes and `429 Throttled` with `Retry-After` for the rest; another principal in the same second is not throttled [adapter]. |
| RA-US-15.2 | A browser request from an origin not in `api.cors_origins` gets no CORS allow headers; one from a listed origin does [adapter]. |
| RA-US-15.3 | `PutSetting` with an unknown key or a value of the wrong type or range → `400 ValidationError`; a non-service-admin → `403` [adapter]. |
| RA-US-15.4 | Disabling an identity provider that a team uses → `409 Conflict`; adding a provider and marking it the default is audited, and `ListSignInProviders` then lists both with the default flagged [adapter]. |
| RA-US-15.5 | A service-level API key is created only by a service admin, holds only the `security_operator` role and shows its secret once; it can call the work order operations, `ListSwarmTemplates`, `GetSwarmTemplate`, `ListTeamDirectory` and `GetMe`, and gets `403` on everything else, every team-scoped route included [adapter]. |
| RA-US-15.6 | Deleting a service-level notification channel named in `work_orders.notification_channel_id` → `409`; a team-admin choosing a service-level channel for a report schedule → `400 ValidationError` [adapter]. |

**RA-US-16 · Suspend and delete a team** *(US-23, TEAM-1, TEAM-9)*

| AC | Criterion |
|---|---|
| RA-US-16.1 | Given a suspended team, when a person or API key calls any mutation except `ResumeTeam` and `DeleteTeam`, then `409 Conflict` with `details.reason = team_suspended`; reads still succeed [adapter]. |
| RA-US-16.2 | A team-admin calling `SuspendTeam` succeeds; a team-admin calling `DeleteTeam` → `403` and one denied audit record; a service admin's `DeleteTeam` returns the team with `state = deleting` and `status.phase = Deleting` [adapter]. |
| RA-US-16.3 | After deletion `GET` of the team, its usage, costs, revisions and audit still return for service admins, with `state = deleted` [adapter]. |
| RA-US-16.4 | The secrets of a deleted team are scheduled for deletion with a 30-day recovery window (RC-17, RC-US-15) [adapter with fake AWS]. |

**RA-US-17 · Log destinations and notification channels** *(US-19, OPS-1, OPS-3)*

| AC | Criterion |
|---|---|
| RA-US-17.1 | A team-admin creates a CloudWatch, S3, OTLP, Splunk HEC or Datadog destination whose credentials are a secret reference; a request carrying a credential value inline → `400 ValidationError` [adapter]. |
| RA-US-17.2 | A viewer calling `CreateLogDestination` or `CreateNotificationChannel` → `403` and one denied audit record; the same viewer can list and describe both [adapter]. |
| RA-US-17.3 | `TestNotificationChannel` on a channel whose delivery fails returns the failure and records it on the channel [adapter with fake Slack]. |

**RA-US-18 · Cost allocation tag status** *(TAG-7, US-21)*

| AC | Criterion |
|---|---|
| RA-US-18.1 | `ListCostTagStatus` returns each AFS tag key with `seenInCur` and `awsStatus` to a service admin; a team-admin → `403` [adapter]. |
| RA-US-18.2 | `PutSetting` of `billing.tag_activation_acknowledged = true` by a service admin acknowledges the banner and is audited; a non-boolean value → `400 ValidationError` [adapter]. |


#### afs-reconciler

**RC-US-1 · Converge a team** *(US-1, US-3)*

| AC | Criterion |
|---|---|
| RC-US-1.1 | Given a new team row, the namespace, quotas, Pod Security labels, default-deny policies, team IAM role with boundary, the team's Bedrock role trusting only the gateway, SecretStore and inference profiles exist within 60 s (p95) and `resource_status` is `Ready` with `observedGeneration = generation` [envtest, e2e]. |
| RC-US-1.2 | A desired-state change is picked up from `afs_desired_state` within 5 s; with notifications blocked it is picked up by the 30 s poll [adapter]. |
| RC-US-1.3 | An agent whose template is disabled, or whose image digest is removed from the template's allowed list, keeps running and gets condition `TemplateRetired=True` within 60 s; re-enabling clears it (D54) [envtest]. |
| RC-US-1.4 | Two teams can each have an agent named `reviewer`; their IAM roles are named by id and carry the human names as tags [adapter with fake AWS]. |

**RC-US-1a · Admission** *(US-3, US-18, TEAM-4)*

| AC | Criterion |
|---|---|
| RC-US-1a.1 | Admission refuses NetworkPolicy, CiliumNetworkPolicy, LoadBalancer or NodePort Services, hostNetwork and privileged pods in team namespaces from any subject except the reconciler [e2e]. |
| RC-US-1a.2 | A pod in team B tolerating team A's dedicated taint or selecting its nodes is refused [envtest]. |
| RC-US-1a.3 | Deleting an admission policy or binding restores it within 60 s with a `drift.corrected` record [envtest]. |
| RC-US-1a.4 | Given a pod in another team's namespace, or any pod outside team namespaces, when it connects to `afs-gateway`, then Cilium denies the connection; a team pod reaches it (US-25, D57) [e2e]. |

**RC-US-2 · Zero-impact agent pods** *(US-4, US-5)*

| AC | Criterion |
|---|---|
| RC-US-2.1 | The rendered pod has: env vars from the ConfigMap and from Secret refs for env bindings; files for file bindings at their exact targets; `/etc/afs/config/<NAME>`; `/etc/afs/aws/config` when links exist; `/etc/afs/tags.json` and `AFS_TAGS`; `AFS_GATEWAY_URL`, `AWS_ENDPOINT_URL_BEDROCK_RUNTIME`, `ANTHROPIC_BASE_URL` and `ANTHROPIC_API_KEY=afs-gateway`; the gateway CA bundle at `/var/run/secrets/afs/gateway/ca.crt` named in `AWS_CA_BUNDLE`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and `NODE_EXTRA_CA_CERTS`; the optional gateway token file; no AWS keys (D57) [envtest]. |
| RC-US-2.2 | Rotating a secret or changing a config value changes the pod-template hash and rolls only the agents that reference it [envtest]. |
| RC-US-2.3 | A secret naming an external system that is off for an agent is not delivered to that agent, and is delivered again within 60 s of the system being switched on [envtest]. |
| RC-US-2.4 | The `afs-api` token is projected only in teams with an assigned work order [envtest]. |
| RC-US-2.5 | A container image with no AFS-specific code receives all of the above (verified with `busybox env` and `cat` in e2e). |
| RC-US-2.6 | With isolation tier `dedicated`, the team's pods carry the toleration and node selector of its own NodePool, land only on its nodes, and no other team's pod lands there [e2e]. |

**RC-US-3 · Revert drift and recover** *(US-13)*

| AC | Criterion |
|---|---|
| RC-US-3.1 | Editing or deleting an owned object restores it within 60 s and writes one `drift.corrected` audit record [envtest]. |
| RC-US-3.2 | Starting against a fresh cluster and a database restored to a point in time converges everything, agent volumes re-created from their latest snapshots, with no manual step within 1 hour at the initial scale [e2e restore drill]. |

**RC-US-4 · Cross-account links** *(US-7)*

| AC | Criterion |
|---|---|
| RC-US-4.1 | A new link gets an `sts:AssumeRole` statement on its exact role ARN, a profile in the pod's AWS config, status *Pending trust*, then *Verified* after a successful assume with the external ID and session tags, or *Failed* with the AWS error code [adapter with fake STS]. |
| RC-US-4.2 | Narrowing the allowed-accounts filter disables links outside it on the next pass [adapter]. |
| RC-US-4.3 | Every profile in the rendered `/etc/afs/aws/config` sets `role_session_name` to the agent's id (the pool's id for a pool), so a CloudTrail event in the linked account names the agent (D51) [adapter with fake STS]. |

**RC-US-5 · Egress follows external systems** *(US-8)*

| AC | Criterion |
|---|---|
| RC-US-5.1 | Switching a system off at service, team or agent level removes its FQDNs from the affected agents' Cilium policies within 60 s, without restarting pods [envtest]. |
| RC-US-5.2 | Denied flows are written with the right `denied_reason` [adapter with recorded Hubble flows]. |
| RC-US-5.3 | An agent whose denied flows exceed `egress.denied_spike_threshold` in 5 minutes produces one `egress.denied_spike` event for that window [unit with fakes]. |

**RC-US-6 · Activity** *(US-11)*

| AC | Criterion |
|---|---|
| RC-US-6.1 | Each running agent gets one sample per 5-minute window; working when it made a model call, reported busy, or used CPU above `reports.idle_cpu_millicores` [unit with fakes]. |

**RC-US-7 · Tags everywhere** *(US-12)*

| AC | Criterion |
|---|---|
| RC-US-7.1 | Every AWS resource the reconciler creates carries the agent's or team's effective tags; a change is applied within 10 minutes; `afs-team-id` and `afs-agent-id` are always present and never removed [adapter with fake AWS]. |
| RC-US-7.2 | An effective tag set above 40 keys is refused before any AWS call and the resource is `Degraded` with a condition naming the limit [unit]. |

**RC-US-8 · Ephemeral teardown** *(US-15)*

| AC | Criterion |
|---|---|
| RC-US-8.1 | When a team's rule fires, within 5 minutes its agents and pools stop, IAM roles and queues are deleted, the namespace is removed, `torn_down_at` and the reason are recorded and `team.torn_down` is published; config revisions, usage and audit remain [envtest, adapter]. |

**RC-US-9 · Swarm pools** *(US-17)*

| AC | Criterion |
|---|---|
| RC-US-9.1 | A pool renders one Deployment with `workers` replicas, its own ServiceAccount and role, scratch volume of the requested size and, when chosen, an EFS mount; scaling changes replicas only [envtest]. |
| RC-US-9.2 | Each receiving pool has one inbox queue; it gets `AFS_INBOX_QUEUE_URL`; a pool handing off to two pools gets one `AFS_HANDOFF_<TO_POOL>_QUEUE_URL` per receiver; a pool receiving from two pools has one inbox that both may send to; no other role can send, and only the receiver can receive [adapter with fake AWS]. |
| RC-US-9.3 | A shared named role is linked and verified in each listed account, like an agent role link [adapter]. |

**RC-US-10 · Work orders flow** *(US-16)*

| AC | Criterion |
|---|---|
| RC-US-10.1 | An EventBridge request is turned into one work order even when delivered twice [adapter]. |
| RC-US-10.2 | A work order gets a remediation team from its template within 2 minutes, with the cap as lifetime budget, the template TTL as expiry and rule `at_work_complete` [envtest]. |
| RC-US-10.3 | PR state read from GitHub moves repositories to `pr_opened` and `merged`; when all are merged, not affected or skipped, the work order is done and the team is torn down [adapter with recorded GitHub]. |
| RC-US-10.4 | Polling 500 repositories every 10 minutes stays under 5,000 requests per hour per installation, using conditional requests [adapter with recorded GitHub]. |
| RC-US-10.5 | Each owner gets one notification per event (assigned, PR opened, merged, SLA warning, SLA breached, completed); outside contacts are emailed through the configured service channel, and nothing is sent to them when it is unset [adapter]. |

**RC-US-11 · Read-only kubectl** *(US-18, ACC-5)*

| AC | Criterion |
|---|---|
| RC-US-11.1 | With viewer kubectl on, a viewer can `get`, `list` and `watch` workloads, events and logs in the namespace and is refused secrets, `exec`, `attach`, `port-forward` and any change; off removes the access entry within 60 s [e2e]. |

**RC-US-12 · Budget periods, backstop and stuck reservations** *(US-9, BUD-3)*

| AC | Criterion |
|---|---|
| RC-US-12.1 | Each budget's counter for a period exists by the first second of the period, created by the reconciler or by the gateway's first call, and `budget.reset` is published once [adapter]. |
| RC-US-12.2 | Raising a budget carries into `limit_usd` and lifts the IAM deny and the `BudgetExhausted` condition within 60 s [adapter with fake IAM]. |
| RC-US-12.3 | At exhaustion the team's Bedrock role gets the explicit deny and the team the `BudgetExhausted=True` condition within 60 s [adapter with fake IAM]. |
| RC-US-12.4 | A gateway killed mid-call leaves a reservation that is released within `gateway.reservation_ttl_seconds` plus 2 minutes; reserved spend returns to zero and one audit record notes the release [adapter]. |
| RC-US-12.5 | When a ceiling override expires with the budget above the base ceiling, the budget is lowered to the ceiling and `budget.ceiling_override_expired` is published once [adapter]. |

**RC-US-13 · Notifications** *(US-9, US-16, US-19, OPS-3)*

| AC | Criterion |
|---|---|
| RC-US-13.1 | A Slack channel row becomes one EventBridge rule filtered on the team id with an API destination; one `budget.threshold_crossed` produces exactly one Slack message [adapter with fake EventBridge and Slack]. |
| RC-US-13.2 | An email channel is delivered through SES with one message per event id, even when the event arrives twice [adapter with fake SES]. |
| RC-US-13.3 | A delivery failure (for example a Slack webhook returning an error, or a failed *Send test*) is retried and shown with its error on the channel's status [adapter with fake Slack]. |
| RC-US-13.4 | Deleting a channel removes its rule and target within 60 s; a team's rule never matches another team's events [adapter]. |

**RC-US-14 · Volumes and snapshots** *(US-4, AGT-9)*

| AC | Criterion |
|---|---|
| RC-US-14.1 | Pausing an agent sets replicas to 0 and keeps the PVC; resuming brings back the same volume with its files [envtest, e2e]. |
| RC-US-14.2 | Deleting an agent with the default rule takes a final snapshot, waits for it to be ready, then deletes the volume; `retain` keeps the volume and records it; `delete` deletes it with no snapshot [envtest]. |
| RC-US-14.3 | A daily schedule keeps exactly `snapshotRetain` ready snapshots [envtest with a fake clock]. |
| RC-US-14.4 | `CreateAgentSnapshot` produces a ready snapshot recorded in `agent_volume_snapshots` [envtest]. |
| RC-US-14.5 | Growing `sizeGiB` expands the volume without recreating the pod's data [e2e]. |

**RC-US-15 · Suspend, resume and delete** *(US-23, TEAM-1, TEAM-9)*

| AC | Criterion |
|---|---|
| RC-US-15.1 | Suspending a team brings every agent and pool to 0 replicas within 60 s, keeps volumes, IAM and secrets, and the gateway refuses the team's calls; resuming restores each agent and pool to its prior state, and an agent paused before the suspend stays paused [envtest]. |
| RC-US-15.2 | Deleting a team runs the finalizer steps in order (recorded by fakes), shows `Deleting` with the current step as a condition, resumes after a failed step on the next pass, and ends with the row `deleted` while its audit and usage stay readable [envtest with fake AWS]. |
| RC-US-15.3 | Deleting a persistent team schedules every one of its secrets for deletion in Secrets Manager with a 30-day recovery window (not an immediate delete) before the namespace goes [adapter with fake AWS]. |

**RC-US-16 · Log delivery and redaction** *(US-14, OPS-1, OPS-4)*

| AC | Criterion |
|---|---|
| RC-US-16.1 | A log line containing the value of one of the agent's bound secrets is delivered as `[redacted:<hash prefix>]`, and the same value gives the same token each time [e2e]. |
| RC-US-16.2 | Strings that look like passwords, bearer tokens, AWS keys, private keys or JWTs are redacted [unit]. |
| RC-US-16.3 | A team's logs never reach another team's destination, and its aggregator holds no other team's credentials [e2e]. |
| RC-US-16.4 | Adding or deleting a destination changes the outputs within 60 s [envtest]. |
| RC-US-16.5 | An agent's connection to a system's sign-in endpoint appears as a sign-in event (agent or pool worker, system, time) in the team's log delivery and in no database table or event [adapter]. |

**RC-US-17 · Export bundles** *(US-13, CFG-5, D49)*

| AC | Criterion |
|---|---|
| RC-US-17.1 | An export request produces one KMS-encrypted bundle in S3 whose checksum and size are recorded, with the request `succeeded` [adapter with fake S3]. |
| RC-US-17.2 | `GetExport` on a succeeded export shows `downloadUrl`, valid 7 days, and `download_expires_at`; after expiry a new export is needed [adapter with fake S3]. |
| RC-US-17.3 | A failed build records `failed` with a summary and no checksum [adapter]. |

**RC-US-18 · Housekeeping jobs** *(US-13, OPS-2, IF-1, D36)* (RC-8)

| AC | Criterion |
|---|---|
| RC-US-18.1 | The outbox publisher delivers every event to EventBridge at least once and in order per aggregate; a failed publish is retried with `attempts` and backoff and a duplicate is harmless to consumers [adapter with fake EventBridge]. |
| RC-US-18.2 | Partitions of `usage_events`, `agent_activity` and `external_traffic` exist at least one month ahead; a run on the last day of a month finds next month's partitions present [adapter]. |
| RC-US-18.3 | The organization account mirror runs hourly; an account removed from the organization shows in the next run [adapter with fake Organizations]. |
| RC-US-18.4 | Idempotency keys past their TTL are purged every 5 minutes [adapter]. |


#### afs-gateway

**GW-US-1 · Hard cap under concurrency** *(US-9)*

| AC | Criterion |
|---|---|
| GW-US-1.1 | Load test: 200 agents at 500 req/s against a nearly exhausted budget; total spend exceeds the budget by at most one in-flight request per agent, and every rejected call gets `402 BudgetExceeded` [load]. |
| GW-US-1.2 | With the database unreachable, every call is rejected with `503 Unavailable` within 2 s and none reaches Bedrock [adapter]. |
| GW-US-1.3 | Added latency p99 < 25 ms excluding Bedrock time [load]. |
| GW-US-1.4 | A request without `maxTokens` reserves the model's `max_output_tokens` times the output price plus the input estimate [unit]. |

**GW-US-2 · Attributed calls** *(US-12, US-14)*

| AC | Criterion |
|---|---|
| GW-US-2.1 | Every Bedrock call uses the team's inference profile and carries `requestMetadata` with the agent's effective tags including `afs-team-id` and `afs-agent-id`, at most 16 pairs in the D27 order, with the dropped keys recorded [adapter with fake Bedrock]. |
| GW-US-2.2 | A token from a pod of team A can never invoke through team B's profile [unit]. |
| GW-US-2.3 | A model the team has not enabled → `403 AccessDenied`, recorded as a rejected call with `outcome` `rejected_model` [adapter]. |
| GW-US-2.4 | A pool worker's call records the pool and the worker pod name [adapter]. |
| GW-US-2.5 | Every call, accepted or rejected, records its latency, and rejected calls record the outcome (`rejected_budget`, `rejected_model`) [adapter]. |

**GW-US-3 · Spend under the tags of its time** *(US-12)*

| AC | Criterion |
|---|---|
| GW-US-3.1 | Every `usage_events` row carries the team's tag set current at the call; after a tag change, new rows carry the new set within 60 s, and old rows keep theirs [adapter]. |

**GW-US-4 · Thresholds once** *(US-9, BUD-4)*

| AC | Criterion |
|---|---|
| GW-US-4.1 | With 200 concurrent callers crossing 50%, 80% and 100% in the same second, exactly one `budget.threshold_crossed` per threshold and one `budget.exhausted` are published for the period [adapter]. |

**GW-US-5 · Streaming** *(US-9, BUD-2)*

| AC | Criterion |
|---|---|
| GW-US-5.1 | A streamed response settles from its final usage block [adapter with fake Bedrock]. |
| GW-US-5.2 | A client that aborts mid-stream, or an upstream error mid-stream, settles on the tokens seen so far and releases the rest of the reservation [adapter with fake Bedrock]. |
| GW-US-5.3 | A stream aborted before any usage is reported settles to the reservation (the worst case stays charged) [adapter with fake Bedrock]. |

**GW-US-6 · Caller identity and suspended or deleted teams** *(US-23, US-25, D57)*

| AC | Criterion |
|---|---|
| GW-US-6.1 | A call from an agent using the AWS SDK's Bedrock Runtime client or the Anthropic SDK with only the environment of the gateway contract §3 succeeds, is attributed to the agent, and its SigV4 signature or API key is ignored [conformance suite]. |
| GW-US-6.2 | A call whose source IP is not a current team pod → `401 Unauthenticated`; a call that sends the optional token of a different pod → `401`; a call with a valid token of the source pod succeeds [adapter]. |
| GW-US-6.3 | A suspended team's call → `403 AccessDenied` with `details.reason = team_suspended`; after the team is deleted its pods no longer exist and a call from its former IP → `401` [adapter]. |
| GW-US-6.4 | Settlement always passes the budget and period it reserved against (`afs.settle_reservation(id, budget_id, period_start, actual)`) [adapter]. |
| GW-US-6.5 | A cached TokenReview result is not reused after 5 minutes [unit with fake clock]. |


#### afs-billing

**BI-US-1 · Reconciliation and variance** *(US-10)*

| AC | Criterion |
|---|---|
| BI-US-1.1 | Given CUR rows for a day, `cost_daily` holds Bedrock, EKS and storage (volumes and snapshots) costs per team, and `cost_reconciliations` is `over_threshold` when the difference exceeds the setting (default 5%), with one `cost.variance_detected` event [adapter with fixture CUR]. |

**BI-US-2 · Price sync** *(US-10)*

| AC | Criterion |
|---|---|
| BI-US-2.1 | With sync on and a changed price in the Price List fixture, exactly one pending proposal exists per model after the run; an older pending proposal is superseded; with sync off, nothing is proposed [adapter]. |

**BI-US-3 · Scheduled reports** *(RPT-8)*

| AC | Criterion |
|---|---|
| BI-US-3.1 | A due schedule produces one CSV equal to the on-demand report for the same query, sent to the schedule's channel; `next_run_at` advances; a delivery failure records `failed` with the error and retries next cycle [adapter]. |

**BI-US-4 · Cost allocation tag status** *(TAG-7)*

| AC | Criterion |
|---|---|
| BI-US-4.1 | Given CUR fixtures and a fake Billing API, each AFS tag key has `seen_in_cur` and `aws_status`; a key active for 3 days but not seen raises one `cost_tags.not_seen` event [adapter]. |

**BI-US-5 · Showback** *(US-12, BUD-7)*

| AC | Criterion |
|---|---|
| BI-US-5.1 | The showback CSV for a month has one row per cost center (and per value of a chosen tag key) whose totals equal `cost_daily` for the month, with an `(untagged)` row for records missing the key [adapter]. |

**BI-US-6 · Costs under the tags they were billed with** *(US-12)*

| AC | Criterion |
|---|---|
| BI-US-6.1 | Given a team tagged `business-unit = payments` until Tuesday 12:00 UTC and `platform` after, with CUR fixtures carrying those tags per line item, Monday's cost is under *payments*, Wednesday's under *platform*, and Tuesday's split by the line items' tags; pool worker lines carry the pool [adapter with fixture CUR]. |

Total: 52 stories, 180 criteria.

### 9.2 Contract-derived acceptance criteria

Criteria the contracts impose that no PRD story states. Same test levels as §9.1.

| AC | Source | Criterion |
|---|---|---|
| CT-API-1 | API §1 Pagination | `maxResults` 1–100 (default 50); `nextToken` absent on the last page; an expired (24 h) or tampered token → `400 ValidationError` [unit, adapter] |
| CT-API-2 | API §1 Filtering | An unknown query parameter → `400 ValidationError` [unit] |
| CT-API-3 | API §1 CSV | Operations marked *CSV* answer `Accept: text/csv` with the same rows and the next token in `X-Afs-Next-Token` [adapter] |
| CT-API-4 | API §1 CORS | Preflight and responses carry exactly the allowed and exposed headers of §1, only for listed origins, never credentials mode [adapter] |
| CT-API-5 | API §6 | Every error body is `{code, message, requestId, details}` with a §6 code; `afs-api` never returns `402` or `502`; no error contains a secret, token or key [unit, contract test] |
| CT-API-6 | API §1 Concurrency | `PATCH`, `PUT` and `DELETE` of an existing resource without `If-Match` → `428`; a `PUT` that creates needs none [adapter] |
| CT-API-7 | API §1 Async | Mutations return the stored desired state with `status.phase = Pending`; `observedGeneration` equals `generation` once applied [adapter, envtest] |
| CT-API-8 | API §4, database §5 | Every mutation writes the row, one audit record and one outbox event in one transaction; a failure writes none [adapter] |
| CT-API-9 | API §7 | Every event type of §7 is emitted by its stated emitter with envelope `{id, occurredAt, teamId, agentId?, poolId?, actor, data}` and no secret value [adapter] |
| CT-API-10 | API §1 Ids, Names | Ids are prefixed ULIDs with the §1 prefixes; names match the §1 pattern and are immutable [unit] |
| CT-API-11 | API §1 Money | Money is a decimal string with at most 6 fraction digits, never a JSON number [unit, contract test] |
| CT-API-12 | API §1 Idempotency | `Idempotency-Key` of 1–255 characters on creates and `:verb` actions; `idempotency_in_progress` carries `Retry-After: 1` [adapter] |
| CT-API-13 | API §1 Concurrency | Every GET of a mutable resource returns `ETag: "<version>"` [adapter] |
| CT-API-14 | API §4.6 | `DeleteSecret` on a bound secret → `409 Conflict` listing the agents [adapter] |
| CT-API-15 | API §4.11 | `GetSettings` returns every root PRD §6.13 key with value, default and range [adapter] |
| CT-DB-1 | Database §1 | `afs-api` runs `migrate.Up` under the advisory lock (concurrent starts safe); the other binaries refuse to start when the schema is older than the version they embed [adapter] |
| CT-DB-2 | Database §5 | `40001` and `40P01` retried up to 3 times with jitter, then `500 InternalError` [adapter] |
| CT-DB-3 | Database §6 | SQLSTATEs map to the §6 API codes, `23503` on write → `ValidationError` and on delete → `Conflict` [unit, adapter] |
| CT-DB-4 | Database §3 | Service roles set `afs.actor = 'system:<service>'` [adapter] |
| CT-GW-1 | Gateway §9 | Every gateway response carries `X-Afs-Gateway-Contract-Version: 1.0.0` [unit] |
| CT-GW-2 | Gateway §2 | Unsupported paths and operations (for example `ApplyGuardrail`) → `404 NotFound` [unit, conformance suite] |
| CT-GW-3 | Gateway §7 | A malformed request → `400`; a body over 20 MB → `413 ValidationError` [unit] |
| CT-GW-4 | Gateway §4 | A `maxTokens` above `models.max_output_tokens` is capped to it [unit] |
| CT-GW-5 | Gateway §7 | Bedrock throttling → `429 Throttled` with `Retry-After`; a Bedrock server error → `502 UpstreamError` with the reservation released [adapter with fake Bedrock] |
| CT-GW-6 | Gateway §7 | Errors before the first stream event are JSON; after it the stream ends with an `exception` (Bedrock) or `error` (Anthropic) event carrying the same code [adapter with fake Bedrock] |
| CT-GW-7 | Gateway §6 | `requestMetadata` sent by the agent is replaced [unit] |
| CT-GW-8 | Gateway §8 | Usage rows are written in batches within 1 s; no prompt or completion content is stored or logged [adapter] |
| CT-GW-9 | Gateway §7 | `402 BudgetExceeded` carries `details.budgetId`, `period`, `remainingUsd` [unit] |
| CT-GW-10 | Gateway §2 | `GET /healthz` answers without authentication [unit] |

## 10. Risks and mitigation

| Risk | Impact | Mitigation |
|---|---|---|
| `-db` has no tag; a pseudo-version of a merge commit | Reproducibility, consumers confused by `v0.13.0` in the PRD | Pin `v0.0.0-20261121202720-b0d8754e4659`; switch to `v0.13.0` when tagged (plan.md §6) |
| `-db` 000015 lands mid-build | Schema drift | Adopt as a separate task in the milestone that needs it; repository tests run against the pinned version |
| Third-party CRDs (Agentic Teammates OQ-7, Cilium, Karpenter, ESO, VolumeSnapshot) | Version churn | Unstructured objects behind typed render functions; CRD YAMLs pinned under `internal/adapter/k8s/testdata/crds` for envtest |
| e2e, load and restore drill need a real cluster | Not runnable in CI | Build-tagged suites that compile in CI (`go vet -tags e2e,load,conformance ./test/...`), run by M6-F with cluster access |
| One OpenAPI file edited by many streams | Merge conflicts | Fragments per contract section under `api/src`, bundled by `tools/openapibundle` |
| Route table and wiring edited by many streams | Merge conflicts | Generated route table; per-area handler files and wiring files (architecture.md §3) |
| Secrets in EventBridge connections and GitHub keys | Secret handling outside `afs-api` | research.md R-6, R-9; open questions OQ-S3, OQ-S4 |
| Gateway on the critical path | Outage blocks all agents | Fail closed, 3 replicas, load test in M3 (root R2) |

## 11. Timeline and milestones

| Milestone | Content (tasks.md) | Exit |
|---|---|---|
| M0 Contract and foundation | Contract extraction, full OpenAPI, `authz`, HTTP skeleton with all routes (501), Postgres foundation, `-db` adoption, OQ-1/OQ-2 spikes | OpenAPI complete and published to consumers |
| M1 Teams and core | Auth, `GetMe`, teams, members, keys, settings, providers, service tags, audit, revisions; rate limit, CORS, idempotency; reconciler engine, team environment, admission, team lifecycle, housekeeping; observability; Helm and Terraform | A team converges on envtest; `make gate` |
| M2 Agents | Catalogs, agents, secrets, config, external systems, log destinations; agent workloads, volumes, agent lifecycle, egress and traffic, log delivery | An agent pod renders with zero AFS code |
| M3 Gateway and budgets | Gateway, budgets, notifications, reports, activity collector, budget reconciler | Hard cap holds in the load test |
| M4 Accounts, tags, billing | Role links and identities, org mirror, tags, CUR, pricing, showback, scheduled reports, export, import, restore | Billing reconciles fixture CUR |
| M5 Swarms | Ephemeral teams, pools, pool reconciler, teardown, viewer kubectl, cost tag status | A swarm runs and tears down on envtest |
| M6 CVE remediation and completion | Security operators, swarm templates, work orders, workload principal, work order reconciler; full e2e, load, restore drill, ASVS review | Feature complete |

## 12. Open questions

PRD OQ-1 to OQ-9 stand with their defaults (PRD §9); tasks build to the defaults and record the answer if
one arrives. Spec-level questions found in review (owner J unless stated), each with the default the tasks
build to: see research.md §5 (OQ-S1 to OQ-S6).

## 13. References

- PRD: `specs/261121-restapi-feature-complete/enterprise-agent-factory-service-restapi-PRD.md`
- Root PRD: `enterprise-agent-factory-service/enterprise-agent-factory-service-initial-PRD.md` (stories US-1..US-26, decisions D1–D57, settings §6.13)
- Contracts: `enterprise-agent-factory-service/docs/contracts/{api,database,gateway}-contract.md`
- Solution design: `enterprise-agent-factory-service/docs/solution-design.md`
- Schema: `enterprise-agent-factory-service-db/migrations/000001–000014`, `docs/data-model.md`
- This repository: `AGENTS.md`, `docs/architectural-decision-record.md`
