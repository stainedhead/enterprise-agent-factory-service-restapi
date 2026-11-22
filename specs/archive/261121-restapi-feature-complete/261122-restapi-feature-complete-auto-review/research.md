# Research: review fixes

Findings come from the Step 5 review at `9454fd5` and are verified in the source PRD sections 3 to 5.
This file records what the streams must know before they start, plus where each fix lands.

## 1. Questions and answers

| # | Question | Answer |
|---|---|---|
| R-1 | Does the database guard allowed digests or enabled models on `agents`? | No. The FK needs only a `team_models` row (`-db` `000001_core_schema.up.sql:233`); nothing knows about allowed digests. The guard must be in the use case (Q1) |
| R-2 | Where do agents guards live today? | `internal/usecase/agents/service.go`: `checkTemplate` `:142-158`, `checkModel` `:160-169`, storage class `:225-229`, `checkBindings` `:173-200` |
| R-3 | How does restore write rows? | `internal/adapter/postgres/restore.go:180-230` writes each agent with `UPDATE ... FROM jsonb_populate_record`, bypassing the use case |
| R-4 | Which address does the denied auditor use? | `MetaFrom(ctx).IP`, which honours `AFS_TRUSTED_PROXY_HOPS` (`middleware/middleware.go:65-100`) |
| R-5 | Can usecase import the redaction scrubbers? | No: they live in `internal/adapter/observability`. FR-R13 puts a pure sanitiser in a new usecase package and takes the scrubber through an injected func |
| R-6 | Which Bedrock families does the gateway know? | Converse, ConverseStream and Anthropic Messages (`gatewayhttp/wire.go`); B enumerates the Invoke families from `parseInvoke` and refuses the rest |
| R-7 | Is `afs.settle_reservation` idempotent? | It returns false for a hold already released and still adds the cost, so a retry after an error is safe and a retry after success is never made |
| R-8 | Which `AFS_*` variables are read outside `settingsFrom`? | `AFS_PAGE_TOKEN_KEY` (`wire_m1w2.go:60`), `AFS_AWS_ACCOUNT_ID` (`wire_identity.go:50`), `AFS_CLUSTER_*` (`wire_kubeconfig.go:19-20`) |
| R-9 | Who reads `AFS_MAX_BODY_BYTES`? | Nothing. Body limits are the constants in `decode.go:15-20` |
| R-10 | Where is `NoOutbox` defined? | `internal/adapter/httpapi/matrix/` fixtures and `run.go` |

## 2. Decisions that remove ambiguity between streams

| # | Decision | Why |
|---|---|---|
| RD-1 | `AFS_MAX_BODY_BYTES` is removed from the chart and docs, not implemented | Per-operation constants already bound bodies; implementing it would touch `decode.go`, which no stream owns |
| RD-2 | Idempotency HMAC key: env `AFS_IDEMPOTENCY_HMAC_KEY`, `settings.IdempotencyHMACKey []byte` (at least 32 bytes), `httpapi.Config.IdempotencyHMACKey` | D adds it, E consumes it; the name is fixed here so both start without a conversation |
| RD-3 | New gateway metric names are fixed: `afs_gateway_settle_failures_total`, `afs_gateway_usage_rows_dropped_total{reason}` | C registers, B writes |
| RD-4 | Recorder interfaces, not metric types, in `usecase` and in the usecase-facing adapters | Keeps the dependency rule; `cmd/*` binds them |
| RD-5 | `acting.Deps` gets an optional `Logger`; nil falls back to `slog.Default()` | A needs no `wire.go` edit |
| RD-6 | A edits `cmd/afs-api/wire_restore.go` (A owns it); no stream except D edits `cmd/afs-api/wire.go` | Removes the main merge hot spot |
