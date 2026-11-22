# Architecture: review fixes

The fixes keep the existing Clean Architecture layout (`adapter/*` and `cmd/*` to `usecase` to `domain`).
Nothing moves between layers. The shape of each fix:

## Stream A: one validator, two callers

```
agents.Service (Create, Update) ──┐
                                  ├──> agents guard validator (usecase/agents)
restore.Service (plan) ───────────┘        checkTemplate, checkModel, storage class, bindings
restore.Service ──> RestoreRepo port ──> postgres/restore.go (applies only the steps not skipped)
```

- The validator is a plain use-case type with table-driven tests. Restore calls it per planned agent step
  inside the restore transaction, using the data the plan already loaded (no query per field).
- A refused step becomes a `skipped` entry with a reason naming the guard. `dryRun` runs the same planner
  and returns the same skips.
- `w2Caller` reads the client address from the request metadata, the same source the denied auditor uses.
- `acting.Deps.Denied` writes with `context.WithoutCancel` and logs failure with operation and request id.

## Stream C: record, then prove it

- The engine, drift recorder and notify drainers call small recorder interfaces. `cmd/afs-reconciler` binds
  them to `observability.Metrics`.
- `condmsg.Sanitize` is a pure usecase package; the observability scrubber is injected as a func.
- A test in `internal/adapter/observability` parses `prometheusrule.yaml` and the dashboards, and fails when
  a referenced metric is unregistered or has no writer on the kept list.

## Stream B: spend integrity

- `gatewayhttp/wire.go` always forwards a limit equal to the reserved cap; unknown Invoke families get 400.
- `gateway` use case gains a bounded settle retry queue behind the existing `Spend` port, with exponential
  backoff for 10 minutes and a last attempt on `Close`. It reports through `SettleRecorder`.
- `UsageWriter.Run(ctx)` derives its timeouts from the caller; drops go to a recorder.

## Stream D: afs-api runtime

- All `AFS_*` reads move into `settingsFrom`; an AST test keeps `os.Getenv` out of the rest of `cmd/afs-api`.
- `httpapi.New` takes `Config.Areas` and returns an error for an unbound operation unless `AllowUnbound`.
- The chart wires `AFS_PAGE_TOKEN_KEY` and `AFS_IDEMPOTENCY_HMAC_KEY` from Secrets by `secretKeyRef`.

## Stream E: secrets fingerprint and contract

- `idemBody` keeps a keyed HMAC of the value (key from `Config.IdempotencyHMACKey`); a body that does not
  parse is refused with 400 before the claim. The import writer returns marshal errors.
- The contract text lands in the root repository through a separate PR.

## Cross-stream dependency map

```
A ──merge──> C ──merge──> B(wiring) ; D(FR-R03, R07, R09, R10, R11) parallel ; D(FR-R15) after A ; E after B and D
C registers metrics (RD-3) ─────────> B writes them
D defines IdempotencyHMACKey (RD-2) ─> E consumes it
```
