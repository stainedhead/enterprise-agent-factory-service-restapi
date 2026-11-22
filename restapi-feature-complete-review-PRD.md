# Review PRD: restapi feature-complete review fixes

| | |
|---|---|
| Repository | `enterprise-agent-factory-service-restapi` |
| Branch | `feat/restapi-feature-complete` |
| Reviewed commit | `9454fd5` (Step 4 of the dev-flow run) |
| Source spec | `specs/261121-restapi-feature-complete/` |
| Produced by | dev-flow `implm-frm-prd` Step 5 (code and design review), finalized in Step 6 (`review-prd`) |
| Date | 2026-11-22 |
| Status | Ready for spec (Step 8 turns this document into `specs/YYMMDD-restapi-feature-complete-auto-review/`) |

## 1. Executive summary

**Overall assessment: request changes.** There is one P0 finding, six P1 findings and ten P2 findings.

The code base is in good shape for its size: about 79,000 lines of non-test Go across the four binaries,
and every one of the 182 contract operations is bound. The structural rules hold, and tooling confirms it:

- **Dependency rule.** `go list -deps` on every package under `internal/domain/...` and `internal/usecase/...`
  shows no `net/http`, `database/sql`, pgx, client-go, AWS SDK or `internal/adapter` import.
- **Authorization.** Each `/v1` route takes its permission from the generated route table
  (`internal/adapter/httpapi/routes_gen.go`). The only routes without a permission are `GetMe` (any
  signed-in caller), `ListSignInProviders`, `/healthz` and `/readyz`. Team ids come from the path. A body
  `teamId` that differs from the path is a 400 (`decode.go:38-44`). The oracle-based matrix
  (`cmd/afs-api/matrix_test.go`) runs every operation against 10 caller types, workload principals and
  security operators included, and fails if any operation still answers the 501 placeholder.
- **Tenant isolation.** Every afs-api transaction goes through `postgres.TxRunner`, which applies
  `afs.actor`, `afs.service_admin`, `afs.team_ids` and `afs.security_operator` with
  `set_config(..., true)` before the use case runs (`internal/adapter/postgres/tx.go:84-101`,
  `session.go:15-47`).
- **Secret handling.** `domain.SecretValue` redacts itself under every formatter and refuses to be
  marshalled (`internal/domain/secret.go:47-66`). Idempotent replays remove `secretKey`
  (`internal/adapter/postgres/idempotency.go:89`). The log handler scrubs tokens, keys and JWTs
  (`internal/adapter/observability/redact.go`).
- **Gateway.** The gateway fails closed when it cannot reserve spend, when token review fails and when
  the pod directory is not synchronised (`internal/usecase/gateway/service.go:195-238`, `identity.go:23-56`).
- **Quality gates.** `go vet ./...` is clean, `gofmt -l .` prints nothing, and every non-fake package
  under `domain`, `usecase` and `authz` has at least 90.0% coverage (the lowest are `usecase/catalogsystems`
  and `usecase/exports`, both at exactly 90.0%).

The problems are at the edges, where an invariant enforced in one path is skipped in another:

- **P0:** `RestoreRevision` writes agent rows straight from old revisions and skips the use-case checks.
  A team admin can use it to put back an image digest, template or model that a service admin has since
  retired.
- **P1:**
  - Mutation audit records log the load balancer's address instead of the client's.
  - The public `/readyz` returns raw driver errors.
  - The gateway does not cap output on requests that omit a token limit.
  - A failed settlement loses the actual spend.
  - Several alerting metrics are never recorded.
  - The default Helm deployment breaks pagination across replicas.

None of these needs a redesign. Each one has a local fix and a test that proves it.

## 2. Problem, goals and non-goals

**Problem.** The feature-complete control plane enforces RBAC, tenant isolation, the image and model
guardrails and the hard budget cap on its main paths. The review found side paths and operational gaps
where those promises do not hold or cannot be observed. These must be closed before the branch merges
to `main` and is deployed to the dev cluster.

**Goals.**

1. Every guardrail enforced on a direct mutation is also enforced on restore (R-01).
2. Audit records name the real client (R-02), and an unauthenticated caller learns nothing about the
   infrastructure (R-03).
3. The gateway's spend accounting matches the contract's worst-case bound and never silently loses
   cost (R-04, R-05).
4. Every alert in the shipped `PrometheusRule` can fire (R-06), and the default chart works with
   3 replicas (R-07).
5. The P2 hardening items are fixed or explicitly accepted with a recorded rationale.

**Non-goals.**

