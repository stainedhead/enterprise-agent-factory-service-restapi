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

(append here)

## Stream D: afs-api runtime and configuration

(append here)

## Stream E: Secrets fingerprint, imports and contract

(append here)

## Orchestrator

(cross-stream requests and their resolution; merge log)

## Lessons learned

(append at completion)
