# Architectural decision record

Decisions local to this repository. Cross-repo decisions live in the root repository's
`docs/architectural-decision-record.md`. Status values: Accepted, Proposed, Superseded.

## ADR-001 One Go module for all control-plane services

- Status: Accepted (J, 2026-11-20)
- Context: the API, reconciler, gateway and billing job share domain types, authz and the store. The CLI
  and the schema have their own repositories.
- Decision: one module, four `cmd/` entry points, shared `internal/` packages.
- Consequences: one CI and release pipeline; binaries version together.

## ADR-002 Standard library HTTP routing

- Status: Accepted (spec 261121-restapi-feature-complete, A-1, 2026-11-21)
- Context: Go's `net/http` ServeMux supports method and path-parameter patterns.
- Decision: use `http.ServeMux` with a thin middleware chain (request id, auth, authz, logging) instead
  of a router framework.
- Consequences: fewer dependencies; route permissions are declared in our own route table. ServeMux
  cannot match a wildcard followed by `:verb` in one segment (`/v1/teams/{teamId}:suspend`), so the
  router strips the verb, registers the base pattern once and dispatches by verb (`httpapi/router.go`).

## ADR-003 OpenAPI from the contract extract, fragments and a bundler

- Status: Accepted (spec 261121-restapi-feature-complete, A-2, 2026-11-21)
- Context: The API contract is markdown in the root repository. Generating OpenAPI from Go code would make the code the contract; one hand-edited file collides between parallel streams.
- Decision: `tools/contractgen` extracts the contract tables into `api/contract/*.json`; OpenAPI is written as fragments under `api/src/` and merged by `tools/openapibundle` into `api/openapi.yaml`. `api/contract_test.go` checks that operations, methods, paths, permissions and scopes equal the extract, that only contract §6 error codes appear, and that every example validates.
- Consequences: `make contract-check` and `make openapi-check` catch drift in CI; the bundle is a build output that is committed so clients can fetch it.

## ADR-004 Generated route table with placeholders for every operation

- Status: Accepted (spec 261121-restapi-feature-complete, A-3, 2026-11-21)
- Context: Streams build handlers in parallel; adding routes one by one would make the route table a merge hotspot and leave routes without a declared permission.
- Decision: `contractgen` generates `httpapi/routes_gen.go` with all 182 operations. Every operation is registered from M0 with a `501` placeholder (`InternalError`, `details.reason = not_implemented`); a stream binds its handlers through its area registrar.
- Consequences: The route-permission test is green from day one; M6-F1 fails if any placeholder remains.

## ADR-005 Reconciler logic in use cases, Kubernetes rendering in an adapter

- Status: Accepted (spec 261121-restapi-feature-complete, A-4, 2026-11-21)
- Context: controller-runtime reconcilers usually hold the logic, which would pull Kubernetes types into use cases.
- Decision: Controllers live in `usecase/reconcile`; rendering and server-side apply live in `adapter/k8s/render`.
- Consequences: Use cases run against fakes; envtest covers the adapter.

## ADR-006 Third-party custom resources as unstructured objects

- Status: Accepted (spec 261121-restapi-feature-complete, A-5, 2026-11-21)
- Context: Cilium, Karpenter and External Secrets publish Go modules with heavy dependency trees and version coupling.
- Decision: Their custom resources are built and read as `unstructured.Unstructured` against pinned CRDs.
- Consequences: Less type safety, offset by golden-file render tests and envtest with the pinned CRDs.

## ADR-007 One work queue keyed by kind and id

- Status: Accepted (spec 261121-restapi-feature-complete, A-6, 2026-11-21)
- Context: Separate controllers per kind complicate ordering of team deletion.
- Decision: One leader-elected reconciler with a queue keyed by `(kind, id)`, per-kind workers, and team-level ordering for deletes.
- Consequences: Simple ordering and one leader; throughput is bounded by worker counts per kind.

## ADR-008 RDS IAM authentication for database connections

- Status: Accepted (spec 261121-restapi-feature-complete, A-7, 2026-11-21)
- Context: Static database passwords are long-lived credentials.
- Decision: Services sign RDS IAM auth tokens locally (`feature/rds/auth`, not a service client); `postgres.Config.Password` supplies a token for each new connection.
- Consequences: No database password in configuration; the RA-US-5.4 archtest allowlist permits the package.

