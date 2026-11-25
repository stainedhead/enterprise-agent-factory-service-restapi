# Upgrading to 1.3.0

1.3.0 adds governed MCP servers (see [Governed MCP servers](mcp-servers.md)). It needs `-db` migrations 000024 to
000026 (`afs-api` runs them at start with `AFS_DATABASE_OWNER_URL`, or run `afs-migrate up`).

## Before you start

- Upgrade from 1.2.0. The migrations add tables, functions and nullable or defaulted columns; they rewrite no table.
- Nothing changes for existing agents until a server is approved and a team switches it on: an agent with no effective
  server gets no `AFS_MCP_*` variables and no `/etc/afs/mcp.json`, so its pod template is unchanged.
- MCP is served only by team gateways, so only `per_namespace` teams can switch a server on.
- Every service connection now assumes its group role (`role` startup parameter), so LOGIN users that are only
  members of `afs_app`, `afs_reconciler`, `afs_gateway`, `afs_billing` or `afs_edge` pass the schema's `current_user`
  guards. No change is needed when the login user is the role itself.

## New settings

| Binary | Settings | Needed when |
|---|---|---|
| `afs-reconciler` | `AFS_TEAM_GATEWAY_MCP_SESSION_KEYS` (from a Secret, Helm `mcpSessionKeys`), `AFS_MCP_BRIDGE_IMAGE` | any team uses MCP; any team hosts a server |
| `afs-gateway` | `AFS_GATEWAY_MCP_SEALING_KEYS` | set by the reconciler on team gateways; nothing to configure |
| `afs-api` | `AFS_MCP_OAUTH_CLIENT_ID`, `AFS_MCP_OAUTH_REDIRECT_URI`, `AFS_CONSOLE_URL` (Helm `mcpOAuth`) | servers with `oauth_user` auth |

Generate a master key with `openssl rand -base64 32` and store it in the Secret named by `mcpSessionKeys.secretName`.
To rotate, put the new key first and the old one second, wait one session lifetime, then drop the old key.

## After the upgrade

1. `GetSettings` shows `mcp.enabled` and the `mcp.*` defaults; MCP is off until a service admin turns it on.
2. Register a test server (`CreateMcpServer`), review and approve it, switch it on for a `per_namespace` test team
   (`PutTeamMcpServer`) and check the team gateway's `McpServerReady` condition.
3. Give a test agent access and check its `AFS_MCP_URL` and `/etc/afs/mcp.json`.

## Deferred in 1.3.0

Real EKS rollout of hosted servers, the Pod Identity association for hosted servers that need AWS, image signature
verification by admission policy, live IdP federation against Entra ID or Okta, and load tests at full scale wait for
an AWS environment.
