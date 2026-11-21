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

## Edge cases and solutions

(append during implementation)

## Deviations from plan

| Date | Task | Deviation | Why |
|---|---|---|---|
| 2026-11-21 | M0 (all streams) | Streams ran serially in the milestone agent's own checkout on `feat/restapi-feature-complete`, one commit group per stream in the listed merge order, instead of parallel worker sub-agents in git worktrees | The M0 milestone agent had no tool to start worker sub-agents. Ownership lines were still honored file by file, so the commit history matches the stream boundaries |
| 2026-11-21 | M0-S2 | ULIDs are encoded by a small stdlib encoder in `internal/domain/ids.go`, not `oklog/ulid/v2` (research R-12) | AGENTS.md: `domain` imports nothing outside the standard library; `internal/archtest` enforces it |
| 2026-11-21 | M0-S2 | `ImageRef` lives in `names.go`, not a separate `image.go` (data dictionary §1) | M0-S's Owns line lists `names.go` and no `image.go` |
| 2026-11-21 | M0-S4 | `make cover` runs `tools/covercheck` (fails any package below 90%) | `go test -cover` alone does not fail on low coverage |
| 2026-11-21 | M0-F | Local integration tests ran against PostgreSQL 16.15 (the sandbox server), not 17; CI uses `postgres:17` | Only PostgreSQL 16 is installed in the build sandbox |

## Contract deltas for the root repository

Collected for the M0 exit task's root PR (labelled `contract`): see research.md §3.1.6 and OQ-S1, OQ-S2;
`TeamTags.requestMetadataDroppedKeys` (M0-B1b, M4-B2).

## Lessons learned

(append at completion)
