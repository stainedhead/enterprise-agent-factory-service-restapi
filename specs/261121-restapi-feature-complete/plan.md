# Plan: `-restapi` feature complete

| | |
|---|---|
| **Feature** | restapi-feature-complete |
| **Date** | 2026-11-21 |
| **Status** | Planning complete; ready for M0 |

## 1. Development approach

- **One milestone agent per milestone, M0 to M6, in order.** Each reads AGENTS.md, this spec and its
  milestone in tasks.md, splits the milestone into the listed streams, runs each stream in its own git
  worktree (`git worktree add ../afs-restapi-<stream> -b feat/restapi-feature-complete-<stream>`), merges
  the streams back into `feat/restapi-feature-complete` in the listed order, and finishes with the
  milestone's exit task. Worker agents never edit `status.md` or `tasks.md`; the milestone agent ticks
  tasks after each merge.
- **TDD for every task** (AGENTS.md): red test that fails for the right reason, least code, refactor. Every
  authorization rule has its denied case. Each task names its acceptance criteria; a task is done when
  those tests pass at the named level and `make gate` is green.
- **Contracts first.** The OpenAPI document for all 182 operations is complete at the end of M0. Handlers
  replace placeholders milestone by milestone. A handler that needs a shape change edits its OpenAPI
  fragment in the same commit and records a contract delta for the root (research §3.1.6).
- **Fakes first for AWS and Kubernetes.** Each port lands with its fake and a contract test that runs
  against both the fake and the adapter (adapter side against `awstest` or envtest), so use case tests use
  fakes and the adapter is proven to match.

## 2. Phase breakdown

| Milestone | Streams (parallel unless noted) | Tasks | Depends on |
|---|---|---|---|
| M0 Contract and foundation | S (serial first), A, B1–B4, C, D, E, F | see tasks.md | — |
| M1 Teams and core | A, B, C, D, E, F, G1, G2, H, I, J, K, L | | M0 |
| M2 Agents | A, B, C, D, E, F, G, H, I | | M1 |
| M3 Gateway and budgets | A, B, C, D, E, F, G, H, I | | M2 |
| M4 Accounts, tags, billing | A, B, C, D, E, F, G | | M3 |
| M5 Swarms | A, B, C, D, E, F | | M4 |
| M6 CVE remediation and completion | A, B, C, D, E, F (F last) | | M5 |

Milestones are sequential because each builds on the last (for example pools reuse agent rendering, the
gateway needs agents and models). Inside a milestone, streams depend only on M-earlier work or on tasks
named in their `Depends` column; a stream blocked on a sibling's task starts with its independent tasks.

## 3. Critical path

M0-A (contract extract) → M0-D (generated routes, middleware) and M0-B (OpenAPI) → M0 exit (publish
OpenAPI to `-cli`/`-website`) → M1-A (auth) → M1-B (teams) → M1-F (engine) → M1-G (team env) → M2-B
(agents) → M2-F (agent workload) → M3-A/B (gateway) → M3-D (load test: hard cap) → M4-C (CUR) → M5-B/C
(pools) → M6-C/E (work orders) → M6-F (full e2e, restore drill).

## 4. Testing strategy

| Level (PRD tag) | Where | Runs in |
|---|---|---|
| [unit] | `*_test.go` beside code; table-driven; `domain`, `authz`, `usecase` with fakes | `make gate` |
| [adapter] | `internal/adapter/**/*_test.go`, `internal/usecase/**` integration tests with real PostgreSQL (`AFS_TEST_DATABASE_URL`, skip when unset), `awstest`, recorded fixtures | `make gate` locally with a database; CI always |
| [envtest] | `internal/adapter/k8s/**`, `internal/usecase/reconcile/**/envtest_test.go` (build tag `envtest`) | `make envtest` (downloads assets via `setup-envtest`); CI job `envtest` |
| [contract test] | `api/contract_test.go` (document), `test/contract` (replay examples against an in-process server with a seeded database) | `make gate` (document), CI (replay) |
| [conformance suite] | `test/conformance` (tag `conformance`): AWS SDK and Anthropic SDK against the gateway with fake Bedrock | `make conformance`; CI job |
| [load] | `test/load` (tag `load`): API reads, reports, gateway 200 agents × 500 req/s, fake Bedrock | dev cluster; M3-D and M6-F |
| [e2e] | `test/e2e` (tag `e2e`, needs `AFS_E2E_KUBECONFIG`) | dev cluster; each milestone adds its cases; M6-F runs all |
| [e2e restore drill] | `test/e2e/restore` | dev cluster; M6-F |

CI additions (M0/M1): job `envtest`; job `conformance` (from M3); `go vet -tags e2e,load,conformance,envtest
./...` so tagged suites compile; OpenAPI bundle drift check; contract extract drift check when the root
checkout is available (checked out with `actions/checkout` of the root repository at `main`).

Coverage: `make cover` gate ≥ 90% for `domain`, `usecase`, `authz` (extended to `usecase/...` subpackages).

## 5. Rollout strategy

- Feature branch `feat/restapi-feature-complete`, one PR at the end of the `implm-frm-prd` run (step 14).
  The OpenAPI document from M0 is announced to `-cli` and `-website` as soon as M0 is pushed (they read it
  from the branch until the PR merges).
- Deploy order per environment (dev → stage → prod): Terraform (RDS, Backup, buckets, IAM), `-db`
  bootstrap roles, Helm release (`afs-api` runs migrations), reconciler, gateway, billing CronJob.
- Gateway rollout: agents get the gateway environment only when the reconciler renders it (M2), and the
  gateway exists from M3; until then pods carry the env but calls fail. The M2 exit task documents this.

## 6. `-db` adoption

- M0-S1: `go get github.com/stainedhead/enterprise-agent-factory-service-db@v0.0.0-20261121202720-b0d8754e4659`
  (pseudo-version of `b0d8754`, migrations 000001–000014). `afs-api` embeds `migrations.FS` and runs
  `migrate.Up`; others check `migrate.Version` ≥ 14.
- When `-db` tags `v0.13.0` at or after `b0d8754`, switch to the tag in the next milestone's first task.
- A later 000015: a task in the milestone that needs it bumps the version, runs `make test-db`, and adjusts
  repositories. Nothing in this spec depends on 000015.

## 7. Success metrics

- All 180 PRD acceptance criteria and the contract-derived criteria (spec §9) covered by passing tests.
- 182 operations implemented; zero placeholders left (M6 exit checks `routes_gen` placeholders = 0).
- NFR targets met in the load suite and restore drill (M6-F).
