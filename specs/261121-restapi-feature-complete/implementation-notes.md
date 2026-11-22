# Implementation notes: `-restapi` feature complete

| | |
|---|---|
| **Feature** | restapi-feature-complete |
| **Date** | 2026-11-21 |

Purpose: the running record of decisions, edge cases and deviations made while building this spec.
Milestone agents append here after each merged stream (newest last, dated, with the task id). Keep design
rationale in architecture.md and research.md; this file records what actually happened.

## Spec review (step 2, 2026-11-21)

`review-spec` run by hand against spec.md, status.md, every phase file, the moved PRD, the three contracts
and the `-db` migrations. Traceability was checked by script: every PRD requirement (53), every story
criterion (180), every contract criterion (29) and every contract operation (182) maps to at least one
task (tasks.md appendices A and B).

**Verdict after fixes: Implementation-ready.** Before fixes: Needs revision (no Fail, four Warnings).

| Dimension | Before | After | Note |
|---|---|---|---|
| Acceptance criteria | ⚠️ Warn | ✅ Pass | All 180 PRD criteria and 29 contract criteria have tasks; RC-18 had no task citing it and the M6-F3 e2e run cited no criteria; criteria proven only in parts were checked to be complete as a union |
| Technical approach | ✅ Pass | ✅ Pass | OpenAPI production, layering, collision-free streams decided (research §3, architecture §3, §8) |
| Component coverage | ⚠️ Warn | ✅ Pass | Six file-ownership collisions between streams fixed; no operation → task index existed (added) |
| Edge case handling | ⚠️ Warn | ✅ Pass | External failure paths and concurrency were missing in 19 places (fixed below); six schema access gaps found |
| Open questions | ✅ Pass | ✅ Pass | PRD OQ-1..9 with defaults; OQ-S1..S8 added with defaults; schema gaps X-DB-1..6 with adoption tasks |
| Out-of-scope clarity | ✅ Pass | ✅ Pass | spec.md §3 non-goals |
| Status initialization | ✅ Pass | ✅ Pass | Phase 0 was In Progress, others Not Started; Phases 0–4 now Complete |

### Findings and fixes

| # | Finding | Fix |
|---|---|---|
| F-1 | **Schema access gaps.** At 000014, `afs_app` cannot look up an API key or a workload before team scope exists, security operators see no teams (empty `ListTeamDirectory`), denied-request audit on service routes and every security operator audit/outbox insert fail the `app_team_scope` WITH CHECK, `afs_reconciler` cannot create remediation teams (RC-12) or mark pools `deleted` | research.md §6 (X-DB-1..6), tasks.md *External dependencies*; adoption tasks M1-D7, M5-C6, M6-E6, M6-X0; read-only interim lookup M0-F8 (OQ-S7); requested from `-db` by M0-X1 |
| F-2 | RC-18 cited by no task | M2-I1..I3 cite it |
| F-3 | M6-F3 (e2e run) cited no criteria, so e2e halves of 11 criteria were untraced | AC column lists them |
| F-4 | Cross-stream criteria assigned to middleware tasks that have no handler to test with (RA-US-3.3, RA-US-6.1) | Moved to or shared with M1-B3/M1-B5, which depend on M1-D1/M1-D2 |
| F-5 | File ownership collisions: Secrets Manager, EKS, EventBridge and STS client constructors claimed by later streams; S3 package split across two M4 streams; the StatefulSet's PVC retention and replicas claimed by both M2-F and M2-G; gateway orchestration file unowned; M5/M6 streams editing files of earlier owners without listing them | Owners reassigned (M1-I, M1-J, M3-A, M3-B, M4-E4 new), M2-F1 takes replicas and restart inputs, Owns lines extended for M5-A, M5-B, M6-D |
| F-6 | Orphaned Secrets Manager secret when the database write fails after `CreateSecret`, and duplicate secrets on retry | M2-C2 (`ClientRequestToken`, compensation), M2-C5 (orphan sweep) |
| F-7 | Dependency failures unspecified: JWKS fetch, database down, TokenReview down, Kubernetes watch lost, STS assume failure after reserving, GitHub rate limits, Athena failures, Secrets Manager put/rotate failures, reconcile errors | M1-A1, M0-F4, M3-A3, M3-A2, M3-B6, M6-E3, M4-C2, M2-C3, M1-F4 |
| F-8 | Input edge cases: oversized or invalid `X-Request-Id`, secret value over 64 KiB, invalid report ranges, import account ids missing from the map, unauthenticated request floods | M0-D2, M2-C3, M3-H1, M4-F5, M1-D3 |
| F-9 | Concurrency: `RestoreRevision` against concurrent edits | M4-G2 (team row lock, optional `If-Match`) |
| F-10 | Service-level change notifications (empty team part, e.g. `agent_templates::tpl_…`) not fanned out | M1-F3 |
| F-11 | Matrix harness ownership: streams had no file for their seed data | Rule in tasks.md *How to use* (`fixtures_<area>_test.go`) |

### Positive observations

- The PRD's 401/404/403 rule, the contracts' error and event catalogs, and the settings table are concrete
  enough to generate code and tests from (contractgen).
- Every PRD story carries its test level, so each task names a level.
- The milestone order matches the data dependencies (agents before gateway, gateway before billing).

## Technical decisions