- New features or contract operations.
- Changing the permission catalog or the root contract's role model (R-16 asks for contract text, not
  new roles).
- The infrastructure-bound items in section 7. They stay open in the original spec's `tasks.md`.
- Performance tuning beyond the timeouts in R-09.

## 3. Review scope and method

The review followed `review-code` for every dimension: correctness, spec alignment, design and patterns,
security, error handling, test coverage and readability. It covered every package in `internal/` and `cmd/`
and the Helm chart in `deploy/helm/afs`. It read code and ran targeted tools instead of the full suites,
because `make gate` was green at `9454fd5`. The tools:

- `go list -deps` for the dependency rule
- `go vet ./...` and `gofmt -l .`
- `go test -cover` on `domain`, `usecase` and `authz`
- greps for `context.Background`, `501`, `TODO`, `placeholder` and `NotImplemented`
- two scratch scripts: one lists use-case mutations that write no audit or outbox record, the other lists
  use cases that take a `domain.Version` and never compare it

Results that are not findings:

- **Outbox.** Mutations without an outbox row are the CD-7 exemptions (members, API keys, settings,
  catalog, config, schedules, swarm templates). Every event type in API contract §7 has an emitter.
- **Versions.** Every use case that takes a `domain.Version` compares it.
- **`context.Background()` below `cmd/`.** Outside test helpers, it appears only in
  `internal/adapter/postgres/usage_writer.go:180` (R-08).
- **501 placeholders.** The placeholder mechanism remains only as a fallback, and the matrix test
  guards it (R-15).
- **Test-only packages.** `matrix`, `pgtest`, `envtesting`, `fakebedrock`, `awstest`, `gwtest` and the
  `*test` fakes are not linked into any binary (`go list -deps ./cmd/...`).

## 4. Strengths (keep)

- **One authorization decision point.** `authz.Check` is the only access decision. Routes and use cases
  call it, and the workload principal is limited to `work-orders:report` on its own team's work orders
  (`internal/authz/catalog_gen.go:250`).
- **Workload tokens.** Verification checks exactly one audience, a lifetime of at most 1 hour and
  agreement between the subject and the `kubernetes.io` claims (`internal/adapter/oidc/cluster.go:62-140`).
- **Secrets Manager writes.** These are idempotent through `ClientRequestToken`. A failed database write
  is compensated with an immediate delete, or tagged for the orphan sweep and counted
  (`internal/usecase/secrets/service.go:127-210`).
- **Strict input handling.** Unknown query parameters and unknown body fields are refused. Body limits
  apply per operation. Page tokens are signed and bound to the operation and caller.
- **Defence in depth.** Budget ceiling and notify-only guards exist in the database
  (`-db` 000013 `budgets_exhaust_guard`, `check_budget_ceiling`).

## 5. Functional requirements (one per finding)

Each requirement names its priority, its evidence (`file:line` at `9454fd5`) and testable acceptance
criteria. Priorities: **P0** is a true blocker (a security, isolation or data-integrity defect that must
not merge), **P1** is high (fix in this cycle), **P2** is medium (fix if cheap, or record as an accepted
task with a rationale).

### P0

#### FR-R01 (P0) RestoreRevision must enforce the agent guardrails that UpdateAgent enforces

*Evidence.*

- `internal/usecase/restore/service.go:63-140` authorizes `revisions:restore` and then calls
  `Repo.Apply` for every planned step.
- `internal/adapter/postgres/restore.go:180-230` writes each agent row with
  `UPDATE afs.agents ... FROM jsonb_populate_record(NULL::afs.agents, $2)`, which covers the fields in
  `internal/domain/revision.go:66-68` (`template`, `image`, `model_id`, `resources`, `state` and others).
- Nothing on this path calls the checks that `CreateAgent` and `UpdateAgent` run:
  - `checkTemplate`, which requires an enabled template and an image digest in `AllowedDigests`
    (`internal/usecase/agents/service.go:142-158`)
  - `checkModel`, which requires a model enabled for the team (`:160-169`)
  - the storage class check (`:225-229`)
  - `checkBindings` (`:173-200`, called at `:234` and `:302`)
- The database does not hold these rules either. The `agents` foreign key only requires a `team_models`
  row (`-db` `000001_core_schema.up.sql:233`), and no table or trigger knows about allowed digests. The
  reconciler keeps a retired image running and only sets `TemplateRetired`
  (`internal/usecase/reconcile/agent/controller.go:252-259`).
- No restore test covers a template, digest or model guard.

