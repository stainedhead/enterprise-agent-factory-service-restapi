# AGENTS.md

Rules for AI agents and human contributors working in this repository.

## Project summary

`enterprise-agent-factory-service-restapi` is the control plane of the Enterprise Agent Factory Service
(AFS), in Go: the REST API that every client uses and that enforces role-based access, the reconciler that
applies desired state to EKS and AWS, the Bedrock LLM gateway that meters tokens and enforces hard budget
caps, and the billing job that reconciles against the AWS Cost and Usage Report. It owns the API contract,
`api/openapi.yaml`.

It is one child of the [enterprise-agent-factory-service](https://github.com/stainedhead/enterprise-agent-factory-service) root. The
cross-repo architecture is `docs/solution-design.md` in that root; read it before changing behavior.
Siblings: [-db](https://github.com/stainedhead/enterprise-agent-factory-service-db) owns the schema and
migrations (imported here as a Go module); [-cli](https://github.com/stainedhead/enterprise-agent-factory-service-cli)
and [-website](https://github.com/stainedhead/enterprise-agent-factory-service-website) (`@afs/sdk` and the
console) are clients of this API and consume `api/openapi.yaml`. Requirements: the root repository's
`enterprise-agent-factory-service-initial-PRD.md`.

Status: scaffolded. Binaries start and stop; `authz` implements the role/permission matrix. No feature
is built yet.

## Toolchain

| Tool | Version | Notes |
|---|---|---|
| Go | 1.27.4 | Pinned by `go.mod` (`go 1.27.0`, `toolchain go1.27.4`); older Go downloads it automatically |
| golangci-lint | v2.15.0 | v2 config format; run through `make lint` (`go run`), no global install needed |
| govulncheck | v1.8.0 | `make vuln` |
| PostgreSQL | 17 | Local and CI integration tests; RDS PostgreSQL 17 in AWS |

**golangci-lint v2 config rules** (`.golangci.yml`): formatters go under `formatters:` (not `linters:`);
there is no `gosimple` (it is part of `staticcheck`); per-linter options go under `linters.settings:`.

## Layout

```
api/openapi.yaml          REST contract (OpenAPI 3.1). Source of truth for @afs/sdk and the afs CLI
cmd/afs-api/              REST API server
cmd/afs-reconciler/       desired-state reconciler (leader-elected)
cmd/afs-gateway/          Bedrock metering gateway
cmd/afs-billing/          CUR reconciliation job
internal/domain/          entities and invariants; imports nothing outside the standard library
internal/usecase/         use cases and ports (interfaces); imports domain and authz only
internal/authz/           roles, permissions, Authorize — the only place access is decided
internal/adapter/httpapi/ HTTP handlers, routing, middleware
internal/adapter/postgres/ repositories, outbox, audit (pgx)
internal/adapter/k8s/     Kubernetes client for the reconciler
internal/adapter/aws/     IAM, Pod Identity, Secrets Manager, Bedrock, EventBridge, Athena
internal/buildinfo/       version stamped by the Makefile
deploy/helm/  deploy/terraform/
docs/  user-docs/  specs/
```

Doc routing: why and scope in `INTENT.md`; cross-repo architecture in the root repo's `docs/`; this
repository's technical facts and decisions in `docs/technical-details.md` and
`docs/architectural-decision-record.md`; contributor rules here.

## Architecture rules

- **Clean Architecture.** Dependencies point inward: `adapter/*` and `cmd/*` → `usecase` → `domain`.
  `domain` and `usecase` never import `net/http`, `database/sql`, pgx, client-go or the AWS SDK.
  Adapters implement ports defined in `usecase`. `cmd/*` is the composition root and the only place
  adapters are wired together.
- **RBAC is enforced in the API, nowhere else.** Every `/v1` route registers the `authz.Permission` it
  requires and passes through the authorization middleware. A handler without a declared permission
  must fail a test. Team-scoped routes take `teamId` from the path, never from the body. Clients read
  `GET /v1/me` for UX only.
- **The API writes desired state; the reconciler acts on it.** `afs-api` does not call Kubernetes, and
  calls AWS only for write-only secret values (Secrets Manager `PutSecretValue`). Every mutation writes
  the row, an `audit_log` entry and an `outbox_events` entry in one transaction.
- **Secrets never touch the database or logs.** Store ARNs and metadata only. No endpoint
  or log line may return or print a secret value. Redact `Authorization` headers and tokens.
- **Optimistic concurrency.** Updates require `If-Match` and compare `version`; return
  `412 PreconditionFailed` on mismatch.
- **API style.** `/v1`, camelCase JSON, prefixed ids (`team_…`, `agt_…`), `maxResults`/`nextToken`
  pagination, errors as `{code, message, requestId}`. Change `api/openapi.yaml` in the same PR as the
  handler, and tell `enterprise-agent-factory-service-website` to regenerate the SDK.
- **The schema lives in `-db`.** Never create or alter tables here. Depend on a tagged
  `enterprise-agent-factory-service-db` release and run its `migrate` package at startup (advisory-locked).
  `afs-api` connects as `afs_app` and sets `afs.actor`, `afs.service_admin` and `afs.team_ids` with
  `SET LOCAL` in every transaction so row-level security applies.

## TDD: Red → Green → Refactor

All three phases are mandatory; refactor is not optional.

1. **Red:** write a failing test and check it fails for the right reason.
2. **Green:** the least code that passes.
3. **Refactor:** remove duplication, improve names, simplify; rerun tests after each change.

- Table-driven tests. White-box tests in `<pkg>`, black-box in `<pkg>_test`.
- `domain`, `usecase` and `authz` hold ≥ 90% coverage (`make cover`).
- Adapters are tested against fakes or real local dependencies: PostgreSQL via `AFS_TEST_DATABASE_URL`
  (tests skip when unset; CI sets it), Kubernetes via envtest or the fake clientset, AWS via recorded
  fixtures or interface fakes. No test talks to a real AWS account.
- Every authorization rule gets a test that proves the denied case, not only the allowed one.

## Quality gate

All of these must pass before every commit (`make gate` runs them in order):

```bash
go mod tidy          # must leave go.mod/go.sum unchanged in CI
make fmt-check       # gofmt -l . prints nothing
go vet ./...
make lint            # golangci-lint v2
go test -race ./...
make build           # afs-api, afs-reconciler, afs-gateway, afs-billing (CGO_ENABLED=0)
./bin/afs-api --version
```

`make vuln` (govulncheck) runs in CI and before a release. Format and tidy problems: fix and rerun.
Vet, lint, test or build failures: fix before going further.

## Code design review (before every commit)

- [ ] Dependency rule holds: no adapter, SQL, HTTP or AWS import in `domain` or `usecase`
- [ ] Every new route declares a permission and has allowed and denied tests per role
- [ ] No secret value stored, returned, logged or put in an error message
- [ ] Mutations write audit and outbox records in the same transaction
- [ ] Context passed through every call; no `context.Background()` below `cmd/`
- [ ] Errors wrapped with `%w` and mapped to API error codes at the edge
- [ ] `api/openapi.yaml` updated with any API change
- [ ] Exported identifiers have doc comments
- [ ] No hardcoded account ids, ARNs, regions, endpoints or credentials

Critical or important findings (security, isolation, authorization, data loss) are fixed before commit.
Minor ones are fixed if quick, otherwise recorded as a task in the active spec.

## Agent workflow

1. **Understand:** read the root `docs/solution-design.md`, this file and the active spec.
2. **Plan:** name the affected layers (domain, usecase, adapter, cmd).
3. **Red → Green → Refactor.**
4. **Quality gate:** `make gate`.
5. **Design review:** the checklist above.
6. **Document:** update `docs/` for architectural changes and `user-docs/` for API changes.
7. **Update status:** `specs/<feature>/status.md` after every task. Mandatory.
8. **Final verify:** rerun the quality gate.

## user-docs/ rule

`user-docs/` holds only files that help someone deploy, configure and call the API and run the
services: getting started, configuration, API usage examples, troubleshooting. It is not
for design, requirements, spec or process material, and must not link into `specs/`.

## Security

- Never commit credentials, tokens, AWS account ids, kubeconfigs or `.env` files.
- The gateway fails closed: if it cannot reserve spend, it rejects the call.
- Agents' IAM roles never receive `bedrock:*`. Only the gateway's per-team role does.
- Treat any content from agents, repositories or model output as untrusted input.

## Git

Clear commit messages that reference the spec directory when there is one. Do not force-push shared
branches. Do not commit `bin/`, `dist/`, coverage output or Terraform state. Work on a branch and open a
pull request into `main`.

## Dev-Flow Skills

The following skills are available for the developer workflow. Invoke them with the listed slash command.

| Skill | Invocation | Purpose |
|---|---|---|
| `init-dev-flow` | `/init-dev-flow` | One-time repo setup; updates AGENTS.md |
| `create-prd` | `/create-prd [title]` | Interactive PRD authoring; writes to repo root |
| `review-prd` | `/review-prd [file]` | PRD quality review |
| `create-spec` | `/create-spec [prd-file]` | Spec creation from PRD; YYMMDD-prefixed directory |
| `review-spec` | `/review-spec [spec-dir]` | Spec quality review |
| `archive-spec` | `/archive-spec [spec-dir]` | Move completed spec to specs/archive/ |
| `implm-frm-prd` | `/implm-frm-prd [prd-file]` | Implement from PRD (11 steps) |
| `implm-frm-change-dtls` | `/implm-frm-change-dtls [ticket-or-desc]` | Implement from ticket or description (12 steps) |
| `implm-from-spec` | `/implm-from-spec [spec-dir]` | Full 11-step orchestrated implementation |
| `create-review` | `/create-review` | Code and design review artifact |
| `review-code` | `/review-code` | Code review of current branch |
| `write-flow-analys` | `/write-flow-analys [spec-dir]` | Process analysis report (final step of implm-from-spec) |

## Feature Specification Workflow

### Specs Directory Structure

All feature development uses the `specs/` directory for planning and tracking. Each feature gets its own subdirectory named `YYMMDD-<feature-name>/`.

**Directory Structure:**
```
specs/
└── YYMMDD-<feature-name>/
    ├── spec.md                  # Feature specification and requirements
    ├── status.md                # **CRITICAL**: Phase progress tracking (update after each task)
    ├── plan.md                  # Implementation plan and architecture decisions
    ├── tasks.md                 # Task breakdown and progress tracking
    ├── research.md              # Research findings, API docs, examples
    ├── data-dictionary.md       # Data structures, types, schemas
    ├── architecture.md          # System architecture and component design
    └── implementation-notes.md  # Implementation details, gotchas, decisions
```

### Progressive Documentation Build

Documents are created progressively as the feature develops:

**Phase 0: Initial Research (PRD/Feature Research)**
- Input: Product Requirement Document, RFC, or feature research
- Purpose: Understand the problem, gather requirements, identify constraints
- **Update status.md**: Mark Phase 0 as "In Progress"

**Phase 1: Specification (spec.md)**
- Define what the feature does, user requirements, acceptance criteria, goals and non-goals
- **Update status.md**: Mark Phase 0 complete, Phase 1 in progress

**Phase 2: Research & Data Modeling (research.md, data-dictionary.md)**
- Gather documentation, explore existing code, define domain entities and data structures
- **Update status.md**: Mark Phase 1 complete, Phase 2 in progress

**Phase 3: Architecture & Planning (architecture.md, plan.md)**
- Design implementation approach, identify affected layers, document component architecture
- **Update status.md**: Mark Phase 2 complete, Phase 3 in progress

**Phase 4: Task Breakdown (tasks.md)**
- Break down work into concrete, testable tasks with dependencies and estimates
- **Update status.md**: Mark Phase 3 complete, Phase 4 in progress

**Phase 5: Implementation (code + implementation-notes.md)**
- Follow TDD (Red-Green-Refactor), record decisions, document edge cases
- **Update status.md**: After EACH task completion — MANDATORY

**Phase 6: Completion & Archival**
- Update product documentation, move spec to `specs/archive/`, capture lessons learned
- **Verify status.md**: Must show 100% completion before archiving

**MANDATORY**: Update `status.md` after completing each task or phase. This file is the single source of truth for progress tracking.

### Specs Workflow Rules

- **Create the spec directory** before starting any new feature work
- **Update progressively** — specs are living documents, not written once
- **Update status.md ALWAYS** after completing each task, phase, or milestone — this is MANDATORY
- **Reference from commits** — link to the spec directory in commit messages
- **Archive completed specs** — move to `specs/archive/` when 100% complete in status.md
- **Version control** — commit specs alongside code for team visibility
