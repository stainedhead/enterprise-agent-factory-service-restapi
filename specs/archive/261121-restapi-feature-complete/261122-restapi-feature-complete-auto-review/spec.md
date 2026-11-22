# Spec: restapi feature-complete review fixes

| | |
|---|---|
| **Feature** | restapi-feature-complete-auto-review |
| **Spec** | `specs/261122-restapi-feature-complete-auto-review/` (created 2026-11-22, step 8 of `implm-frm-prd`) |
| **Source PRD** | [restapi-feature-complete-review-PRD.md](restapi-feature-complete-review-PRD.md) (moved here by step 8) |
| **Parent spec** | `specs/archive/261121-restapi-feature-complete/` |
| **Branch** | `feat/restapi-feature-complete` (base `87e7cb7` plus the step 7 commit) |

## Executive summary

The feature-complete control plane enforces RBAC, tenant isolation, image and model guardrails and the hard
budget cap on its main paths. The Step 5 review (reviewed commit `9454fd5`) found 17 places where an
invariant holds on one path and not on another: 1 P0, 6 P1, 10 P2. This spec turns them into five
parallel fix streams, A to E, that merge back before the branch goes to `main`.

## Goals

1. Restore enforces every guardrail a direct mutation enforces (FR-R01).
2. Audit names the real client (FR-R02), and an unauthenticated caller learns nothing about
   infrastructure (FR-R03).
3. Gateway spend matches the contract's worst-case bound and never silently loses cost (FR-R04, FR-R05).
4. Every shipped alert can fire (FR-R06); the default chart works with 3 replicas (FR-R07).
5. P2 hardening items are fixed, or accepted with a recorded rationale (FR-R08 to FR-R17).

## Non-goals

- New features or contract operations; changes to the permission catalog or role model.
- The infrastructure-bound items (M2-F8, M6-F3, M6-F4, M6-F5, M6-F6). They stay open in
  `specs/archive/261121-restapi-feature-complete/tasks.md` and no task here may mark them done.
- Performance tuning beyond the FR-R09 timeouts.
- A `-db` migration (Q1 default: use case only; record a `-db` follow-up).

## Requirements

The requirements are FR-R01 to FR-R17 in the source PRD section 5, with their evidence and numbered
acceptance criteria. They are not restated here. `tasks.md` cites them as `FR-Rnn AC<n>`.

| FR | Pri | Stream | One line |
|---|---|---|---|
| FR-R01 | P0 | A | Restore skips template, digest, model, storage class and binding guards |
| FR-R02 | P1 | A | Mutation audit `source_ip` is the proxy, not the client |
| FR-R12 | P2 | A | Use-case denial audit dropped on error and cancellable |
| FR-R06 | P1 | C | Five metrics never recorded; three alerts cannot fire |
| FR-R13 | P2 | C | Raw AWS and Kubernetes error text in status conditions |
| FR-R04 | P1 | B | Requests without a token limit go to Bedrock uncapped |
| FR-R05 | P1 | B (counter in C) | Failed settlement drops actual cost |
| FR-R08 | P2 | B (counter in C) | Dropped usage rows not exported; `context.Background` in adapter |
| FR-R03 | P1 | D | Public `/readyz` returns raw pgx errors |
| FR-R07 | P1 | D | Default chart breaks pagination across replicas |
| FR-R09 | P2 | D | No `ReadTimeout` or `IdleTimeout` |
| FR-R10 | P2 | D | Startup migration ignores IAM auth |
| FR-R11 | P2 | D | `os.Getenv` outside `settingsFrom`; dead `AFS_MAX_BODY_BYTES` |
| FR-R15 | P2 | D (last) | Mutable package-level handler registry; 501 fallback not refused |
| FR-R14 | P2 | E | Idempotency fingerprint derived from the secret value |
| FR-R17 | P2 | E | Ignored `json.Marshal` errors in the import writer |
| FR-R16 | P2 | E | CD-7, CD-9, CD-12 not in the root contract |

## Non-functional requirements

From the source PRD section 6: refused, redacted or denied cases tested for FR-R01 to FR-R03 and FR-R14;
no secret, token or account id logged, returned or stored; no added latency on the gateway response path
(`AsyncSettle` keeps GW-8 under 25 ms added p99; `make load` passes); every new failure path has a counter
and an alert or a documented reason for none; the FR-R06 dead-metric check runs in `go test ./...`; restore
validation reuses the agents checks inside the restore transaction with no N+1 and a 200-agent restore
inside the 30 s statement timeout; `make gate` after every merge; 90% coverage on `domain`, `usecase`,
`authz`; `go vet -tags e2e,load,conformance ./test/...` still compiles.

## Open questions and the decisions taken (authorized by J; recorded in `implementation-notes.md`)

| # | Decision | Taken |
|---|---|---|
| Q1 | FR-R01 digest guard | Use case only. `-db` trigger recorded as a follow-up, not built |
| Q2 | FR-R05 persistence | Bounded in-memory queue (10,000) plus counter and alert. Crash falls back to hold expiry |
| Q3 | FR-R04 unknown Invoke family | Refuse with 400 `ValidationError` (fail closed) |
| Q4 | FR-R16 outbox exemptions | List members, API keys, settings and the other CD-7 mutations as exempt in the contract |

## Acceptance (whole spec)

All 17 FR acceptance criteria met or explicitly accepted with rationale in `implementation-notes.md`;
`make gate` green on the integration branch; `make cover` at or above 90% on the three inner layers; the
root-repository contract PR for FR-R16 opened (not merged by this run).
