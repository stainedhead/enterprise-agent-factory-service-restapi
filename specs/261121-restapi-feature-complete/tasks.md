# Tasks: `-restapi` feature complete

| | |
|---|---|
| **Feature** | restapi-feature-complete |
| **Date** | 2026-11-21 |
| **Status** | Planning complete; M0 not started |

## How to use this file

- **One milestone agent per milestone (M0 → M6), in order.** It runs the milestone's streams in parallel
  git worktrees, merges them into `feat/restapi-feature-complete` in the listed merge order, then does the
  milestone's exit task (`Mn-X`). Only the milestone agent ticks boxes here and updates `status.md`.
- **Streams own files.** A stream creates and edits only the paths in its *Owns* line, plus new
  `*_test.go` files beside them and one line in the shared lists named in architecture.md §3 (handler
  registrar list is pre-created; `cmd/*/main.go` wire list; `go.mod`). Anything else is a dependency on
  another stream and must be listed in *Depends*.
- **Task ids** `Mn-<stream><seq>`. **AC** names the acceptance criteria the task's tests prove: PRD story
  criteria as `RA-US-3.2` (story RA-US-3, second bullet; catalogue in spec.md §9.1) and contract criteria
  as `CT-…` (spec.md §9.2). A partial AC is marked `(part)`: the rest is proven by the other tasks that cite
  it. **Req** names the PRD requirement (§4). **Depends** names tasks (earlier milestones are implicit).
- **Every task is TDD:** write the AC tests first (red), implement (green), refactor; `make gate` green
  before the commit. Every route task includes allowed and denied tests per role (rows in the M1-D6 matrix
  harness). Every mutation task asserts row + audit + outbox in one transaction (CT-API-8).
- **No test touches real AWS or a real cluster outside `[e2e]`/`[load]`.** AWS through port fakes and the
  `awstest` endpoint recorder; Kubernetes through envtest (`make envtest`) or the fake clientset.
- Test levels in brackets follow PRD §5.

## Progress summary

| Milestone | Streams | Tasks | Done |
|---|---|---|---|
| M0 Contract and foundation | 10 (+ exit) | 40 | 0 |
| M1 Teams and core | 13 (+ exit) | 59 | 0 |
| M2 Agents | 9 (+ exit) | 43 | 0 |
| M3 Gateway and budgets | 9 (+ exit) | 36 | 0 |
| M4 Accounts, tags, billing | 7 (+ exit) | 25 | 0 |
| M5 Swarms | 6 (+ exit) | 20 | 0 |
| M6 CVE remediation and completion | 6 (+ exit) | 25 | 0 |
| **Total** | **60** | **248** | **0** |


---

## M0 — Contract and foundation

Goal: the full v1 OpenAPI document, the contract extract, `authz`, the HTTP skeleton with every route, and
the PostgreSQL foundation. Stream S runs first and alone; then A, C, E, F in parallel; B1–B4 in parallel
once A5 lands (they can start authoring before); D after A4 and C1. Merge order: S, A, F, C, B1, B2, B3,
B4, D, E.

### M0-S · Bootstrap (serial, first)

Owns: `go.mod`, `go.sum`, `internal/domain/{ids,names,money,status,concurrency,errors,request,enums}.go`,
`internal/archtest/`, `Makefile`, `.github/workflows/ci.yml`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-S1 | Adopt `-db` as `github.com/stainedhead/enterprise-agent-factory-service-db@v0.0.0-20261121202720-b0d8754e4659` and `pgx/v5` v5.13.0; a test asserts `migrations.FS` holds 000001–000014 | RA-4 (part), plan.md §6 | — |
| [ ] | M0-S2 | Shared domain primitives: prefixed ULID ids (all contract §1 prefixes), `Name`, `ConfigName`, `Money` (decimal string, ≤ 6 fraction digits), `ImageRef` (`@sha256:` required), `Version`/`ETag`/`If-Match` parsing, `Phase`/`Condition`, domain error kinds (`Validation{fields}`, `NotFound`, `Conflict{reason, details}`, `PreconditionFailed`, `PreconditionRequired`, `AccessDenied{perm}`, `Unavailable`, `Internal`), enums of contract §5 | CT-API-10, CT-API-11, RA-US-4.1 (part) | M0-S1 |
| [ ] | M0-S3 | `internal/archtest`: dependency rule (domain/usecase import only stdlib, authz, each other); `cmd/afs-api` constructs no AWS service client except Secrets Manager (allowlist: `feature/rds/auth`), no Kubernetes client, no S3 client | RA-US-5.4, RA-US-2b.4 (part), RA-US-14.5 (part) | M0-S1 |
| [ ] | M0-S4 | Makefile targets `contract`, `openapi`, `test-db`, `envtest`, `e2e`, `load`, `conformance`; `cover` over `domain/...`, `usecase/...`, `authz/...` with 90% threshold; CI: tagged suites compile (`go vet -tags e2e,load,conformance,envtest ./...`), OpenAPI bundle drift, contract extract drift when the root checkout is present, `envtest` job | NFR Quality | M0-S1 |

### M0-A · Contract extraction and tooling

