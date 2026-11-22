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

(append here)

## Stream B: Gateway spend integrity

(append here)

## Stream C: Observability

(append here)

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

(append here)

## Orchestrator

(cross-stream requests and their resolution; merge log)

## Lessons learned

(append at completion)
