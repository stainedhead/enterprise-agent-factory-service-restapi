# Implementation notes: `-restapi` feature complete

| | |
|---|---|
| **Feature** | restapi-feature-complete |
| **Date** | 2026-11-21 |

Purpose: the running record of decisions, edge cases and deviations made while building this spec.
Milestone agents append here after each merged stream (newest last, dated, with the task id). Keep design
rationale in architecture.md and research.md; this file records what actually happened.

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
| N-10 | Viewer kubectl federated roles are configured per identity provider through a Helm-provided file, not the database | OQ-9 default: no access entry without a configured role; no schema column exists |

## Edge cases and solutions

(append during implementation)

## Deviations from plan

(append during implementation)

## Contract deltas for the root repository

Collected for the M0 exit task's root PR (labelled `contract`): see research.md §3.1.6 and OQ-S1, OQ-S2;
`TeamTags.requestMetadataDroppedKeys` (M0-B1b, M4-B2).

## Lessons learned

(append at completion)
