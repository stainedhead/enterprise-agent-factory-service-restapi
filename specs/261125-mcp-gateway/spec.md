# Spec: governed MCP servers on the team gateway (1.3.0)

Requirements live in the root repository, not here:
[mcp-gateway-PRD.md](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/mcp-gateway-PRD.md)
(approved by J, 2026-11-25) and the contracts gateway v1.3.0 §13, API v1.6.0 §4.14 and database v1.6.0 §7.5.

## In scope for `-restapi`

Stories MCP-RA-1 to MCP-RA-9 (PRD Appendix E.3):

| Story | Summary |
|---|---|
| MCP-RA-1 | The 48 API operations of API contract §4.14, permissions, allowed and denied tests per role; policy-dependent approval |
| MCP-RA-2 | Gateway MCP server side: `/mcp`, `/mcp/{server}`, versions, sealed sessions, headers, meta-tools, admission, limits |
| MCP-RA-3 | Gateway MCP client side: upstream sessions, inventory, RFC 8785 digests, drift, automatic checks, BM25 index |
| MCP-RA-4 | Gateway credentials: static, OAuth client, workload federation, service-account OAuth, SigV4; no passthrough |
| MCP-RA-5 | `mcp_events` writer (fail open), OTel spans and metrics |
| MCP-RA-6 | Reconciler: hosted servers, bridge, network policy, `/etc/afs/mcp.json`, `AFS_MCP_*`, federated identity volumes, conditions |
| MCP-RA-7 | `cmd/afs-mcp-bridge` (stdio ↔ Streamable HTTP) |
| MCP-RA-8 | MCP usage, unused, refusal, discovery and adoption reports |
| MCP-RA-9 | Pin `-db` v1.3.0, version 1.3.0, docs, ADR, user docs |

## Acceptance

The PRD's acceptance criteria for MCP-US-1 to MCP-US-11 and test cases TC-01 to TC-62 that are provable in this
repository, each named by the test that proves it in `implementation-notes.md`. AWS, EKS and real IdP checks are
recorded as deferred.

## Non-goals

Embedding search (MCP-OQ-6), a public MCP registry mirror, agent-local stdio servers (PRD §10.3).
