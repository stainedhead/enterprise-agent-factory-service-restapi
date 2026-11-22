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

(append here)

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