## ADR-009 Gateway caches desired state

- Status: Accepted (spec 261121-restapi-feature-complete, A-8, 2026-11-21)
- Context: A query per model call cannot meet the < 25 ms p99 overhead budget.
- Decision: The gateway caches team, model, price, budget and tag data, invalidated by `LISTEN afs_desired_state` and a 60 s TTL.
- Consequences: Changes reach the gateway within a notification or at most 60 s; spend reservations are never cached.

## ADR-010 Batched usage writes

- Status: Accepted (spec 261121-restapi-feature-complete, A-9, 2026-11-21)
- Context: An insert per call adds latency and load.
- Decision: Usage rows are batched (at most 1 s or 500 rows) and written with `COPY`.
- Consequences: A crash can lose at most one batch of usage rows; spend counters are settled per call, so budgets stay exact.

## ADR-011 Build-tagged e2e, load, conformance and restore suites

- Status: Accepted (spec 261121-restapi-feature-complete, A-10, 2026-11-21)
- Context: These suites need a cluster and an AWS account, which CI does not have.
- Decision: They carry build tags (`e2e`, `load`, `conformance`) and CI compiles them with `go vet -tags e2e,load,conformance,envtest ./...`.
- Consequences: They cannot rot silently; they run on the dev cluster in M6-F.

## ADR-012 Workload principal from projected ServiceAccount tokens

- Status: Accepted (spec 261121-restapi-feature-complete, M6-D, 2026-11-22)
- Context: Agents and pool workers call the API (for work order progress) and must not hold long-lived keys.
- Decision: `afs-api` validates a projected ServiceAccount token offline against the cluster issuer's JWKS
  (`AFS_CLUSTER_OIDC_ISSUER`): audience exactly `afs-api`, life at most one hour. `afs.resolve_workload` (a
  SECURITY DEFINER function in `-db`) maps namespace and service account to team, agent or pool. The principal holds
  only `work-orders:report`, bounded by `Principal.Workload.TeamID` equal to the assigned team.
- Consequences: no Kubernetes API call on the request path; with no issuer configured, workload tokens are refused.
  A workload cannot read the version it would send in `If-Match`, so `UpdateWorkOrderRepo` accepts a missing
  `If-Match` from a workload only (version 0 means the current version).

## ADR-013 Lookups before team scope use SECURITY DEFINER functions

- Status: Accepted (spec 261121-restapi-feature-complete, M0-F8 and M6-X0, 2026-11-22)
- Context: API key and workload lookup, and the team directory for security operators, run before row-level security
  has a team to scope by.
- Decision: `-db` provides `afs.resolve_workload`, `afs.team_directory` and the credential lookup; `afs-api` calls them
  and no connection uses an elevated login. The interim `TxRunner.Lookup` was removed.
- Consequences: a schema gap is requested from `-db`, never worked around with elevated writes.

## ADR-014 One envelope for outbox events

- Status: Accepted (spec 261121-restapi-feature-complete, M6, 2026-11-22)
- Context: Events written by the API and by database functions had different payload shapes before `-db` 000019.
- Decision: the section 7 envelope `{id, occurredAt, teamId, poolId?, actor, data}` is published as stored;
  `housekeeping.Envelope` wraps a flat payload and gives it a stable id (`evt_` plus the zero-padded outbox id).
- Consequences: consumers see one shape; redelivery keeps the same id.

## ADR-015 Tag sync as a team-level reconcile kind

- Status: Accepted (spec 261121-restapi-feature-complete, M6-X2, 2026-11-22)
- Context: secrets (created by `afs-api`) and pool inbox queues are tagged when created; a later change to team
  or service tags, an agent or pool rename, or a removed tag was not applied to them.
- Decision: a `tag_sync` kind, one resource per team, computes the same effective set the other controllers use and
  applies the difference through a `Tagger` port (Secrets Manager and SQS adapters). A missing resource is skipped;
  `afs-tag-keys` limits removal to the keys AFS applied.
- Consequences: no change to the team, agent or pool controllers; the reconciler role gains `UntagResource` on
  secrets. The platform-owned work order intake queue is not tagged.

## ADR-016 Documentation split by reader

