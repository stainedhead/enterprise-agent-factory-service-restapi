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
- Drift: a change is compatible when only descriptive keys (`description`, `title`, `_meta`, `icons`) change anywhere,
  or optional input properties or optional prompt arguments are added. Any other change, including annotations, is
  incompatible and always held. A compatible change is auto-approved only under `auto_approve_compatible`, only for an
  approved primitive, only when it crosses no risk line, and only when the whole listing passes the definition checks.
- A truncated listing (more than 2,000 primitives) never marks anything removed.

## Reconciler and agent access (T7d, T9)

- `AFS_MCP_URL` and the other variables appear only when the agent has at least one effective server, so a 1.2.0 pod
  template is unchanged. The variables name the servers, so a team-level change that alters an agent's effective set
  rolls that agent's pod (the config hash includes the MCP delivery). Recorded as a known cost.
- `per_agent` instance mode is accepted and stored but served by one instance per team for now.
- The team gateway's egress covers server hosts and federation token endpoints. An OAuth authorization server on a host
  other than the MCP server's is not yet added to egress; refresh to such a server fails until it is.
- Hosted servers that need AWS get their own ServiceAccount; the Pod Identity association is not created (needs AWS).
- Image signature verification by admission policy and the Appendix D notifications are not built.
- Every service connection now sets the `role` startup parameter (`postgres.Config.Role`); found when the gateway's
  LOGIN user, a member of `afs_gateway`, failed `current_user` guards in `-db` functions (ADR-037).
- An earlier commit message named the spec `specs/251125-mcp-gateway`; the directory is `specs/261125-mcp-gateway`.
