# Implementation notes: review fixes

Each stream appends only inside its own section. Record, in order: decisions (with the Q or RD id), the
design review outcome per fix (checklist from `AGENTS.md`), gotchas, draft ADR or docs text for the
orchestrator to apply in Step 11, and follow-ups.

## Decisions taken at spec time (authorized by J)

| Id | Decision |
|---|---|
| Q1 | FR-R01 enforced in the use case only. A `-db` trigger for allowed digests is a recorded follow-up, not built |
| Q2 | FR-R05: bounded in-memory retry (10,000 entries, 10 minute window), counter and alert. No new table |
| Q3 | FR-R04: an Invoke family whose limit field is unknown is refused with 400 `ValidationError` |
| Q4 | FR-R16: the CD-7 mutations are listed as exempt in the contract (no new event types) |
| RD-1 | `AFS_MAX_BODY_BYTES` removed from the chart and docs |
| RD-2 | Idempotency key: `AFS_IDEMPOTENCY_HMAC_KEY`, `settings.IdempotencyHMACKey`, `httpapi.Config.IdempotencyHMACKey` |
| RD-3 | Metric names `afs_gateway_settle_failures_total` and `afs_gateway_usage_rows_dropped_total{reason}` |
| RD-4 | Recorder interfaces in `usecase`; `cmd/*` binds them to metrics |
| RD-5 | `acting.Deps.Logger` optional, nil falls back to `slog.Default()` |
| RD-6 | `wire.go` is D's; A edits `wire_restore.go`; E edits `wire_secrets.go` |
| Step 7 | The original spec was archived with M2-F8 and M6-F3 to M6-F6 open; see the archived dashboard |

## Stream A: Restore guardrails and audit identity

**Decisions (authorized by J).**

- **A-D1. One validator.** `agents.Guard` (`internal/usecase/agents/guard.go`) holds `CheckTemplate`, `CheckModel`,
  `CheckStorageClass`, `CheckBindings` over a narrow `agents.GuardSource` (the catalog reads of `agents.Repository`).
  `agents.Service` calls it; `restore.Service` calls it through `Service.Guards`. Not duplicated SQL (AC5).
- **A-D2. Fail closed.** `restore.Service.Guards` is required; a nil source makes `Run` return an error instead of
  skipping the checks. `cmd/afs-api/wire_restore.go` passes `postgres.NewAgentRepo()`.
- **A-D3. Whole step skipped.** A refused agent step is dropped whole (not field by field) and reported in `skipped`
  with `refused by the <guard>: <reason>`. Guard names: `template guard`, `template guard (digest)`, `model guard`,
  `storage class guard`, `binding guard`. Only fields the step changes are checked, as `UpdateAgent` does, so a
  persona-only restore of an agent on a retired template still applies (AC4).
- **A-D4. Bindings too.** `agent_secret_bindings` create/update steps run `CheckBindings` (a secret of another agent or
  a pool, or an unknown secret, is skipped). One `Secrets` read prefetches every secret the plan names.
- **A-D5. No N+1 (A-7).** The guards read through a per-restore memo (`guardedSource`): one lookup per distinct template,
  model and storage class. 200 agents on one template cost one `Template` query and none for an unchanged model.
- **A-D6. Storage class.** `RestoreSpecs` does not restore `storage_class` today, so the check is dormant; it is unit
  tested by adding the field temporarily. `UpdateAgent` also does not re-check the class on a volume patch (pre-existing,
  not changed here; candidate follow-up).
- **A-D7. Source IP (FR-R02).** `w2Caller` reads `middleware.MetaFrom(ctx).IP`; with no metadata middleware it falls back
  to the socket peer, never a client header.
- **A-D8. Denied audit (FR-R12).** `Deps.Denied` writes with `context.WithoutCancel`, logs a failed write
  (operation, requestId) via `Deps.Logger` (nil: `slog.Default()`), still returns AccessDenied (RD-5).