*Why P0.* A team admin can restore a revision from before a service admin retired an image digest (for
example after a CVE) or disabled a model. The restore points the agent back at it, and the reconciler
deploys it. That bypasses a service-admin guardrail through the API. Budgets are not affected, because
database triggers guard them.

*Acceptance criteria.*

1. An adapter-level test (`[adapter]`, PostgreSQL) does the following:
   - team admin creates an agent on template digest D1
   - service admin removes D1 from the template's allowed digests and the agent moves to D2
   - team admin calls `RestoreRevision` to the earlier revision

   The agent keeps image D2. The response lists the agent change in `skipped` with a reason that names
   the template guard. The audit record counts it as skipped.
2. The same holds when the restore would switch `template` to a disabled template, `model_id` to a model
   that is not enabled for the team, or the volume storage class to one that is not offered.
3. `dryRun: true` reports the same skips and writes nothing.
4. A restore that only changes fields with no guard (description, persona, snapshot settings) still
   applies them. RA-US-14.6 stays green for revisions that pass the guards.
5. The validation lives in one place that both the agents use case and the restore use case call. It is
   not duplicated SQL. Both paths have table-driven tests for each refused case.

### P1

#### FR-R02 (P1) Mutation audit records must use the trusted client address

*Evidence.*

- `internal/adapter/httpapi/h2_common.go:36-49` (`w2Caller`) takes `SourceIP` from `r.RemoteAddr`. Its
  comment says "a trusted proxy header is not honored here". All 134 handlers that build a caller go
  through it.
- The denied-request auditor uses `MetaFrom(ctx).IP` instead
  (`internal/adapter/httpapi/middleware/deniedaudit.go:50`). That address respects
  `AFS_TRUSTED_PROXY_HOPS` (`middleware/middleware.go:65-100`).
- Behind the ALB, every allowed mutation's `audit_log.source_ip` is the load balancer's address. Denied
  records show the client. Forensics across the two kinds of record are inconsistent and the allowed
  ones are useless.

*Acceptance criteria.*

1. `w2Caller` takes the client address from the request metadata middleware, so allowed and denied
   records agree.
2. A table-driven test sends a mutation with `X-Forwarded-For: 203.0.113.7, 10.0.0.5`, using hops 0, 1
   and 2, and asserts the recorded `SourceIP` (`10.0.0.5`-peer, `10.0.0.5`, `203.0.113.7`). Allowed and
   denied requests must record the same value.
3. With hops 0, a client-supplied `X-Forwarded-For` never changes the recorded address.

#### FR-R03 (P1) The public readiness endpoint must not return dependency error text

*Evidence.*

- `internal/adapter/httpapi/server.go:146-163` writes `fmt.Sprint(err)` into the `/readyz` body.
- `cmd/afs-api/wire.go:173-174` makes that error the raw `pool.Ping` error. pgx connect errors include
  the database user, the database name, the host and the address.
- `GetReadiness` is a public route of the API listener. The operations-port checks already avoid this
  ("database: not reachable", `wire.go:161-167`).

*Acceptance criteria.*

1. `/readyz` on the API listener answers `{"status":"not ready","checks":{"database":"unavailable"}}`
   (or another fixed word) when the check fails, and the cause is logged with the request id.
2. A unit test that injects a check error with the text `failed to connect to user=afs_app host=10.1.2.3`
   asserts the response body contains neither `afs_app` nor `10.1.2.3`.
3. The operations-port `/readyz` behaviour does not change.

#### FR-R04 (P1) The gateway must cap output on every request, including those without a token limit

*Evidence.*

- `internal/adapter/gatewayhttp/wire.go:102-114` (`parseConverse`) and `:117-131` (`parseInvoke`) leave
  `maxTokens = 0` and `capPath = nil` when the body names no limit.
- `internal/usecase/gateway/service.go:132-133` reserves for `OutputCap(0, modelMax)`, which is the
  model's `max_output_tokens` or 8192.
- `upstreamBody` (`wire.go:165-174`) only rewrites the limit when `p.maxTokens > a.MaxOutputTokens`, so a
  request without a limit goes upstream without one. Bedrock then applies the model's own maximum, which
  can be many times the reserved worst case.
- Gateway contract §4 bounds overspend to "a call's actual cost exceeds its reserved worst case". Here
  the overspend is systematic, and a pool with many workers multiplies it.
- `server_test.go` has no case with the limit absent.

*Acceptance criteria.*

1. A Converse or ConverseStream request without `inferenceConfig.maxTokens` reaches Bedrock with
   `inferenceConfig.maxTokens` set to the reserved cap. An existing `inferenceConfig` object keeps its
   other members.
