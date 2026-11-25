# Implementation notes

- Annotations are pointer booleans because the MCP defaults for absent hints are not all false (destructive and
  open-world default to true). A read-only tool is never destructive.
- Within one policy level the most specific rule wins (exact name > glob > annotation class), so a team can block
  `delete_*` and allow `delete_branch`. Across levels a block always wins.
- Annotation-class rules apply only when the server's annotations are trusted after review (PRD §7.3).
- An unknown approval policy or tier fails closed to security approval.
- `Tokenize` strips a plural `s` only from words longer than three characters, so `bus` stays `bus`.
- Canonicalisation is checked against the RFC 8785 examples (§3.2.2 values, §3.2.3 UTF-16 key order).
- `GET /mcp` answers `405`: the spec lets a server decline the optional server-to-client stream, and the gateway's
  list-changed notifications reach 2025-11-25 clients only on their next list call for now. `DELETE` answers `204`
  because sealed sessions hold no state.
- Visibility refusals answer `-32602 Unknown tool` (or resource, prompt) and record the reason only in the ledger
  (CD-46). A name absent from the snapshot is recorded as `primitive_unknown`.
- On the aggregated endpoint resources and templates are qualified in `uri` and `uriTemplate`, and `resources/read`
  of a URI that matches an allowed template's literal parts is admitted (the upstream still validates the URI).
- The gateway forwards only `traceparent`, `tracestate` and `progressToken` from the agent's `_meta`, and adds
  `io.afs/requestId` to results.
- Rate limits and concurrency slots are per gateway replica; with two replicas the effective limit is up to twice the
  setting. Recorded as a known limit; a shared limiter waits for the 1.2.0 gateway's store choice.
- `afs-mcp-bridge` rewrites request ids, serialises requests, refuses server-to-client requests with `-32601`, drops
  stdio notifications, restarts the child with backoff and replays the last successful `initialize`.