**Findings while testing.** The `-db` schema already refuses a digest the template no longer allows (the skip then
read `could not be restored: image digest ...`, no guard name). It does not refuse a model disabled for the team: the
adapter test for that case failed without the use case guard (restore put the model back). So the P0 was real for
models; for digests the use case guard adds the named reason and avoids relying on a trigger.

**Design review (AGENTS.md checklist), per fix.**

| Fix | Dependency rule | Permission/denied tests | No secrets | Audit/outbox same tx | Ctx, `%w` | OpenAPI | Docs |
|---|---|---|---|---|---|---|---|
| R01 | restore imports `agents` (usecase to usecase), no adapter imports | viewer refusal test unchanged; guarded refusals tested at unit and adapter level | binding check reads ids/owners only | skip counted in the same audit record | ctx passed to every read | no shape change (`skipped` already exists) | exported identifiers documented |
| R02 | httpapi imports `middleware` (no cycle) | unit table plus e2e (allowed and denied record the same value) | n/a | n/a | n/a | none | comment updated |
| R12 | acting only | failing writer and cancelled-context tests | n/a | n/a | `WithoutCancel` | none | `Logger` documented |

**Draft ADR line (for Step 11).** Restore reuses the agents guardrails: `RestoreRevision` validates each agent and binding
step with `agents.Guard` and skips what `UpdateAgent` would refuse. Defence in depth with a `-db` trigger for allowed
digests and enabled models is a recorded follow-up (Q1), not built.

**Docs text for the orchestrator.** `docs/technical-details.md`: restore `skipped` reasons now name a guard
(`template guard`, `template guard (digest)`, `model guard`, `storage class guard`, `binding guard`). `user-docs`: same,
for the RestoreRevision usage section.

**Shared-file edits.** None outside Stream A's Owns list, except new test files and the `Guards` field wired in
`cmd/afs-api/wire_restore.go` (owned by A). `internal/usecase/restore/restoretest` and `agents/agentstest` untouched.

## Stream B: Gateway spend integrity

### FR-R08 (B-8, B-9) and wiring (B-10)

- Decision (RD-4): `gateway.UsageDropRecorder.UsageRowsDropped(reason, rows)` in `usecase/gateway/usage.go`
  with reasons `queue_full` and `write_failed`; `UsageWriterConfig.Recorder` is optional. A batch dropped after
  every attempt reports its row count; a row whose cost cannot be encoded reports one `write_failed`.
- Decision: `UsageWriter.Run(ctx)`. Each batch write derives its 5 s timeout from `ctx`; the flush `Close`
  triggers uses `context.WithoutCancel(ctx)`. `usage_writer.go` holds no `context.Background()` (a test greps
  for it). The three existing integration call sites now pass `t.Context()` (one-line edits in
  `gateway_integration_test.go` and `m3_cross_stream_integration_test.go`).
- Decision: `cmd/afs-gateway` passes `context.WithoutCancel(ctx)` to `Run`: after SIGTERM the HTTP drain
  still produces rows that must be written; `Close` is what stops the writer. Shutdown order is now
  drain HTTP, `Service.Close(ctx)` (books, then the last settle attempt), `Usage.Close()`.
- B-10 (stub, for the integrator): `cmd/afs-gateway/recorders.go` binds `settleRecorder` and
  `usageDropRecorder` to `gatewayCounters`. C is not in this branch, so `newGatewayCounters` builds the two
  counters itself (named per RD-3, NOT registered; registering here would panic as a duplicate once C-7
  registers them). After C merges: replace its body with C's `Metrics` fields (one line, instructions in the
  comment) and remove the two `deferredWriters` entries in C's dead-metric test list in the same commit. That
  list does not exist on this branch, so the removal was not done here. `make load` passed (api 344 s, gateway 8.5 s).
- Design review: no secret or prompt in any recorded or logged value (team, budget, amount only); the
  recorders are ports in `usecase`, bound only in `cmd`; contexts passed through.

### FR-R05 (B-5 to B-7)

