# Tasks

| # | Task | State |
|---|---|---|
| T1 | Domain package `internal/domain/mcp` with table tests, ≥ 90% coverage | Done (98.6%) |
| T2 | JSON-RPC and Streamable HTTP protocol layer (`internal/adapter/mcpwire`): messages, header rules, sealed sessions, client | Done (95.9%) |
| T3 | `cmd/afs-mcp-bridge` and `internal/adapter/mcpbridge` with a stdio fixture server test | Done (93.9%) |
| T4 | Permissions in `internal/authz` and the generated catalog, denied tests | Not started |
| T5 | Use cases (catalog, review with the three approval levels, switches, policy, credentials, OAuth state, federated identities, ledger, captures) | Not started |
| T6 | Postgres adapters against `-db` 1.3.0 | Blocked on `-db` 000024 (after 1.2.0's 000023) |
| T7 | Handlers and `api/openapi.yaml` v1.6.0 | Not started |
| T8a | MCP route handler `internal/adapter/mcpgateway` behind ports (identity, admission, upstream, ledger): endpoints, sessions, lists and cursors, meta-tools, admission refusals, rate and concurrency limits, upstream error mapping | Done (94.6%) |
| T8b | Wire the handler into the team gateway mux; admission cache from `afs.mcp_admission`; upstream adapter with credentials; ledger writer; OTel | Blocked on 1.2.0 team gateway code |
| T8c | Inventory refresh `internal/usecase/mcpinventory` and drift comparison `mcp.Compare`: list with pagination and the 2,000 cap, digests, holds, auto-approval only for compatible changes that cross no risk line and pass the checks | Done (100%) |
| T9 | Reconciler outputs and conditions | Blocked on 1.2.0 reconciler code |
| T10 | Reports | After T6 |
| T11 | Docs, ADR, user docs, version 1.3.0, CHANGELOG | Last |