2. An Invoke request for a model family whose limit field the gateway knows gets that field set to the
   cap when it is absent. An Invoke request for a family whose limit the gateway cannot set is refused
   with 400 `ValidationError` naming the missing limit. It is not forwarded uncapped.
3. Tests in `internal/adapter/gatewayhttp` cover: limit absent, limit below the cap (unchanged), limit
   above the cap (lowered), for Converse, Invoke and Anthropic Messages.
4. The reserved amount and the forwarded limit are equal in every case, and a test asserts it.

#### FR-R05 (P1) A failed settlement must not lose the actual spend

*Evidence.*

- `internal/usecase/gateway/service.go:258-279` retries `Settle` three times within about 0.6 s and then
  only logs "settle reservation failed; the hold will expire".
- The reconciler releases expired holds without adding cost (`afs.release_expired_reservations`,
  database contract §4.2). A database blip after a completed call therefore takes the call's actual cost
  out of `spent_usd` for good, and the budget cap stops reflecting real spend.
- The only signal is `afs_gateway_reservations_expired_total`, which also counts gateway crashes. No
  metric counts settle failures.

*Acceptance criteria.*

1. When settlement fails after its retries, the gateway keeps a durable or retried record of
   `(holdID, budgetID, period, actual)` and settles it later. The settlement is idempotent:
   `afs.settle_reservation` already adds the cost to a released hold.
2. A use-case test with a fake `Spend` that fails N times and then succeeds proves the period's spent
   amount ends up with the actual cost.
3. A new counter, `afs_gateway_settle_failures_total`, is incremented for every settlement that falls
   back, and the chart's `PrometheusRule` alerts on it.
4. On shutdown, pending settlements are flushed before exit, and a test proves it with `Close`.

#### FR-R06 (P1) Every metric that an alert or dashboard reads must be recorded

*Evidence.*

- `internal/adapter/observability/metrics.go:59-62,73` registers these metrics:
  - `afs_reconcile_duration_seconds`
  - `afs_reconcile_errors_total`
  - `afs_reconcile_lag_seconds`
  - `afs_drift_corrected_total`
  - `afs_notification_failures_total`
- Nothing in `cmd/` or `internal/` ever observes or increments them. A grep for the fields outside
  `metrics.go` returns nothing.
- `deploy/helm/afs/templates/prometheusrule.yaml:55`, `:61` and `:99` alert on three of them, and
  `dashboards/afs-slis.json:205,370` charts them. Those alerts can never fire.

*Acceptance criteria.*

1. The reconcile engine records duration and errors per kind and the lag from change to observed
   generation. The drift recorder increments `afs_drift_corrected_total{kind}`. The notification drainers
   increment `afs_notification_failures_total{channel_type}`.
2. A test in `cmd/afs-reconciler` (or the engine package, through a recorder interface) runs one
   successful and one failing reconcile and asserts both series change.
3. A test parses `prometheusrule.yaml` and the dashboard and checks that every metric name they
   reference is registered and has at least one writer (a list kept in the test). That check fails the
   build when an alert targets a dead metric.

#### FR-R07 (P1) The default deployment must give every replica the same page-token key

*Evidence.*

- `cmd/afs-api/wire_m1w2.go:57-67` falls back to a random per-process key when `AFS_PAGE_TOKEN_KEY` is
  unset, and only logs a warning.
- The chart runs 3 replicas (`deploy/helm/afs/values/api.yaml:2`) but never sets the variable
  (`templates/api-deployment.yaml:39-58`).
- A `nextToken` from one replica is refused as "invalid or expired" by the others, so CLI
  auto-pagination and console paging fail about two times in three for any list longer than one page.

*Acceptance criteria.*

1. The chart injects `AFS_PAGE_TOKEN_KEY` from a Kubernetes Secret, referenced by name and never inlined.
   A Helm golden test shows the `secretKeyRef`.
2. afs-api refuses to start when `AFS_API_REPLICAS > 1` and the key is missing or shorter than 32 bytes.
   A `settingsFrom` table test covers this, and the key is read in `settingsFrom`, not in `wireM1W2`.
3. An e2e test in `cmd/afs-api` builds two handlers from the same settings, takes a `nextToken` from
   handler 1 and redeems it on handler 2.

### P2

#### FR-R08 (P2) Usage rows dropped by the gateway must be counted and the writer must take a context

*Evidence.*

