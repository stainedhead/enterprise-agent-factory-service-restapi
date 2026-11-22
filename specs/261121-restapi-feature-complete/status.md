# Status: restapi-feature-complete

| | |
|---|---|
| **Feature** | restapi-feature-complete |
| **Created** | 2026-11-21 |
| **Spec** | `specs/261121-restapi-feature-complete/` |
| **Branch** | `feat/restapi-feature-complete` |
| **Dashboard** | [restapi-feature-complete-status.md](restapi-feature-complete-status.md) |

**MANDATORY:** update this file after every task and every phase.

## Overall progress

| Phase | Name | Status | Notes |
|---|---|---|---|
| 0 | Initial research (PRD) | Complete | PRD reviewed twice before this run |
| 1 | Specification (`spec.md`) | Complete | Reviewed in step 2 |
| 2 | Research and data modeling | Complete | research.md, data-dictionary.md |
| 3 | Architecture and planning | Complete | architecture.md, plan.md |
| 4 | Task breakdown | Complete | tasks.md: 60 streams, 255 tasks |
| 5 | Implementation (M0–M6) | In Progress | M0 complete 2026-11-21; M1 complete 2026-11-22 (step 3 started 2026-11-21T20:57:16Z); M2 next. Task progress in tasks.md |
| 6 | Completion and archival | Not Started | |

## Phase 0 checklist

- [x] Spec directory created and PRD moved in (`create-spec`)
- [x] Research questions identified (research.md §1)
- [x] Phase files initialized (spec, research, data-dictionary, architecture, plan, tasks, implementation-notes)

## Milestones (Phase 5)

| Milestone | Status | Tasks done |
|---|---|---|
| M0 Contract and foundation | Complete | 41 / 41 |
| M1 Teams and core | Complete | all M1 tasks (see tasks.md; e2e cases G1b, G1d, H1, RC-US-1a.4 and terraform/helm/tflint validation deferred to the cluster suite and CI) |
| M2 Agents | Not Started | 0 |
| M3 Gateway and budgets | Not Started | 0 |
| M4 Accounts, tags, billing | Not Started | 0 |
| M5 Swarms | Not Started | 0 |
| M6 CVE remediation and completion | Not Started | 0 |

## Blockers

- None blocking M1. X-DB-1..6 were delivered by `-db` PR #12 (merged at 23e9240, migrations 000015–000016)
  and adopted during M0.
- Open request **X-DB-7** to `-db`: SELECT on `public.afs_schema_migrations` (or a SECURITY DEFINER
  `afs.schema_version()`) for `afs_reconciler`, `afs_gateway`, `afs_billing`, so they can refuse to start on an
  older schema (database contract §1). Needed before the reconciler, gateway and billing binaries start against a
  database (M1-M3); `afs-api` is unaffected.
- Root contract PR with the delta list CD-1..CD-10 (implementation-notes.md) not opened yet: left to the
  orchestrator.

## Recent activity