Owns: `tools/contractgen/`, `tools/openapibundle/`, `api/contract/`, `api/embed.go`, `api/contract_test.go`,
generated `internal/authz/catalog_gen.go`, `internal/adapter/httpapi/routes_gen.go`, `internal/domain/events_gen.go`,
`internal/domain/settings_gen.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-A1 | `contractgen` parses contract §4 tables into `api/contract/operations.json`: 182 operationIds, method, path, permission (with field-level and conditional variants for `UpdateTeam`, `DeclareTeamExternalSystem`, `UpdateWorkOrderRepo`, `ListWorkOrders`), scope (`team`, `service`, `public`, `any`), CLI, SDK, screen, priority, CSV flag; golden test on a fixture copy of the tables; `api/contract/SOURCE` records contract version 1.1.0 and source SHA-256 | RA-1 (part) | M0-S1 |
| [ ] | M0-A2 | Parse §3 into `permissions.json` (permission × role, with conditions `mayManageAwsAccounts`, `viewerKubectl`, `assigned or own repos`, `assigned team`) and generate `internal/authz/catalog_gen.go` | RA-1a (part), RA-US-1.2 (part) | M0-A1 |
| [ ] | M0-A3 | Parse §6 errors, §7 events and root PRD §6.13 settings into `errors.json`, `events.json`, `settings.json`; generate `domain/events_gen.go` (type constants, emitter, envelope) and `domain/settings_gen.go` (key, type, default, range) | CT-API-5 (part), CT-API-9 (part), RA-16 (part) | M0-A1 |
| [ ] | M0-A4 | Generate `httpapi/routes_gen.go`: one entry per operation (method, ServeMux pattern, permission spec, scope, CSV, idempotent-create flag, mutation flag) | RA-1 (part) | M0-A1 |
| [ ] | M0-A5 | `openapibundle`: merge `api/src/**` into a stable, sorted `api/openapi.yaml`; `make openapi`; drift check | RA-1 (part) | M0-S4 |
| [ ] | M0-A6 | `api/contract_test.go`: bundle is valid OpenAPI 3.1; operationIds, methods, paths, `x-afs-permission`, `x-afs-scope` equal `operations.json`; only §6 error codes; every operation has request/response examples; no schema property is a replica count | RA-US-4.6, CT-API-5 (part) | M0-A5, M0-B1b, M0-B2b, M0-B3b, M0-B4b |
| [ ] | M0-A7 | `api/embed.go` exposes the bundle and `ContractVersion = "1.1.0"` | RA-1 (part), RA-US-6.5 (part) | M0-A5 |

### M0-B1 · OpenAPI: components, caller, teams, members (contract §1, §2, §4.1–§4.3, §5, §6)

Owns: `api/src/openapi.base.yaml`, `api/src/components/`, `api/src/paths/{01-caller,02-teams,03-members}.yaml`,
`api/src/schemas/{01-caller,02-teams,03-members}.yaml`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-B1a | Base: `info.version` 1.1.0, servers, security schemes (IdP bearer, `afsk_` key bearer, workload bearer); parameters (`teamId`, `maxResults`, `nextToken`, `Idempotency-Key`, `If-Match`, `X-Request-Id`, report query params); headers (`ETag`, `X-Request-Id`, `X-Afs-Contract-Version`, `Idempotent-Replayed`, `Retry-After`, `X-Afs-Next-Token`); `Error` with every §6 code and `details` variants (`fields`, `requiredPermission`, `reason`, `missingTags`, `ambiguousRepos`, `unownedRepos`); `Status`; page wrapper; standard error responses (incl. `428`) on every operation | CT-API-1 (part), CT-API-5 (part), CT-API-6 (part) | — |
| [ ] | M0-B1b | §4.1–§4.3 paths and schemas: `Me`, sign-in providers (no secret), `Team` (guardrail fields, ephemeral fields, `effectiveCeilingUsd`), `TeamTags` (+ `requestMetadataDroppedKeys`), `Member`, `ApiKey` (`secretKey` only on create), team directory, kubeconfig (YAML), suspend/resume/status, models, allowed accounts, service keys, security operators; examples | RA-1 (part) | M0-B1a |

### M0-B2 · OpenAPI: agents, pools, identity, secrets, config, external systems (§4.4–§4.8)

Owns: `api/src/paths/{04-agents,04a-pools,05-identity,06-secrets,07-config,08-external-systems}.yaml` and matching `api/src/schemas/*`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-B2a | §4.4 agents (no replica field), snapshots, storage classes, agent templates; §4.4a stages, pools, scale, pause/resume, status, handoffs, pool secret bindings, config, external systems, role links | RA-1 (part), RA-US-4.6 (part) | M0-B1a |
| [ ] | M0-B2b | §4.5 role links (`trustPolicy`), identities; §4.6 secrets (value only in create/put/rotate requests, `writeOnly`); §4.7 config values (`source`, resolved); §4.8 external systems; examples | RA-1 (part) | M0-B1a |

### M0-B3 · OpenAPI: budgets, reports, delivery, audit, revisions, work orders (§4.9–§4.10a)

Owns: `api/src/paths/{09-budgets-reports,10-delivery-audit,10a-work-orders}.yaml` and matching schemas.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-B3a | §4.9 budget (`forecastUsd`, `projectedCapAt`), usage, costs, team/agent/service reports with the report query and `text/csv` responses, report schedules, cost tag status | RA-1 (part), CT-API-3 (part) | M0-B1a |
| [ ] | M0-B3b | §4.10 log destinations, notification channels (with delivery status; `:test` response per research OQ-S1), audit events (CSV), revisions, `RestoreRevision` (`dryRun`, diff shape decided here); §4.10a work orders (`ownerSource` on create response only), repos, team work orders, swarm templates; examples | RA-1 (part) | M0-B1a |

### M0-B4 · OpenAPI: service administration (§4.11)

Owns: `api/src/paths/11-admin.yaml`, `api/src/schemas/11-admin.yaml`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-B4a | Settings (every key with value, default, range), identity providers, service notification channels, service tags, models, prices, price proposals, cost reconciliations, catalog systems, storage classes, agent templates, org accounts, admin audit | RA-1 (part) | M0-B1a |
| [ ] | M0-B4b | Exports (`202`, `downloadUrl`), imports (`dryRun` default true, 100 MB body, report shape), health and readiness; examples | RA-1 (part) | M0-B1a |

### M0-C · Authorization (`internal/authz`)

Owns: `internal/authz/` except `catalog_gen.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-C1 | Replace the scaffold: roles `team_viewer`, `team_admin`, `service_admin`, `security_operator`, workload grant; catalog from `catalog_gen.go`; catalog-equality test against `permissions.json` (no extra or missing permission or grant; `budgets:set-ceiling` gone) | RA-1a, RA-US-1.2 | M0-A2 |
| [ ] | M0-C2 | `Authorize(principal, perm, resource) Decision`: service admins everything; team roles only on bound teams (`NotFound` when unbound); security operator only its column and `Deny` on every team-scoped resource; workload only `work-orders:report` on its team's assigned work orders | RA-US-1.4 (part), RA-US-2b.2 (part), RA-US-15.5 (part) | M0-C1 |
| [ ] | M0-C3 | Conditional grants with allowed and denied cases: `teams:set-aws-accounts` iff `mayManageAwsAccounts`, `teams:get-kubeconfig` iff `viewerKubectl`, declare external systems iff `mayAddExternalSystems`, `work-orders:read` for assigned or own-repo work orders, `work-orders:report` on the assigned team | RA-US-1.5 | M0-C2 |
| [ ] | M0-C4 | `Effective` for `GetMe` (flag-dependent, D42) and `CanGrant` (team-admins cannot grant `service_admin` or `security_operator`); coverage ≥ 90% | RA-US-3a.1 (part), RA-US-3a.2 (part), RA-3 (part) | M0-C3 |

### M0-D · HTTP skeleton

Owns: `internal/adapter/httpapi/{server,router,errors,pagination,csv,decode,handlers}.go`, `internal/adapter/httpapi/middleware/`,
`cmd/afs-api/main.go`, `test/contract/`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-D1 | Router from `routes_gen.go`; every operation registered with a placeholder (`501`, `InternalError`, `details.reason = not_implemented`); `handlers.go` lists one empty registrar per area (`teams`, `members`, `apikeys`, `me`, `agents`, `pools`, `identity`, `secrets`, `config`, `extsys`, `budgets`, `reports`, `delivery`, `audit`, `revisions`, `workorders`, `swarmtemplates`, `admin*`, `exports`); route test fails for a route without a permission or explicit `Public` | RA-US-1.1, RA-1 (part) | M0-A4 |
| [ ] | M0-D2 | Middleware: request id (client id ≤ 64 chars echoed, else generated; in the error body), `X-Afs-Contract-Version` on every response incl. errors and `401`/`404`/`429`, error envelope and domain error → HTTP mapping, panic recovery, body limit (1 MiB; route override), strict query parameters (unknown → `ValidationError`), `teamId` from the path only (body `teamId` that differs → `ValidationError`) | RA-US-6.3 (part), RA-US-6.5 (part), RA-US-1.6, CT-API-2, CT-API-5 (part) | M0-D1, M0-A7 |
| [ ] | M0-D3 | Route authorization middleware applying PRD §3 in order: `401` → unbound or unknown team `404` → security operator on a team route `403` → bound without permission `403` + `details.requiredPermission` → service route `403`; hooks for denied audit and cross-team `404` metric; unit tests with fake authenticator and team lookup | RA-US-1.3 (part), RA-US-1.4 (part) | M0-D1, M0-C2 |
| [ ] | M0-D4 | `GET /v1/openapi.yaml` (public), `GetHealth`, `GetReadiness` (injected checks: database, schema version); `cmd/afs-api` with a `wire()` component list | RA-1 (part) | M0-D1 |
| [ ] | M0-D5 | Pagination (1–100, default 50; HMAC-signed opaque `nextToken`, expires after 24 h → `ValidationError`); CSV writer for operations flagged CSV (`Accept: text/csv`, `X-Afs-Next-Token`) | CT-API-1, CT-API-3 (part) | M0-D1 |
| [ ] | M0-D6 | `test/contract`: in-process server + seeded database; replays OpenAPI examples of every non-placeholder operation and validates requests and responses with `libopenapi-validator` | CT-API-7 (part), RA-US-6.5 (part) | M0-D4, M0-F1 |

### M0-E · Spikes and decisions

Owns: `test/e2e/spike_test.go`, `docs/architectural-decision-record.md`, research.md §3.3 entries.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-E1 | OQ-1: document Pod Identity session tags (desk research), IAM condition shape and the D51 `role_session_name` rule; `[e2e]` probe that prints session tags from a pod and a chained role | OQ-1, RC-2 (part), RC-3 (part) | M0-S4 |
| [ ] | M0-E2 | OQ-2: document per-resource tag limit after EBS CSI tags and the Bedrock `requestMetadata` limit; `[e2e]` probe counting a CSI volume's tags | OQ-2, RC-9 (part), GW-5 (part) | M0-S4 |
| [ ] | M0-E3 | Record ADRs A-1..A-10 of architecture.md §8 (ADR-002 accepted) | — | — |

### M0-F · PostgreSQL foundation and cross-cutting ports

Owns: `internal/adapter/postgres/{db,tx,session,errors,migrate,audit,outbox,idempotency}.go`, `internal/adapter/postgres/pgtest/`,
`internal/usecase/port/`, `internal/usecase/port/fakes/`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-F1 | `pgtest`: per-package fresh database, `bootstrap.RolesSQL`, `migrate.Up`, LOGIN users for `afs_owner`, `afs_app`, `afs_reconciler`, `afs_gateway`, `afs_billing`; skip when `AFS_TEST_DATABASE_URL` is unset; seed helpers | — | M0-S1 |
| [ ] | M0-F2 | Pool (optional RDS IAM auth token), `migrate.Up` on the owner connection under the advisory lock (3 concurrent starts safe), `migrate.Version` check that refuses to start on an older schema; service roles set `afs.actor = 'system:<service>'` | CT-DB-1, CT-DB-4 | M0-F1 |
| [ ] | M0-F3 | `TxRunner` with `SET LOCAL` session settings (`afs.actor`, `afs.service_admin`, `afs.team_ids`, `afs.security_operator`), `READ COMMITTED`, retry `40001`/`40P01` ×3 with jitter; a transaction without settings sees no team rows | RA-4 (part), RA-US-6.2, CT-DB-2 | M0-F1 |
| [ ] | M0-F4 | SQLSTATE → domain errors (database contract §6), incl. `23503` on write vs delete and `22023` logged | CT-DB-3 | M0-F3 |
| [ ] | M0-F5 | `AuditWriter` (request id, actor type, before/after without secret values, outcome) and `OutboxWriter` (contract §7 envelope); row + audit + outbox in one transaction, all rolled back together; denied audit in its own transaction | CT-API-8 (part), CT-API-9 (part), RA-US-6.3 (part) | M0-F3 |
| [ ] | M0-F6 | `IdempotencyStore` per database contract §7.1 (`in_progress`, `completed`, replay, mismatch, in progress, 5xx not stored) with `secretKey` stripping before storage | RA-US-9.1 (part), RA-US-9.3 (part) | M0-F3 |
| [ ] | M0-F7 | Ports `TxRunner`, `Session`, `AuditWriter`, `OutboxWriter`, `IdempotencyStore`, `Clock`, `IDGen`, `SettingsReader` and their fakes in `port/fakes` | — | M0-F3 |

### M0-X · Milestone exit

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M0-X1 | Merge streams; `make gate`, `make test-db`; M0-A6 equality green (all 182 operations documented); write the contract delta list (shapes the contract does not fix, the `notification.test_requested` event of OQ-S1, `requestMetadataDroppedKeys`) into implementation-notes.md and open the root `contract` PR; notify `-cli` and `-website` that `api/openapi.yaml` 1.1.0 is on the branch; update status.md | — | all M0 |

---

## M1 — Teams, members, keys, core reconciler, deploy

Merge order: L, D, A, E, B, C, F, G2, G1, H, I, J, K.

### M1-A · Authentication and caller identity (RA-2, RA-3)

Owns: `internal/adapter/oidc/`, `internal/usecase/auth/`, `internal/usecase/me/`, `internal/adapter/postgres/{principal,apikey_auth,me}_repo.go`,
`internal/adapter/httpapi/{h_me,dto_me}.go`, `internal/adapter/httpapi/middleware/authn.go`, `cmd/afs-api/wire_auth.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-A1 | JWKS cache: 10 min TTL, single-flight refetch on unknown `kid`, removed key stops validating after refetch; issuer, audience, expiry checks [unit with fake JWKS] | RA-US-2.3, RA-US-2.1 (part) | — |
| [ ] | M1-A2 | Providers from `identity_providers` (enabled only; audiences `consoleClientId`, `cliClientId`); a disabled provider's token → `401` | RA-US-2.1 | M1-A1 |
| [ ] | M1-A3 | Principal resolution: JIT `users` upsert (own row), bindings from `role_bindings` incl. groups (`groupsClaim`); a group binding on team A counts only when the issuer is A's provider or the default when A has none | RA-US-2.2, RA-2 | M1-A2 |
| [ ] | M1-A4 | API key auth: `afsk_` prefix, SHA-256 lookup, expired or deleted → `401`, `lastUsedAt` written at most once a minute | RA-US-2.4 | — |
| [ ] | M1-A5 | `ListSignInProviders` (public): every enabled provider with `consoleClientId`, `cliClientId`, `isDefault`, no client secret ARN | RA-US-2.5 | M1-A2 |
| [ ] | M1-A6 | `GetMe`: principal type, `serviceAdmin`, `securityOperator`, per bound team role and `authz.Effective` permissions (flag-dependent) for viewer, team-admin, service admin, security operator, team API key | RA-US-3a.1 (part), RA-US-3a.2, RA-3 | M1-A3, M1-A4 |
| [ ] | M1-A7 | Authn middleware: bearer dispatch (IdP JWT, `afsk_`, later workload), `401` for missing/expired/unknown with contract headers; `Authorization` never logged | RA-US-6.5 (part), NFR Security | M1-A3, M1-A4 |

### M1-B · Teams (RA-1 teams, RA-4, RA-14 fields, RA-10 guardrails)

Owns: `internal/domain/team.go`, `internal/usecase/teams/`, `internal/adapter/postgres/teams_repo.go`, `internal/adapter/httpapi/{h_teams,dto_teams}.go`,
`internal/adapter/httpapi/middleware/suspended.go`, `cmd/afs-api/wire_teams.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-B1 | `Team` aggregate: guardrail field set, `kind` immutable, effective ceiling (override while unexpired), state transitions, override validation (both fields, reason, future expiry, above `maxBudgetUsd`) [unit] | RA-US-6.4 (part), RA-14 (part) | — |
| [ ] | M1-B2 | Teams repository: insert, get, list (RLS), update with `version`, mark deleting; `team_models`, `team_tags` | RA-4 (part) | — |
| [ ] | M1-B3 | `CreateTeam` (persistent): required tags (`details.missingTags`), models, provider, first team-admin binding, one transaction, one audit, one `team.created`; any failure writes nothing; team-admin or viewer → `403` + one denied audit | RA-US-3.1, RA-US-3.2, RA-US-3.4 | M1-B1, M1-B2 |
| [ ] | M1-B4 | `ListTeams` (bound teams; service admins all), `GetTeam`, `GetTeamStatus` (from `resource_status`) | RA-1 (part), TEAM-5 | M1-B2 |
| [ ] | M1-B5 | `UpdateTeam`: `teams:write` fields; guardrail fields need `teams:set-guardrails` (team-admin changing `isolationTier` or `viewerKubectl` → `403`); `allowedAwsAccounts` by the conditional grant; ceiling override (reason audit only, `team.ceiling_override_set`); `maxBudgetUsd` below budget → `400`; `kind` change → `400`; `team.updated` | RA-US-6.4, RA-US-12.1 (part), RA-US-13.2 (part), RA-14 (part) | M1-B3 |
| [ ] | M1-B6 | `SuspendTeam`/`ResumeTeam` (`teams:suspend`, team-admin allowed), `DeleteTeam` (service admin; team-admin → `403` + denied audit; returns `state = deleting`, `status.phase = Deleting`); events | RA-US-16.2 | M1-B3 |
| [ ] | M1-B7 | Suspended-team guard: on a suspended team every mutation except `ResumeTeam` and `DeleteTeam` → `409` `details.reason = team_suspended`; reads succeed | RA-US-16.1 | M1-B6 |
| [ ] | M1-B8 | `ListTeamModels`/`PutTeamModels` (`teams:set-guardrails`), `GetTeamTags`/`PutTeamTags` (team-owned keys only; `requestMetadataDroppedKeys` computed by `domain.TagSet.BedrockMetadata`; `team.tags_changed`) | RA-1 (part), TAG-1 (part) | M1-B3, M1-E3 |
| [ ] | M1-B9 | Deleted teams stay readable to service admins (`state = deleted`): team, revisions and audit routes | RA-US-16.3 (part) | M1-B6, M1-E4, M1-E5 |

### M1-C · Members and API keys

Owns: `internal/domain/{member,apikey}.go`, `internal/usecase/{members,apikeys}/`, `internal/adapter/postgres/{members,apikeys}_repo.go`,
`internal/adapter/httpapi/{h_members,h_apikeys,dto_members,dto_apikeys}.go`, `cmd/afs-api/wire_members.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-C1 | `ListMembers`, `AddMember`, `UpdateMember`, `RemoveMember` with `CanGrant` (team roles only; `If-Match`); events and audit | RA-1 (part), ACC-2 | — |
| [ ] | M1-C2 | `ListApiKeys` (`api-keys:read`; viewers `403`, D50), `CreateApiKey` (team role, expiry ≤ 90 days, `secretKey` once), `DeleteApiKey`; a replayed create returns metadata without `secretKey` and `Idempotent-Replayed: true`; no stored response contains `secretKey` | RA-US-9.3 (part), RA-4 (part) | M1-D1 |
| [ ] | M1-C3 | Matrix rows for every member and key operation (viewer, team-admin, service admin, security operator, team keys of each role, unbound user) | RA-US-1.3 (part) | M1-D6 |

### M1-D · Request pipeline (RA-4, RA-15)

Owns: `internal/adapter/httpapi/middleware/{idempotency,ifmatch,ratelimit,cors,deniedaudit}.go`, `internal/adapter/httpapi/matrix/` (test harness).

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-D1 | Idempotency middleware for creating `POST`s and `:verb` actions (key 1–255): replay with `Idempotent-Replayed: true` writes nothing; different body → `409 idempotency_mismatch`; concurrent duplicate → `409 idempotency_in_progress` + `Retry-After: 1`; 5xx not stored; per principal | RA-US-9.1, RA-US-9.2, RA-US-3.3, CT-API-12 | — |
| [ ] | M1-D2 | `If-Match` helpers: missing → `428`, stale → `412`, current → `200` with `ETag` + 1; PUT that creates needs none; `ETag` on every GET of a mutable resource | RA-US-6.1, CT-API-6, CT-API-13 | — |
| [ ] | M1-D3 | Rate limiter: 20 req/s, burst 40 per principal, divided by `AFS_API_REPLICAS`; `429 Throttled` + `Retry-After`; another principal unaffected | RA-US-15.1, RA-15 | — |
| [ ] | M1-D4 | CORS from `api.cors_origins` (preflight, allowed and exposed headers of contract §1, no credentials mode) | RA-US-15.2, CT-API-4 | M1-E1 |
| [ ] | M1-D5 | Denied audit: every `403` writes one audit record (actor, operation, team when known, required permission, `denied`, request id) in its own transaction; the request transaction writes nothing; cross-team `404` counted in metric `afs_api_cross_team_not_found_total` | RA-US-1.7, RA-US-6.3 (part) | — |
| [ ] | M1-D6 | Matrix harness: for every implemented operation × caller type (viewer, team-admin, service admin, security operator, team key per role, service key, workload, unbound user), expected outcome computed from `permissions.json` and the PRD §3 rule, asserting 2xx/404/403; each mutating op asserts row + audit + outbox (CT-API-8) | RA-US-1.3 (part), CT-API-8 (part) | — |

### M1-E · Service administration I

Owns: `internal/domain/{settings,idp}.go`, `internal/usecase/{settings,idp,servicetags,audit,revisions}/`, matching `postgres/*_repo.go` and `httpapi/h_*.go`, `cmd/afs-api/wire_admin1.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-E1 | `GetSettings` (every key, value, default, range) and `PutSetting` with type and range checks from `settings_gen.go`; unknown key or bad value → `400`; non-service-admin → `403`; audited | RA-US-15.3, RA-US-18.2 (part), RA-16, CT-API-15 | — |
| [ ] | M1-E2 | Identity providers: list, create, get (teams using it), update (disabling one in use → `409`), delete (in use → `409`); one default; audited; `ListSignInProviders` then lists both with the default flagged | RA-US-15.4 | M1-A5 |
| [ ] | M1-E3 | `ListServiceTags`, `PutServiceTag`, `DeleteServiceTag` (`admin:service-tags`) | RA-1 (part), TAG-1 | — |
| [ ] | M1-E4 | `ListAuditEvents` (team, CSV) and `ListAdminAuditEvents` (service, CSV) with filters and pagination | RA-1 (part), CT-API-3 (part) | — |
| [ ] | M1-E5 | `ListRevisions`, `GetRevision` | RA-1 (part) | — |

### M1-F · Reconciler engine (RC-4, RC-5)

Owns: `internal/usecase/reconcile/engine/`, `internal/adapter/postgres/{changefeed,status}.go`, `internal/adapter/k8s/{client,apply,leader,drift}.go`,
`internal/adapter/k8s/envtesting/`, `internal/adapter/k8s/testdata/crds/`, `cmd/afs-reconciler/`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-F1 | Kubernetes base: client, server-side apply (field manager `afs-reconciler`, managed labels), envtest harness with pinned CRDs (Cilium, Karpenter, ESO, Agentic Teammates OQ-7 placeholder, VolumeSnapshot) | RC-1 (part) | — |
| [ ] | M1-F2 | Leader election on a `Lease`; readiness reports leader state; failover < 30 s [envtest] | NFR Availability | M1-F1 |
| [ ] | M1-F3 | Change feed: `LISTEN afs_desired_state` (payload `<table>:<team_id>:<id>`), reconnect → poll; 30 s `generation <> observed_generation` poll; full resync every 10 min and at startup; pickup within 5 s, and by the poll with notifications blocked | RC-US-1.2, RC-4 | — |
| [ ] | M1-F4 | Engine: work queue keyed `(kind, id)`, table → controller registry, backoff retries, per-team ordering; leader-only periodic job scheduler (`Every`) | RC-4 (part) | M1-F3 |
| [ ] | M1-F5 | Status writer: `resource_status` upsert with D24 vocabulary only, `observedGeneration`, conditions; `team.status_changed`/`agent.status_changed`/`pool.status_changed` on phase change | RC-5, CT-API-7 (part), CT-API-9 (part) | M1-F4 |
| [ ] | M1-F6 | Drift: watch owned objects, revert edits and deletes within 60 s, one `drift.corrected` audit record and event, `DriftCorrected` condition | RC-US-3.1 | M1-F1, M1-F5 |
| [ ] | M1-F7 | `cmd/afs-reconciler`: wiring list, schema version check, health, readiness, metrics | CT-DB-1 (part) | M1-F2 |

### M1-G1 · Team environment, Kubernetes side (RC-1)

Owns: `internal/usecase/reconcile/team/`, `internal/adapter/k8s/render/{namespace,quota,netpol,cilium_team,secretstore,gatewayca}.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-G1a | Namespace `afs-t-<slug>`, Pod Security `restricted` labels, ResourceQuota and LimitRange from `quotas` [envtest] | RC-1 (part), TEAM-2 | M1-F1 |
| [ ] | M1-G1b | Default-deny NetworkPolicies; Cilium policies allowing DNS and `afs-gateway`; Cilium ingress policy on `afs-gateway` admitting only `afs-t-*` pods (D57) [envtest]; `[e2e]` case for RC-US-1a.4 | RC-1 (part), RC-US-1a.4 (part) | M1-F1 |
| [ ] | M1-G1c | ESO `SecretStore` bound to the team role; gateway CA bundle ConfigMap copied into the namespace | RC-1 (part) | M1-F1 |
| [ ] | M1-G1d | Team controller: compose Kubernetes and AWS ports, conditions `NamespaceReady`, `IamReady`, `Ready` with `observedGeneration = generation` within 60 s p95 [envtest]; `[e2e]` case | RC-US-1.1 (part) | M1-G1a, M1-G1b, M1-G1c, M1-G2b, M1-G2c |

### M1-G2 · Team environment, AWS side (RC-1)

Owns: `internal/adapter/aws/{config.go,awstest/,iam/,bedrock/control*.go}`, `internal/usecase/reconcile/team/aws_*.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-G2a | AWS base: config loader (Pod Identity credentials, region from env), `awstest` endpoint recorder for SDK clients | — | — |
| [ ] | M1-G2b | IAM adapter and port fake: roles `afs-<ulid>` under `/afs/<namespace>/`, permissions boundary, tags `afs-team-id`, `afs-team-slug`; team role; Bedrock role `afs-<team ulid>-bedrock` trusting only the gateway's role and allowing only the team's inference profiles; no role but the Bedrock role ever has `bedrock:*`; two teams' agents named `reviewer` get distinct id-named roles | RC-US-1.4, RC-1 (part), NFR Security | M1-G2a |
| [ ] | M1-G2c | Bedrock control adapter: one application inference profile per enabled team model, tagged, ARN written to `team_models`; removed with the model | RC-1 (part) | M1-G2a |
| [ ] | M1-G2d | IAM conditions on `aws:PrincipalTag/kubernetes-namespace`, role tags `afs-team-id` (D26) [unit] | RC-2 (part), OQ-1 | M1-G2b, M0-E1 |

### M1-H · Admission (RC-19)

Owns: `internal/usecase/reconcile/admission/`, `internal/adapter/k8s/render/admission.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-H1 | ValidatingAdmissionPolicies and bindings for `afs-t-*`: refuse NetworkPolicy/CiliumNetworkPolicy changes except by the reconciler, LoadBalancer/NodePort Services, hostNetwork/PID/IPC, hostPath, privileged, AFS label changes, another team's dedicated tolerations or selectors [envtest]; `[e2e]` case | RC-US-1a.2, RC-US-1a.1 (part), RC-19 | M1-F1 |
| [ ] | M1-H2 | Deleted policy or binding restored within 60 s with `drift.corrected` [envtest] | RC-US-1a.3 | M1-H1, M1-F6 |

### M1-I · Team lifecycle (RC-17 team part)

Owns: `internal/usecase/reconcile/lifecycle/`, `internal/adapter/aws/secretsmanager/delete.go`, `internal/adapter/aws/eks/accessentry_delete.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-I1 | Ordered delete finalizer: idempotent named steps shown as conditions while `Deleting`, a failed step retried next pass, final row `deleted` with audit and usage readable; team steps: schedule every team secret for deletion with a 30-day recovery window before the namespace goes, revoke IAM, delete inference profiles and access entry, delete namespace; hook points for agent (M2) and pool (M5) steps [envtest with fake AWS] | RC-US-15.2, RC-US-15.3, RA-US-16.4, RC-17 (part) | M1-G1d, M1-G2b |
| [ ] | M1-I2 | Team suspend and resume: record each workload's prior state and scale hooks (agents M2, pools M5) | RC-US-15.1 (part), RC-17 (part) | M1-F4 |

### M1-J · Housekeeping (RC-8)

Owns: `internal/usecase/reconcile/housekeeping/`, `internal/adapter/aws/eventbridge/putevents.go`, `internal/adapter/postgres/housekeeping.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-J1 | Outbox publisher: EventBridge `PutEvents` (source `afs`), at least once, ordered per aggregate, `attempts` with backoff; duplicate harmless; outbox lag metric [adapter with fake EventBridge] | RC-US-18.1, CT-API-9 (part) | M1-F4 |
| [ ] | M1-J2 | Partitions of `usage_events`, `agent_activity`, `external_traffic` at least one month ahead (a run on the last day of a month finds next month present); monthly `afs.drop_partitions_older_than(13)`; never touches `audit_log` | RC-US-18.2, RC-8 | M1-F4 |
| [ ] | M1-J3 | `afs.purge_idempotency_keys()` every 5 minutes | RC-US-18.4 | M1-F4 |

### M1-K · Deploy (DP-1)

Owns: `deploy/terraform/`, `deploy/helm/afs/`, `test/e2e/harness.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-K1 | Terraform modules: RDS PostgreSQL 17 (PITR, RPO 5 min, IAM auth, KMS), AWS Backup with cross-region and cross-account copies, export and reports buckets (KMS), Pod Identity roles for the four binaries (least privilege per research and contracts); `terraform validate`, `tflint` in CI; no hardcoded ids | DP-1, NFR Recovery | — |
| [ ] | M1-K2 | Helm chart: four binaries, ServiceAccounts with Pod Identity, ≥ 3 replicas of `afs-api` and `afs-gateway` spread across AZs, PDBs, gateway TLS via cert-manager, billing CronJobs, ServiceMonitor; per-component values files; `helm lint` and golden `helm template` tests | DP-1, NFR Availability | — |
| [ ] | M1-K3 | `test/e2e` harness (skips without `AFS_E2E_KUBECONFIG`), shared fixtures for later e2e cases | — | — |

### M1-L · Observability

Owns: `internal/adapter/observability/`, `internal/adapter/httpapi/middleware/metrics.go`, `deploy/helm/afs/templates/{prometheusrule,dashboards}*.yaml`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-L1 | `slog` JSON with request id; redacting handler (`Authorization`, bearer tokens, `afsk_` keys, `X-Afs-Gateway-Token`, JWT-shaped strings); OpenTelemetry tracer and meter (OTLP); Prometheus registry and `/metrics` server | NFR Observability, NFR Security | — |
| [ ] | M1-L2 | RED metrics per route; `/healthz`, `/readyz` (database, schema version, leader) for every binary | NFR Observability | M1-L1 |
| [ ] | M1-L3 | Dashboards and alerts for the NFR SLIs (API p99, reconcile lag, drift per hour, gateway reservation failures and added latency, expired reservations, outbox lag, notification failures, CUR variance) on the metric names in architecture.md | NFR Observability | M1-L2 |

### M1-X · Milestone exit

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M1-X1 | Merge, `make gate`, `make test-db`, `make envtest`; matrix harness green for implemented operations; `docs/technical-details.md` updated; status.md | — | all M1 |

---

## M2 — Agents, secrets, configuration, external systems, agent pods

Merge order: A, C, D, E, B, I, F, G, H.

### M2-A · Catalogs II: models, prices, storage classes, agent templates, catalog systems

Owns: `internal/domain/{model,catalog,extsys_catalog}.go`, `internal/usecase/{models,storageclasses,agenttemplates,catalogsystems}/`, matching repos and handlers, `cmd/afs-api/wire_catalog.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-A1 | `ListModels` (enabled, `models:read`), `ListAdminModels`, `CreateModel` (`maxOutputTokens` required), `UpdateModel` | RA-1 (part) | — |
| [ ] | M2-A2 | `ListModelPrices`, `CreateModelPrice` (`admin:pricing`, effective-dated) | RA-1 (part) | M2-A1 |
| [ ] | M2-A3 | `ListStorageClasses`, `ListStorageClassesAdmin`, `PutStorageClass`, `DeleteStorageClass` (in use → `409`) | RA-1 (part) | — |
| [ ] | M2-A4 | `ListAgentTemplates` (enabled only), `ListAdminAgentTemplates`, `PutAgentTemplate`, `DeleteAgentTemplate` (team-admin → `403`; in use → `409`) | RA-US-4.4, RA-1 (part) | — |
| [ ] | M2-A5 | Catalog systems: list, create, get, update (endpoints, service switch), delete (built-ins → `409`) | RA-1 (part), EXT-1 | — |

### M2-B · Agents API

Owns: `internal/domain/agent.go`, `internal/usecase/agents/`, `internal/adapter/postgres/agents_repo.go`, `internal/adapter/httpapi/{h_agents,dto_agents}.go`, `cmd/afs-api/wire_agents.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-B1 | `Agent` aggregate: digest image, volume grow-only, storage class create-only, no replicas [unit] | RA-US-4.1 (part), RA-US-4.2 | — |
| [ ] | M2-B2 | `CreateAgent`: model enabled for the team, storage class offered, no binding of another agent's secret, template checks (disabled, digest not allowed, unknown → `400` naming it); `agent.created` | RA-US-4.1, RA-US-4.3 (part) | M2-B1, M2-A3, M2-A4 |
| [ ] | M2-B3 | `ListAgents`, `GetAgent`, `UpdateAgent`, `DeleteAgent` (marks; reconciler finalizes); events | RA-1 (part) | M2-B2 |
| [ ] | M2-B4 | `PauseAgent`, `ResumeAgent`, `RestartAgent`, `GetAgentStatus`; events | RA-1 (part), AGT-1 | M2-B3 |
| [ ] | M2-B5 | `ListAgentSnapshots`, `CreateAgentSnapshot` (manual request row) | RA-1 (part), AGT-9 | M2-B3 |
| [ ] | M2-B6 | An agent whose template is later disabled is not changed by the API (only the reconciler flags it) [adapter] | RA-US-4.5 (part) | M2-B3 |
| [ ] | M2-B7 | Matrix rows for every agent operation | RA-US-1.3 (part) | M2-B4, M2-B5 |

### M2-C · Secrets (RA-5)

Owns: `internal/domain/secret.go`, `internal/usecase/secrets/`, `internal/adapter/aws/secretsmanager/{write,client}.go`, `internal/adapter/postgres/secrets_repo.go`,
`internal/adapter/httpapi/{h_secrets,dto_secrets}.go`, `cmd/afs-api/wire_secrets.go`, `test/fuzz/secrets/`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-C1 | Secrets Manager writer adapter and fake: `CreateSecret` (name, tags `afs-team-id`, `afs-agent-id`), `PutSecretValue`, new version for rotate; `SecretValue` type never marshals or prints | RA-5 | — |
| [ ] | M2-C2 | `CreateSecret` (team and agent scope): name from `afs.secret_path`, Secrets Manager first, then the ARN row; Secrets Manager failure → no row, `503 Unavailable` without the value | RA-US-5.1, RA-US-5.2 | M2-C1 |
| [ ] | M2-C3 | `ListSecrets`, `GetSecret`, `UpdateSecret`, `DeleteSecret` (bound → `409` listing agents), `PutSecretValue` (`204`), `RotateSecret` (`{value}` → new version, `secret.rotated`); events carry no value | RA-1 (part), CT-API-14, SEC-4 | M2-C2 |
| [ ] | M2-C4 | Fuzz test over every secret route: the value never appears in a response body, header, log line, trace attribute, audit record or event | RA-US-5.3 | M2-C3, M1-L1 |

### M2-D · Configuration values

Owns: `internal/domain/config.go`, `internal/usecase/config/`, `internal/adapter/postgres/config_repo.go`, `internal/adapter/httpapi/{h_config,dto_config}.go`, `cmd/afs-api/wire_config.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-D1 | Namespace values: list, put (creates or updates), delete (linked → `409`); `AFS_OWNED_REPOS` entries validated (`host/owner/repo` or `host/owner/*`, comma or whitespace separated) | RA-1 (part), CFG-7 | — |
| [ ] | M2-D2 | Agent values: list resolved with `source`, put `{source: literal\|namespace, value?}`, delete | RA-1 (part) | M2-D1 |

### M2-E · External systems

Owns: `internal/domain/extsys.go`, `internal/usecase/extsys/`, `internal/adapter/postgres/extsys_repo.go`, `internal/adapter/httpapi/{h_extsys,dto_extsys}.go`, `cmd/afs-api/wire_extsys.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-E1 | `ListTeamExternalSystems` (catalog + declared, team state), `SetTeamExternalSystemState` | RA-1 (part), EXT-3 | — |
| [ ] | M2-E2 | `DeclareTeamExternalSystem` (only with `mayAddExternalSystems`; allowed and denied cases), `UpdateTeamExternalSystem`, `DeleteTeamExternalSystem` (team-declared only) | RA-US-1.5 (part), EXT-2 | — |
| [ ] | M2-E3 | `ListAgentExternalSystems` (effective, `disabledAt`), `SetAgentExternalSystemState` | RA-1 (part), EXT-4 | M2-E1 |

### M2-F · Agent workloads (RC-2, TEAM-6)

Owns: `internal/usecase/reconcile/agent/`, `internal/adapter/k8s/render/{agent,agentictm,configmap,externalsecret,awsconfig,gatewayenv,nodepool}.go`,
`internal/adapter/k8s/crd/`, `internal/adapter/aws/eks/podidentity.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-F1 | Agentic Teammates custom resource (pinned version, OQ-7) rendering a 1-replica StatefulSet, ServiceAccount, Pod Identity association, volume claim; pod-template hash of referenced versions [envtest] | RC-2 (part), AGT-3 | M1-G1d |
| [ ] | M2-F2 | ConfigMap (`envFrom` + `/etc/afs/config/<NAME>`); ExternalSecrets for env and file bindings at exact targets; a secret naming an external system that is off for the agent is not delivered, and is again within 60 s of switching on [envtest] | RC-US-2.1 (part), RC-US-2.3 | M2-F1 |
| [ ] | M2-F3 | `/etc/afs/aws/config` (each profile `role_session_name` = agent id), `/etc/afs/tags.json` and `AFS_TAGS`; gateway environment and CA bundle per gateway contract §3; optional gateway token projection; `afs-api` token projection only in teams with an assigned work order; no AWS keys [envtest] | RC-US-2.1, RC-US-2.4, RC-US-4.3 (part) | M2-F1 |
| [ ] | M2-F4 | Rotating a secret or changing a config value changes the pod-template hash of the referencing agents only [envtest] | RC-US-2.2 | M2-F2 |
| [ ] | M2-F5 | Dedicated tier: Karpenter NodePool per team (taints, labels, tags) and matching tolerations and selectors on the team's pods [envtest]; `[e2e]` case | RC-US-2.6 (part), TEAM-6 | M2-F1 |
| [ ] | M2-F6 | `TemplateRetired=True` within 60 s when the template is disabled or the digest is removed, agent keeps running; cleared on re-enable [envtest] | RC-US-1.3, RA-US-4.5 (part), RC-5 (part) | M2-F1 |
| [ ] | M2-F7 | Dedicated agent IAM role (`iamIdentity = dedicated`) with tags `afs-team-id`, `afs-agent-id`; team-shared agents use the team role [adapter with fake AWS] | RC-2 (part), AGT-6 (part) | M2-F1 |
| [ ] | M2-F8 | `[e2e]` case: an image with no AFS code (`busybox env`, `cat`) receives every variable and file | RC-US-2.5 | M2-F3 |

### M2-G · Volumes, snapshots, agent lifecycle (RC-16, RC-17 agent part)

Owns: `internal/usecase/reconcile/snapshot/`, `internal/usecase/reconcile/lifecycle/agent_*.go`, `internal/adapter/k8s/render/{pvc,volumesnapshot}.go`, `internal/adapter/postgres/snapshots.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-G1 | PVC retention `Retain`; growing `sizeGiB` patches the PVC (online expansion) [envtest]; `[e2e]` data kept | RC-US-14.5 (part), RC-16 | M2-F1 |
| [ ] | M2-G2 | Snapshots via `VolumeSnapshot` on the daily or weekly schedule and on manual requests, pruned to `snapshotRetain`, recorded in `agent_volume_snapshots`, tagged [envtest with fake clock] | RC-US-14.3, RC-US-14.4 | M2-F1 |
| [ ] | M2-G3 | Pause (replicas 0, PVC kept), resume (same volume), restart (pod-template annotation) [envtest]; `[e2e]` files survive | RC-US-14.1 | M2-F1 |
| [ ] | M2-G4 | Agent delete steps in the finalizer: `snapshot_then_delete` waits for a ready snapshot then deletes, `retain` keeps and records, `delete` deletes without snapshot; Pod Identity association and dedicated role revoked [envtest] | RC-US-14.2, RC-US-15.2 (part) | M2-G2, M1-I1 |
| [ ] | M2-G5 | A missing PVC is re-created from the latest ready snapshot [envtest] | RC-US-3.2 (part), RC-16 | M2-G2 |
| [ ] | M2-G6 | Team suspend and resume for agents: all to 0 within 60 s, each back to its prior state; an agent paused before stays paused [envtest] | RC-US-15.1 (part) | M2-G3, M1-I2 |

### M2-H · Egress and traffic (RC-7 traffic)

Owns: `internal/usecase/reconcile/collector/traffic/`, `internal/usecase/reconcile/egress/`, `internal/adapter/k8s/render/cilium_agent.go`, `internal/adapter/k8s/hubble/`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-H1 | Per-agent Cilium FQDN egress from `agent_external_access`; switching a system off at service, team or agent level removes its FQDNs within 60 s without restarting pods; `EgressPolicyReady` [envtest] | RC-US-5.1, EXT-5 | M2-F1 |
| [ ] | M2-H2 | Hubble flows → `external_traffic` (connections, bytes; requests and errors empty per OQ-8) with `denied_reason`, team tag set, pool and worker pod; redaction applied before storage [adapter with recorded flows] | RC-US-5.2, RC-7 (part), TAG-6 (part) | M2-I2 |
| [ ] | M2-H3 | `egress.denied_spike` once per agent per 5-minute window above `egress.denied_spike_threshold` [unit with fakes] | RC-US-5.3 | M2-H2 |
| [ ] | M2-H4 | Connections to a system's sign-in endpoint written as sign-in events (agent or worker, system, time) to the team's log delivery only, never a table or event [adapter] | RC-US-16.5 | M2-H2, M2-I3 |

### M2-I · Log destinations and delivery (RC-18)

Owns: `internal/domain/delivery_log.go`, `internal/usecase/logdest/`, `internal/usecase/reconcile/logs/`, `internal/usecase/redact/`, `internal/adapter/k8s/render/fluentbit.go`,
`internal/adapter/postgres/logdest_repo.go`, `internal/adapter/httpapi/{h_logdest,dto_logdest}.go`, `cmd/afs-api/wire_logdest.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-I1 | Log destinations API: list, create (CloudWatch, S3, OTLP, Splunk HEC, Datadog; credentials only as a secret reference, inline value → `400`), get, update, delete; viewer create → `403` + denied audit, viewer list and describe allowed | RA-US-17.1, RA-US-17.2 (part) | — |
| [ ] | M2-I2 | Redaction library: passwords, bearer tokens, AWS keys, private keys, JWTs; bound secret values → `[redacted:<first 8 hex of HMAC-SHA-256 with a per-team key>]`, deterministic [unit] | RC-US-16.2, RC-US-16.1 (part) | — |
| [ ] | M2-I3 | Per-team Fluent Bit aggregator: routing by namespace from the node DaemonSet, one output per destination, redaction filter before every output, credentials only through an ESO Secret only the aggregator reads; outputs change within 60 s of a destination change [envtest]; `[e2e]` cases | RC-US-16.4, RC-US-16.3 (part), RC-US-16.1 (part) | M2-I1, M2-I2 |

### M2-X · Milestone exit

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M2-X1 | Merge, gate, `test-db`, `envtest`; matrix green; note in implementation-notes that pods carry gateway env before the gateway exists (plan §5); status.md | — | all M2 |

---

## M3 — Gateway, budgets, notifications, reports

Merge order: E, F, H, I, G, A, B, C, D.

### M3-A · Gateway identity and routing (GW-1, GW-3)

Owns: `cmd/afs-gateway/`, `internal/usecase/gateway/{identity,route,model}*.go`, `internal/adapter/gatewayhttp/{server,routes,errors}.go`,
`internal/adapter/k8s/{podcache,tokenreview}/`, `internal/adapter/aws/bedrock/runtime*.go`, `internal/adapter/aws/sts/assume.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-A1 | `afs-gateway` binary: TLS listener, `/healthz`, `X-Afs-Gateway-Contract-Version: 1.0.0` on every response, error body of gateway contract §7, schema version check | CT-GW-1, CT-GW-10, CT-DB-1 (part) | — |
| [ ] | M3-A2 | Pod cache: watch pods in team namespaces and Cilium endpoint identities; source IP → pod → namespace, ServiceAccount → team, agent or pool, worker pod; unknown IP → `401` [unit with fake clientset] | GW-US-6.2 (part), GW-1 | — |
| [ ] | M3-A3 | Optional `X-Afs-Gateway-Token`: TokenReview (audience `afs-gateway`), result cached ≤ 5 min, must name the source pod, else `401` [unit with fake clock] | GW-US-6.2 (part), GW-US-6.5 | M3-A2 |
| [ ] | M3-A4 | Paths `converse`, `converse-stream`, `invoke`, `invoke-with-response-stream`, `/anthropic/v1/messages`; SigV4 `Authorization` and `x-api-key` ignored and never logged; agent `requestMetadata` replaced; unsupported → `404`; body > 20 MB → `413`; malformed → `400`; model not enabled → `403` recorded `rejected_model`; suspended team → `403 team_suspended`; deleted team's former IP → `401` | GW-US-2.3, GW-US-6.3, CT-GW-2, CT-GW-3, CT-GW-7 | M3-A2 |
| [ ] | M3-A5 | Bedrock runtime adapter: assume the team's Bedrock role (cached credentials), call through the team's inference profile, TLS to the VPC endpoint only; a pod of team A can never use team B's profile [unit] | GW-US-2.2, GW-3 (part) | M3-A4 |

### M3-B · Reservation, settlement, usage, thresholds (GW-2, GW-3, GW-4, GW-7)

Owns: `internal/usecase/gateway/{reserve,settle,usage,threshold,cache}*.go`, `internal/adapter/postgres/spend.go`, `internal/adapter/postgres/usage_writer.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-B1 | Reservation math: `maxTokens` else `max_output_tokens`, capped to it; input bytes ÷ 3; prices effective at call time [unit] | GW-US-1.4, CT-GW-4, GW-2 | — |
| [ ] | M3-B2 | Spend adapter: `ensure_spend_counter` then `reserve_spend_held(holder = pod)`; NULL → `402 BudgetExceeded` with `budgetId`, `period`, `remainingUsd`; `settle_reservation(id, budget_id, period_start, actual)` always with the reserved budget and period; every reservation settled on success, error or abort | GW-US-6.4, RC-US-12.1 (part), CT-GW-9, GW-7 | M3-B1 |
| [ ] | M3-B3 | Fail closed: database or reservation unavailable → `503` within 2 s, nothing sent to Bedrock [adapter] | GW-US-1.2, GW-3 | M3-B2, M3-A4 |
| [ ] | M3-B4 | Usage writer: batched `COPY` within 1 s; `tag_set_id` current at the call (new set within 60 s of a tag change, old rows keep theirs), pool and worker pod, latency, outcomes `ok`, `error`, `rejected_budget`, `rejected_model`; no prompt or completion content stored or logged | GW-US-2.4, GW-US-2.5, GW-US-3.1, CT-GW-8 | M3-B2 |
| [ ] | M3-B5 | Thresholds: insert-once `budget_threshold_events` → one `budget.threshold_crossed` per threshold per period; one `budget.exhausted` on the first refusal at 100% (200 concurrent callers) [adapter] | GW-US-4.1, GW-4 | M3-B2 |
| [ ] | M3-B6 | Upstream errors: Bedrock throttling → `429` with `Retry-After`; Bedrock 5xx → `502 UpstreamError` and the reservation released | CT-GW-5 | M3-B2, M3-A5 |
| [ ] | M3-B7 | Caches (team state, models, prices, budgets, tags) invalidated by `LISTEN afs_desired_state` and a 60 s TTL | GW-US-3.1 (part), A-8 | M3-B2 |

### M3-C · Streaming (GW-6)

Owns: `internal/adapter/gatewayhttp/{eventstream,sse}.go`, `internal/usecase/gateway/stream.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-C1 | `ConverseStream` and `InvokeModelWithResponseStream`: forward events as they arrive, usage from the final metadata event, settle [adapter with fake Bedrock] | GW-US-5.1 (part), GW-6 | M3-A4, M3-B2, M3-D1 |
| [ ] | M3-C2 | Anthropic Messages SSE with `message_delta.usage` | GW-US-5.1 (part) | M3-C1 |
| [ ] | M3-C3 | Client abort or upstream error mid-stream settles on tokens seen and releases the rest; no usage reported → settles to the reservation; errors before the first event are JSON, after it an `exception` (Bedrock) or `error` (Anthropic) event with the same code | GW-US-5.2, GW-US-5.3, CT-GW-6 | M3-C1, M3-C2 |

### M3-D · Gateway conformance and load

Owns: `internal/adapter/aws/bedrock/fakebedrock/`, `test/conformance/`, `test/load/gateway/`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-D1 | Fake Bedrock: Converse, ConverseStream, InvokeModel, InvokeModelWithResponseStream with usage, throttle and 5xx modes | — | — |
| [ ] | M3-D2 | Conformance suite (AWS SDK Bedrock Runtime client and the Anthropic SDK, only the gateway contract §3 environment): every path, streaming, every §7 error, reservation math; calls attributed to the agent, signature and key ignored [conformance suite] | GW-US-6.1, CT-GW-2 (part) | M3-A4, M3-C3 |
| [ ] | M3-D3 | Load: 200 agents at 500 req/s against a nearly exhausted budget: overshoot at most one in-flight request per agent, every rejection `402`; added latency p99 < 25 ms [load] | GW-US-1.1, GW-US-1.3 | M3-B5, M3-D1 |

### M3-E · Budgets API (RA-14)

Owns: `internal/domain/budget.go`, `internal/usecase/budgets/`, `internal/adapter/postgres/budgets_repo.go`, `internal/adapter/httpapi/{h_budgets,dto_budgets}.go`, `cmd/afs-api/wire_budgets.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-E1 | `Budget` aggregate: amount ≤ effective ceiling, `notify` only persistent and only with `teams:set-guardrails`, lifetime not removable; forecast (linear) and `projectedCapAt` (absent when not within the period) [unit] | RA-US-7.5, RA-14 | — |
| [ ] | M3-E2 | `GetBudget` with spent, reserved, percent, forecast, projected cap | RA-1 (part) | M3-E1 |
| [ ] | M3-E3 | `PutBudget`: above the ceiling → `400`; up to an unexpired override allowed for a team-admin; team-admin setting the override → `403`; `notify` on ephemeral → `400`, by team-admin → `403`, by service admin → `200` | RA-US-13.1, RA-US-13.2, RA-US-13.3 | M3-E1 |

### M3-F · Budget reconciler (RC-6, RC-14)

Owns: `internal/usecase/reconcile/budget/`, `internal/adapter/postgres/budget_jobs.go`, `internal/adapter/aws/iam/deny.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-F1 | Period opening: `ensure_spend_counter` for every budget by the first second of the period; `budget.reset` once | RC-US-12.1, RC-14 | — |
| [ ] | M3-F2 | Budget change carried into `limit_usd` within 60 s | RC-US-12.2 (part) | M3-F1 |
| [ ] | M3-F3 | Sweeper every minute: `release_expired_reservations()`, one audit record per non-zero release, alert metric; a killed gateway's hold released within TTL + 2 min [adapter] | RC-US-12.4, GW-7 (part) | M3-F1 |
| [ ] | M3-F4 | Ceiling override expiry: clear, lower a budget above the base ceiling, `budget.ceiling_override_expired` once | RC-US-12.5 | M3-F1 |
| [ ] | M3-F5 | Backstop: explicit deny on the team's Bedrock role and `BudgetExhausted=True` within 60 s of exhaustion; both removed within 60 s of reset, increase or override [adapter with fake IAM] | RC-US-12.2, RC-US-12.3, RC-6 | M3-F2 |

### M3-G · Notifications (RC-15)

Owns: `internal/domain/delivery_channel.go`, `internal/usecase/notify/`, `internal/usecase/reconcile/notify/`, `internal/adapter/aws/{eventbridge/rules.go,sqs/,ses/,secretsmanager/read.go}`,
`internal/adapter/postgres/channels_repo.go`, `internal/adapter/httpapi/{h_channels,dto_channels}.go`, `cmd/afs-api/wire_channels.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-G1 | Team channels API: list, create (Slack, email, webhook, Jira, EventBridge; credentials as secret reference), get (with delivery status), update, delete; viewer create → `403` + denied audit, list and describe allowed | RA-US-17.2 (part), OPS-3 | — |
| [ ] | M3-G2 | Service channels API (`admin:settings`): list, create, update, delete; deleting the channel named in `work_orders.notification_channel_id` → `409` | RA-US-15.6 (part) | M3-G1 |
| [ ] | M3-G3 | Reconciler: each channel → one EventBridge rule filtered on team id and event types (plus `report.*` events addressed to the channel, OQ-S2) with an API destination and connection (credential read in memory only, OQ-S3), or for email an SQS target drained and sent with SES once per event id; removed within 60 s of delete; a team's rule never matches another team's events | RC-US-13.1, RC-US-13.2, RC-US-13.4, RC-15 | M3-G1 |
| [ ] | M3-G4 | Delivery failures retried and shown on the channel status (`DeliveryHealthy`), `notification.delivery_failed`; `TestNotificationChannel` per OQ-S1 returns the failure and records it [adapter with fake Slack] | RC-US-13.3, RA-US-17.3 | M3-G3 |

### M3-H · Reports (RA-6)

Owns: `internal/domain/report.go`, `internal/usecase/reports/`, `internal/adapter/postgres/reports_repo.go`, `internal/adapter/httpapi/{h_reports,dto_reports}.go`, `cmd/afs-api/wire_reports.go`, `test/load/api/`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-H1 | Report query parsing; ranges over two days read daily views, shorter ranges 5-minute tables [unit] | RA-US-7.2 (part) | — |
| [ ] | M3-H2 | `GetUsage`, `GetTeamReport`, `GetAgentReport`, `GetServiceReport` (usage, costs, activity, traffic): totals equal the underlying sums; `groupBy=tag:<key>` one row per value plus `(untagged)`; `poolId` filter and `groupBy=pool`; traffic without cost; `notes`; viewer of A → `404` for B, `403` for service reports; deleted team readable by service admins | RA-US-7.1, RA-US-7.4, RA-US-7.6, RA-US-16.3 (part), RA-6 | M3-H1 |
| [ ] | M3-H3 | `Accept: text/csv` returns the same rows as JSON, `X-Afs-Next-Token` | RA-US-7.3, CT-API-3 (part) | M3-H2 |
| [ ] | M3-H4 | Report p99 < 1 s for 90 days at the initial scale [load] | RA-US-7.2 (part) | M3-H2 |

### M3-I · Activity collector (RC-7 activity)

Owns: `internal/usecase/reconcile/collector/activity/`, `internal/adapter/k8s/{metrics,agentstatus}.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-I1 | One sample per running agent (and pool worker) per 5-minute window: working when it made a model call, reported busy, or CPU above `reports.idle_cpu_millicores`; tag set stamped; status endpoint fallback (OQ-7) [unit with fakes] | RC-US-6.1, RC-7 (part) | — |

### M3-X · Milestone exit

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M3-X1 | Merge, gate, `test-db`, `envtest`, `conformance`; load results recorded; OQ-3 answer recorded or default kept; status.md | — | all M3 |

---

## M4 — Accounts, tags, billing, pricing, scheduled reports, export, import, restore

Merge order: A, B, C, D, E, F, G.

### M4-A · Identities, role links, accounts (RC-3, RC-8 org mirror)

Owns: `internal/domain/identity.go`, `internal/usecase/{identity,accounts}/`, `internal/usecase/reconcile/links/`, `internal/usecase/reconcile/housekeeping/orgmirror.go`,
`internal/adapter/aws/{organizations/,sts/probe.go}`, matching repos and handlers, `cmd/afs-api/wire_identity.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M4-A1 | Organizations account mirror hourly; a removed account shows in the next run [adapter with fake Organizations] | RC-US-18.3, TEAM-8 | — |
| [ ] | M4-A2 | `ListOrgAccounts` (`admin:accounts`), `ListAllowedAccounts` (`identities:read`) | RA-1 (part) | M4-A1 |
| [ ] | M4-A3 | Agent role links (list, create, get with `trustPolicy`, delete, verify) and agent identities (list, create, delete) | RA-1 (part), AGT-6, AGT-10 | — |
| [ ] | M4-A4 | Links reconciler: `sts:AssumeRole` on the exact ARN, profile with `role_session_name` = agent id, status `pending_trust` → `verified` after an assume with external ID and session tags, or `failed` with the AWS error code; on create, daily and on demand; `role_link.verified`/`failed` [adapter with fake STS] | RC-US-4.1, RC-US-4.3, RC-3 | M4-A3 |
| [ ] | M4-A5 | Narrowing the allowed-accounts filter disables links outside it on the next pass | RC-US-4.2 | M4-A4 |

### M4-B · Tags (RC-9, GW-5)

Owns: `internal/usecase/reconcile/tags/`, `internal/usecase/gateway/metadata.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M4-B1 | Effective tags on every managed AWS resource (roles, profiles, queues, snapshots, secrets, NodePools); changes within 10 minutes; `afs-team-id`/`afs-agent-id` always present; above 40 keys refused before any AWS call, resource `Degraded` with a condition naming the limit | RC-US-7.1, RC-US-7.2, RC-9 | — |
| [ ] | M4-B2 | Gateway `requestMetadata`: at most 16 pairs in the D27 order including `afs-team-id` and `afs-agent-id`, dropped keys recorded (and exposed by `GetTeamTags`); refresh on notification, ≤ 60 s [adapter with fake Bedrock] | GW-US-2.1, GW-5 | — |

### M4-C · CUR and reconciliation (BI-1, BI-2, BI-7)

Owns: `cmd/afs-billing/`, `internal/usecase/billing/{cur,reconcile}/`, `internal/adapter/aws/athena/`, `internal/adapter/postgres/billing.go`, `internal/usecase/costs/`, `internal/adapter/httpapi/{h_costs,dto_costs}.go`, `cmd/afs-api/wire_costs.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M4-C1 | `afs-billing` binary: jobs selected by flag, schema version check, health, metrics | CT-DB-1 (part) | — |
| [ ] | M4-C2 | CUR ingest via Athena: Bedrock by inference-profile tag, EKS split cost by namespace (incl. dedicated nodes), EBS volumes and snapshots by `afs-team-id` → `cost_daily` [adapter with fixture CUR] | BI-US-1.1 (part), BI-1 | M4-C1 |
| [ ] | M4-C3 | Tag sets from CUR line item tags; a day with a tag change split by line items; pool workers carry the pool | BI-US-6.1, BI-7 | M4-C2 |
| [ ] | M4-C4 | Reconciliation per team-day: `over_threshold` above `billing.variance_alert_pct`, one `cost.variance_detected` | BI-US-1.1, BI-2 | M4-C2 |
| [ ] | M4-C5 | `ListCosts` (team, CSV) and `ListCostReconciliations` (`admin:reports`); deleted team costs readable by service admins | RA-1 (part), RA-US-16.3 (part) | M4-C2 |

### M4-D · Pricing (BI-3, RA-7)

Owns: `internal/usecase/billing/prices/`, `internal/usecase/pricing/`, `internal/adapter/aws/pricing/`, matching repo and handlers, `cmd/afs-api/wire_pricing.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M4-D1 | Price List sync when `pricing.price_list_sync` is on: one pending proposal per changed model, older pending superseded, nothing when off; `pricing.proposal_created` [adapter] | BI-US-2.1, BI-3 | M4-C1 |
| [ ] | M4-D2 | `ListModelPriceProposals`, `ApproveModelPriceProposal`, `RejectModelPriceProposal` (service admins only); `pricing.proposal_decided`; effective from `max(effectiveFrom, approvedAt)`, no earlier usage repriced; the gateway uses the new price after the effective time [adapter]; `[e2e]` case | RA-US-8.1, RA-US-8.2, RA-7 | — |

### M4-E · Showback and scheduled reports (BI-4, BI-5, RA-6 schedules)

Owns: `internal/usecase/billing/{showback,schedules}/`, `internal/usecase/reportschedules/`, `internal/adapter/aws/s3/`, matching repo and handlers, `cmd/afs-api/wire_schedules.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M4-E1 | Showback CSV per month: one row per cost center and per value of a chosen tag key, totals equal `cost_daily`, `(untagged)` row; written to the reports bucket | BI-US-5.1, BI-4 | M4-C3 |
| [ ] | M4-E2 | Report schedules API: list (`reports:read`), create, update, delete (`reports:schedule`); the team's own channel only (service-level or another team's channel → `400`); viewer → `403` | RA-US-7a.1, RA-US-7a.2, RA-US-15.6 (part) | — |
| [ ] | M4-E3 | Schedule runner: due schedules produce a CSV equal to the on-demand report, stored and announced per OQ-S2 (`report.delivered`), `next_run_at` advances; failure records `failed` with the error (`report.failed`) and retries next cycle | BI-US-3.1, BI-5 | M4-E2 |

### M4-F · Export and import (RA-12, RC-20)

Owns: `internal/domain/export.go`, `internal/usecase/{exports,imports}/`, `internal/usecase/reconcile/export/`, `internal/adapter/aws/s3/bundle.go`, matching repo and handlers, `cmd/afs-api/wire_exports.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M4-F1 | Bundle model: `schemaVersion`, canonical JSON, SHA-256, contents of root CFG-5; never a secret value, key hash or client secret [unit] | RA-US-14.1 (part) | — |
| [ ] | M4-F2 | `CreateExport` (`202`, `pending`), `GetExport`, `ListExports`; non-service-admin → `403` | RA-US-14.7 (part), RA-12 | M4-F1 |
| [ ] | M4-F3 | Bundle builder: KMS-encrypted S3 object, `sha256`, `size_bytes`, `s3_uri`, 7-day pre-signed `download_url` and `download_expires_at`, `succeeded`; failure → `failed` with summary, no checksum; `export.completed`/`export.failed`; checksum matches content [adapter with fake S3] | RC-US-17.1, RC-US-17.2, RC-US-17.3, RA-US-14.1, RC-20 | M4-F2 |
| [ ] | M4-F4 | `CreateImport` dry run (default): per-team create/update/skip, secrets needing values, nothing written; checksum mismatch or body over 100 MB → `400` | RA-US-14.2, RA-US-14.4 | M4-F1 |
| [ ] | M4-F5 | `CreateImport` apply: one transaction per team, account map, one audit per resource, role links `pending_trust`, a failed team reported with nothing written; `import.applied`; no S3 call [unit] | RA-US-14.3, RA-US-14.5 | M4-F4 |

### M4-G · Restore (RA-13)

Owns: `internal/domain/revision.go`, `internal/usecase/restore/`, `internal/adapter/postgres/restore.go`, `internal/adapter/httpapi/h_restore.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M4-G1 | `RestoreRevision` dry run: diff, nothing written | RA-US-14.6 (part) | — |
| [ ] | M4-G2 | Apply in one transaction (team settings, agents, bindings, config values, external system switches, tags, budget); `generation` + 1; one audit naming the source revision; a binding to a missing secret skipped and reported; viewer → `403` | RA-US-14.6, RA-US-14.7 (part), RA-13 | M4-G1 |

### M4-X · Milestone exit

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M4-X1 | Merge, gate, `test-db`, `envtest`; switch to the `-db` tag if published; status.md | — | all M4 |

---

## M5 — Budgeted workloads and swarms

Merge order: A, B, F, E, C, D.

### M5-A · Ephemeral teams (RA-8 team part)

Owns: `internal/usecase/teams/ephemeral*.go` (new files in M1-B's package, coordinated by this stream only).

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M5-A1 | `CreateTeam` with `kind: ephemeral`: needs expiry, teardown rule and lifetime budget (`400` otherwise); expiry beyond `teams.ephemeral_max_days` → `400`; team and budget in one transaction | RA-US-10.1, RA-8 | — |
| [ ] | M5-A2 | Ephemeral → persistent, or removing the lifetime budget → `400` | RA-US-10.2 | M5-A1 |

### M5-B · Pools API (RA-8 pools)

Owns: `internal/domain/pool.go`, `internal/usecase/pools/`, `internal/adapter/postgres/pools_repo.go`, `internal/adapter/httpapi/{h_pools,dto_pools}.go`, `internal/usecase/secrets/pool_scope.go`, `cmd/afs-api/wire_pools.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M5-B1 | Stages: list, create, update, delete | RA-1 (part) | — |
| [ ] | M5-B2 | Pools: list, create, get, update, delete; persistent team → `409` checked before the write; name like an agent → `409`; template checks for `CreatePool`; `pool.created/updated/deleted` | RA-US-10.3 (part), RA-US-4.3 (part) | — |
| [ ] | M5-B3 | `ScalePool` (0–200, else `400`; `pool.scaled`), `PausePool`, `ResumePool`, `GetPoolStatus` (desired, reporting, working, idle, impaired) | RA-US-10.3 (part) | M5-B2 |
| [ ] | M5-B4 | Handoffs (list, put), pool secret bindings (list, put), `CreateSecret` with `scope: pool` (4-argument `secret_path`) | RA-1 (part) | M5-B2 |
| [ ] | M5-B5 | Pool config values (list resolved, put, delete) and pool external systems (list, put) | RA-1 (part) | M5-B2 |
| [ ] | M5-B6 | Pool role links (list, create, delete, verify): account outside the allowed accounts or a role name other than the pool's → `400` | RA-US-10.4 | M5-B2 |
| [ ] | M5-B7 | Viewers read pools and get `403` on every pool mutation; matrix rows | RA-US-10.5, RA-US-1.3 (part) | M5-B3, M5-B4, M5-B5, M5-B6 |

### M5-C · Pool workloads (RC-11, RC-17 pool part)

Owns: `internal/usecase/reconcile/pool/`, `internal/adapter/k8s/render/pool.go`, `internal/adapter/aws/sqs/queue_policy.go`, `internal/usecase/reconcile/lifecycle/pool_*.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M5-C1 | One Deployment per pool with `workers` replicas, own ServiceAccount and Pod Identity role, secrets, ConfigMap, egress policy, scratch volume, optional EFS; scaling changes replicas only [envtest] | RC-US-9.1, RC-11 | M5-B2 |
| [ ] | M5-C2 | Inbox queue `afs-<team ulid>-<short hash>` per receiving pool; `AFS_INBOX_QUEUE_URL` and `AFS_HANDOFF_<TO_POOL>_QUEUE_URL`; only senders' roles may send, only the receiver may receive [adapter with fake AWS] | RC-US-9.2 | M5-C1 |
| [ ] | M5-C3 | Shared named role linked and verified in each listed account | RC-US-9.3 | M5-C1 |
| [ ] | M5-C4 | Pool pause and resume; team suspend and resume for pools (prior state kept); pool delete steps (volumes, queues) in the finalizer [envtest] | RC-US-15.1 (part), RC-US-15.2 (part), RC-17 (part) | M5-C1 |
| [ ] | M5-C5 | Pool usage, activity and traffic rows carry the pool and worker pod (collector and gateway tests extended) | GW-US-2.4 (part), RC-7 (part), AGT-8 | M5-C1 |

### M5-D · Ephemeral teardown (RC-10)

Owns: `internal/usecase/reconcile/teardown/`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M5-D1 | `teams_due_for_teardown` → RC-17 delete flow within 5 minutes; `torn_down_at` and reason; `team.teardown_started`, `team.torn_down`; revisions, usage and audit kept [envtest, adapter] | RC-US-8.1, RC-10 | M5-C4 |

### M5-E · Viewer kubectl (RA-10, RC-13)

Owns: `internal/usecase/kubeconfig/`, `internal/usecase/reconcile/kubectl/`, `internal/adapter/aws/eks/accessentry.go`, `internal/adapter/httpapi/h_kubeconfig.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M5-E1 | `GetTeamKubeconfig`: off → `403`; on → YAML using the `aws eks get-token` exec plugin, no token (OQ-S5) | RA-US-12.1, RA-10 | — |
| [ ] | M5-E2 | Access entry for the provider's federated role (configured per provider, OQ-9) mapped to a namespace read-only Role (no secrets, exec, attach, port-forward); removed within 60 s when off; none without a configured role; `[e2e]` case | RC-US-11.1, RC-13 | — |

### M5-F · Cost allocation tag status (RA-11, BI-6)

Owns: `internal/usecase/billing/costtags/`, `internal/usecase/costtags/`, `internal/adapter/aws/costexplorer/`, matching repo and handler.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M5-F1 | Daily: keys seen in CUR and AWS activation status; a key active 3 days but not seen → one `cost_tags.not_seen` [adapter with fake Billing API] | BI-US-4.1, BI-6 | — |
| [ ] | M5-F2 | `ListCostTagStatus` for service admins (team-admin → `403`); `PutSetting billing.tag_activation_acknowledged = true` audited, non-boolean → `400` | RA-US-18.1, RA-US-18.2, RA-11 | — |

### M5-X · Milestone exit

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M5-X1 | Merge, gate, `test-db`, `envtest`; OQ-9 answer recorded or default kept; status.md | — | all M5 |

---

## M6 — CVE remediation and feature complete

Merge order: A, B, C, D, E, then F.

### M6-A · Security operators, service keys, team directory (RA-9 part)

Owns: `internal/usecase/secops/`, `internal/usecase/apikeys/service*.go`, `internal/usecase/teams/directory.go`, matching repos and handlers, `cmd/afs-api/wire_secops.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M6-A1 | `ListSecurityOperators`, `AddSecurityOperator`, `RemoveSecurityOperator` (`admin:settings`); a team-admin adding one → `403` | RA-US-11.10 | — |
| [ ] | M6-A2 | `ListServiceApiKeys`, `CreateServiceApiKey` (role `security_operator` only, secret once, replay without `secretKey`), `DeleteServiceApiKey` | RA-US-9.3 (part), RA-US-15.5 (part) | — |
| [ ] | M6-A3 | `ListTeamDirectory` (`teams:read-directory`: id, slug, display name of every non-deleted team) | RA-US-11.1 (part), RA-1 (part) | — |
| [ ] | M6-A4 | Security operator access: `403` on every team-scoped route for existing and unknown teams; a service key may call the work order operations, swarm list/get, directory and `GetMe` and nothing else; `GetMe` for operators | RA-US-1.4, RA-US-15.5, RA-US-3a.2 (part) | M6-A2 |

### M6-B · Swarm templates

Owns: `internal/domain/swarmtemplate.go`, `internal/usecase/swarmtemplates/`, matching repo and handler.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M6-B1 | List and get (`swarm-templates:read`), create, update, enable, disable, delete (`admin:swarm-templates`; delete while an open work order uses it → `409`); a security operator creating or changing one → `403` | RA-US-11.1 (part), RA-9 | — |

### M6-C · Work orders API (RA-9, D38)

Owns: `internal/domain/workorder.go`, `internal/usecase/workorders/`, `internal/adapter/postgres/workorders_repo.go`, `internal/adapter/httpapi/{h_workorders,dto_workorders}.go`, `cmd/afs-api/wire_workorders.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M6-C1 | `WorkOrder` aggregate: statuses, `pr_opened` needs a PR link, severity, SLA state [unit] | RA-US-11.9 (part) | — |
| [ ] | M6-C2 | `CreateWorkOrder` owner resolution through `afs.resolve_repo_owners`: exact beats wildcard (`ownerSource: team_config`), two teams at the best match → `400 ambiguousRepos` with both ids, none → `400 unownedRepos`, explicit owner wins (`explicit`), deleted teams skipped, unknown explicit team → `400 fields`; nothing written on error | RA-US-11.2, RA-US-11.3, RA-US-11.4, RA-US-11.5 | M6-C1 |
| [ ] | M6-C3 | Second open work order for a CVE → `409`; a disabled swarm template → `400`, its revisions readable and existing teams unchanged; `work_order.created` | RA-US-11.6, RA-US-11.7 | M6-C2, M6-B1 |
| [ ] | M6-C4 | `ListWorkOrders` (filters, `teamId`, CSV; all for operators and service admins, assigned or own repos for teams), `GetWorkOrder`, `UpdateWorkOrder`, `CancelWorkOrder`, `ListWorkOrderRepos`, `ListTeamWorkOrders`; team-admins read only; an unrelated team → `404`; security operators can create, update, cancel, list and filter | RA-US-11.8, RA-US-11.1 (part) | M6-C2 |
| [ ] | M6-C5 | `UpdateWorkOrderRepo` with `work-orders:report` on the assigned team or `work-orders:write`; `pr_opened` without a PR link → `400`; `work_order.repo_updated`, `work_order.pr_opened` | RA-US-11.9 | M6-C4 |

### M6-D · Workload principal (RA-2b)

Owns: `internal/adapter/oidc/cluster.go`, `internal/usecase/auth/workload*.go`, `internal/adapter/postgres/workload_repo.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M6-D1 | Offline validation against the cluster issuer JWKS (cached like the IdP keys): audience `afs-api`, expiry ≤ 1 h; audience `afs-gateway`, expired, or another cluster's issuer → `401`; works with the Kubernetes API unreachable [unit with fake JWKS] | RA-US-2b.3, RA-US-2b.4 | — |
| [ ] | M6-D2 | Namespace and ServiceAccount → agent or pool through the database; principal `type: workload`; `GetMe`; audit with agent or pool id and worker pod | RA-US-3a.1 (part), RA-2b | M6-D1 |
| [ ] | M6-D3 | A pool token of team A with an assigned work order can `UpdateWorkOrderRepo` on it and call `GetMe`; `404` on unassigned work orders and team B routes; `403` on other team A routes and service routes | RA-US-2b.1, RA-US-2b.2 | M6-D2, M6-C5 |

### M6-E · Work order reconciler (RC-12)

Owns: `internal/usecase/reconcile/workorder/`, `internal/adapter/github/`, `internal/adapter/aws/sqs/intake.go`.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M6-E1 | Intake: drain the EventBridge intake queue, one work order per `source_event_id` even when delivered twice [adapter] | RC-US-10.1 | — |
| [ ] | M6-E2 | Remediation team from the template within 2 minutes: lifetime budget = cap, expiry = TTL, rule `at_work_complete`, stages, pools, handoffs, secrets and config from the template; `work_order.assigned` [envtest] | RC-US-10.2 | — |
| [ ] | M6-E3 | GitHub adapter (OQ-5, OQ-S4): App JWT, installation token, PR state with conditional requests; 500 repositories every 10 minutes stay under 5,000 requests per hour per installation [adapter with recorded GitHub] | RC-US-10.4 | — |
| [ ] | M6-E4 | Tracking: repositories to `pr_opened` and `merged`; all merged, not affected or skipped → work order done and team torn down; failed and cancelled; `work_order.merged`, `completed`, `failed`, `cancelled` | RC-US-10.3 | M6-E3, M6-E2 |
| [ ] | M6-E5 | Owners notified once per event and recipient through RC-15 (assigned, PR opened, merged, SLA warning `work_orders.sla_warning_hours` before due, SLA breached, completed); outside contacts emailed through `work_orders.notification_channel_id`, nothing when unset | RC-US-10.5, RC-12 | M6-E4 |

### M6-F · Feature-complete verification (last)

Owns: `test/e2e/**`, `test/load/**`, `internal/domain/events_coverage_test.go`, `implementation-notes.md` results section.

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M6-F1 | No placeholder route remains; `test/contract` replays every OpenAPI example; every §7 event type has an emission test (`events_coverage_test.go`); every response carries `X-Afs-Contract-Version` | CT-API-7, CT-API-9, RA-US-6.5, RA-1 | all M6 |
| [ ] | M6-F2 | Full matrix: every operation × every caller type | RA-US-1.3, CT-API-8 | M6-F1 |
| [ ] | M6-F3 | Run the `[e2e]` suite on the dev cluster: RC-US-1.1, RC-US-1a.1, RC-US-1a.4, RC-US-2.5, RC-US-2.6, RC-US-11.1, RC-US-14.1, RC-US-14.5, RC-US-16.1, RC-US-16.3, RA-US-8.1; record results | (listed) | M6-F1 |
| [ ] | M6-F4 | Restore drill: fresh cluster and a database restored to a point in time converge with volumes from snapshots, no manual step, within 1 hour | RC-US-3.2, NFR Recovery | M6-F3 |
| [ ] | M6-F5 | Load at initial scale: API reads p99 < 300 ms, reports < 1 s, gateway rerun, reconcile lag p95 < 60 s for 200 teams and 2,000 agents | RA-US-7.2, GW-US-1.1, GW-US-1.3, NFR Performance | M6-F3 |
| [ ] | M6-F6 | OWASP ASVS L2 checklist, `govulncheck`, coverage ≥ 90% on `domain`, `usecase`, `authz` | NFR Security, NFR Quality | M6-F2 |

### M6-X · Milestone exit

| ✓ | ID | Task | AC / Req | Depends |
|---|---|---|---|---|
| [ ] | M6-X1 | Merge, full gate and suites; status.md at 100% for Phase 5; hand over to step 4 (documentation) | — | all M6 |