- `internal/adapter/postgres/usage_writer.go:68-73` drops a row when the queue is full, and `:178-191`
  drops a batch after its attempts. Both only update in-memory counters (`Dropped`, `Failed`), which
  nothing exports.
- `:180` creates `context.Background()` below `cmd/`, against the context rule in `AGENTS.md`.

*Acceptance criteria.*

1. `afs_gateway_usage_rows_dropped_total{reason="queue_full"|"write_failed"}` is exported and tested.
2. `Run` takes a parent context that the composition root provides. The per-batch timeout derives from
   it (with `context.WithoutCancel` for the final flush on shutdown). There is no `context.Background()`
   in the adapter.

#### FR-R09 (P2) The afs-api HTTP server must bound slow requests

*Evidence.* `cmd/afs-api/main.go:62-66` sets only `ReadHeaderTimeout`. There is no `ReadTimeout` or
`IdleTimeout`, so a client can trickle a 100 MB `CreateImport` body (`decode.go:18-20`) and hold the
connection indefinitely.

*Acceptance criteria.*

1. `ReadTimeout` and `IdleTimeout` are set from settings with documented defaults (`user-docs/configuration.md`).
   The read timeout is long enough for a 100 MB import on the documented link speed, or `CreateImport`
   gets a per-route deadline through `http.ResponseController`.
2. A test with a deliberately slow body asserts the server closes the connection after the configured
   timeout.

#### FR-R10 (P2) Startup migrations must authenticate under RDS IAM auth

*Evidence.* `cmd/afs-api/wire.go:125-131` calls `postgres.Migrate(ctx, s.OwnerURL, s.MigrationTimeout, nil)`
before the IAM password function is built (`:132-142`). With `AFS_DB_IAM_AUTH=true` and
`migrate.ownerUser` set in the chart, the owner connection has no password and startup fails.

*Acceptance criteria.*

1. With IAM auth enabled, `Migrate` receives `postgres.IAMPassword(OwnerURL, region, creds)`.
2. A `wire` unit test with a fake credentials provider asserts the password function is passed.
3. `user-docs/configuration.md` says which combinations are supported.

#### FR-R11 (P2) Configuration must be read and validated in one place

*Evidence.*

- `cmd/afs-api/wire_m1w2.go:60` (`AFS_PAGE_TOKEN_KEY`), `wire_identity.go:50` (`AFS_AWS_ACCOUNT_ID`) and
  `wire_kubeconfig.go:19-20` (`AFS_CLUSTER_*`) call `os.Getenv` outside `settingsFrom`, so a bad value is
  never reported at startup.
- `AFS_MAX_BODY_BYTES` is set in `deploy/helm/afs/values/api.yaml` but no code reads it (body limits are
  the constants in `decode.go:15-20`).

*Acceptance criteria.*

1. Every `AFS_*` variable afs-api reads is a `settings` field parsed and validated in `settingsFrom`, with
   a table test per variable.
2. `AFS_MAX_BODY_BYTES` is either read and applied as the default body limit, or removed from the chart
   and the docs.
3. A test fails when `cmd/afs-api` calls `os.Getenv` outside `main.go` and `settingsFrom`.

#### FR-R12 (P2) Use-case denials must record their audit reliably

*Evidence.* `internal/usecase/acting/acting.go:57-62` (`Deps.Denied`) drops the `WriteDenied` error
(`_ =`) and uses the request context, so a client that disconnects can cancel the denial record.
The middleware path logs the failure (`middleware/deniedaudit.go:56-58`).

*Acceptance criteria.*

1. `Denied` writes with `context.WithoutCancel(ctx)` and logs a failure with the operation and request id
   (through a logger in `Deps`), and the call still returns `AccessDenied`.
2. A use-case test with a failing fake audit writer asserts `AccessDenied` is returned and the failure is
   logged. A test with a cancelled context asserts the record is still written.

#### FR-R13 (P2) Reconcile status conditions must not expose raw upstream error text

*Evidence.* The following write `err.Error()` from AWS and Kubernetes clients straight into status
conditions that team viewers read through the API:

- `internal/usecase/reconcile/engine/engine.go:312,314`
- `internal/usecase/reconcile/tagsync/tagsync.go:157`
- `internal/usecase/reconcile/lifecycle/finalizer.go:159-161`

AWS `AccessDenied` messages carry account ids and the reconciler's role ARN. The text is neither
scrubbed nor bounded. Notification errors already go through `RedactURLs` and truncation
(`reconcile/notify/jobs.go:318`).

*Acceptance criteria.*

