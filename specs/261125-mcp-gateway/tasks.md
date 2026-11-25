# Tasks

| # | Task | State |
|---|---|---|
| T1 | Domain package `internal/domain/mcp` with table tests, ≥ 90% coverage | Done (98.6%) |
| T2 | JSON-RPC and Streamable HTTP protocol layer (`internal/adapter/mcpwire`): messages, header rules, sealed sessions, client | Done (95.9%) |
| T3 | `cmd/afs-mcp-bridge` and `internal/adapter/mcpbridge` with a stdio fixture server test | Done (93.9%) |
| T4 | Permissions in `internal/authz` and the generated catalog, denied tests | Done (contract v1.6.0 extracts; `mcpApprovalPolicy` condition; `TestMcpPermissions`) |
| T5 | Use cases (catalog, review with the three approval levels, switches, policy, credentials, OAuth state, federated identities, ledger, captures) | Done (`internal/usecase/mcpcatalog`) |
| T6 | Postgres adapters against `-db` 1.3.0 | Done: `McpRepo` on `-db` 000026 (4d8a72d); `TestMcpCatalogEndToEnd` against real PostgreSQL |
| T7a | `api/openapi.yaml` v1.6.0 (55 operations, `mcp` on agents and pools, gateway settings, settings, events), API-level domain rules (`internal/domain/mcp.go`), 501 placeholders (`PendingMcpOps`) | Done |
| T7b | Handlers through the use cases; `PendingMcpOps` shrinks | Done: 53 operations bound (`h_mcp.go`), wired in `wire_mcp.go`, every one in the authorization matrix (`fixtures_mcp.go`) |
| T7c | `StartMcpOAuthConnect` and `CompleteMcpOAuthConnect`: RFC 9728/8414 discovery, PKCE with an HMAC-derived verifier (-db 000026), RFC 9207 issuer check, code exchange with RFC 8707 resource | Done: `internal/adapter/mcpoauth`, `mcpcatalog/oauth.go`, `TestMcpOAuthConnectEndToEnd`; `PendingMcpOps` is empty |
| T7d | `mcp` member on agent and pool bodies (`refuseMcpUntilBuilt`), `mayAddMcpServers` and `mcpPlacement` on team gateway settings, `governedBy` on external systems | Done: `usecase/mcpaccess`, `McpAccessRepo`, `TestAgentMcpEndToEnd`, `TestExternalSystemGovernedBy` |
| T8a | MCP route handler `internal/adapter/mcpgateway` behind ports (identity, admission, upstream, ledger): endpoints, sessions, lists and cursors, meta-tools, admission refusals, rate and concurrency limits, upstream error mapping | Done (94.6%) |
| T8b | Wire the handler into the team gateway mux; admission cache from `afs.mcp_admission`; upstream adapter with credentials; ledger writer; OTel | Done: `wire_mcp.go`, `mcpup`, `TestGatewayMcpEndToEnd` |
| T8c | Inventory refresh `internal/usecase/mcpinventory` and drift comparison `mcp.Compare`: list with pagination and the 2,000 cap, digests, holds, auto-approval only for compatible changes that cross no risk line and pass the checks | Done (100%) |
| T9 | Reconciler outputs and conditions | Done: agent and pool `AFS_MCP_*` and `/etc/afs/mcp.json`, team gateway MCP egress, credentials, federation and session keys, hosted servers, conditions; `TestReconcilerMcpAgainstTheSchema` |
| T10 | Reports | Done with T5/T6: usage, refusals, discovery, adoption and unused views (`GetMcpUsageReport`, `GetTeamMcpUsageReport`) |
| T11 | Docs, ADR, user docs, version 1.3.0, CHANGELOG | Done: ADR-035 to ADR-037, `user-docs/mcp-servers.md`, `upgrading-to-1.3.0.md`, Helm `mcpSessionKeys` |
