# Tasks: restapi feature-complete review fixes

Source: [restapi-feature-complete-review-PRD.md](restapi-feature-complete-review-PRD.md) section 8 (streams),
section 5 (requirements). 17 findings, 5 streams, 48 tasks. Each stream runs as one parallel worker in its own
git worktree, cut from `feat/restapi-feature-complete`, on its own branch.

**Task rules (all streams).**

- Every task is one TDD cycle: Red (failing test, fails for the stated reason), Green (least code), Refactor
  (tests green). Test level tags: `[unit]`, `[adapter]` (PostgreSQL via `AFS_TEST_DATABASE_URL`, `httptest` or
  fakes), `[e2e]` (in-process, `cmd/afs-api`), `[helm]` (chart golden).
- The task is not done until the `AGENTS.md` design checklist is run and the outcome written in the stream's
  section of `implementation-notes.md`. Then tick the box here and update the stream row in `status.md`.
- `make gate` in the worktree before every commit. Commit messages name
  `specs/261122-restapi-feature-complete-auto-review` and the task id.
- Security tasks (R01, R02, R03, R14) carry the denied, refused or redacted case as a test.

## Ownership and shared-file rules

Ownership is disjoint: a path appears in exactly one stream's Owns line. A stream that needs an edit in a file
it does not own sends a request to the orchestrator (recorded in `status.md` "Cross-stream requests") and
waits for the owner's merge; it never edits the file.

| Shared file | Rule |
|---|---|
| `cmd/afs-api/wire.go` | Owned by D only. Every other stream avoids it: A uses `wire_restore.go`, E uses `wire_secrets.go`; `acting.Deps.Logger` is optional (RD-5) |
| `api/openapi.yaml` | Owned by D, and only for the `/readyz` response (D-1). No other stream changes the API shape. A stream that finds it must change it asks the orchestrator first |
| `specs/.../implementation-notes.md` | Pre-created with one `## Stream X` section per stream. A stream edits only its own section (append-only inside it). Decisions, review outcomes and ADR drafts go there. Conflicts keep both sides |
| `specs/.../status.md` and this `tasks.md` | A stream ticks only its own stream's tasks and its own status row |
| `docs/technical-details.md`, `docs/architectural-decision-record.md`, `README.md` | Not edited by streams. Draft text goes in the stream's notes section; the orchestrator applies it in Step 11 |
| `user-docs/configuration.md` | Owned by D (all settings docs, including E's key and B's nothing). Other streams put their doc text in their notes section and ask D, or the orchestrator applies it in Step 11 |
| `internal/adapter/observability/` dead-metric test list | Owned by C. The one cross-stream edit allowed: B-8 removes the `deferredWriters` entries for the two B metrics in the same commit that wires them |
| `go.mod`, `go.sum`, `Makefile`, `.golangci.yml` | No stream edits them. A needed dependency is a cross-stream request |
| `AGENTS.md` | Owned by E (outbox line only, E-6) |

