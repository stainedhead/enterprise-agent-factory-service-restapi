# Dev-Flow Process Analysis

**Feature:** restapi-feature-complete
**Spec directory:** specs/archive/261121-restapi-feature-complete
**Review spec:** specs/archive/261121-restapi-feature-complete/261122-restapi-feature-complete-auto-review
**Report generated:** 2026-11-22

Hand-run: the dev-flow plugin and `prd-to-spec` were not installed, so the orchestrator followed `commands/implm-frm-prd.md`
by hand with inline templates. Timing below comes from `git log` author timestamps (UTC). Where the dashboard and git
differ, git wins and the difference is noted.

---

## 1. Executive Summary

The control plane of the Enterprise Agent Factory Service is four Go binaries in one module. `afs-api` is the REST API
and the only security boundary (authentication, `internal/authz`, validation, desired state with audit and outbox in one
transaction). `afs-reconciler` applies desired state to EKS and AWS and reverts drift, `afs-gateway` meters and caps
Bedrock spend and fails closed, and `afs-billing` reconciles the Cost and Usage Report. All 182 operations of the API
contract are implemented across milestones M0 to M6, followed by a review pass that fixed 17 findings in 5 streams.

**Total runtime:** 14 h 0 min (first spec commit 2026-11-21T20:47:19Z to the last commit 2026-11-22T10:46:54Z; the dashboard started the clock at 20:31:38Z, before the first commit).
**Overall assessment:** the build ran in parallel milestone streams with integrator commits at each milestone boundary and
held its quality gate. The review pass found one real P0 (restore bypassing the agents guardrails) and was cheap relative
to the build. Five tasks needing real infrastructure stayed open by decision. Integration cost was concentrated in a few
cross-stream seams, listed in section 6.

---

## 2. Step-by-Step Timing

Start and end are from the dashboard, cross-checked against the nearest commits.

| Step | Name | Start | End | Runtime (min) | Key Outputs |
|---|---|---|---|---|---|
| 1 | Create spec from PRD | 2026-11-21T20:31:38Z | 20:47:12Z (commit 20:47:19Z) | 16 | spec directory, 9 phase files |
| 2 | Review spec | 20:47:12Z | 20:55:16Z (commit 20:55:35Z) | 8 | gaps fixed, 6 `-db` dependencies tracked |
| 3 | Implement product | 20:57:16Z | 2026-11-22T07:49:27Z | 652 | M0 to M6, first-parent integrator commits, 4 parallel workers per milestone |
| 4 | Documentation and user docs | 07:49:27Z | 07:52:03Z (commit 07:52:13Z) | 3 | product docs, ADR-012 to 016, 11 user guides |
| 5 | Code and design review | 07:52:13Z | 08:05:43Z (commit 08:05:51Z) | 14 | review PRD: 1 P0, 6 P1, 10 P2 |
| 6 | Prepare review PRD | 08:05:43Z | 08:06:36Z (commit 08:06:46Z) | 1 | AC tightened, 5 fix streams |
| 7 | Archive original spec | 08:07:00Z | 08:08:02Z | 1 | `git mv` to `specs/archive/` |
| 8 | Spec review fixes (create review spec) | 08:09:00Z | 08:10:37Z | 2 | review spec, 48 tasks (dashboard says 5 min; git shows 2 between the step 7 and step 8 commits) |
| 9 | Implement review fixes | 08:11:00Z | 10:09:15Z (commit 648dff1) | 118 | 5 streams A to E, integrator, contract check repaired |
| 10 | Archive fixes spec | 10:09:15Z | 10:10:10Z (commit 8cedd96) | 1 | review spec nested in the archive |
| 11 | Final quality pass | 10:10:10Z | 10:37:37Z | 27 | docs updates, ADR-017 to 022, full suite |
| 12 | Process analysis | 10:37:37Z | 10:40:54Z | 3 | this file |
| 13 | Archive layout check | 10:40:54Z | 10:42:54Z | 2 | layout confirmed |
| 14 | Pull request | 10:42:54Z | 10:46:54Z (PR opened) | 4 | PR, checks, merge |

