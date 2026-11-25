# Plan

Order follows PRD Appendix G and the coordinator's dependency rule: work that does not touch 1.2.0's migrations or
gateway code first, then the rest once 1.2.0's `-db` and gateway code are on main.

| Step | Depends on | Layer |
|---|---|---|
| P1 `internal/domain/mcp`: slugs, qualified names, annotations, RFC 8785 digest, policy, risk, approval, BM25 | nothing | domain |
| P2 `internal/mcp/jsonrpc` protocol layer and `cmd/afs-mcp-bridge` | nothing | adapter, cmd |
| P3 `internal/authz` permissions and matrix rows | contracts (merged, root #29) | authz |
| P4 Use cases for catalog, review, team switches, policy, credentials, federated identities, ledger reads | P1, P3 | usecase |
| P5 Postgres adapters | `-db` 1.3.0 (000024 after 1.2.0's 000023) | adapter |
| P6 HTTP handlers and `api/openapi.yaml` v1.6.0 | P4 | adapter |
| P7 Gateway MCP route on the team gateway | 1.2.0 team gateway code on main | adapter, cmd |
| P8 Reconciler: hosted servers, mcp.json, federation volumes | 1.2.0 reconciler code on main | adapter |
| P9 Reports, docs, version 1.3.0 | P5 to P8 | all |