**Merge order.** A, then C, then B and D in parallel (D's FR-R15 tasks D-9 to D-10 only after A has merged), then E.
After each merge: rebase the remaining worktrees, run `make gate`, then `make cover` for `domain`, `usecase`
and `authz`. Remove the worktree after its merge. Never force-push the shared branch.

```bash
git worktree add ../restapi-fix-a -b fix/review-a feat/restapi-feature-complete   # likewise b, c, d, e
```

---

## Stream A: Restore guardrails and audit identity

**Findings:** FR-R01 (P0), FR-R02 (P1), FR-R12 (P2). **Order:** start first, merge first.
**Owns:** `internal/usecase/restore/`, `internal/adapter/postgres/restore.go` (and its tests),
`internal/domain/revision.go`, `internal/usecase/agents/`, `internal/adapter/httpapi/h2_common.go`
(`w2Caller` only; `w2SetArea` is D's, see D-9), `internal/usecase/acting/acting.go`,
`cmd/afs-api/wire_restore.go`.
**Depends on:** nothing. **Blocks:** C's merge (order), D-9 and D-10.

- [x] **A-1** FR-R01 AC5 `[unit]` Extract one guard validator (template enabled and digest in `AllowedDigests`, model enabled for team, storage class offered, bindings) from `agents.Service`. Red: table test per refused case calls the validator directly. Green: move the logic. Refactor: `CreateAgent` and `UpdateAgent` call it; their existing tests stay green unchanged.
- [x] **A-2** FR-R01 AC1, AC2 `[unit]` Restore planning calls the validator for every planned agent step. A refused step is dropped from the apply list and reported in `skipped` with a reason naming the guard (template, digest, model, storage class). Red: fake repo plus fake template and model sources, table per refused case. Depends on A-1.
- [x] **A-3** FR-R01 AC1 `[adapter]` The apply path writes only the steps not skipped. The scenario from the PRD: create agent on digest D1, service admin removes D1 and agent moves to D2, restore to the earlier revision, agent keeps D2, `skipped` names the template guard, the audit record counts it as skipped. Depends on A-2.
- [x] **A-4** FR-R01 AC2 `[adapter]` Same adapter scenario for a disabled template, a model not enabled for the team, and an unoffered volume storage class. Depends on A-3.
- [x] **A-5** FR-R01 AC3 `[unit]` `[adapter]` `dryRun: true` reports identical skips and writes nothing (no row, audit or outbox). Depends on A-3.
- [x] **A-6** FR-R01 AC4 `[adapter]` A restore that changes only unguarded fields (description, persona, snapshot settings) still applies them; RA-US-14.6 stays green for revisions that pass the guards.
- [x] **A-7** FR-R01 NFR `[adapter]` Performance: validation reads from the data the plan loaded (no query per field); a restore of 200 agents finishes inside the 30 s statement timeout. Depends on A-3.
- [x] **A-8** FR-R02 AC1, AC2, AC3 `[unit]` `[e2e]` `w2Caller` takes `SourceIP` from the request metadata (`MetaFrom(ctx).IP`). Table test with socket peer `192.0.2.1` and `X-Forwarded-For: 203.0.113.7, 10.0.0.5`: hops 0 records `192.0.2.1`, hops 1 `10.0.0.5`, hops 2 `203.0.113.7`; an allowed and a denied request with the same headers record the same value; with hops 0 a client `X-Forwarded-For` never changes the address.
- [x] **A-9** FR-R12 AC1, AC2 `[unit]` `acting.Deps.Denied` writes with `context.WithoutCancel(ctx)`, logs a failed write (operation, request id) through optional `Deps.Logger` (nil falls back to `slog.Default()`), and still returns `AccessDenied`. Tests: failing fake audit writer (returns `AccessDenied`, failure logged); cancelled context (record still written).
- [x] **A-10** All `[unit]` Design review per fix against the `AGENTS.md` checklist; coverage of `usecase/restore`, `usecase/agents`, `usecase/acting` at or above 90%; notes and a draft ADR line (restore reuses agents guards; `-db` trigger follow-up per Q1) in `implementation-notes.md` Stream A.

---

## Stream B: Gateway spend integrity

**Findings:** FR-R04, FR-R05, FR-R08 (writes the counters that C registers). **Order:** parallel with D;
merges after C (its wiring commit needs C's metrics).
**Owns:** `internal/adapter/gatewayhttp/` (all, including `wire.go`, `server.go` and tests),
`internal/usecase/gateway/`, `internal/adapter/postgres/usage_writer.go` and its tests,
`internal/adapter/postgres/spend.go` and its tests, `cmd/afs-gateway/`.
**Depends on:** C's metric names (RD-3, fixed in advance) for B-8 only. B-1 to B-7 use interfaces and fakes and
do not wait for C.

- [ ] **B-1** FR-R04 AC1 `[unit]` Converse and ConverseStream without `inferenceConfig.maxTokens` reach Bedrock with it set to the reserved cap; an existing `inferenceConfig` keeps its other members. Red: `server_test.go` case with the limit absent.
- [ ] **B-2** FR-R04 AC2 `[unit]` Invoke for a model family whose limit field the gateway knows gets that field set to the cap when absent. Invoke for any other family is refused with 400 `ValidationError` naming the missing limit and is not forwarded (Q3).
- [ ] **B-3** FR-R04 AC3 `[unit]` Table tests for Converse, Invoke and Anthropic Messages: limit absent (set), limit below the cap (unchanged), limit above the cap (lowered).
- [ ] **B-4** FR-R04 AC4 `[unit]` Invariant test: the reserved amount equals the forwarded limit in every B-1 to B-3 case. Depends on B-1 to B-3.
- [ ] **B-5** FR-R05 AC1, AC2 `[unit]` Bounded retry queue in `usecase/gateway` for `(holdID, budgetID, period, actual)`: default 10,000 entries, exponential backoff, 10 minute window, retries only on an error and never after a success. Full queue or expired window counts and logs team, budget and amount. Red: fake `Spend` that fails N times then succeeds; the period's spent amount ends at the actual cost.
- [ ] **B-6** FR-R05 AC3 `[unit]` A `SettleRecorder` port is called once for every settlement that falls back to the queue (and for queue-full and window-expired drops). Depends on B-5.
- [ ] **B-7** FR-R05 AC4 `[unit]` On `Close`, pending settlements get one more attempt bounded by `SettleTimeout`; test proves it. A crash still falls back to hold expiry (Q2). Reliability: no added latency on the response path when `AsyncSettle` is on. Depends on B-5.
- [ ] **B-8** FR-R08 AC2 `[adapter]` `UsageWriter.Run` takes a parent context from `cmd/afs-gateway`; the per-batch timeout derives from it, with `context.WithoutCancel` for the shutdown flush. No `context.Background()` remains in the adapter (test or grep check).
- [ ] **B-9** FR-R08 AC1 `[adapter]` The usage writer reports drops to a recorder with `reason` of `queue_full` or `write_failed`; both paths tested with a fake recorder.
- [ ] **B-10** FR-R05 AC3, FR-R08 AC1 `[unit]` Wiring commit, after C has merged: `cmd/afs-gateway` binds `SettleRecorder` to `afs_gateway_settle_failures_total` and the drop recorder to `afs_gateway_usage_rows_dropped_total{reason}`; remove the two `deferredWriters` entries in C's dead-metric test list (the one allowed cross-stream edit); run `make load`. Depends on B-6, B-9 and C-7.
- [ ] **B-11** All Design review per the checklist; coverage of `usecase/gateway` at or above 90%; notes (decisions Q2, Q3, the Invoke family list) in `implementation-notes.md` Stream B.

---

## Stream C: Observability

**Findings:** FR-R06, FR-R13, and the registered counters for FR-R05 and FR-R08. **Order:** parallel; merges
second (after A, before B's wiring commit).
**Owns:** `internal/adapter/observability/`, `internal/usecase/reconcile/engine/`,
`internal/usecase/reconcile/tagsync/`, `internal/usecase/reconcile/lifecycle/finalizer.go`,
`internal/usecase/reconcile/notify/`, new `internal/usecase/reconcile/condmsg/`, `cmd/afs-reconciler/`,
`deploy/helm/afs/templates/prometheusrule.yaml`, `deploy/helm/afs/dashboards/`.
**Depends on:** nothing. **Blocks:** B-10.

- [x] **C-1** FR-R06 AC1, AC2 `[unit]` `engine.Recorder` interface; the engine records `afs_reconcile_duration_seconds` and `afs_reconcile_errors_total` per kind and `afs_reconcile_lag_seconds` (change to observed generation). Red: one successful and one failing reconcile through a fake recorder; both series change.
- [x] **C-2** FR-R06 AC1 `[unit]` The drift path (`engine/drift.go`) increments `afs_drift_corrected_total{kind}` through the recorder.
- [x] **C-3** FR-R06 AC1 `[unit]` The notification drainers in `reconcile/notify/` increment `afs_notification_failures_total{channel_type}` on a failed send.
- [x] **C-4** FR-R06 AC2 `[unit]` `cmd/afs-reconciler` binds the recorders to `observability.Metrics`; a test in `cmd/afs-reconciler` runs one success and one failure and asserts the registered series change. Depends on C-1 to C-3.
- [x] **C-5** FR-R06 AC3 `[unit]` Dead-metric test in `go test ./...`: parses `prometheusrule.yaml` and `dashboards/*.json`, extracts every metric name, and fails when one is unregistered or lacks a writer on the kept list. Red against the current tree (five dead metrics). Depends on C-1 to C-4 to go green.
- [x] **C-6** FR-R13 AC1 `[unit]` New `condmsg` package: strip ARNs and 12-digit account ids, apply an injected scrubber, cut to 512 bytes. Table test feeds an AWS `AccessDenied` message containing an ARN and asserts no 12-digit account id remains.
- [x] **C-7** FR-R05 AC3, FR-R08 AC1 `[unit]` Register `afs_gateway_settle_failures_total` and `afs_gateway_usage_rows_dropped_total{reason}` (RD-3) with accessors B can bind; add a `PrometheusRule` alert on settle failures and a dashboard panel for dropped rows. List both in the dead-metric test under `deferredWriters` (writer: stream B, B-10). Alert test in `alerts_test.go`.
- [x] **C-8** FR-R13 AC1, AC2 `[unit]` Apply `condmsg.Sanitize` at `engine.go:312,314`, `tagsync.go:157` and `finalizer.go:159-161`; the full error goes to the reconciler log through the redacting handler. One test per call site.
- [x] **C-9** All Design review per the checklist; notes in `implementation-notes.md` Stream C, including the dashboard and rule changes.

---

## Stream D: afs-api runtime and configuration

**Findings:** FR-R03, FR-R07, FR-R09, FR-R10, FR-R11, then FR-R15. **Order:** parallel; FR-R03 and FR-R07 first;
FR-R15 (D-9, D-10) last, after A has merged.
**Owns:** `cmd/afs-api/main.go`, `wire.go`, `wire_m1w2.go`, `wire_identity.go`, `wire_kubeconfig.go`, new
`cmd/afs-api/*_test.go` files for its tasks, `internal/adapter/httpapi/server.go`, `router.go`, `handlers.go`,
`h_pricing.go`, `h_restore.go`, `h_secops.go`, `h_schedules.go`, `h_exports.go` (the `CreateImport` deadline line
only), `internal/adapter/httpapi/h2_common.go` (`w2SetArea` only, and only after A has merged), `deploy/helm/afs/templates/api-deployment.yaml`, `deploy/helm/afs/values/`, `api/openapi.yaml`
(`/readyz` only), `user-docs/configuration.md`.
**Depends on:** A merged, for D-9 and D-10 (`w2SetArea` lives in A's `h2_common.go`). **Blocks:** E-1.

- [ ] **D-1** FR-R03 AC1, AC2, AC3 `[unit]` `/readyz` on the API listener answers 503 with `{"status":"not ready","checks":{"database":"unavailable"}}` and logs the cause with the request id. Red: inject the error text `failed to connect to user=afs_app host=10.1.2.3` and assert the body contains neither `afs_app` nor `10.1.2.3`. The operations-port `/readyz` is unchanged. Update the `/readyz` response in `openapi.yaml`.
- [ ] **D-2** FR-R07 AC2 `[unit]` `settingsFrom` reads `AFS_API_REPLICAS` and `AFS_PAGE_TOKEN_KEY`; startup is refused when replicas is above 1 and the key is missing or shorter than 32 bytes. Table test; the key is no longer read in `wireM1W2`.
- [ ] **D-3** FR-R07 AC1 `[helm]` The chart injects `AFS_PAGE_TOKEN_KEY` (and `AFS_API_REPLICAS`) from a Kubernetes Secret by `secretKeyRef`, never inlined. Golden test shows the `secretKeyRef`. Depends on D-2.
- [ ] **D-4** FR-R07 AC3 `[e2e]` Two handlers built from the same settings: a `nextToken` from handler 1 is redeemed on handler 2. Depends on D-2.
- [ ] **D-5** FR-R14 (E's key, RD-2) `[unit]` `[helm]` Add `settings.IdempotencyHMACKey` (`AFS_IDEMPOTENCY_HMAC_KEY`, at least 32 bytes), `httpapi.Config.IdempotencyHMACKey`, and the chart `secretKeyRef`; table test; document it. Do this early; E-1 waits for it.
- [ ] **D-6** FR-R09 AC1, AC2 `[e2e]` `AFS_HTTP_READ_TIMEOUT` (60 s) and `AFS_HTTP_IDLE_TIMEOUT` (120 s) in `settingsFrom` and `main.go`; `CreateImport` extends its read deadline to 10 minutes through `http.ResponseController.SetReadDeadline`. Red: a deliberately slow body is closed after the configured timeout. Document both in `user-docs/configuration.md`.
- [ ] **D-7** FR-R10 AC1, AC2, AC3 `[unit]` With IAM auth enabled, `Migrate` receives `postgres.IAMPassword(OwnerURL, region, creds)`. `wire` unit test with a fake credentials provider asserts the password function is passed. `user-docs/configuration.md` lists the supported combinations.
- [ ] **D-8** FR-R11 AC1, AC2, AC3 `[unit]` Move `AFS_AWS_ACCOUNT_ID` and `AFS_CLUSTER_*` into `settings` with a table test per variable. Remove `AFS_MAX_BODY_BYTES` from the chart and docs (RD-1, decision recorded). Add a go/ast test that fails when `os.Getenv` is called in `cmd/afs-api` outside `main.go` and `settingsFrom`. Depends on D-2 and D-5.
- [ ] **D-9** FR-R15 AC1 `[unit]` After A has merged: `wire` passes an explicit registrar list in `httpapi.Config.Areas`; remove the package-level `Areas`, `w2SetArea` (in A's `h2_common.go`, now merged, so D may edit that function once) and the `init()` appends in `h_pricing.go`, `h_restore.go`, `h_secops.go`, `h_schedules.go`.
- [ ] **D-10** FR-R15 AC2, AC3 `[unit]` `httpapi.New` returns an error for an unbound operation unless `Config.AllowUnbound` is set; unit test without a database; the matrix test stays green. Depends on D-9.
- [ ] **D-11** All Design review per the checklist; coverage; notes in `implementation-notes.md` Stream D (including RD-1 and the supported IAM combinations).

---

## Stream E: Secrets fingerprint, imports and contract

**Findings:** FR-R14, FR-R17, FR-R16. **Order:** parallel, but E-1 to E-3 need D-5 merged; merges last.
**Owns:** `internal/adapter/httpapi/dto_secrets.go`, `h_secrets.go`, `cmd/afs-api/wire_secrets.go`,
`internal/adapter/postgres/imports_repo.go` (and its tests), `AGENTS.md` (outbox line only), new
`internal/adapter/httpapi/matrix/outbox_exemptions_test.go`, the root repository's
`docs/contracts/api-contract.md` (a separate PR in the root).
**Depends on:** D-5 (`Config.IdempotencyHMACKey`, RD-2). E-4 to E-6 have no dependency and can start at once.

- [ ] **E-1** FR-R14 AC1 `[unit]` The value's contribution to `idemBody` is HMAC-SHA-256 under the server key from `Config.IdempotencyHMACKey`, never a plain hash. Red: the current unsalted tag is recoverable. Wire the key in `wire_secrets.go`. Depends on D-5.
- [ ] **E-2** FR-R14 AC2 `[unit]` An unparsable secret body is refused with 400 before the idempotency claim (`CreateSecret` and `RotateSecret`); test covers both.
- [ ] **E-3** FR-R14 AC3 `[unit]` The stored `request_sha256` differs between two servers with different keys for the same request. Depends on E-1.
- [ ] **E-4** FR-R17 AC1, AC2 `[adapter]` `imports_repo.go:91` and `:114` return the marshal error wrapped with `%w` and the table name; a value that cannot be marshalled (a channel) makes the import step fail and writes nothing.
- [ ] **E-5** FR-R16 AC2 `[unit]` Test in the `matrix` package compares the matrix `NoOutbox` set with an embedded copy of the contract exemption list (members, API keys, settings, catalog, config, schedules, swarm templates); fails when they diverge.
- [ ] **E-6** FR-R16 AC1 (Q4) Root-repository PR to `docs/contracts/api-contract.md`: list the CD-7 mutations as exempt from CT-API-8, record CD-9 (one `client_id` per identity provider) and CD-12 (a workload principal may omit `If-Match` on `UpdateWorkOrderRepo`). Align this repository's `AGENTS.md` line "every mutation writes ... an outbox_events entry" with the outcome. Open the PR; do not merge it.
- [ ] **E-7** All Design review per the checklist; secret-path denial tests present; notes in `implementation-notes.md` Stream E, with the root PR link.

---

## Summary

| Stream | Findings | Tasks | Merge position |
|---|---|---|---|
| A | FR-R01, FR-R02, FR-R12 | A-1 to A-10 | 1 |
| C | FR-R06, FR-R13 (+ counters for R05, R08) | C-1 to C-9 | 2 |
| B | FR-R04, FR-R05, FR-R08 | B-1 to B-11 | 3, wiring B-10 after C |
| D | FR-R03, FR-R07, FR-R09, FR-R10, FR-R11, FR-R15 | D-1 to D-11 | 3, D-9 and D-10 after A |
| E | FR-R14, FR-R17, FR-R16 | E-1 to E-7 | 4 |

All 17 findings covered: R01 A, R02 A, R03 D, R04 B, R05 B (counter C), R06 C, R07 D, R08 B (counter C), R09 D,
R10 D, R11 D, R12 A, R13 C, R14 E, R15 D, R16 E, R17 E. The deferred items M2-F8 and M6-F3 to M6-F6 are not tasks here.