- Decision (Q2): `usecase/gateway/settle_queue.go`. After the three inline attempts fail, `Service.settle`
  queues `(holdID, budgetID, period, actual)` in memory: 10,000 entries, backoff 1 s doubling to 1 minute
  (`Config.SettleRetryBase/Max`), 10 minute window from the first failure (`SettleRetryWindow`). Retried only
  after an error; a success removes the entry. One goroutine runs while the queue is not empty and is not
  part of `Wait` (shutdown must not wait for a 10 minute window).
- Decision (RD-4): `SettleRecorder.SettleFailed(teamID, budgetID, amount, reason)` in `usecase`, optional
  `Deps.SettleRecorder`. Reasons: `queued` (every fallback), `queue_full` (also a call after `Close`),
  `window_expired`. A settlement that is queued and later expires is reported twice; the counter is a rate
  signal. Drops log team, budget and amount only.
- Decision: the retry context is `context.WithoutCancel` of the settle context (values kept, the inline
  deadline dropped; a test with a fake that refuses a dead context pins it). No `context.Background()` in
  the use case; `Close(ctx)` takes the caller's context and bounds the final attempt by `SettleTimeout`.
- `Service.Close(ctx)`: `Wait`, stop the loop, one more attempt per pending entry in one `SettleTimeout`
  window; failures are logged, the hold falls back to expiry. `cmd/afs-gateway` must call `Close` instead of
  `Wait` (B-10).
- Response path: with `AsyncSettle` the fallback happens inside the background books-closing goroutine; a
  test shows `Do` returns in under 100 ms while every settlement fails.
- Design review: no secret in logs (ids and amount only); dependency rule holds (no adapter imports);
  errors wrapped as before; race detector green.

### FR-R04 (B-1 to B-4)

- Decision: the output limit is written by `parsed.upstreamBody` whenever the request sent none, an unusable
  one, or more than the reserved cap (`a.MaxOutputTokens`, the same number the reservation was priced on).
  A request below the cap keeps its own limit, which is also what `OutputCap` reserved. `setInt` now creates
  missing or null parent objects (`inferenceConfig`) and keeps their other members.
