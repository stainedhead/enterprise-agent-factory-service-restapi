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

(append here)

## Stream C: Observability

(append here)

## Stream D: afs-api runtime and configuration

(append here)

## Stream E: Secrets fingerprint, imports and contract

(append here)

## Orchestrator

(cross-stream requests and their resolution; merge log)

## Lessons learned

(append at completion)