- Status: Accepted (spec 261121-restapi-feature-complete, step 4, 2026-11-22)
- Context: operators, API callers and contributors need different things, and `user-docs/` must not link into specs.
- Decision: `user-docs/` has getting started, the configuration reference, usage examples and one guide per area;
  `docs/` has the product summary and details, the technical details and this record; the README links all of them.
- Consequences: a change of an environment variable or an operation updates `user-docs/configuration.md` or
  `usage-examples.md` in the same change.

## ADR-017 Restore reuses the agents guardrails

- Status: Accepted (spec 261122-restapi-feature-complete-auto-review, stream A)
- Context: `RestoreRevision` wrote agent and binding state straight back, so a model disabled for the team, a retired
  template digest or a foreign secret binding could return through a restore.
- Decision: `agents.Guard` is the single validator, called by `agents.Service` and by `restore.Service` (required; a nil
  source fails closed). A refused step is skipped whole and reported with the guard's name. Only fields a step changes are
  checked, as `UpdateAgent` does.
- Consequences: no duplicated SQL. A `-db` trigger for allowed digests and enabled models as defence in depth is a
  recorded follow-up, not built.

## ADR-018 Idempotency fingerprints use a server HMAC key

- Status: Accepted (spec 261122-restapi-feature-complete-auto-review, streams D and E)
- Context: a bare hash of a secret value in an idempotency record allows offline guessing.
- Decision: fingerprints of secret values are HMAC-SHA-256 under `AFS_IDEMPOTENCY_HMAC_KEY`, resolved once in `wire`.
  The page token key follows the same rule (`AFS_PAGE_TOKEN_KEY`, `components.PageKey`). Both come from a Secret by
  `secretKeyRef`; above one replica a missing or short key stops startup.
- Consequences: replicas must share both keys; a single replica draws random keys, so retries across replicas answer 409.

## ADR-019 Metrics are bound by recorders defined in use cases

- Status: Accepted (spec 261122-restapi-feature-complete-auto-review, streams B and C)
- Context: engine, notify and gateway code must not import the metrics package, and charted metrics had no writer.
- Decision: use cases define small recorder interfaces (`engine.Recorder`, `notify.FailureRecorder`,
  `gateway.SettleRecorder`, `gateway.UsageDropRecorder`); `cmd/*` binds them to `observability.Metrics`. A dead-metric
  test fails the build when a metric in the alert rules or dashboards has no writer.
- Consequences: new alert `AFSGatewaySettleFailures` and a dashboard panel; a new charted metric needs a writer in the
  same change.

## ADR-020 Condition text is sanitised where it is stored

- Status: Accepted (spec 261122-restapi-feature-complete-auto-review, stream C)
- Context: condition messages carry upstream error text, which is untrusted and can hold ARNs, account ids or secrets.
- Decision: `condmsg.Sanitize` runs at the point a condition is written (not at the API edge): scrubber first, then ARNs
  and 12-digit ids, cut to 512 bytes. The full error goes to the log.
- Consequences: every writer of a condition goes through `engine.writeFailure` or `condmsg`; controllers that build a
  `domain.Condition` message directly were not audited and are a follow-up.

## ADR-021 Handlers register through returned Registrars; unbound operations fail startup

- Status: Accepted (spec 261122-restapi-feature-complete-auto-review, stream D)
- Context: package-level `init()` appends and area names let two areas share a name and silently replace each other, and an
  operation without a handler only showed up at request time.
- Decision: `Bind*` returns a `Registrar`; `wire` passes the explicit list in `Config.Registrars`; `httpapi.New` refuses
  to start while any operation is unbound unless `Config.AllowUnbound` (development without a database, and tests).
- Consequences: adding an operation without a handler fails `TestNewRefusesUnboundOperations` and startup.

## ADR-022 Contract extract is pinned to a root contract version

- Status: Accepted (spec 261122-restapi-feature-complete-auto-review, integration)
- Context: the extract was cut from contract 1.1.0 while root `main` was 1.2.0, so the CI contract check was failing,
  and worktrees skipped the check silently because the root was absent.
- Decision: `contractgen` pins the contract version (1.2.0) and skips database-only `unset` settings (CD-8). The check is
  only meaningful with `AFS_ROOT_DIR` pointing at root `main`; run `make contract` after a root contract change.
- Consequences: `-skip-missing` is for local convenience only; CI checks out the root `main`.