1. Condition messages pass through one sanitiser that removes ARNs and account ids and runs the
   observability scrubbers, and they are cut to a fixed length (for example 512 bytes). The full error
   goes to the reconciler log.
2. A table test feeds an AWS-style `AccessDenied` message with an ARN and asserts the stored message has
   no 12-digit account id.

#### FR-R14 (P2) The idempotency fingerprint of a secret request must not be derivable from the value

*Evidence.*

- `internal/adapter/httpapi/dto_secrets.go:137-152` adds `valueTag`, the first 4 bytes of an unsalted
  `sha256("afs-idem-value|" + value)`, to the body hashed into `idempotency_keys.request_sha256`.
- Someone with read access to the database who knows the other fields can recover the tag in 2^32 steps
  and then test dictionary guesses of weak secret values offline.
- When the body does not parse, `idemBody` returns `nil`, so every malformed body shares one hash.

*Acceptance criteria.*

1. The value's contribution is an HMAC-SHA-256 under a server key (from settings, shared by replicas),
   never a plain hash.
2. An unparsable body is refused with 400 before the idempotency claim. A test covers it.
3. A test asserts the stored `request_sha256` differs between two servers with different keys for the
   same request.

#### FR-R15 (P2) Handler binding must not depend on mutable package state

*Evidence.*

- `internal/adapter/httpapi/handlers.go:51-58` declares a package-level `Areas` slice.
- Wiring code changes it (`h2_common.go:23-31` `w2SetArea`, `h_schedules.go:24-25`) and `init()`
  functions append to it (`h_pricing.go:14`, `h_restore.go:12`, `h_secops.go:17`).
- `router.go:125-139` keeps a 501 placeholder for any operation left unbound, and `New` does not refuse
  that outside tests.
- Two handlers built in one process share the mutated slice, and a missing registrar is only caught by
  the database-backed matrix test.

*Acceptance criteria.*

1. `wire` passes an explicit list of registrars in `httpapi.Config`. The package-level `Areas`, `w2SetArea`
   and the `init()` appends are removed.
2. `httpapi.New` returns an error when an operation is unbound and the config does not opt into the
   development fallback (`AllowUnbound`), and a unit test proves it without a database.
3. The matrix test stays green.

#### FR-R16 (P2) Record the open contract decisions in the root contract

*Evidence.* The following decisions are recorded only in this repository:

- **CD-7.** No outbox row for mutations that have no §7 event type, against CT-API-8 "every mutation
  writes ... one outbox event" (`specs/261121-restapi-feature-complete/spec.md:671`).
- **CD-9.** One `client_id` per identity provider, while the contract has separate console and CLI ids.
- **CD-12.** A workload principal may omit `If-Match` on `UpdateWorkOrderRepo`.

They are in `specs/261121-restapi-feature-complete/implementation-notes.md:281,283,286`. The root
`docs/contracts/api-contract.md` does not state them, and `-cli` and `-website` read the root.

*Acceptance criteria.*

1. A root-repository PR (the contract change process in API contract §8) records each of the three
   decisions, or changes the code to match the contract. This repository's `AGENTS.md` line "every
   mutation writes ... an outbox_events entry" is aligned with the outcome.
2. The matrix `NoOutbox` list matches the contract's exemption list. A test compares them.

#### FR-R17 (P2) Do not discard marshalling errors in the import writer

*Evidence.* `internal/adapter/postgres/imports_repo.go:91` and `:114` use `raw, _ := json.Marshal(doc)`.
A value that cannot be marshalled turns into an empty `$1` and inserts or updates a row with NULLs.

*Acceptance criteria.*

1. Both call sites return the error wrapped with `%w` and the table name.
2. A unit test with a value that cannot be marshalled (for example a channel) asserts the import step
   fails and writes nothing.

## 6. Non-functional requirements for the fix cycle

- **Security.**
  - FR-R01 to FR-R03 and FR-R14 must each carry a test that proves the refused or redacted case, not
    only the allowed one (AGENTS.md TDD rule).
  - No fix may log, return or store a secret value, token or account id.
- **Reliability.**
  - FR-R05 and FR-R08 must not add latency to the gateway's response path. Settlement and usage writes
    stay off the path when `AsyncSettle` is on (GW-8, < 25 ms added p99).
  - The existing gateway load test (`make load`) must still pass.
- **Observability.**
  - Every new failure path has a counter and an alert or a documented reason for none.
  - FR-R06's dead-metric check runs in `go test ./...`.