- Decision (Q3, Invoke family list): when the body has no known limit field, `limitPathOf` picks it from the
  model id vendor segment (cross-region prefixes `us.`, `eu.`, `apac.` and foundation-model ARNs handled):
  `anthropic` -> `max_tokens` (`claude-v2*`, `claude-instant*` -> `max_tokens_to_sample`),
  `amazon.titan-text*` -> `textGenerationConfig.maxTokenCount`, `meta` -> `max_gen_len`, `mistral` and
  `cohere` -> `max_tokens`. Any other family is refused at parse time with 400 `ValidationError` ("has no
  output limit ...") before identification-dependent work, a reservation or a forward. Amazon Nova is not
  in the list: use Converse.
- Decision: Anthropic Messages keeps its existing 400 for an absent `max_tokens` (the Messages API requires
  it, and an existing test pins it). It is never forwarded uncapped; a new test asserts nothing is reserved
  or sent. FR-R04 AC3's "absent" case for that wire is therefore the refusal.
- Tests: `server_cap_test.go` (table over Converse, ConverseStream, Invoke, InvokeStream, Anthropic, other
  families; invariant reserved == `ReserveAmount(price, forwardedLimit, bodyBytes)`). Red first: 13 cases
  failed with "no output limit reached Bedrock".
- Design review: no secret or credential touched; no new route; no context or error-wrapping change; domain
  error mapping unchanged (`domain.Validation`); doc comments added.

## Stream C: Observability

Worktree `/home/user/wt/restapi-rf-c`, branch `rf/c`, cut from 80db7e1. Authorized by J; no questions asked.

### Decisions

- **Recorder shape (RD-4).** `engine.Recorder` (new `engine/recorder.go`) has `ReconcileDone(kind, d, err)`, `ReconcileLag(kind, d)` and `DriftCorrected(kind)`. It is an optional `engine.Config.Recorder`. `notify.FailureRecorder` (`NotificationFailed(channelType)`) is an optional `Failures` field on `TestRunner`, `FailureDrainer` and `EmailDrainer`. `cmd/afs-reconciler/metrics_bind.go` (`reconcileMetrics`) binds both to `observability.Metrics`; `notifyDeps.Failures` carries it into `wireNotify`.
- **Lag definition.** Time from a desired-state change being queued (change feed or the stale-generation poll) to the first successful pass that applies it. The engine keeps `changedAt[key]` (first mark wins, so retries keep counting from the original change) and observes lag on success only. A full resync alone records no lag. A pass with `NoStatus` observes nothing.
- **Drift counter** increments once per corrected object, independent of whether a `DriftRecorder` is configured.
- **Sanitiser.** `condmsg.Sanitize` applies an injected scrubber, replaces ARNs with `<arn>` and bare 12-digit ids with `<account-id>`, and cuts to 512 bytes on a rune boundary. The scrubber is a package-level `atomic.Pointer` set by `SetScrubber` because usecase packages cannot import `observability`; `newReconcileMetrics` installs `observability.Scrub`. Call sites: `engine.writeFailure` (both the failing and the `Ready` condition), `tagsync` refused-tags condition, `lifecycle.next` (both the `InProgress` and error branches). The error itself is returned unchanged, so the engine's `reconcile failed` log line carries the full text through the redacting handler.
- **Dead-metric test** (`observability/deadmetrics_test.go`). Every `afs_*` name in `prometheusrule.yaml` and `dashboards/*.json` must be declared, be on the `writers` list, and have a selector use (`.Field.` or `.Method(`) in non-test source under `cmd/` or `internal/`. `deferredWriters` exempts the two gateway counters until stream B binds them; once a writer exists the test demands the entry be removed. Verified red on the tree before the fixes: four charted metrics had no writer (`afs_reconcile_errors_total`, `afs_reconcile_lag_seconds`, `afs_drift_corrected_total`, `afs_notification_failures_total`; `afs_reconcile_duration_seconds` is registered but not charted).
- **Stream B contract.** `Metrics.SettleFailed()` and `Metrics.UsageRowDropped(reason)` (or the fields `GatewaySettleFailures` and `GatewayUsageRowsDropped`) are the accessors. B-10 binds them in `cmd/afs-gateway` and deletes the two `deferredWriters` entries in `deadmetrics_test.go` (the allowed cross-stream edit; the `writers` entries already name both the accessors and the fields).
- **Chart.** New alert `AFSGatewaySettleFailures` (warning, `increase(...[15m]) > 0`) in group `afs-gateway`; dashboard panel 13 "Gateway settle failures and dropped usage rows" (`sum by (reason)` for drops).

### Design review (AGENTS.md checklist)

Dependency rule holds: engine and notify define ports, `observability` imports no usecase, `condmsg` imports only the standard library. No new routes. No secret reaches a condition or log: the scrubber runs before ARN removal. No mutations added. Context passed through. Exported identifiers documented. No account ids beyond the test constant 111122223333. No `api/openapi.yaml` change.

### Edits outside Stream C's Owns list (for the integrator)

- `deploy/helm/afs/tests/render_test.go` (added `AFSGatewaySettleFailures` to the expected alert list) and `deploy/helm/afs/tests/testdata/default.golden.yaml` (regenerated with `-update`): unavoidable consequence of the new alert and panel. Expect a trivial golden conflict with D's chart changes (`api-deployment.yaml` env); regenerate with `go test -update` after rebase.

### Docs text for the orchestrator (Step 11)

- `docs/technical-details.md`: Observability: reconcile duration, errors, lag and drift are recorded by the engine through `engine.Recorder`; notification failures through `notify.FailureRecorder`; a test fails the build when a charted metric has no writer. Condition messages are sanitised by `condmsg` (ARNs, 12-digit ids, scrubbers, 512 bytes); full errors go to the log.
- ADR line: condition text is untrusted upstream output; it is sanitised at the point it is stored, not at the API edge.

### Follow-ups

- Other `err.Error()` writes into conditions in controllers outside the three FR-R13 call sites (for example `notify` and per-kind controllers returning messages through `engine.WithCondition`) pass through `engine.writeFailure` and so are covered; controllers that build `domain.Condition` messages directly were not audited here.

## Stream D: afs-api runtime and configuration

Worktree `/home/user/wt/restapi-rf-d`, branch `rf/d`. Done: D-1 to D-8, D-11. Not done: D-9, D-10 (see Follow-ups).

**Decisions.**

- D-1: the public `/readyz` reports `unavailable` for every failing check; the cause is logged with `request_id`
  through `slog.ErrorContext`. The operations-port `/readyz` is untouched. The bundled `api/src/paths/11-admin.yaml`
  example and 503 description changed, `make openapi` regenerated `api/openapi.yaml`.
- D-2: `settings.PageTokenKey`. A key under 32 bytes is ignored (as before); with `AFS_API_REPLICAS` above 1 a missing
  or short key stops startup. The area wiring files (not D's) still call `pageTokenKey(log)`, so it now returns the key
  that `wire` fixed once from settings (`setPageTokenKey`, mutex-guarded package variable); no `os.Getenv` remains. The
  integrator may replace the variable with `components.PageKey` when it next touches those files.
- D-3: chart Secret `api.keysSecret.name` (default `afs-api-keys`), keys `pageTokenKey` and `idempotencyHmacKey`, by
  `secretKeyRef`. `AFS_API_REPLICAS` stays a plain value (it is not secret).
- D-5: `settings.IdempotencyHMACKey`, `httpapi.Config.IdempotencyHMACKey`, same rule as D-2. `wire` draws a random key for
  a single replica when unset and puts the resolved key in `s` before `wireSecrets(ctx, c, s, log)`, so E reads
  `s.IdempotencyHMACKey` or `cfg.IdempotencyHMACKey`.
- D-6: `newServer` in `main.go` builds the listener (`ReadHeaderTimeout` 10 s, `ReadTimeout` 60 s, `IdleTimeout` 120 s,
  no `WriteTimeout`). `CreateImport` calls `http.NewResponseController(w).SetReadDeadline(+10m)`; the e2e test proves a
  one-second pause with a 300 ms read timeout is cut on `/v1/teams` and served on `/v1/imports`.
- D-7: `settings.AWSCredentials` and `settings.Migrate` are test seams. Under IAM auth `Migrate` gets
  `postgres.IAMPassword(OwnerURL, ...)`. Supported combinations are in `user-docs/configuration.md`.
- D-8 and RD-1: `AFS_MAX_BODY_BYTES` removed from `values/api.yaml` and the golden file (it was never in the docs).
  `AFS_AWS_ACCOUNT_ID` (12 digits), `AFS_CLUSTER_NAME`, `AFS_CLUSTER_ENDPOINT` (https), `AFS_CLUSTER_CA_DATA` (base64) and
  `AFS_API_ADDR` are settings; the go/ast test allows `os.Getenv` only in `main.go` and `settingsFrom`
  (`AFS_LOG_LEVEL` and the metrics variables are read in `main.go`).
- Test edits outside D's list, for the new settings: `cmd/afs-api/kubeconfig_e2e_test.go`, `secops_e2e_test.go`,
  `matrix_test.go` (`withCluster(settings)` replaces `t.Setenv`), and `deploy/helm/afs/tests/kubectl_test.go` (the
  "token in env name" guard now looks at inline values only) plus the golden file.

**Design review (AGENTS.md checklist).** Dependency rule untouched (`httpapi` imports stdlib only for the new code).
No secret value is logged or put in an error (the settings errors name the variable, never the value; a test checks
the key). Readiness now hides driver text. No route or permission changed, so no new authz test. Context is passed
through. `api/openapi.yaml` updated for `/readyz`. Exported identifiers documented.

**Follow-ups for the integrator.**

- D-9 and D-10 are not done. They need the `Bind*` functions in other streams' files to stop calling `w2SetArea`
  (about 15 files, some owned by A) and `AllowUnbound` set in about 10 `httpapi` and `cmd/afs-api` tests. Suggested
  shape: each `Bind*` returns a `Registrar` and `wire` collects them in
  `cfg.Registrars`; `New` collects unbound operations after registration and returns an error unless
  `cfg.AllowUnbound`; `wire` sets it when there is no database.
- Docs text for Step 11: the `/readyz` redaction and the IAM combinations table are already in `user-docs`.

## Stream E: Secrets fingerprint, imports and contract

Worktree `/home/user/wt/restapi-rf-e`, branch `rf/e` (base 80db7e1). Not pushed.

**Decisions.**

- **E-1 key source (D-5 not merged).** `SecretsAPI.IdemKey` carries the key. `cmd/afs-api/wire_secrets.go`
  `idempotencyHMACKey` reads `AFS_IDEMPOTENCY_HMAC_KEY` with `os.LookupEnv` (E may not edit `settings`, which is D's).
  Fewer than 32 bytes or unset: warn and draw a random per-process key (same behavior as `pageTokenKey`), so the
  fingerprint is never unkeyed; a retry that reaches another replica then gets 409 `idempotency_mismatch`, which D-2/D-5
  remove by refusing startup with replicas above 1. **Integrator:** after D-5 merges, pass `s.IdempotencyHMACKey` into
  `wireSecrets` and delete `idempotencyHMACKey`; D-8's AST test (no `os.Getenv` outside `settingsFrom`) does not flag
  `os.LookupEnv`, but the lookup should go anyway.
- **Fingerprint shape.** `idemBody(key, raw, value)` removes `value` and adds `valueMac` = hex HMAC-SHA-256
  (full 32 bytes, prefix `afs-idem-value|`). A key under 32 bytes returns an internal error (500, no detail): a server
  misconfiguration, never an unkeyed hash. A body that is not a JSON object returns 400 instead of `nil`.
- **E-2.** `decodeSecretBody` already ran before the claim; the shared-hash path was the `nil` return of `idemBody`, now
  an error. Tests cover create and rotate with four malformed bodies each and assert no claim is made.
- **E-4.** Both marshal sites return `import <table>: marshal row: %w`; test (white-box, real PostgreSQL) covers insert and
  update paths, asserts `json.UnsupportedTypeError` is wrapped, no change is recorded and the row count is unchanged.
  Red was the old behavior (insert: NOT NULL violation from the empty `$1`; update: no error).
- **E-5.** `matrix/outbox_exemptions_test.go` embeds the 69 exempt operations by contract category and compares both ways.
  The list is larger than the seven categories named in tasks.md: it also holds identity/role-link/snapshot/export/pool
  secret-update ops (category `config`), settings-like ops (budget, IdPs, channels, log destinations) and catalog ops.
- **E-6.** Proposed root text: `/tmp/claude-0/-home-user/27a36fd7-1505-5fa8-a56a-0bccddbe95fd/scratchpad/rf-e-root-contract.md`
  (section 4.12 exemption table, CD-9 client id, CD-12 workload `If-Match`). The root repo was not edited and no PR was
  opened (instructed). `AGENTS.md`: the outbox rule line, and the matching checklist item, now say "plus an outbox entry
  when the contract defines an event type; exempt operations in contract section 4.12". The checklist edit goes slightly
  beyond "outbox line only"; it is the same rule.

**Design review (AGENTS.md checklist).** Dependency rule unchanged (httpapi, cmd only; no new imports in domain or usecase).
No new route. No secret value in any hash input, log or error (tests assert the response never echoes the value). Audit and
outbox behavior unchanged. Context passed (no `context.Background()`). Errors wrapped with `%w`. No OpenAPI change (the 400
for a malformed body already exists). Exported `IdemKey` documented. No hardcoded ids or ARNs.

**Doc text for D (`user-docs/configuration.md`).** `AFS_IDEMPOTENCY_HMAC_KEY`: at least 32 bytes, from a Secret, identical on
every `afs-api` replica; keys the fingerprint of secret values in idempotency records. Unset: random per process, retries
across replicas answer 409.

**ADR draft line.** Idempotency fingerprints of secret values are HMAC-SHA-256 under a server key, never a bare hash.

## Orchestrator

(cross-stream requests and their resolution; merge log)

## Lessons learned

(append at completion)
