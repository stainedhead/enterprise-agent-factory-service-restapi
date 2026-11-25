# Architecture

- `internal/domain/mcp` is pure (standard library only) and holds every rule the gateway and API share: slugs and
  qualified names, annotation defaults, RFC 8785 canonical digests, policy evaluation (service ∩ team ∩ agent, most
  specific rule within a level, block wins across levels, read-only mode), risk tiers and definition checks, the
  three approval levels, and the BM25 index.
- The database function `afs.mcp_admission` is the stored decision (CD-51). The gateway caches it for 60 s; the domain
  policy package is tested against it so both agree.
- The gateway's MCP route sits on the team gateway listener beside model routes; upstream clients are per (team,
  server) with their own pools and circuit breakers.