Decisions taken while creating the spec (step 1, 2026-11-21, under J's authorization of the run):

| # | Decision | Why |
|---|---|---|
| N-1 | Feature name `restapi-feature-complete` (given by the run, not derived from the PRD file name); spec directory `specs/261121-restapi-feature-complete/`; dashboard `restapi-feature-complete-status.md` | Run instruction |
| N-2 | The PRD was moved into the spec directory and a short stub with the same file name stays at the repo root, pointing to the moved file | The root `AGENTS.md`, this repo's `AGENTS.md` and other repositories link to the root path; a stub keeps those links working without a second copy that could drift. When step 7 archives the spec, the stub's link must be updated to `specs/archive/261121-restapi-feature-complete/…` |
| N-3 | `-db` adopted as pseudo-version `v0.0.0-20261121202720-b0d8754e4659` (commit `b0d8754`, migrations 000001–000014); switch to `v0.13.0` when tagged | `-db` has no tag yet; the PRD names `v0.13.0` |
| N-4 | The full OpenAPI document (182 operations) is produced in M0, not only `/v1/me`, teams and members as PRD §8 M0 says | `-cli` and `-website` depend on it; contract extract + fragments make it cheap (research §3.1) |
| N-5 | RC-8 (outbox, partitions, retention, purge) placed in M1 and its org mirror in M4 | PRD §8 places RC-8 in no milestone (OQ-S6) |
| N-6 | Every route exists from M0 with a placeholder handler; streams bind handlers in their own files | Collision-free parallel streams (architecture §3) |
| N-7 | `RotateSecret` takes `{value}` and writes a new Secrets Manager version (no rotation Lambda); `rotation{enabled, days}` is metadata | The contract has no rotation function; `afs-api` may only write secrets |
| N-8 | `/v1/openapi.yaml` is public and not a contract operation | PRD RA-1 requires it; the contract lists no operation for it |
| N-9 | e2e, load, conformance and restore suites are build-tagged, compiled in CI and run on the dev cluster by M6-F (and earlier by anyone with access) | No cluster or AWS account in CI |
| N-11 | Schema gaps X-DB-1..6 are requested from `-db` rather than worked around with elevated writes; only read-only lookups get an interim path (M0-F8) | Elevated writes would bypass the database guards that are part of the security design |
| N-10 | Viewer kubectl federated roles are configured per identity provider through a Helm-provided file, not the database | OQ-9 default: no access entry without a configured role; no schema column exists |

## M1 integration decisions (step 3, 2026-11-22)

| date | task | decision | reason |
|---|---|---|---|
| 2026-11-22 | M1-D1/B1 | One idempotency path: w2's `acting.Idem` claims and completes the key in the mutation's own transaction. The w1 middleware stage and `middleware/idempotency.go` are deleted. M2+ creates and `:verb` actions must use `acting.Idem`. | The middleware completed in a separate transaction, leaving a crash window between the mutation and the stored response. `secretKey` is stripped by the store and refused by the DB CHECK; `TestIdempotencyEndToEnd` proves replay, mismatch (409) and that it is never stored. |
| 2026-11-22 | M1-A/B | `postgres.SettingsReader` (w1) is the only settings reader; w2's copy and `m1w2Settings` are removed. | One cache and one set of defaults. |
| 2026-11-22 | M1-B/D | `cfg.Teams` and `cfg.TeamState` come only from `m1w2Teams` (teams use case); w1's `teamFlagLookup` is removed. | One source for team existence and suspended state, so the cross-team 404 and the suspended guard agree. |
| 2026-11-22 | M1-A/C | `domain.HashAPIKey` is the canonical hash; `auth.HashAPIKey` delegates to it (`TestAPIKeyHashIsTheIssuedHash`). | Issuing (C) and authenticating (A) must hash identically. |
| 2026-11-22 | M1-D6 | `Registrars` is the default plus every `httpapi.Areas` registrar, appended in `wire`. | An explicit list had replaced the default, leaving w2's handlers unrouted; the matrix now covers all M1 operations. |
| 2026-11-22 | M1-D3 | `DeleteApiKey` is exempt from the If-Match requirement (`ifMatchOptional`). | ApiKey has no version (website CQ-2). |
| 2026-11-22 | M1-C3 | Matrix `Case.Suspended`: team keys of a suspended team get 401 (database contract 4.2); users keep their role-based answer. | Matches `afs.authenticate_api_key`. |
| 2026-11-22 | M1-I1 | A failing delete step keeps phase Deleting (engine `writeFailure` takes the Outcome); the finalizer reports through `ConditionStore` over `resource_status` (`Replace: true`). | Backoff must not flip a deleting team to Failed. |
| 2026-11-22 | M1-I/J/L | Reconciler wiring: housekeeping jobs registered on the engine, `team.Hooks.Deleting = lifecycle.TeamDeleting(finalizer)`, `observability.Start` with the shared registry; new required `AFS_EVENT_BUS_NAME` and `AFS_CLUSTER_NAME`; `AFS_DB_IAM_AUTH` for both binaries. | Spec M1-F7/J/L. |
| 2026-11-22 | M1-X | afs-api links the SDK credential-provider services (sts, sso, ssooidc, signin) for RDS IAM auth only; archtest allows these, not `adapter/aws`. | `config.LoadDefaultConfig` pulls them in; afs-api never calls them. |
| 2026-11-22 | M1-X | Not wired yet: `PriorStateStore`, scale hooks and `Hooks.Suspended`; per-kind reconcile error and lag metrics. | No M2/M5 hooks exist to bind; recorded for M2+. |

## M2 integration decisions (step 3, 2026-11-22)

Merge order: w1 (A catalogs, D config, E external systems), w2 (C secrets, B agents API), w4 (I log destinations, H egress and traffic), w3 (F workloads, G volumes, snapshots, lifecycle). Each was merged with `--no-ff` onto the M1 exit; no rebase.

| date | task | decision | reason |
|---|---|---|---|
| 2026-11-22 | M2-X1 | One `mergeRoutes` (`cmd/afs-reconciler/wire_egress.go`, dedupes targets and fan-outs). `routes(extra...)` passes base, `agentRoutes()` and the log and egress routes through it once. w3's copy is removed. | The registry rejects a table routed twice (`agents`, `teams`, `team_models`, ...). The orphan secret sweep (w2) is a leader job, not a route, so it needs no merge. `TestRoutesMergeEveryRouteList` proves the combined registry builds. |
| 2026-11-22 | M2-X1 | One label constant: `render.LabelAgentID = traffic.LabelAgentID` and `render.LabelPoolID = traffic.LabelPoolID` (`afs.stainedhead.io/agent-id`, `/pool-id`). The egress renderer's own copy is deleted. | The usecase traffic collector cannot import an adapter, so the usecase package holds the literal and the renderers alias it. |
| 2026-11-22 | M2-X1 | Agent seeding in matrix fixtures: w2's `World.seedAgent` (returns placeholders) is the only helper; w1's cases use `seedAgentID` on top of it, which also clears config values and opt-outs before the agent. Postgres test helpers: w2's `seedAgent(t, db, teamID, name)` kept, w3's renamed `seedFullAgent`, one `teamSession`, one `isNotFound`; w2's `resourcesJSON` renamed `agentResourcesJSON`. | Name collisions only; behavior unchanged. |
| 2026-11-22 | M2-A/D/E | `ConfigRepo` and `ExtSystemRepo` keep reading `afs.agents` for agent existence under RLS. | w2 exposes no existence port; a join keeps the 404 identical to the agents API (cross-team 404). |
| 2026-11-22 | M2-B/G | `RestartAgent` writes `agent.restarted` with the agent as aggregate and raises `generation`; w3 derives the restart nonce from the newest such event (`TestAgentsE2E` asserts exactly one event per restart). `DeleteAgent` sets `state=deleting` and keeps the on-delete rule; w3's `AgentLifecycle.Complete` sets `deleted` and `deleted_at` (asserted in the same e2e and `TestAgentLifecycleComplete`). | Closes the three cross-stream assumptions. |
| 2026-11-22 | M2-F | -db a6ecc3c change feed: `agent_identities`, `external_system_optouts`, `agent_config_values`, `agent_role_links`, `team_tags`, `secrets`, `storage_classes`, `external_systems`, `agent_templates` and `agents` have `_notify` triggers. `agent_external_access` is a view (no trigger possible) and `agent_secret_bindings` has none; both are covered because their sources notify (`agent_external_access` joins `agents`, `external_systems`, `external_system_optouts`) and `agents` is updated in the same transaction as a binding change. The `agent_external_access` route in `agentRoutes()` never fires and is harmless. **-db note, no edit:** a binding-only change from a future writer that does not touch `agents` would not notify; the 5-minute resync would catch it. | Verified in the migration SQL and in a migrated database. |
| 2026-11-22 | M2-A1 bug | The router splits a trailing `:word` off the last path segment only when `word` is a registered verb of that route group. Model ids such as `anthropic.claude-3-5-sonnet-20241022-v2:0` are accepted (`modelIDRE` allows `:`), created, patched and read. | Bedrock ids end in `:0`. Tests: `TestColonIDIsNotAVerb`, `TestM2W1ModelsAndPrices`, `TestNewModel`. |
| 2026-11-22 | M1-B bug | `teams.StatusKind` is `team` (what the engine writes and the contract names); the teams repository join uses it. `TestTeamStatusKindMatchesTheEngine`. | It had been `teams`, so team status never appeared in `GetTeam`. |
| 2026-11-22 | M1-F6 bug | `TestDriftWatcher` flake: root cause was the own-write cache. `Client.Apply` recorded the new resourceVersion after `Patch` returned, but the informer can deliver the echo first, so the echo looked foreign. Now `beginWrite`/`endWrite` mark a write in flight and `IsOwnWrite` waits for it; the last 8 versions per object are kept (a later write no longer hides an earlier echo). `-count=10` of the envtest ran 10/10 green (it failed about 1 in 4 before). Unit tests `TestOwnWriteEchoBeforeThePatchReturns`, `TestOwnWriteKeepsRecentVersions`. | The race was in the code, not the test. |
| 2026-11-22 | M2-C | `afs_api_orphan_secrets_total` is `observability.MetricAPIOrphanSecrets`, a field of `observability.Metrics` (`APIOrphanSecrets`) and in `AllMetricNames`. | One place for metric names. |
| 2026-11-22 | M2-X1 | **Pods carry gateway environment before the gateway exists (plan section 5).** The agent renderer (M2-F3) sets `AFS_GATEWAY_URL`, `AWS_ENDPOINT_URL_BEDROCK_RUNTIME`, `ANTHROPIC_BASE_URL`, a placeholder `ANTHROPIC_API_KEY`, `AFS_GATEWAY_TOKEN_FILE` and the CA bundle variables (`render.GatewayEnv`, gateway contract section 3) from M2 on; `afs-gateway` is not built until M3. Until then an agent calling the endpoint gets a connection error, which is the intended fail-closed behavior; no AWS keys or Bedrock access are given to pods. | Rendering in M2 avoids a pod-template change (and a restart of every agent) when M3 lands. |
| 2026-11-22 | M2-C/I | Operational env: `AFS_AWS_REGION` unset makes secret writes answer 503; `AFS_LOG_REDACTION_KEY` enables log delivery (`AFS_LOG_AGGREGATOR_IMAGE`, `AFS_LOG_ROUTE_NAMESPACE` optional); `AFS_HUBBLE_ADDR`, `AFS_HUBBLE_TLS` for traffic; `AFS_NODECLASS_JSON` for dedicated nodes. | Recorded from the worker reports. |
| 2026-11-22 | M2-A..I | Behaviors fixed by the workers: no outbox for catalog, config and external-system mutations (CD-7, `NoOutbox` in the matrix); PUT upserts (200, optional If-Match) and creates return 201 with Location; deletes need If-Match; create agent 201, delete and restart 202, pause and resume 200, snapshot create 202; `DeclareTeamExternalSystem` without `mayAddExternalSystems` is 403 for everyone including service admins (database guard); `DeleteSecret` schedules a 30-day deletion after commit with the sweep as backstop; `log_destinations` has no generation column so generation equals version. | Recorded so M3+ and the CLI and SDK follow the same shapes. |

## M3 integration decisions (step 3, 2026-11-22)

Merge order: w1 (E budgets API, F budget reconciler), w2 (H reports, I activity collector), w3 (G notifications), w4 (gateway A to D). `m3/w1` and `m3/w4` were each added with `git merge --squash` and one commit, because each branch's history held a build artifact of about 100 MB; `m3/w2` and `m3/w3` were `--no-ff` merges. Conflicts were the two `wire.go` files, `go.mod` and the spec status file: both sides kept.

| date | task | decision | reason |
|---|---|---|---|
| 2026-11-22 | M3-X1 | `budget.exhausted` is published by the gateway only (once, `budget_threshold_events` marker 101). The reconciler's backstop sets `BudgetExhausted=True` and the IAM deny but publishes no event. `budget.reset` is published by the reconciler only, once per period (audit mark `budget.period_opened`). `TestGatewayAndBudgetReconcilerShareACounterAndEmitEachEventOnce` proves one event of each kind and one counter, with the gateway opening the counter first and with the reconciler first. | Two emitters of one event would double every notification. |
| 2026-11-22 | M3-X1 | Spend counters open the same way from both sides: both compute the period start as the UTC month (monthly) or `starts_at` (lifetime) and both call the idempotent `afs.ensure_spend_counter`; the reconciler pre-opens the next month 2 minutes ahead. A reservation made by the gateway survives the reconciler's passes (tested). | Either may be first; neither may create a second counter or disturb a hold. |
| 2026-11-22 | M3-X1 | `usage_events` written by the gateway carry `tag_set_id` (the team's current tag set), `pool_id` and `worker_pod` (pool workers) and `agent_id`. A call is a row with outcome `ok` or `error`; `rejected_budget` and `rejected_model` are not calls. Reports (`usage` calls), the activity collector and, in M4, `cost_daily.calls` use this one rule. `TestGatewayUsageRowsFeedReportsAndTheActivityCollector` writes rows through the real writer and reads them through the collector's source. | The collector and reports would otherwise disagree on activity. |
| 2026-11-22 | M3-X1 | The costs report measures are `estimatedUsd` (gateway metering) and `billedUsd` (CUR, daily, after reconciliation); usage rows carry `costUsd`. `api/openapi.yaml` was silent (`Report.rows` is a free object), so its description now names every measure of every report kind, and `TestReportMeasuresAreDocumentedInTheOpenAPIDocument` fails when code and document drift. The code was not changed. | The document is the contract; the free-form rows needed a documented shape. |
| 2026-11-22 | M3-X1 | `notification.test_requested` stays an extension event (`domain.ExtensionEventEmitters`); `contractgen -check` passes against the root checkout. Listed in restapi-pending for API contract section 7. | Not in contract 1.1.0. |
| 2026-11-22 | M3-X1 | Router: w2's `tagFilterAllowed` (report operations accept `tag.<key>`) and M2's verb split coexist; `TestTagFiltersAreOnlyForReportOperations` covers both and refuses a tag filter on other operations. | The two changes touch the same file. |
| 2026-11-22 | M3-X1 | Every reconciler route list (agents, logs, egress, notifications) goes through the single `mergeRoutes`; the budget and activity collectors register leader-only jobs and add no routes. | The registry rejects a table routed twice. |
| 2026-11-22 | M3-X1 | Gateway Helm: ClusterRole and ClusterRoleBinding `afs-gateway` (get/list/watch on pods, namespaces, `ciliumendpoints`; create on `tokenreviews`) in `templates/gateway-rbac.yaml`, gateway values for `AFS_GATEWAY_CILIUM_CHECK` and `AFS_GATEWAY_CACHE_TTL`, and the CI values supply `AFS_GATEWAY_ROLE_ARN` and `AFS_BEDROCK_VPCE_URL`. `TestGatewayRBACIsReadOnlyPlusTokenReview` pins the rules. `helm lint` and `tflint` run in CI (deploy.yml); the render tests with the Helm library ran here. | The pod cache and TokenReview fail without these grants. |
| 2026-11-22 | M3-X1 | Design review, gateway: it fails closed (no budget `402`, database or reservation trouble `503` within 2 s, TokenReview outage with a token `503`). `TestNoPromptCredentialOrTokenIsLogged` drives ok, 5xx, throttled, unknown-caller, malformed and unsupported-path calls carrying a prompt, a SigV4 key, an `x-api-key` and a gateway token and asserts none reaches the log. | AGENTS.md security rules. |
| 2026-11-22 | M1 minor | `observability.ServeOps` shuts down with `context.WithoutCancel(ctx)` plus a 5 s timeout instead of `context.Background()`. `usage_writer.go` keeps `context.Background()` for its 5 s flush: the writer is a background goroutine with no request, and it must flush after the server context is cancelled. | Design review item below cmd/. |
| 2026-11-22 | M3-X1 | OQ-3 default kept: pod identity by source IP for every runtime; `X-Afs-Gateway-Token` optional, checked only when sent. | No runtime was found that needs the token path. |
| 2026-11-22 | M3-X1 | Coverage: `budgets` and `reconcile/budget` were at 87.5% and 89.5%; error-path tests (store failures, per-step failures, pre-open retry, per-team backstop failures) bring them to 95.3% and 98.8%. `budgetstest.Repo` gained `Fail`. | `make cover` needs 90%. |

## M3-G decisions (notifications, `m3/w3`)

| date | task | decision | reason |
|---|---|---|---|
| 2026-11-22 | M3-G4 | `notification.test_requested` is not in contract 1.1.0, so `contractgen` cannot emit it. `domain.ExtensionEventEmitters` (hand-written `events_ext.go`) lists it, and `EventType.Known` (template in `tools/contractgen/gen.go` and the generated `events_gen.go`) accepts it. Remove the entry when the root contract lists the type (CD-3). | The outbox writer and `NewEvent` refuse unknown types. |
| 2026-11-22 | M3-G4 | The reconciler learns of a test by reading `notification.test_requested` rows of the outbox every 2 s (it has SELECT on all tables), not through the change feed. The result goes on the channel's `resource_status` as the `DeliveryDetail` condition (JSON: lastDeliveredAt, lastFailureAt, lastError, lastTestId, testDelivered, testError); `DeliveryHealthy` carries the health. The API polls the status for 10 s. A replayed idempotent test returns the stored `202` with the test id; the outcome is on the channel. | No schema column holds delivery history; conditions are the only status the API reads. |
| 2026-11-22 | M3-G3 | One channel, one rule named `afs-<channel id>`, plus a connection and API destination with the same name for Slack, webhook and Jira. A Slack webhook URL is the credential: it is read from the team secret in memory and given to EventBridge as the destination endpoint (OQ-S3), never stored by AFS. Webhook secrets go in the connection as the `Authorization` API key; Jira secrets are `user:api-token` (basic auth). | OQ-S3. |
| 2026-11-22 | M3-G3 | A channel with no `eventTypes` receives every contract event family except `notification.*` (a failing channel must not be fed its own failures) and `report.*` (only the channel a schedule addresses, OQ-S2). A service-level channel with no event types has no rule. Every team channel rule carries the team id in its pattern. | RC-US-13.4; loop avoidance. |
| 2026-11-22 | M3-G3 | Email: the rule targets one platform FIFO queue (`AFS_NOTIFY_EMAIL_QUEUE_ARN`, content-based deduplication) with the channel id in the body; the reconciler drains it and sends with SES once per (channel, event id) using an in-memory set (24 h, bounded) on top of the queue's deduplication window. | RC-US-13.2; no schema for a sent-log. |
| 2026-11-22 | M3-G4 | Delivery failures after EventBridge's retries land in the dead-letter queue (`AFS_NOTIFY_DLQ_ARN`); the reconciler drains it into the channel status and emits `notification.delivery_failed`. A recorded failure stays until a later success, a successful test, or a configuration change (a new version resets health). Failure text is stripped of URLs. | RC-US-13.3. |
| 2026-11-22 | M3-G4 | `adapter/notifyhttp` is the only code that calls a team-supplied URL from the reconciler (tests of webhook and Jira channels): https only, no URL credentials, the dialer refuses non-public addresses (loopback, private, link-local, metadata), no redirects, 10 s, 200-byte redacted response snippet. | SSRF; a webhook URL is a credential. |
| 2026-11-22 | M3-G | `secretsmanager.Reader` (`read.go`) is a separate type from the write client, built only in `cmd/afs-reconciler`; `afs-api` links the package but never constructs a reader. | Keep the read path out of the API's wiring. |

## Edge cases and solutions

(append during implementation)

## Deviations from plan

| Date | Task | Deviation | Why |
|---|---|---|---|
| 2026-11-21 | M0 (all streams) | Streams ran serially in the milestone agent's own checkout on `feat/restapi-feature-complete`, one commit group per stream in the listed merge order, instead of parallel worker sub-agents in git worktrees | The M0 milestone agent had no tool to start worker sub-agents. Ownership lines were still honored file by file, so the commit history matches the stream boundaries |
| 2026-11-21 | M0-S2 | ULIDs are encoded by a small stdlib encoder in `internal/domain/ids.go`, not `oklog/ulid/v2` (research R-12) | AGENTS.md: `domain` imports nothing outside the standard library; `internal/archtest` enforces it |
| 2026-11-21 | M0-S2 | `ImageRef` lives in `names.go`, not a separate `image.go` (data dictionary §1) | M0-S's Owns line lists `names.go` and no `image.go` |
| 2026-11-21 | M0-S4 | `make cover` runs `tools/covercheck` (fails any package below 90%) | `go test -cover` alone does not fail on low coverage |
| 2026-11-21 | M0-S1 / N-3 | `-db` re-adopted at `v0.0.0-20261121211911-23e924052840` (commit 23e9240, migrations 000001–000016) instead of b0d8754; `internal/archtest/dbmodule_test.go` asserts 16 migrations | Coordinator: `-db` PR #12 delivers X-DB-1..6 (`authenticate_api_key`, `resolve_workload`, `team_directory`, `instantiate_work_order_team`, pool state grant, INSERT-only audit/outbox policies) |
| 2026-11-21 | M0-F8 | The interim read-only lookup transaction is not built; the API key, workload and team directory lookups call `afs.authenticate_api_key`, `afs.resolve_workload` and `afs.team_directory` directly | X-DB-1..3 were adopted in M0, so the interim path would be removed before use (coordinator instruction) |
| 2026-11-21 | M0-F5 | Audit and outbox inserts never use `INSERT … RETURNING` | The 000016 INSERT-only policies (`app_own_audit`, `app_operator_work_order_events`) give `afs_app` no SELECT on those rows, so RETURNING would fail |
| 2026-11-21 | M0-B* | OpenAPI is authored as fragments generated by a script into `api/src/` and bundled by `tools/openapibundle`; the fragments, not the script, are the source of truth | Keeps the bundle reviewable per area |
| 2026-11-21 | M0-C2 | An authenticated caller with no binding calling an any-scope route that needs a permission (e.g. `ListTeams`) gets `403` with `requiredPermission`, not `404` | `404` is for unbound team-scoped resources; an any-scope route has no team to hide |
| 2026-11-21 | M0-F8 | The X-DB lookups live in `internal/adapter/postgres/lookup.go` (`Credentials`), a file M0-F's Owns line does not name | The interim lookup it replaces had no file listed either |
| 2026-11-21 | M0-F2 | `CheckSchema` reads `public.afs_schema_migrations` directly instead of calling `migrate.Version`, which runs `CREATE TABLE IF NOT EXISTS` first. Service roles (`afs_reconciler`, `afs_gateway`, `afs_billing`) have no SELECT on the tracking table (42501), so they cannot do the startup check of database contract §1 yet. Requested from `-db` as **X-DB-7** (grant SELECT on the tracking table to the service roles, or a SECURITY DEFINER `afs.schema_version()`). `afs-api` is not affected: it migrates on the owner connection and `migrate.Up` returning without error means the schema is current. A test pins the current behavior | Ask, do not work around (task instruction) |
| 2026-11-21 | M0-F6 | The idempotency claim is inserted in the mutation's own transaction, not committed first: a concurrent duplicate waits on the primary key and then replays (or runs, if the first rolled back), so a 5xx never leaves a key behind. `in_progress` is therefore seen only within one transaction | Database contract §7.1 rule 2 (roll back on 5xx so the key disappears) |
| 2026-11-21 | M0-F1 | `pgtest` creates one LOGIN user per role per test database with a random per-run password, and drops them with the database | No credential in code; works with CI's TCP password auth and the local trust socket |
| 2026-11-21 | M0-D2 | Strict query checking reads each operation's query parameters from the embedded `api/openapi.yaml` at startup, not from `routes_gen.go` (whose `Query` lists only contract filters, not `maxResults`/`nextToken`) | The OpenAPI document is the contract the clients follow |
| 2026-11-21 | M0-D2 | An oversized body is `400 ValidationError` (`field: body`), not `413` | Contract §6 has no 413 |
| 2026-11-21 | M0-D5 | Page tokens are bound to the operation and the caller as well as signed and time-limited | A token replayed by another caller or on another list is refused |
| 2026-11-21 | M0-D5 | CSV cells starting with `=`, `+`, `@`, tab, CR, or `-` not followed by a number are prefixed with `'` | CSV formula injection; negative money stays numeric |
| 2026-11-21 | M0-D1 | Unmatched paths and unknown `:verb`s answer `404 NotFound` in the error envelope; ServeMux's own 405 is not used | One error shape for every response |
| 2026-11-21 | M0-D6 | The replay validates every response the server gives, not only non-placeholder operations: with no authenticator wired yet, every non-public operation answers `401`, and that envelope is validated against each operation's documented `401`. Placeholders (`501`, not in the document) are counted and skipped | Proves the error envelope and headers on all 182 operations from M0 |
| 2026-11-21 | M0-D4 | `afs-api` starts without a database when `AFS_DATABASE_URL` is unset (health, OpenAPI and placeholders only); `AFS_DATABASE_OWNER_URL` enables startup migration | Local development and the `--version` gate step need no database |
| 2026-11-21 | M0-E1, M0-E2 | Spikes are desk research only; the probes shell out to `kubectl` and the AWS CLI instead of using client-go and the AWS SDK, and have not been run | No AWS account or cluster is available to agents; the module does not depend on client-go yet. Defaults in research.md §3.3 stand until a person runs `make e2e` |
| 2026-11-21 | M0-E3 | A-1..A-10 recorded as ADR-002 (accepted) and ADR-003..ADR-011 | ADR numbering continues the existing file |
| 2026-11-21 | M0-X1 | X-DB-1..6 were not requested: `-db` delivered them unprompted (PR #12). X-DB-7 is requested instead. The root `contract` PR is not opened by the milestone agent; the delta list below is ready for it | Opening a PR in the root repository is outside this milestone's checkout; the orchestrator decides |
| 2026-11-21 | M0-F | Local integration tests ran against PostgreSQL 16.15 (the sandbox server), not 17; CI uses `postgres:17` | Only PostgreSQL 16 is installed in the build sandbox |

## M3 gateway decisions (worker w4: M3-A, B, C, D)

| Date | Task | Decision or deviation | Why |
|---|---|---|---|
| 2026-11-22 | M3-B4 | Usage rows are written with one batched `INSERT ... SELECT FROM unnest(...)` per flush, not `COPY` | `COPY` into `usage_events` is refused under row-level security; the batch keeps the 1 s flush. A `-db` grant that allows COPY would restore the spec's wording |
| 2026-11-22 | M3-B2 | A team with no budget fails closed: `402 BudgetExceeded` | GW-3: the gateway never forwards a call it cannot reserve for |
| 2026-11-22 | M3-B2 | A `notify`-only budget never blocks; its calls settle with an empty hold id | Notify-only budgets raise thresholds, not caps |
| 2026-11-22 | M3-B5 | `budget.exhausted` is deduplicated in `budget_threshold_events` with threshold marker 101 | The table's key is (budget, period, threshold); 101 cannot collide with a real percentage |
| 2026-11-22 | M3-B3 | Everything before the call reaches Bedrock is bounded by 2 s (`FailClosedTimeout`); past it the call is `503` | GW-3 |
| 2026-11-22 | M3-B2 | Settlement runs on `context.WithoutCancel` with retries, and in production asynchronously (`Config.AsyncSettle`): the answer is sent, then settle, usage row and thresholds | Keeps added latency off the DB round trip; `Service.Wait()` drains on shutdown |
| 2026-11-22 | M3-A4 | `requestMetadata` is replaced only on Converse; Invoke bodies carry no gateway tags. Tag ordering is the basic system, service, team order | Invoke bodies are model specific; GW-5 tag ordering is M4-B2 |
| 2026-11-22 | M3-A4 | A suspended-team call is recorded as outcome `error`; unauthenticated calls are not recorded | `usage_events` needs a team and a subject |
| 2026-11-22 | M3-A | Gateway RBAC needed in the Helm chart (not done here): list/watch `pods` and `namespaces`, create `tokenreviews`, list/watch `ciliumendpoints` (cluster scope); the Cilium cross-check is on by default, `AFS_GATEWAY_CILIUM_CHECK=false` turns it off | The pod cache and TokenReview read them |
| 2026-11-22 | M3-A | New files beyond the Owns globs: `gatewayhttp/{wire,eventstream,sse}.go`, `gatewayhttp/gwtest/`, `usecase/gateway/gatewaytest/`, `cmd/afs-gateway/{settings,wire,tls}.go`, `postgres/{gateway_store,usage_writer}.go`; `go.mod` gained bedrockruntime, anthropic-sdk-go, eventstream and sts | Supporting code of the owned streams |
| 2026-11-22 | M3-D3 | Load run (in-memory spend, 6 s, 200 agents on distinct loopback addresses, 500 req/s, budget for a third): 1000 ok, 2000 refused all `402`, overshoot 0, nothing left reserved, added latency p50 73 us, p99 163 us | Database-side concurrency is proven by the postgres integration tests (60 concurrent callers on a small budget) |

## M4-C and M4-D decisions (worker w2, `m4/w2`)

| Task | Decision | Why |
|---|---|---|
| M4-C1 | `afs-billing --job <name> [--day D]`; each job file registers itself from `init()` into a `register(name, fn)` map | M4-E adds jobs (showback, report schedules) in new files without editing the runner |
| M4-C2 | Athena SQL assumes the CUR 2.0 standard export: `line_item_*`, `split_line_item_*`, `resource_tags` keyed with the `user_` prefix (`AFS_CUR_TAG_PREFIX`). One query per UTC day returns Bedrock by inference profile, EKS pods by namespace (pods on a dedicated node excluded, the node attributed whole by its team tag), EBS storage | One query per day keeps the cost bounded; classification in SQL keeps the adapter small |
| M4-C2 | An empty fetch means "no data yet": the day's rows are left alone. A rerun deletes the day's `cur_*` rows and inserts the new ones in one transaction | Idempotent per day, no flicker for readers |
| M4-C2 | Retry 4 attempts, 5 s doubling to 1 min; alert after 3 failed days (`afs_billing_cur_ingest_alerts_total` and a log line) | M4-C2 AC |
| M4-C3 | Tag sets are built by `afs.tag_set_for(team, jsonb)` from the line's managed tag keys; a day with a tag change is split by line items because the SQL groups by the tag map; pool workers carry `pool_id` | BI-US-6.1, BI-7 |
| M4-C4 | Reconciliation compares the gateway estimate with `cur_bedrock` only. EKS and storage have no gateway estimate | Comparing total CUR cost with model-call cost would always flag |
| M4-C4 | A day with no CUR rows at all is skipped, not reconciled | Else every metered day looks `missing_billing` until AWS delivers |
| M4-C4 | `cost.variance_detected` is published in the same transaction when the stored status turns `over_threshold`, not on reruns of a team-day that already was | One event per team-day |
| M4-C4 | `cost_daily.calls` counts `usage_events` with outcome `ok` or `error` only | Brief; matches the reports |
| M4-C | **-db gap X-DB-8**: `afs_billing` has no SELECT on `service_settings` and `team_models`. Inference profile attribution degrades to the `afs-team-id` tag (`Directory.ProfilesUnavailable`, no `model_id`); `pricing.price_list_sync` falls back to `AFS_PRICING_PRICE_LIST_SYNC`, failing closed when neither is available. Two tests flip (`TestBillingDirectoryReadsInferenceProfiles` variant, `TestPriceSyncEnabledFallsBackWhenTheSettingIsRefused` skips) once -db grants the reads | The -db pin a6ecc3c is not changed by this stream |
| M4-C | A CronJob process is short-lived, so gauges such as `CURVariance` are set but never scraped; the alert counter is the same. Alerting for billing should come from job failure and logs until a pushgateway or a long-running mode exists | Known limitation, not in the chart |
| M4-C5 | `ListCosts` default range is the shared 7 days; the cursor is an offset with the range, `ListCostReconciliations` is keyset by (day, team) descending; `variancePct` is a 2-decimal string and `0.00` when nothing was billed | OpenAPI pattern allows two decimals |
| M4-D1 | Price List products: `attributes.model` (or `modelId`), `inferenceType` (input, output, cache read, cache write), `regionCode`, `terms.OnDemand` with `unit` `1K tokens`, `1M tokens` or `tokens`; batch, provisioned and custom products are skipped. Prices are normalized to USD per million tokens, six decimals | The shapes are the Price List API's documented JSON; confirm against a real response before the first run |
| M4-D1 | A list product matches a catalog model by exact id, else by a normalized key (`anthropic.claude-sonnet-4-5-v1:0` and `Claude Sonnet 4.5` both give `claudesonnet45`); unmatched products are reported in the log | Display names differ from model ids |
| M4-D1 | A price equal to the current one retires the open proposal (superseded); a price equal to the open proposal does nothing | One pending proposal per model (unique index) |
| M4-D2 | Proposal ids are the database's `mpp_<ULID>`, not the numeric ids of the OpenAPI examples (`^[0-9]+$` in the path pattern, id example `"1042"`). `version` is 1 while pending and 2 after (the table has none). Notes are limited to 500 characters (database), where OpenAPI says 1000 | `api/openapi.yaml` is not in this stream's Owns; it needs the pattern, example and maxLength aligned |
| M4-D2 | The idempotency operation name takes letters only (database check), so the proposal id is hashed into the claimed request instead of the operation | One key reused on another proposal is a mismatch |
| M4-D2 | The database writes the price on approval at `greatest(effective_from, now())`; the event carries the same effective time | RA-7 |

Files outside the Owns lines: `cmd/afs-api/wire.go` (two additive lines: `wireCosts`, `wirePricing`), `internal/adapter/httpapi/matrix/fixtures_{costs,pricing}.go`, `go.mod`/`go.sum` (Athena and Pricing SDK modules), `user-docs/billing.md`.

## Contract deltas for the root repository

Collected for the M0 exit task's root PR (labelled `contract`): see research.md §3.1.6 and OQ-S1, OQ-S2.
Each is decided in `api/openapi.yaml` 1.1.0 and needs the root api-contract.md to state it:

| # | Delta | Decided as |
|---|---|---|
| CD-1 | Delete response codes | `202` with the resource body for resources that carry a reconcile `status` (agents, pools, teams, role links…); `204` otherwise |
| CD-2 | `If-Match` on `PUT` | Required on `PATCH` and `DELETE`; optional on `PUT` (upsert semantics); `428` when required and absent |
| CD-3 | `TestNotificationChannel` (OQ-S1) | `NotificationTestResult{testId, delivered, error, pending}`: `200` with `delivered` true/false and `error` once the reconciler reports, `202` with `pending: true` before; new event `notification.test_requested` |
| CD-4 | `RestoreRevision` shape | `RestoreResult{dryRun, revision, changes[RestoreChange{action create/update/delete/skip, fields…}], skipped[]}` |
| CD-5 | Shapes the contract does not fix | `Snapshot`, `StorageClass`, `AuditEvent`, `Revision`, `CostRow` (see `api/src/schemas/`) |
| CD-6 | `TeamTags.requestMetadataDroppedKeys` | Read-only list of tag keys dropped from Bedrock `requestMetadata` because of its limit (OQ-2) |
| CD-7 | Outbox events for mutations not in §7 | Members, API keys, settings and other mutations have no §7 event type, yet CT-API-8 requires an outbox row per mutation; needs event types or an exemption |
| CD-8 | Settings keys | Database contract §7.0 seeds `work_orders.cost_center` and `work_orders.owner_email`, which root PRD §6.13 does not list; `GetSettings` documents the 12 PRD keys only |
| CD-9 | Identity provider client ids | The contract has separate console and CLI client ids; `afs.identity_providers` has one `client_id` |
| CD-10 | Unbound caller on any-scope route | `403` with `requiredPermission` (see deviation above) |

## Lessons learned

(append at completion)