**Notable observations:**
- Step 3 is 80 percent of the elapsed time. Milestone gaps (M0 21:50, M1 00:11, M2 01:10, M3 04:59, M4 05:50, M5 06:42, M6 07:49 UTC)
  show the longest stretch was M2 to M3 (3 h 49 min, from 01:10 to 04:59), the large gateway and budgets milestone.
- Step 9's first 100 minutes were five parallel worktrees (merges at 09:52 to 09:53); the integrator took 16 minutes
  (09:53 to 10:09) and found a failing contract check that the worktrees had skipped silently.
- Steps 4 to 8 together took 29 minutes.

---

## 3. Commit and Push Summary

**Total commits on the branch beyond `main`:** 206 (181 non-merge, 25 merges), plus the final stability, analysis and
PR commits listed below. The first-parent history (62 commits at the time of writing, milestone integrator and process
commits) is listed; stream-branch commits are in `git log`.

| Commit | Timestamp (git) | Message |
|---|---|---|
| 0b9f0e1 | 2026-11-21T20:47:19+00:00 | spec: create 261121-restapi-feature-complete from the PRD (implm-frm-prd step 1) |
| 1ee6b41 | 2026-11-21T20:55:35+00:00 | spec: review 261121-restapi-feature-complete and fix gaps (implm-frm-prd step 2) |
| e3b87d1 | 2026-11-21T21:03:54+00:00 | M0-S: adopt -db b0d8754, domain primitives, archtest, Makefile and CI targets |
| 747a484 | 2026-11-21T21:27:00+00:00 | M0-A, M0-B, M0-C: contract extraction, full OpenAPI 1.1.0, authz; adopt -db 23e9240 |
| a23bf94 | 2026-11-21T21:33:42+00:00 | M0-F7: cross-cutting ports and in-memory fakes |
| 693d93b | 2026-11-21T21:39:22+00:00 | M0-F1..F6, F8: PostgreSQL foundation |
| 1b037b3 | 2026-11-21T21:44:09+00:00 | M0-D1..D3, D5: HTTP skeleton, middleware, authorization, pagination, CSV |
| b91efab | 2026-11-21T21:46:54+00:00 | M0-D4, D6: afs-api wiring and OpenAPI example replay |
| b8890b5 | 2026-11-21T21:48:23+00:00 | M0-E1..E3: OQ-1/OQ-2 desk research, e2e probes, ADRs |
| 081de14 | 2026-11-21T21:50:02+00:00 | M0-X1: milestone M0 exit |
| 7c64964 | 2026-11-21T23:46:42+00:00 | Merge m1/w1: auth and request pipeline (M1-A, M1-D) |
| 1b593c9 | 2026-11-21T23:47:19+00:00 | Merge m1/w2: teams, members, API keys, service admin I (M1-B, M1-C, M1-E) |
| bb5195b | 2026-11-21T23:47:33+00:00 | Merge m1/w3: reconciler engine, team environment, admission (M1-F, M1-G, M1-H) |
| 032c3e9 | 2026-11-21T23:48:41+00:00 | Merge m1/w4: lifecycle, housekeeping, deploy, observability (M1-I..L) |
| bfe8b05 | 2026-11-21T23:53:14+00:00 | M1 integration: one idempotency path (use case transaction), one settings reader and team lookup, adopt - |
| 68d704c | 2026-11-21T23:58:45+00:00 | M1 integration: reconciler wiring for lifecycle, housekeeping and observability; IAM database auth |
| 048eacf | 2026-11-21T23:59:33+00:00 | M1-L: RED metrics, cross-team counter and probes for afs-api through observability |
| f9d9940 | 2026-11-22T00:02:50+00:00 | M1-C3, D6: register every area in the router, matrix cases for all M1 operations, If-Match exemption for  |
| 4814c07 | 2026-11-22T00:07:10+00:00 | M1-X: covercheck skips fake packages, archtest allows SDK credential providers, CI envtest assets, lint f |
| e5f1c5c | 2026-11-22T00:09:53+00:00 | M1-X: redacting logger in both mains, lint with envtest build tag |
| 1bfb195 | 2026-11-22T00:11:38+00:00 | M1-X1: integration decisions, M1 status and ticks, technical details, user-docs |
| 89576f2 | 2026-11-22T00:58:01+00:00 | M2-X1: merge m2/w1 (catalogs, config, external systems) |
| 62bf7e5 | 2026-11-22T00:58:09+00:00 | M2-X1: merge m2/w2 (secrets, agents API) |
| 2db4754 | 2026-11-22T00:58:30+00:00 | M2-X1: reconcile w1/w2 overlaps (one agent seeding helper, resourcesJSON rename) |
| 3e78a66 | 2026-11-22T00:58:56+00:00 | M2-X1: merge m2/w4 (log destinations, egress, traffic) |
| 4a17d38 | 2026-11-22T00:59:48+00:00 | M2-X1: merge m2/w3 (agent workloads, volumes, snapshots, lifecycle); one mergeRoutes, one label constant |
| dd92466 | 2026-11-22T01:01:43+00:00 | M2-X1: router splits only registered verbs (model ids with a colon), team status kind is 'team', orphan s |
| 71d2837 | 2026-11-22T01:03:32+00:00 | M2-X1: drift watch waits for a write in flight before judging its echo (flaky TestDriftWatcher), keeps re |
| 9d54720 | 2026-11-22T01:05:44+00:00 | M2-X1: nolint for metric name |
| 46aeee4 | 2026-11-22T01:10:36+00:00 | M2-X1: integration decisions, M2 status and ticks, e2e checks for restart event and delete state |
| 6868abe | 2026-11-22T04:37:06+00:00 | M3-E, M3-F: budgets API and budget reconciler (squash of m3/w1, specs/261121-restapi-feature-complete) |
| bd28993 | 2026-11-22T04:37:11+00:00 | Merge m3/w2: M3-H reports, M3-I activity collector |
| 359f1df | 2026-11-22T04:37:38+00:00 | Merge m3/w3: M3-G notifications |
| a723d9c | 2026-11-22T04:38:15+00:00 | M3-A, M3-B, M3-C, M3-D: gateway, reservations, streaming, conformance and load (squash of m3/w4, specs/26 |
| e123bb5 | 2026-11-22T04:42:14+00:00 | M3-X1: cross-stream checks (counter and events once, usage rows, log hygiene), gateway Helm RBAC and valu |
| ef938d6 | 2026-11-22T04:59:29+00:00 | M3-X1: exit results, budget coverage tests, integration decisions, technical details (specs/261121-restap |
| 39275bd | 2026-11-22T05:36:05+00:00 | Merge m4/w1: M4-A identities, role links, accounts; M4-B tags |
| ef4d49b | 2026-11-22T05:36:33+00:00 | Merge m4/w2: M4-C CUR and costs; M4-D pricing |
| af11408 | 2026-11-22T05:36:49+00:00 | Merge m4/w3: M4-E showback and scheduled reports |
| d5b9b4b | 2026-11-22T05:37:11+00:00 | Merge m4/w4: M4-F export and import; M4-G restore |
| bd1f7fa | 2026-11-22T05:50:19+00:00 | M4-X1: integration (billing report jobs, export blob over S3, OpenAPI price proposal fixes, reconciler RB |
| f403bd8 | 2026-11-22T06:21:49+00:00 | Merge m5/w1: M5-A ephemeral teams, M5-B pools API |
| 15fa9dd | 2026-11-22T06:21:55+00:00 | Merge m5/w3: M5-E viewer kubectl, M5-F cost tags |
| b2d39ea | 2026-11-22T06:22:01+00:00 | Merge m5/w2: M5-C pool workloads, M5-D teardown |
| 549a2f2 | 2026-11-22T06:42:50+00:00 | M5-X1: integration (adopt -db 000018: report rollups, restore of secret bindings, billing reads; pool re- |
| aa5e040 | 2026-11-22T07:19:29+00:00 | Merge m6/w1: M6-A secops and service keys, M6-D workload principal |
| cf59213 | 2026-11-22T07:19:32+00:00 | Merge m6/w2: M6-B swarm templates, M6-C work orders |
| 6378ad2 | 2026-11-22T07:19:34+00:00 | Merge m6/w3: M6-E work order reconciler |
| 668d904 | 2026-11-22T07:42:38+00:00 | M6-X1: integration of M6 (adopt -db 000019; workload If-Match, outbox envelope, identity provider area fi |
| fc32aea | 2026-11-22T07:49:27+00:00 | M6-X2: B1 tag sync for team secrets and pool queues (tag_sync reconcile kind, Secrets Manager and SQS tag |
| 9454fd5 | 2026-11-22T07:52:13+00:00 | Step 4: documentation and user docs (product docs, ADR-012..016, README, getting started, configuration r |
| f0844d5 | 2026-11-22T08:05:51+00:00 | Step 5: code and design review as review PRD (1 P0, 6 P1, 10 P2) (specs/261121-restapi-feature-complete) |
| 87e7cb7 | 2026-11-22T08:06:46+00:00 | Step 6: review PRD finalized per review-prd (ready for spec; AC tightened, priorities confirmed, fix stre |
| b79ca95 | 2026-11-22T08:08:02+00:00 | Step 7: archive specs/261121-restapi-feature-complete to specs/archive/ |
| 80db7e1 | 2026-11-22T08:10:37+00:00 | Step 8: create review spec specs/261122-restapi-feature-complete-auto-review |
| 28b8aaa | 2026-11-22T09:52:56+00:00 | Merge rf/a (specs/261122-restapi-feature-complete-auto-review) |
| 78f6978 | 2026-11-22T09:53:06+00:00 | Merge rf/c (specs/261122-restapi-feature-complete-auto-review) |
| f923560 | 2026-11-22T09:53:11+00:00 | Merge rf/b (specs/261122-restapi-feature-complete-auto-review) |
| 64f0021 | 2026-11-22T09:53:11+00:00 | Merge rf/d (specs/261122-restapi-feature-complete-auto-review) |
| af5cd82 | 2026-11-22T09:53:11+00:00 | Merge rf/e (specs/261122-restapi-feature-complete-auto-review) |
| 648dff1 | 2026-11-22T10:09:15+00:00 | Step 9: integrate review fixes, FR-R15 unbound-operation check, B-10 and E wiring, contract extract 1.2.0 |
| 8cedd96 | 2026-11-22T10:10:10+00:00 | Step 10: archive review spec inside the original spec archive (specs/archive/261121-restapi-feature-compl |

**Pull request:** opened at step 14 (number and merge recorded on the PR). Pushes went to `feat/restapi-feature-complete` only.

---

## 4. Spec vs. Implementation Comparison

The spec gives build order (M0 to M6), not time estimates, so no planned durations exist. Actual times are from git.

| Phase | Planned (spec) | Actual (git log) | Difference | Notes |
|---|---|---|---|---|
| M0 foundation | N/A | 20:57 to 21:50 (53 min) | n/a | contract extract, OpenAPI, authz, Postgres foundation |
| M1 auth, teams, reconciler engine | N/A | 21:50 to 00:11 (141 min) | n/a | four workers |
| M2 catalogs, secrets, agents | N/A | 00:11 to 01:10 (59 min) | n/a | four workers |
| M3 gateway, budgets, reports | N/A | 01:10 to 04:59 (229 min) | n/a | largest milestone |
| M4 identities, billing, export | N/A | 04:59 to 05:50 (51 min) | n/a | |
| M5 pools, ephemeral teams | N/A | 05:50 to 06:42 (52 min) | n/a | |
| M6 work orders, secops | N/A | 06:42 to 07:49 (67 min) | n/a | plus tag sync (M6-X2) |
| Documentation | N/A | 3 min | n/a | |
| Review and review fixes | N/A | 07:52 to 10:09 (137 min) | n/a | |

**Phases skipped:** M2-F8 and M6-F3, F4, F5, F6 (dev cluster e2e, restore drill, full-scale load, `govulncheck` and ASVS
checklist). They need real infrastructure or a toolchain download the sandbox blocks, and stay open in `tasks.md`.
**Phases added:** M6-X2 tag sync; the review cycle (steps 5 to 10); repair of the contract check in step 9.

---

## 5. Token / Message Usage

Exact token counts are unavailable. Estimate: the orchestrator ran one long session with each milestone and each review
stream delegated to a worker agent (4 workers per milestone for M1 to M5, 3 for M6, 5 review streams, plus an
integrator and an independent verifier). Context was the limiting resource for the orchestrator, not wall time.

---

## 6. Process Observations

### What worked well
- Parallel worktrees per milestone with an integrator commit at each boundary kept the first-parent history readable.
- Writing the review as a PRD with a fenced "do not touch" list let five streams run in parallel with only one
  conflict (`status.md`, one row per stream).
- Test seams enforced by tests (a go/ast check on `os.Getenv`, a dead-metric test, the unbound-operation check) turned
  review findings into build failures instead of reminders.

### What caused delays or rework
- The contract check passed silently in every worktree (`-skip-missing`, no root checkout) and had been failing against root
  `main` (extract cut from contract 1.1.0, root at 1.2.0), so CI was already red. Found only in the integrator's checkout.
- Disk filled during the first load run (Go build cache 45 GB), crashed Postgres and cost a rerun. `go clean -cache` fixed it.
- Stream D could not finish D-9 and D-10 (about 15 files owned by other streams); the integrator did them after merge.
- The local root checkout was on an unmerged branch, so the gate needed `AFS_ROOT_DIR` pointed at an export of root `main`.
- The dashboard and git disagreed on step 8 duration (5 min against 2).

### Recommendations for future runs
- Make `contract-check` fail without `AFS_ROOT_DIR` outside local use, or have worktrees point at a root export.
- Put the root contract PRs (here E-6) in the same run so the extract pins to a merged version.
- Give shared wiring files to one stream or to the integrator up front.
- Clean the Go build cache between milestones when many worktrees build in parallel.
- Tag `-db` before the run; the pseudo-version pin remains an open item.

---

## 7. Manual vs. Automated Comparison

**Estimated manual duration:** about 6 to 9 person-weeks (assumes a senior Go developer familiar with the stack, 182
operations with tests, a reconciler, a metering gateway, billing jobs, 22 ADRs and user docs; excludes meetings and
external reviews).
**Actual automated runtime:** 14 h 0 min elapsed, from git timestamps.
**Efficiency gain:** roughly 10 to 15 times in elapsed time. The estimate is rough: the automated run left five
infrastructure-bound tasks open and needs human review of 200 thousand added lines.

---

## 8. Final suite (step 11)

All run on the final head against a fresh Postgres 16 database (`afs_restapi_final`), envtest binaries and an export of root `main` (`AFS_ROOT_DIR`):

| Target | Result |
|---|---|
| `make gate` (tidy, fmt-check, vet, vet-tags, openapi-check, contract-check against root main, golangci-lint v2.15.0, `go test -race ./...`, build) | exit 0 |
| `make cover` (domain, usecase, authz at or above the 90 percent target) | exit 0 |
| `make test-db` | exit 0 |
| `make envtest` | exit 0 |
| `make conformance` | exit 0 |

The suite took 27 minutes including a cold Go build cache. Check runs on the PR are recorded in the PR itself.
