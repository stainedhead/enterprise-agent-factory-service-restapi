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
| 5 | Implementation (M0–M6) | In Progress | M0 complete 2026-11-21 (step 3 started 2026-11-21T20:57:16Z); M1 next. Task progress in tasks.md |
| 6 | Completion and archival | Not Started | |

## Phase 0 checklist

- [x] Spec directory created and PRD moved in (`create-spec`)
- [x] Research questions identified (research.md §1)
- [x] Phase files initialized (spec, research, data-dictionary, architecture, plan, tasks, implementation-notes)

## Milestones (Phase 5)

| Milestone | Status | Tasks done |
|---|---|---|
| M0 Contract and foundation | Complete | 41 / 41 |
| M1 Teams and core | Not Started | 0 |
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
- 2026-11-21: M1-F3, F4, F5 done (worker w3): reconciler engine (work queue keyed by kind and id with per-team ordering, capped 1 s to 5 min backoff, table routing with service-level fan-out, leader-only job scheduler), `LISTEN afs_desired_state` change feed with reconnect and 30 s stale poll, status writer with D24 vocabulary and status_changed events, drift recorder.
- 2026-11-21: M1-F1, F2 done (worker w3): Kubernetes client with server-side apply (field manager `afs-reconciler`, managed labels, own-write cache), envtest harness with pinned CRD stand-ins (Cilium, Karpenter, ESO, Agentic Teammates placeholder, VolumeSnapshot), Lease leader election (failover under 30 s proven on envtest), drift watcher.
- 2026-11-21: M1-G2a..G2d done (worker w3): AWS settings and Pod Identity-only loader, `awstest` endpoint recorder, IAM adapter (create or sync roles named by id under `/afs/<namespace>/`, boundary, tags, inline policies; refuses roles it does not own), team and Bedrock role trust and permission policies (Bedrock role trusts only the gateway role and allows only the team's inference profiles; no role gets `bedrock:*`), Bedrock control adapter for application inference profiles, D26 condition helpers.
- 2026-11-21: M1-G1a..G1d: team environment applied by TeamCluster and team controller; envtest TestTeamEnvironmentConverges (e2e cases deferred to the cluster suite)
- 2026-11-21: M1-F6: drift end-to-end (watcher, engine, DriftCorrected); engine now matches drift flags after the pass (race fix); envtest TestDriftIsRevertedAndRecordedOnce