- 2026-11-21: spec created by `create-spec` (step 1 of `implm-frm-prd`).
- 2026-11-21: `review-spec` (step 2): gaps fixed; Phases 0–4 complete; implementation-ready.
- 2026-11-21: step 3 started; M0-S1..S4 done (`-db` b0d8754 adopted, domain primitives, archtest, Makefile and CI targets).
- 2026-11-21: M0-A1..A7 (contractgen, generated catalog/routes/events/settings, openapibundle, contract test), M0-B1a..B4b (full `api/openapi.yaml` 1.1.0, 182 operations, examples validated), M0-C1..C4 (authz rewrite, 99% coverage) done. `-db` bumped to 23e9240 (PR #12: migrations 000015–000016, X-DB-1..6).
- 2026-11-21: M0-F7 done: cross-cutting ports (`internal/usecase/port`) and transactional fakes (`port/fakes`), incl. `CredentialLookup`/`TeamDirectory` for the X-DB-1..3 functions.
- 2026-11-21: M0-F1..F6, F8 done: `pgtest` (fresh `afsr_` database and LOGIN users per package), pool, owner migration under the advisory lock, schema check, `TxRunner` (SET LOCAL session, READ COMMITTED, 40001/40P01 retry), SQLSTATE mapping, audit and outbox writers (no RETURNING), idempotency store, X-DB-1..3 lookups. Open: X-DB-7 (service roles cannot read `public.afs_schema_migrations`).
- 2026-11-21: M0-D1..D3, D5 done: router from `routes_gen.go` (182 operations, `:verb` dispatch, 501 placeholders, per-area registrars), request id and contract version on every response, error envelope, panic recovery, body limits, strict query from the OpenAPI parameters, `teamId` from the path, PRD §3 authorization middleware with denied and cross-team hooks, HMAC page tokens, CSV writer.
- 2026-11-21: M0-D4, D6 done: `cmd/afs-api` `wire()` (optional database: owner migration, pool, readiness, denied-audit hook), `/v1/openapi.yaml`, health and readiness; `test/contract` replays all 182 operations' examples in-process on a seeded database and validates requests and responses (181 validated, 1 placeholder).
- 2026-11-21: M0-E1..E3 done: OQ-1/OQ-2 desk research in research.md §3.3, `[e2e]` probes in `test/e2e/spike_test.go` (to be run by a person with the dev account), ADR-002 accepted and ADR-003..011 recorded.
- 2026-11-21: M0-X1 done: `make gate`, `make test-db` and `make cover` green; 182-operation equality green against root main c605c4b; X-DB-1..6 delivered by -db, X-DB-7 requested; contract delta list CD-1..CD-10 written; `api/openapi.yaml` 1.1.0 complete for all 182 operations on the branch (-cli and -website notified through the milestone report). **M0 complete.**
- 2026-11-21: M1-A1..A7 done (worker m1/w1): go-jose JWKS verifier (asymmetric only, 10 min cache, one rate-limited refetch, 503 when no keys cached), provider list, JIT sign-in and group bindings with the provider rule, `afsk_` keys through `afs.authenticate_api_key`, `ListSignInProviders`, `GetMe`, authn middleware; fake-IdP end-to-end test on a real database.
- 2026-11-21: M1-D1..D7 done (worker m1/w1): idempotency (claim in the use case transaction, replay, mismatch, in progress), If-Match rules from the OpenAPI and ETag, token-bucket limiter per principal and per source address, CORS from `api.cors_origins`, denied audit (route and handler 403s) with cross-team 404 counter, oracle-based matrix harness (`internal/adapter/httpapi/matrix`) driven from `cmd/afs-api`; module already at X-DB-1 and X-DB-6, proven by the e2e denied-audit cases.
- 2026-11-21: M1-B1..B9 done on branch m1/w2 (worker w2): `Team` aggregate, teams use cases (create with required tags, models, first admin, idempotency; list, get, status, update with guardrail and ceiling-override rules, suspend, resume, delete, models, tags), `TeamRepo`, handlers (`h2_teams.go`) and the suspended-team guard (`suspended.go`, one-line hook in `server.go`).
- 2026-11-21: M1-C1, C2 done: members (`CanGrant`, If-Match) and API keys (secretKey once, replay without it, hard delete) use cases, repos, handlers; M1-C3 (matrix rows) waits for the M1-D6 harness.
- 2026-11-21: M1-E1..E5 done: settings, identity providers (in-use rules, one default, CD-9 `cli_client_id` detected at run time), service tags, audit (team and service, CSV) and revisions, with repos, handlers and tests; wired in `cmd/afs-api/wire_m1w2.go`.
- 2026-11-21: M1-F3, F4, F5 done (worker w3): reconciler engine (work queue keyed by kind and id with per-team ordering, capped 1 s to 5 min backoff, table routing with service-level fan-out, leader-only job scheduler), `LISTEN afs_desired_state` change feed with reconnect and 30 s stale poll, status writer with D24 vocabulary and status_changed events, drift recorder.
- 2026-11-21: M1-F1, F2 done (worker w3): Kubernetes client with server-side apply (field manager `afs-reconciler`, managed labels, own-write cache), envtest harness with pinned CRD stand-ins (Cilium, Karpenter, ESO, Agentic Teammates placeholder, VolumeSnapshot), Lease leader election (failover under 30 s proven on envtest), drift watcher.
- 2026-11-21: M1-G2a..G2d done (worker w3): AWS settings and Pod Identity-only loader, `awstest` endpoint recorder, IAM adapter (create or sync roles named by id under `/afs/<namespace>/`, boundary, tags, inline policies; refuses roles it does not own), team and Bedrock role trust and permission policies (Bedrock role trusts only the gateway role and allows only the team's inference profiles; no role gets `bedrock:*`), Bedrock control adapter for application inference profiles, D26 condition helpers.
- 2026-11-21: M1-G1a..G1d: team environment applied by TeamCluster and team controller; envtest TestTeamEnvironmentConverges (e2e cases deferred to the cluster suite)
- 2026-11-21: M1-F6: drift end-to-end (watcher, engine, DriftCorrected); engine now matches drift flags after the pass (race fix); envtest TestDriftIsRevertedAndRecordedOnce
- 2026-11-21: M1-H1, M1-H2: admission policies and bindings (usecase/reconcile/admission, render/admission.go, AdmissionCluster); envtest TestAdmissionPolicies and TestAdmissionPolicyRestored ([e2e] case deferred to the cluster suite)
- 2026-11-21: M1-F7: cmd/afs-reconciler wired (settings, schema check with afs.schema_version fallback, leader election, probes, metrics); user-docs/reconciler.md; render and TeamSource tests added
- 2026-11-21: M1-I1, M1-I2 done (`m1/w4`): `usecase/reconcile/lifecycle` (ordered idempotent delete finalizer with a condition per step, secrets scheduled with the 30-day window before the namespace, hooks for agent and pool steps, suspend/resume with recorded prior state), `aws/secretsmanager` and `aws/eks` delete adapters, `postgres/lifecycle_repo.go` (finalize `deleted` + audit, secret rows). Tested with fakes and a real database; the envtest case of M1-I1 needs the M1-F1 harness and is left to the integrator.
- 2026-11-21: M1-J1..J3 done (`m1/w4`): `usecase/reconcile/housekeeping` (outbox publisher: ordered per aggregate, at least once, backoff 1 s to 5 min, lag metric; daily partition ensure with two months ahead, monthly drop of 13-month-old partitions, 5-minute idempotency purge), `aws/eventbridge` PutEvents adapter, `postgres/housekeeping.go`; integration tests prove partitions, retention (audit_log untouched) and purge.
- 2026-11-21: M1-K1..K3, M1-L1..L3 done (`m1/w4`): Terraform modules (rds, backup, buckets, boundary, pod-identity) checked by an HCL/policy test module; Helm chart `deploy/helm/afs` rendered and asserted with the Helm library (`deploy/helm/afs/tests`, golden); `test/e2e/harness.go`; `observability` (redacting slog, OTLP, Prometheus registry, ops server with /metrics /healthz /readyz, RED middleware), PrometheusRule and dashboard on the names in `names.go`. `terraform validate`, `tflint` and `helm lint` run in `.github/workflows/deploy.yml` (no such binaries in the build sandbox).
- 2026-11-22: M1 integrated on feat/restapi-feature-complete (merges of m1/w1..w4). One idempotency path (`acting.Idem`, in the mutation transaction); `postgres.SettingsReader`, `m1w2Teams` and `domain.HashAPIKey` are the single sources; -db a6ecc3c adopted (migrations 1-17, X-DB-7 flipped); `cli_client_id` column and `max_output_tokens` read directly. Reconciler wired with housekeeping, lifecycle finalizer (`team.Hooks.Deleting`), ConditionStore over resource_status and `observability.Start`; afs-api serves every M1 route and has matrix rows for all of them (M1-C3). M1-X1: gate, cover, test-db, envtest and lint (`--build-tags envtest`) green.
- 2026-11-22: M2-C1..C5 done (`m2/w2`): secret domain with `SecretValue` (never marshals or prints), Secrets Manager writer (`aws/secretsmanager/write.go`, `list.go`), `usecase/secrets` (Secrets Manager first with a derived ClientRequestToken, compensation then `afs-orphan` tag and `afs_api_orphan_secrets_total`, 503 without the value, bound delete 409), `postgres/secrets_repo.go`, handlers, `cmd/afs-api/wire_secrets.go`, matrix rows, secret fuzz test (`test/fuzz/secrets`), orphan sweep job in the reconciler (`housekeeping/orphansecrets.go`, tagged or no row for 1 h, 30-day window, audited).
- 2026-11-22: M2-B1..B7 done (`m2/w2`): `domain/agent.go` (digest image, grow-only volume, create-only storage class, no replicas, pause/resume/restart/delete transitions), `usecase/agents` (model, storage class, template and binding checks on create; template and model checked only when changed so an agent whose template is disabled later stays editable, RA-US-4.5; restart raises `generation`, the reconciler derives the nonce), `postgres/agents_repo.go` (template columns written only when changed so the DB template guard does not fire), handlers `h_agents.go` (create 201, delete and restart 202, pause and resume 200, snapshot create 202), `cmd/afs-api/wire_agents.go`, matrix rows for all 11 operations (`matrix/fixtures_agents.go`), end-to-end test `cmd/afs-api/agents_e2e_test.go`.