- **Performance.**
  - FR-R01's restore validation reuses the agents use-case checks inside the restore transaction. No N+1
    query per field.
  - A restore of 200 agents still finishes within the existing statement timeout (30 s).
- **Quality.**
  - `make gate` passes after every stream merge.
  - `domain`, `usecase` and `authz` keep at least 90% coverage.
  - `go vet -tags e2e,load,conformance ./test/...` still compiles.

## 7. Deferred items (not findings)

These are open in `specs/261121-restapi-feature-complete/tasks.md` and need infrastructure the build
sandbox does not have. They are not part of this fix cycle and must not be marked done by it:

| Task | What it needs |
|---|---|
| M2-F8 | `[e2e]` case on the dev cluster: an image with no AFS code receives every variable and file |
| M6-F3 | `[e2e]` suite on the dev cluster with real AWS |
| M6-F4 | Restore drill: fresh cluster plus RDS point-in-time restore converges within 1 hour |
| M6-F5 | Load at initial scale (200 teams, 2,000 agents) and reconcile lag p95 |
| M6-F6 | OWASP ASVS L2 checklist and `govulncheck` (toolchain download blocked here; CI runs `make vuln`) |

## 8. Fix streams (parallel work with git worktrees)

The findings fall into five independent streams. Each stream runs in its own git worktree on its own
branch, cut from `feat/restapi-feature-complete`. One agent teammate owns each stream and only edits the
files listed for it. A stream that needs a file another stream owns asks the owner, or waits for the
owner's merge.

| Stream | Findings | Files owned | Order |
|---|---|---|---|
| A: Restore guardrails and audit identity | FR-R01 (P0), FR-R02, FR-R12 | `internal/usecase/restore/`, `internal/adapter/postgres/restore.go`, `internal/domain/revision.go`, `internal/usecase/agents/` (validation extracted for reuse), `internal/adapter/httpapi/h2_common.go` (`w2Caller` only), `internal/usecase/acting/acting.go` | Start first, merge first (holds the P0) |
| B: Gateway spend integrity | FR-R04, FR-R05, FR-R08 | `internal/adapter/gatewayhttp/wire.go`, `server.go` and tests, `internal/usecase/gateway/`, `internal/adapter/postgres/usage_writer.go`, `spend.go`, `cmd/afs-gateway/` | Parallel. Uses metrics that stream C adds; wires them in `cmd/afs-gateway` after C merges |
| C: Observability | FR-R06, FR-R13, and the counters for FR-R05 and FR-R08 | `internal/adapter/observability/`, `internal/usecase/reconcile/engine/`, `reconcile/tagsync/`, `reconcile/lifecycle/finalizer.go`, `reconcile/notify/`, `cmd/afs-reconciler/`, `deploy/helm/afs/templates/prometheusrule.yaml`, `deploy/helm/afs/dashboards/` | Parallel. Merges before B's wiring commit |
| D: afs-api runtime and configuration | FR-R03, FR-R07, FR-R09, FR-R10, FR-R11, then FR-R15 | `cmd/afs-api/main.go`, `wire.go`, `wire_m1w2.go`, `wire_identity.go`, `wire_kubeconfig.go`, `internal/adapter/httpapi/server.go`, `router.go`, `handlers.go`, `h_pricing.go`, `h_restore.go`, `h_secops.go`, `h_schedules.go`, `deploy/helm/afs/templates/api-deployment.yaml`, `deploy/helm/afs/values/`, `user-docs/configuration.md` | FR-R03 and FR-R07 first. FR-R15 last, after A merges (it touches `h2_common.go` `w2SetArea`) |
| E: Secrets fingerprint, imports and contract | FR-R14, FR-R17, FR-R16 | `internal/adapter/httpapi/dto_secrets.go`, `h_secrets.go`, `internal/adapter/postgres/imports_repo.go`, `AGENTS.md` (outbox line), the root repository's `docs/contracts/api-contract.md` (separate PR in the root) | Parallel. FR-R14's server key comes through D's `settings`; agree on the field name before starting |

Merge order: A, then C, then B and D (D's FR-R15 after A), then E. After each merge, rebase the remaining
worktrees and run `make gate`.

## 9. Implementation guidance

- **TDD for every fix (Red, Green, Refactor).**
  - Write the failing test from the acceptance criteria first and check it fails for the stated reason.
  - Make the smallest change that passes, then refactor with the tests green.
  - Security fixes need the denied, refused or redacted case as a test.
  - Adapter fixes run against PostgreSQL through `AFS_TEST_DATABASE_URL`, `httptest` or fakes. No test
    talks to real AWS.
- **A code review per fix.** Before committing each fix, run the `AGENTS.md` design checklist:
  - dependency rule holds
  - permission and denied tests are in place
  - no secret value appears
  - audit and outbox records are written in the same transaction
  - context is passed through
  - errors are wrapped with `%w`
  - `api/openapi.yaml` is updated if the API changed
  - doc comments are present
  - nothing is hardcoded

  Record the review outcome in the fix spec's `implementation-notes.md`. Critical or important findings
  in the review are fixed before the commit.
- **Agent teammates.** One teammate per stream in section 8, each working only in its own files. The
  orchestrating agent tracks the streams in the review spec's `status.md` and resolves cross-stream
  requests. Each teammate updates `status.md` after every task (mandatory).
- **Git worktrees.**
  - Create one worktree per stream from `feat/restapi-feature-complete`, for example:

    ```bash
    git worktree add ../restapi-fix-a -b fix/review-a feat/restapi-feature-complete
    ```

  - Merge back in the order of section 8 and remove the worktree after the merge.
  - Never force-push the shared branch.
- **Quality gate.** `make gate` in each worktree before every commit, and again on the integration
  branch after each merge. `make cover` stays at or above 90% for `domain`, `usecase` and `authz`.
- **Commits.** Reference the review spec directory created in Step 8 in every commit message.

## 10. Dependencies

- `enterprise-agent-factory-service-db`: no schema change is required. FR-R01 relies on the existing
  `agent_templates` and `team_models` tables. If the fix prefers a database guard for allowed digests, that
  is a new `-db` migration and release, which is out of scope for this cycle unless the team chooses it
  (open question Q1).
- Root repository: FR-R16 needs a contract PR in `enterprise-agent-factory-service`
  (`docs/contracts/api-contract.md`).
- `-cli` and `-website`: no API shape changes are expected. FR-R04's new 400 for an uncappable Invoke
  family is a gateway response that agents see. It does not affect the SDK or CLI.

## 11. Open questions

| # | Question | Default if unanswered |
|---|---|---|
| Q1 | FR-R01: enforce allowed digests in the use case only, or also add a `-db` trigger as defence in depth? | Use case only in this cycle; record a `-db` follow-up |
| Q2 | FR-R05: persist failed settlements (a table in `-db`) or retry in memory with a bounded queue flushed on shutdown? | Bounded in-memory retry plus the new counter and alert. A crash still falls back to hold expiry, which the contract allows |
| Q3 | FR-R04: refuse an Invoke request whose model family limit field is unknown, or forward it with a reservation at the model's documented maximum? | Refuse with 400 (fail closed) |
| Q4 | FR-R16: should members, API keys and settings get event types (contract change), or should CT-API-8 list them as exempt? | List them as exempt in the contract |

## 12. Finding index

| ID | Priority | Area | One line |
|---|---|---|---|
| FR-R01 | P0 | Authorization and guardrails | `RestoreRevision` skips template, digest, model and storage class checks on agents |
| FR-R02 | P1 | Audit | Mutation audit `source_ip` is the proxy, not the client |
| FR-R03 | P1 | Information disclosure | Public `/readyz` returns raw pgx errors |
| FR-R04 | P1 | Budget cap | Requests without a token limit go to Bedrock uncapped |
| FR-R05 | P1 | Budget cap | Failed settlement drops the actual cost |
| FR-R06 | P1 | Observability | Five metrics never recorded; three alerts can never fire |
| FR-R07 | P1 | Correctness | Default chart breaks pagination across replicas |
| FR-R08 | P2 | Observability | Dropped usage rows not exported; `context.Background` in adapter |
| FR-R09 | P2 | Hardening | No `ReadTimeout` or `IdleTimeout` on afs-api |
| FR-R10 | P2 | Deployment | Startup migration ignores IAM auth |
| FR-R11 | P2 | Configuration | `os.Getenv` outside `settingsFrom`; dead `AFS_MAX_BODY_BYTES` |
| FR-R12 | P2 | Audit | Use-case denial audit errors dropped; cancellable context |
| FR-R13 | P2 | Information disclosure | Raw AWS and Kubernetes error text in status conditions |
| FR-R14 | P2 | Secrets | Idempotency fingerprint derived from the secret value |
| FR-R15 | P2 | Design | Mutable package-level handler registry; 501 fallback not refused |
| FR-R16 | P2 | Contract | CD-7, CD-9 and CD-12 not in the root contract |
| FR-R17 | P2 | Error handling | Ignored `json.Marshal` errors in the import writer |
