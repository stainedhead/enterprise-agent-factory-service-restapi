# API contract: `-restapi` ↔ `-cli`, `@afs/sdk` and the console

| | |
|---|---|
| **Status** | Contract **v1.6.0** (2026-11-25) adds governed MCP servers: catalog, team switches and team servers, review, policy, credentials and OAuth connect, the MCP ledger and reports, and federated identities (§4.14; [mcp-gateway-PRD.md](../../mcp-gateway-PRD.md), approved by J; ships with `-restapi` 1.3.0). Contract v1.5.0 (2026-11-25) adds the team model gateway: model endpoints, routes, provider switches and the inference report (§4.13; [model-endpoint-gateway-PRD.md](../../model-endpoint-gateway-PRD.md), approved by J; ships with `-restapi` 1.2.0). Contract v1.4.0 (2026-11-23) adds the team orchestration agent and model access ([team-orchestration-agent-PRD.md](../../team-orchestration-agent-PRD.md), approved by J; §4.4d, changes to §3, §4.2, §4.4, §4.4a, §4.4c, §4.11 and §5 to §7; decisions CD-26 to CD-38, see §9); it ships with `-restapi` 1.1.0 and is not yet built. Contract v1.3.2 (2026-11-22) records decisions from the build of the Paperclip collaboration change (see §9). v1.3.1 (2026-11-22) corrects v1.3.0 after the `-db`, `-restapi`, `-website` and `-cli` PRD reviews (see §9). v1.3.0 (2026-11-22) adds Paperclip team collaboration (§4.4b, §4.4c, the `team_collaborator` role and the events of §7; see §9); v1.2.0, draft for implementation (2026-11-21); v1.2.0 records the decisions of `-restapi` milestone M0 (CD-1 to CD-10, see §9) and, as an amendment of 2026-11-22, those of the feature-complete build (CD-7 exemption list, CD-11, CD-12, swarm template secrets, `Me.servicePermissions`; see §9); v1.1.0 was revised after the `review-prd` pass (root PRD D23 to D57) |
| **Provider** | [enterprise-agent-factory-service-restapi](https://github.com/stainedhead/enterprise-agent-factory-service-restapi) (`afs-api`) |
| **Consumers** | [-cli](https://github.com/stainedhead/enterprise-agent-factory-service-cli) (`afs`), [-website](https://github.com/stainedhead/enterprise-agent-factory-service-website) (`@afs/sdk`, console) |
| **Machine-readable form** | `-restapi/api/openapi.yaml` (OpenAPI 3.1). Built from this document; once an operation is in the YAML, the YAML wins and this document is updated to match in the same PR |
| **Requirements** | [Root PRD](../../enterprise-agent-factory-service-initial-PRD.md) §6; [solution design](../solution-design.md) §5, §10. Where the solution design or a repository PRD disagrees with this document, this document wins |
| **Related** | [Gateway contract](gateway-contract.md) (agents ↔ `afs-gateway`); [database contract](database-contract.md) |

This contract fixes everything a client may rely on: conventions, authentication, the permission
catalog, every operation (with its permission, CLI command, SDK method and console screen), resource
shapes, errors and events. A client never decides access; it reads `GET /v1/me` only to shape the UX.

## 1. Conventions

| Topic | Rule |
|---|---|
| Base | `https://<host>/v1`, JSON (`application/json; charset=utf-8`), UTF-8, camelCase fields |
| Ids | Server-generated prefixed ULIDs: `team_`, `agt_`, `pool_`, `stg_`, `sec_`, `rb_`, `key_`, `usr_`, `idp_`, `ext_`, `arl_`, `prl_`, `aid_`, `ncv_`, `acv_`, `pcv_`, `bud_`, `mpp_`, `vsnap_`, `wo_`, `swt_`, `exp_`, `rsc_`, `log_`, `nch_`, `egr_`, `xso_`, the same prefixes as the database contract. Clients treat them as opaque strings |
| Names | Team `slug`, agent `name`, secret `name`: `^[a-z][a-z0-9-]{1,38}[a-z0-9]$`, immutable after create |
| Timestamps | RFC 3339 UTC with `Z` (`2026-11-21T18:00:00Z`) |
| Money | Decimal string with up to 6 fraction digits, USD only (`"1234.50"`). Never a JSON number |
| Pagination | `?maxResults=1..100` (default 50) and `?nextToken=<token>` → `{"items": [...], "nextToken": "<token>"}`. `nextToken` absent on the last page. Tokens are opaque, expire after 24 h and are bound to the operation and the caller that received them: a token replayed by another caller or on another list is refused with `ValidationError` |
| Filtering | Query parameters named after fields (`?state=active&kind=ephemeral`). Unknown parameters → `ValidationError` |
| Concurrency | Every mutable resource has `version` (integer) and is returned with `ETag: "<version>"`. `PATCH` and `DELETE` on an existing resource require `If-Match` (absent → `428`); on `PUT` it is optional, because `PUT` is an upsert: with it, a mismatch → `412 PreconditionFailed`; without it, the write applies to the current version (CD-2). One more exception: a workload principal may omit `If-Match` on `UpdateWorkOrderRepo` (§4.10a, CD-12) |
| Deletes | `DELETE` of a resource that carries a reconcile `status` (`DeleteTeam`, `DeleteAgent`, `DeletePool`, `DeleteLogDestination`, `DeleteNotificationChannel`) answers `202` with the resource body (`status.phase` `Pending`, `state` `deleting` where the resource has one). Every other `DELETE` answers `204` with no body (CD-1) |
| Body size and unknown routes | A request body over the limit is `400 ValidationError` (`details.fields[0].field = body`), not `413`. An unknown path or an unknown `:verb` is `404 NotFound` in the error envelope, never a bare `404` or a `405` page |
| Idempotency | `POST` that creates (and `POST …:verb` actions) accepts `Idempotency-Key` (1–255 chars). Keys are scoped to the calling principal and kept for service setting `api.idempotency_key_ttl_seconds` (default 3600, so 1 hour). A repeat with the same key and the same operation and body returns the stored status and body with header `Idempotent-Replayed: true`; the same key with a different body or operation → `409 Conflict` (`details.reason = idempotency_mismatch`); a repeat while the first is still running → `409 Conflict` (`details.reason = idempotency_in_progress`, `Retry-After: 1`). Error responses of 5xx are not stored, so a retry runs again |
| Actions | Non-CRUD verbs are `POST …/{id}:verb` (`:suspend`, `:restart`, `:approve`) |
| Async | Mutations return the stored desired state at once with `status.phase` `Pending`. Clients poll `GET …` (or `…/status`) until `Ready`, `Degraded` or `Failed`. `status.observedGeneration` equals `generation` when applied |
| Request id | Every response has `X-Request-Id`; a client may send its own (≤ 64 chars). It appears in errors and audit |
| Versioning | Additive changes (new optional field, new operation, new enum value documented as open) are minor. Removing or renaming a field, changing a type or making a field required is a new major path (`/v2`). Clients ignore unknown fields |
| Rate limits | 429 `Throttled` with `Retry-After` seconds. Default 20 req/s per principal, burst 40 |
| CSV | Report and list endpoints marked *CSV* also answer `Accept: text/csv`. A cell that would be read as a formula (starts with `=`, `+`, `@`, tab or carriage return, or with `-` not followed by a number) is prefixed with `'`; negative amounts stay numeric. A CSV body has no `nextToken` field, so the next page's token travels in the response header `X-Afs-Next-Token` (absent on the last page). The CLI's `--output csv` is valid only on those operations (D39) |
| Contract version | Every response carries `X-Afs-Contract-Version: <semver>` (this document's version). A client built against an older minor keeps working and may print a notice once per session; a newer major is refused by path (`/v2`) (D47) |
| CORS | `afs-api` answers CORS preflight and sets `Access-Control-Allow-Origin` only for origins listed in service setting `api.cors_origins` (default empty: no browser origin allowed). Allowed headers: `Authorization`, `Content-Type`, `If-Match`, `Idempotency-Key`, `X-Request-Id`; exposed headers: `ETag`, `X-Request-Id`, `Idempotent-Replayed`, `Retry-After`, `X-Afs-Contract-Version`, `X-Afs-Next-Token`. No credentials mode (tokens go in `Authorization`) (D43) |

### 1.1 Status object

Every desired-state resource carries:

```json
"status": {
  "phase": "Pending | Reconciling | Ready | Degraded | Failed | Deleting",
  "observedGeneration": 7,
  "conditions": [{"type": "NamespaceReady", "status": "True|False|Unknown", "reason": "<CamelCase>", "message": "<text>", "lastTransitionTime": "<RFC 3339>"}],
  "updatedAt": "<RFC 3339>"
}
```

These are the only status words (root PRD D24). Lifecycle is separate: `Team.state`, `Agent.paused`,
`Pool.state`. Budget exhaustion is the condition `BudgetExhausted=True` on a `Ready` team, agent or pool;
consoles show it as *Budget blocked*. Other conditions in v1: `NamespaceReady`, `IamReady`,
`SecretsSynced`, `VolumeReady`, `EgressPolicyReady`, `TemplateRetired`, `DriftCorrected`.

## 2. Authentication

| Client | Flow | Credential sent |
|---|---|---|
| Console | OIDC authorization code + PKCE against the team's or the default provider (`ListSignInProviders` returns `consoleClientId`); a provider picker when more than one exists, `?provider=<idp id>` skips it (D42) | `Authorization: Bearer <IdP access token>` |
| CLI | OIDC device authorization grant (`afs sso login`) with the provider's public `cliClientId` from `ListSignInProviders` (D48) | Same; cached under `~/.afs/sso/cache` (0600) |
| Pipelines | Team API key (`afsk_…` prefix), created once and shown once | `Authorization: Bearer afsk_<secret>` |
| Security tooling | Service-level API key with the `security_operator` role (created by service admins) | `Authorization: Bearer afsk_<secret>` |
| Agents and pools (workload) | Projected ServiceAccount token, audience `afs-api`, mounted at `/var/run/secrets/afs/api/token`, validated offline against the cluster's OIDC issuer JWKS (issuer = the cluster's service account issuer, audience `afs-api`); `afs-api` never calls Kubernetes, so it does not use TokenReview (D29) | `Authorization: Bearer <projected token>` |

The API validates IdP tokens against the provider's JWKS (any enabled service-level provider). The
groups claim (`groupsClaim` of the provider) maps to group role bindings. For a team-scoped request, a
group binding counts only when the token's issuer is the team's provider (or the default provider when
the team has none). Team API keys carry one team and one role (`team_admin`, `team_collaborator` or `team_viewer`). Service-level API keys
carry no team and only the `security_operator` role. Security operators (people bound to the role, or
those keys) can use only the work order operations, `ListSwarmTemplates`, `GetSwarmTemplate`,
`ListTeamDirectory` and `GetMe`; every team-scoped route (`/v1/teams/{teamId}/…`), including
`ListTeamWorkOrders`, returns `403 AccessDenied` to them, and they list one team's work orders with
`ListWorkOrders?teamId=`. `afs-api` sets `afs.security_operator` for them so the database lets them
read and write work orders across teams.

A **workload principal** (`type: workload`, `id` = the agent or pool id, team from the token's namespace)
holds only `work-orders:report` on work orders assigned to its own team, and may call `GetMe`. Everything
else returns `403`. Its actions are audited with the agent or pool id and, for pools, the worker pod name.
Because it holds no read permission, it cannot learn the `version` that `If-Match` needs: on
`UpdateWorkOrderRepo` it may omit `If-Match`, and the update then applies to the current version under a
row lock. An `If-Match` it does send is still validated (`400` if malformed) and compared (`412` if
stale). Every other caller, and every other operation, keeps the `428` rule (CD-12).

`401 Unauthenticated` for a missing, expired or unknown credential; `403 AccessDenied` for a valid
caller without the permission. A caller bound to no role on a team gets `404 NotFound` on that team's
resources (never revealing existence); a bound caller without the permission gets `403`. A signed-in
caller with no binding at all gets `403 AccessDenied` (with `details.requiredPermission`) on a route that
is not scoped to one team and needs a permission, such as `ListTeams`: there is no team whose existence
to hide (CD-10).

**Identity providers: two client ids.** The contract and OpenAPI carry `consoleClientId` and
`cliClientId` (D48). They map to `afs.identity_providers.client_id` (the console client) and
`cli_client_id` (-db 000017, optional, the public client for the CLI device grant). When
`cli_client_id` is empty, `ListSignInProviders` omits `cliClientId` and `afs sso login` exits 255 naming
the provider (CD-9). On the wire the two members stay `consoleClientId` and `cliClientId` (the
OpenAPI document is authoritative); the database column names do not appear in the API.

## 2.1 Collaboration edge credentials

The public collaboration endpoint (§4.4c) is not `afs-api`, but it accepts the same credentials: the team provider's access token or a team API key, sent in `X-Afs-Authorization: Bearer <token>` (the Paperclip credential stays in `Authorization`), or, for browsers, a host-only edge session obtained by signing in through the team's provider. Workload and security-operator principals are never admitted.

## 3. Permission catalog

The catalog is implemented once, in `-restapi/internal/authz`. `-website/apps/console/src/domain/permission.ts`
holds the same list for tooltips and a drift test; the console's `can()` reads the per-team
`permissions[]` that `GetMe` returns, which is the API's own decision (D42). A change here is a change in
both, in the same cycle.

| Permission | Viewer | Team-admin | Service admin | Security operator | Notes |
|---|:-:|:-:|:-:|:-:|---|
| `teams:read` | ✓ | ✓ | ✓ | | A team role sees only its bound teams |
| `teams:write` | | ✓ | ✓ | | Display name, description, owner, sign-in provider |
| `teams:suspend` | | ✓ | ✓ | | Suspend and resume |
| `teams:create`, `teams:delete` | | | ✓ | | |
| `teams:set-guardrails` | | | ✓ | | Ceiling, ceiling override, quotas, isolation tier, `viewerKubectl`, models, `mayManageAwsAccounts`, `mayAddExternalSystems`, and `onExhaust: notify` on a budget |
| `teams:set-aws-accounts` | | ✓ when `mayManageAwsAccounts` | ✓ | | Checked by the API and again by the database |
| `teams:read-directory` | | | ✓ | ✓ | Team id, slug and display name only (D38, D56) |
| `members:read` / `members:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | Team-admins cannot grant `service_admin` or `security_operator` |
| `api-keys:read` / `api-keys:write` | | ✓ / ✓ | ✓ / ✓ | | Viewers do not see API keys (D50) |
| `agents:read` / `agents:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | Write covers pause, resume, restart, snapshot |
| `secrets:read-metadata` / `secrets:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | No permission reads a value |
| `config:read` / `config:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | Namespace and agent configuration values |
| `identities:read` / `identities:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | Agent IAM identity, role links, external identities |
| `external-systems:read` / `external-systems:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | Team and agent switches; declare when `mayAddExternalSystems` |
| `budgets:read` / `budgets:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | Amount ≤ effective ceiling; `onExhaust: notify` needs `teams:set-guardrails` (D33) |
| `usage:read` | ✓ | ✓ | ✓ | | Usage, costs, reconciliation |
| `reports:read` | ✓ | ✓ | ✓ | | Run team and agent reports, CSV; own teams |
| `reports:schedule` | | ✓ | ✓ | | Create, update, delete report schedules (D23) |
| `tags:read` / `tags:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | Team tags and required values |
| `logs:read` / `logs:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | |
| `notifications:read` / `notifications:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | |
| `audit:read`, `revisions:read` | ✓ | ✓ | ✓ | | Own teams |
| `revisions:restore` | | ✓ | ✓ | | |
| `models:read` | ✓ | ✓ | ✓ | | Enabled models, storage classes, agent templates |
| `admin:settings`, `admin:models`, `admin:pricing`, `admin:identity-providers`, `admin:service-tags`, `admin:external-systems`, `admin:storage-classes`, `admin:agent-templates`, `admin:accounts`, `admin:reports`, `admin:audit`, `admin:export` | | | ✓ | | Service level |
| `pools:read` / `pools:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | Stages, pools, handoffs, pool secrets, config, external systems and role links; ephemeral teams only. Write covers scale, pause, resume |
| `teams:get-kubeconfig` | ✓ when `viewerKubectl` | ✓ when `viewerKubectl` | ✓ | | Read-only kubeconfig for the namespace (ACC-5). Service admins turn it on with `teams:set-guardrails` |
| `work-orders:read` | ✓ assigned or own repos | ✓ assigned or own repos | ✓ | ✓ all | |
| `work-orders:write` | | | ✓ | ✓ | Create, update, cancel |
| `work-orders:report` | | ✓ on the assigned team | ✓ | | Update the status and PR of the assigned work order's repositories. Also held by the assigned team's workload principals (D29) |
| `swarm-templates:read` | | | ✓ | ✓ | List and describe templates (D37) |
| `admin:swarm-templates`, `admin:cost-tag-status` | | | ✓ | | Create, update, enable, disable, delete templates; cost tag status |
| `admin:model-endpoints`, `admin:inference-providers` | | | ✓ | | v1.5.0: service model endpoints, credentials, federation; provider switches including Bedrock |
| `model-endpoints:read` / `model-endpoints:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | v1.5.0: the team's routes; opt in, declare team endpoints (when allowed), team credentials |
| `mcp-servers:read`, `mcp-servers:review`, `mcp-servers:suspend` | | | ✓ | ✓ | v1.6.0: service MCP paths, review queue, suspend any server |
| `mcp-servers:write`, `mcp-settings:write` | | | ✓ | | v1.6.0: enterprise servers, service policy, shared credentials, `mcp.*` settings, query capture |
| `team-mcp-servers:read` / `team-mcp-servers:write` | ✓ / | ✓ / ✓ | ✓ / ✓ | | v1.6.0: the team's switches, team servers, team policy and credentials |
| `team-mcp-servers:approve` | | ✓ when `mcp.team_server_approval` allows the tier | ✓ | | v1.6.0: condition `mcpApprovalPolicy`, decided in the use case |
| `mcp-events:read` | ✓ | ✓ | ✓ | ✓ | v1.6.0: the MCP ledger and reports; team paths for team roles |
| `agent-federated-identities:write` | | ✓ | ✓ | | v1.6.0: agent and pool federated identities |

**Security operator** (service-level role, D18): only the permissions in its column. It cannot be
granted by team-admins and never appears in a team binding. **Workload principal**: `work-orders:report`
on its own team's assigned work orders, nothing else.

**Collaboration permissions and the `team_collaborator` role** (v1.3.0, PC-D6). The role `team_collaborator` (a team role,
like `team_viewer`; bindings and team API keys may carry it, service-level ones may not) holds **every Viewer permission in
the table above, including the conditional ones** (`teams:get-kubeconfig` when `viewerKubectl` is switched on for the team, and `work-orders:read` for assigned work orders or own repositories) plus `collaboration:contribute` and `collaboration:api`. The new permissions:

| Permission | Viewer | Collaborator | Team-admin | Service admin | Security operator | Notes |
|---|:-:|:-:|:-:|:-:|:-:|---|
| `collaboration:read` | ✓ | ✓ | ✓ | ✓ | | Settings, status, hostname, links (`GetTeamCollaboration`, `ListCollaborationLinks`) |
| `collaboration:manage` | | | ✓ | ✓ | | `PutTeamCollaboration`, connect, disconnect, rotate keys |
| `collaboration:access` | ✓ | ✓ | ✓ | ✓ | | Sign in to Paperclip through the edge; `GET` and `HEAD` only (checked by the edge) |
| `collaboration:contribute` | | ✓ | ✓ | ✓ | | Every method through the edge, including WebSocket client frames |
| `collaboration:api` | | ✓ | ✓ | ✓ | | Use the REST API surface (`X-Afs-Authorization`); writes also need `collaboration:contribute` |

`teams:set-guardrails` also covers `mayExposeCollaboration`. `ListCollaborationEndpoints` uses `admin:settings`.

**v1.4.0 scope changes (orchestration PRD §4, OR-D9).** No permission is added or renamed; these change scope:

| Permission | Change |
|---|---|
| `collaboration:read` | Also `GetTeamOrchestration` |
| `collaboration:manage` | Also `PutTeamOrchestration` (exposure levels, registration mode) and `ChangeCollaborationTool` |
| `teams:set-guardrails` | Sets `maxCollaborationExposure` (replaces `mayExposeCollaboration`) and a team's model access (`PutTeamModels` with `access`, `CreateTeam.modelAccess`; OR-D16) |
| `agents:write` | Still covers update, pause, resume, restart and snapshot of the orchestration agent, except its fixed fields (§4.4d), and an agent's `models`. It no longer creates or deletes an orchestration agent, for any role: only `CreateTeam`, `instantiate_work_order_team` and team deletion do |
| `pools:write` | Also a pool's `models` |
| `admin:models` | Also `PutServiceModelAccess` |
| `admin:settings` | Also `ListOrchestrationEndpoints` and the `orchestration.*` and `models.*` settings | |

The new
permissions are a change in `internal/authz` and `domain/permission.ts` in the same cycle. Team-admins grant
`team_collaborator` like the other team roles and cannot grant `service_admin` or `security_operator`.

Service admins hold every permission on every team. `GET /v1/me` returns, per bound team, the
effective permission list; for service admins it returns `"serviceAdmin": true` and the full list;
for security operators `"securityOperator": true` with their column's permissions.

## 4. Operations

Columns: **Op** = OpenAPI `operationId`; **Perm** = permission (team-scoped unless marked *svc*);
**CLI** = `afs <service> <operation>`; **SDK** = `@afs/sdk` method; **Screen** = console screen in the
root PRD §5.3. Every mutation writes the row and an audit record in one transaction, and, when it has an event type in §7, an outbox event in the same transaction. Mutations with no §7 event type (members, API keys, settings, identities, config values and the like) write no outbox row (CD-7); §4.12 lists them.
P = priority from the root PRD. One row per operation; the CLI and SDK columns are the complete lists
(the CLI adds only `configure`, `sso login`, `sso logout` and `completion`, which call no operation).

### 4.1 Caller and sign-in

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `GetMe` | `GET /v1/me` | any signed-in | `sso get-caller-identity` | `me.get()` | header, all | P0 |
| `ListSignInProviders` | `GET /v1/identity-providers` (per enabled provider: `id`, `kind`, `name`, `issuerUrl`, `consoleClientId`, `cliClientId`, `isDefault`; no secret) | none (public) | `sso list-providers` | `auth.listProviders()` | sign-in | P0 |

### 4.2 Teams

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListTeams` | `GET /v1/teams` | `teams:read` | `teams list-teams` | `teams.list()` | S2, team switcher | P0 |
| `CreateTeam` | `POST /v1/teams` | `teams:create` *svc* | `teams create-team` | `teams.create()` | S3, P2 | P0 |
| `GetTeam` | `GET /v1/teams/{teamId}` | `teams:read` | `teams describe-team` | `teams.get()` | T1, V1 | P0 |
| `UpdateTeam` | `PATCH /v1/teams/{teamId}` | `teams:write`; guardrail fields `teams:set-guardrails`; `allowedAwsAccounts` `teams:set-aws-accounts` | `teams update-team` | `teams.update()` | T1, S6 | P0 |
| `DeleteTeam` | `DELETE /v1/teams/{teamId}` | `teams:delete` *svc* | `teams delete-team` | `teams.delete()` | S2 | P0 |
| `SuspendTeam` / `ResumeTeam` | `POST /v1/teams/{teamId}:suspend` / `:resume` | `teams:suspend` | `teams suspend-team` / `resume-team` | `teams.suspend()` / `resume()` | S2, T1 | P0 |
| `GetTeamStatus` | `GET /v1/teams/{teamId}/status` | `teams:read` | `teams get-team-status` | `teams.getStatus()` | T1 | P0 |
| `ListTeamModels` / `PutTeamModels` | `GET`/`PUT /v1/teams/{teamId}/models` | `teams:read` / `teams:set-guardrails` | `teams list-team-models` / `put-team-models` | `teams.models.list()` / `put()` | S3, T1 | P0 |
| `GetTeamTags` | `GET /v1/teams/{teamId}/tags` | `tags:read` | `teams get-team-tags` | `teams.tags.get()` | T13 | P1 |
| `PutTeamTags` | `PUT /v1/teams/{teamId}/tags` | `tags:write` | `teams put-team-tags` | `teams.tags.put()` | T13, S3 | P1 |
| `ListAllowedAccounts` | `GET /v1/teams/{teamId}/allowed-accounts` | `identities:read` | `teams list-allowed-accounts` | `teams.allowedAccounts.list()` | S6, T8 | P1 |
| `GetTeamKubeconfig` | `GET /v1/teams/{teamId}/kubeconfig` (YAML, `exec` credential plugin, no token in the file) | `teams:get-kubeconfig` | `teams get-kubeconfig` | `teams.getKubeconfig()` | T1, V1 | P2 |
| `ListTeamDirectory` | `GET /v1/team-directory` (`id`, `slug`, `displayName` only; every non-deleted team) | `teams:read-directory` *svc* | `teams list-team-directory` | `teams.directory.list()` | P3 create dialog | P2 |

`CreateTeam` with a slug whose collaboration hostname is still held (30 days after the deletion of a team that had a collaboration agent, TEAM-9) → `409 Conflict`, `details.reason = hostname_held` (v1.3.1).

`CreateTeam` body includes the first team-admin binding, the team's models, required tag values and,
when more than one provider exists, `identityProviderId`. A team missing a required tag is refused
with `ValidationError` naming the keys. An ephemeral team (`kind: ephemeral`) also needs `expiresAt`
(at most `teams.ephemeral_max_days` ahead), `teardownRule` (`at_expiry` \| `at_cap_or_expiry` \|
`at_work_complete`) and a lifetime `budget` in the same request; `kind` cannot change later.
`mayExposeCollaboration` (boolean, default `true`; whether the team may publish its Paperclip endpoints, §4.4b), `isolationTier`, `viewerKubectl`, `maxBudgetUsd`, `quotas`, `ceilingOverrideUsd` and
`ceilingOverrideExpiresAt` are guardrail fields (`teams:set-guardrails`). An override needs both fields
and a `ceilingOverrideReason` (write-only, stored in the audit record, not on the team), an expiry in the future and an amount above `maxBudgetUsd`; while it is unexpired it is
the effective ceiling. At expiry the reconciler clears it, lowers a budget above `maxBudgetUsd` to it and
emits `budget.ceiling_override_expired` (D34). `maxBudgetUsd` below the current budget amount →
`ValidationError`. A change to `kind` → `ValidationError`. `SuspendTeam` and `DeleteTeam` act as the
root PRD's TEAM-9 table says; on a suspended team every mutation except `ResumeTeam` and `DeleteTeam`
→ `409 Conflict` (`details.reason = team_suspended`).

**v1.4.0: orchestration and model access on teams.** `CreateTeam` also creates the team's orchestration agent in the same
transaction (§4.4d, ORC-2): `afs-api` generates the tool's boot secrets in Secrets Manager first (failure → `503
Unavailable`, nothing is created), then writes the team, calls `afs.create_orchestration_agent` (database contract §7.3.7),
records the boot secrets, and writes audit and outbox in one transaction. New optional body fields:

- `orchestration {tool?, webUiExposure?, webApiExposure?, registrationMode?, models?{access, modelIds, defaultModelId},
  modelId?, resources?{cpu, memory}, volume?{sizeGiB, onDelete?}}` (ORC-26). Defaults: the service default tool of
  `orchestration.tools`; levels from `orchestration.default_web_ui_exposure` and `.default_web_api_exposure`, each lowered
  to the team's limit (never refused for being above it, because they are defaults), and `off` for a surface the tool
  lacks; `registrationMode` `manual` for Paperclip, `managed` for Buzz; `models` `{access: all}` with the default model of
  §4.4 *Models*; `modelId` is shorthand for `models.defaultModelId` (CD-36); resources and volume size from
  `orchestration.default_resources` and `.default_volume_gib`. Explicit levels above the limit → `ValidationError`
  (`field: orchestration.webUiExposure`, `reason: exposure_above_limit`); a disabled tool → `ValidationError`
  (`field: orchestration.tool`, `reason: tool_not_enabled`); a tool whose runtime is unset → `409 Conflict`
  (`orchestration_runtime_unset`); no model the agent could default to → `ValidationError`
  (`field: orchestration.models.defaultModelId`, `reason: no_model_available`).
- `maxCollaborationExposure` (`off` \| `private` \| `public`; default `orchestration.default_limit`; guardrail field,
  `teams:set-guardrails`). `UpdateTeam` sets it too; lowering it lowers the effective levels within 60 s and keeps the
  team's choices (ORC-14). `mayExposeCollaboration` is its deprecated alias (§4.4d).
- `modelAccess` (`all` \| `selected`; default `models.default_team_access`; guardrail field). With `selected`, the existing
  `models[]` is the list (default `models.default_team_models`); `models[]` sent without `modelAccess` means `selected`, as a
  1.0.1 client meant it (CD-30). Each listed model must be enabled for the service (`model_not_enabled_for_service`).
  After create, `modelAccess` changes only through `PutTeamModels`; `UpdateTeam` does not take it.

`Team` returns `orchestrationAgentId†` (null only while the backfill has not reached a 1.0.1 team), `maxCollaborationExposure`
and `modelAccess`. `CreateTeam` emits `team.created`, `agent.created`, `orchestration.created` and one `secret.created` per
boot secret. `DeleteTeam` deletes the orchestration agent last, after the workers (TEAM-9).

**`ListTeamModels`** → `{access, modelIds, items: [TeamModel]}` (`ETag` is the team's `version`). `access` is the team's
mode; `modelIds` is the stored selection (empty for `all`, and possibly naming models the service has since disabled);
`items` is the team's allowed set (service ∩ team), each a `Model` (§5) with `callable†`, `priceState†` and
`inferenceProfileState†` (`creating`, `active`, `retiring` or `null`). **`PutTeamModels`** body `{access, modelIds?}`:
`selected` takes `modelIds` (0 to 100 distinct ids, each enabled for the service); `all` takes no `modelIds`
(`ValidationError`, `reason: model_ids_not_allowed_for_all`) and keeps the stored selection for a later switch back; a body
with only `modelIds` is `selected` (deprecated form). Narrowing never edits an agent's stored selection (MOD-6). Emits
`team.models_changed` and, as in 1.0.1, `team.updated`.

### 4.3 Members and API keys

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListMembers` | `GET /v1/teams/{teamId}/members` | `members:read` | `members list-members` | `members.list()` | T6 | P0 |
| `AddMember` | `POST /v1/teams/{teamId}/members` | `members:write` | `members add-member` | `members.add()` | T6 | P0 |
| `UpdateMember` | `PATCH /v1/teams/{teamId}/members/{bindingId}` | `members:write` | `members update-member` | `members.update()` | T6 | P0 |
| `RemoveMember` | `DELETE /v1/teams/{teamId}/members/{bindingId}` | `members:write` | `members remove-member` | `members.remove()` | T6 | P0 |
| `ListApiKeys` | `GET /v1/teams/{teamId}/api-keys` | `api-keys:read` | `api-keys list-api-keys` | `apiKeys.list()` | T6 | P1 |
| `CreateApiKey` | `POST /v1/teams/{teamId}/api-keys` | `api-keys:write` | `api-keys create-api-key` | `apiKeys.create()` | T6 | P1 |
| `DeleteApiKey` | `DELETE /v1/teams/{teamId}/api-keys/{keyId}` | `api-keys:write` | `api-keys delete-api-key` | `apiKeys.delete()` | T6 | P1 |
| `ListServiceApiKeys` | `GET /v1/admin/api-keys` | `admin:settings` *svc* | `admin list-api-keys` | `admin.apiKeys.list()` | S10 | P2 |
| `CreateServiceApiKey` | `POST /v1/admin/api-keys` (role `security_operator` only) | `admin:settings` *svc* | `admin create-api-key` | `admin.apiKeys.create()` | S10 | P2 |
| `DeleteServiceApiKey` | `DELETE /v1/admin/api-keys/{keyId}` | `admin:settings` *svc* | `admin delete-api-key` | `admin.apiKeys.delete()` | S10 | P2 |
| `ListSecurityOperators` | `GET /v1/admin/security-operators` (users or IdP groups) | `admin:settings` *svc* | `admin list-security-operators` | `admin.securityOperators.list()` | S10 | P2 |
| `AddSecurityOperator` | `POST /v1/admin/security-operators` | `admin:settings` *svc* | `admin add-security-operator` | `admin.securityOperators.add()` | S10 | P2 |
| `RemoveSecurityOperator` | `DELETE /v1/admin/security-operators/{bindingId}` | `admin:settings` *svc* | `admin remove-security-operator` | `admin.securityOperators.remove()` | S10 | P2 |

`CreateApiKey` and `CreateServiceApiKey` return `secretKey` once; it is never returned again, never
logged and never stored with the idempotency record. A replay with the same `Idempotency-Key` returns the
key's metadata without `secretKey` and `Idempotent-Replayed: true`; a caller that lost the secret deletes
the key and creates another (D52).

### 4.4 Agents

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListAgents` | `GET /v1/teams/{teamId}/agents` | `agents:read` | `agents list-agents` | `agents.list()` | T2 | P0 |
| `CreateAgent` | `POST /v1/teams/{teamId}/agents` | `agents:write` | `agents create-agent` | `agents.create()` | T3 | P0 |
| `GetAgent` | `GET /v1/teams/{teamId}/agents/{agentId}` | `agents:read` | `agents describe-agent` | `agents.get()` | T2 | P0 |
| `UpdateAgent` | `PATCH /v1/teams/{teamId}/agents/{agentId}` | `agents:write` | `agents update-agent` | `agents.update()` | T3 | P0 |
| `DeleteAgent` | `DELETE /v1/teams/{teamId}/agents/{agentId}` | `agents:write` | `agents delete-agent` | `agents.delete()` | T2 | P0 |
| `PauseAgent` / `ResumeAgent` / `RestartAgent` | `POST /v1/teams/{teamId}/agents/{agentId}:pause` / `:resume` / `:restart` | `agents:write` | `agents pause-agent` / `resume-agent` / `restart-agent` | `agents.pause()` / `resume()` / `restart()` | T2 | P0 |
| `GetAgentStatus` | `GET /v1/teams/{teamId}/agents/{agentId}/status` | `agents:read` | `agents get-agent-status` | `agents.getStatus()` | T2 | P0 |
| `ListAgentSnapshots` | `GET /v1/teams/{teamId}/agents/{agentId}/snapshots` | `agents:read` | `agents list-snapshots` | `agents.snapshots.list()` | T2 | P0 |
| `CreateAgentSnapshot` | `POST /v1/teams/{teamId}/agents/{agentId}/snapshots` | `agents:write` | `agents create-snapshot` | `agents.snapshots.create()` | T2 | P0 |
| `ListStorageClasses` | `GET /v1/storage-classes` | `models:read` | `agents list-storage-classes` | `storageClasses.list()` | T3 | P0 |
| `ListAgentTemplates` | `GET /v1/agent-templates` (enabled templates with allowed digests) | `models:read` | `agents list-agent-templates` | `agentTemplates.list()` | T3 | P0 |

Agent spec fields (create and update): `name` (create only), `description`, `template`, `image`
(digest-pinned), `modelId`, `resources`, `iamIdentity` (`team_shared` \| `dedicated`), `volume`
{`sizeGiB` grow-only, `storageClass` create-only, `onDelete`, `snapshotSchedule`, `snapshotRetain`},
`secretBindings[]` {`secretId`, `delivery` `env` \| `file`, `target`}. The agent has no replica count.

**Models (v1.4.0, MOD-3, MOD-7).** Agents (workers and the orchestration agent) carry `models {access: all|selected,
modelIds[], defaultModelId, effectiveModelIds†}`. `all` (the default on create) means every model the team may use, now and
later; `selected` names 1 to 50 of them. `defaultModelId` is the model the runtime starts with. `effectiveModelIds†` is
service ∩ team ∩ agent ∩ callable at read time; stored ids that dropped out stay in `modelIds` (the console strikes them
through). Rules (CD-31):

- Create: `models` optional. Its `defaultModelId`, else the deprecated `modelId`, else setting `models.default_model_id`
  when the team allows it, else the team's first callable model by id; none → `ValidationError`
  (`field: models.defaultModelId`, `reason: required`). `modelId` alone creates `{access: all, defaultModelId: modelId}`.
- Update (`PATCH`): members of `models` that are sent replace the stored ones; omitted members are kept. `modelId` alone sets
  only `defaultModelId`. `modelId` and `models.defaultModelId` with different values → `ValidationError`
  (`reason: conflicting_fields`).
- Refusals (`ValidationError`): a listed model not enabled for the service or not allowed for the team
  (`field: models.modelIds[i]`, `reason: model_not_enabled_for_service` or `model_not_enabled_for_team`); `selected` with no
  ids (`models_selection_empty`); a default outside the selection or the team's set (`default_model_not_allowed`). A model
  with no approved price may be selected; the gateway refuses it until it is priced.
- A change of `models` bumps `generation` and rolls the pod (MOD-4). An agent whose default leaves its effective set gets
  condition `ModelUnavailable=True` (with the model and reason) and the event `agent.model_unavailable`; one whose effective
  set is empty is `Degraded` (MOD-6). `ListAgents` and `GetAgent` show `models` to every role with `agents:read`.

v1.4.0 makes the role and collaboration rules below historical: see §4.4d (*Agents and the orchestration agent*).

v1.3.0 adds `role` and `collaboration` (§4.4b):

- `role`: `worker` (default) or `collaboration`; set on create, immutable (`UpdateAgent` changing it → `ValidationError`).
  `ListAgents` accepts `?role=`.
- `collaboration` (worker agents): `{connect, paperclipAgentId?, linkState†, lastSyncedAt†?, lastError†?}`. On create,
  `connect` defaults to `true` when the team has a collaboration agent, else `false`; `connect: true` with no collaboration
  agent → `ValidationError` (`field: collaboration.connect`). `connect` and, in `manual` registration, `paperclipAgentId`
  are writable by `UpdateAgent` (the same effect as `ConnectAgentToCollaboration`); changing `connect` rolls the agent.
  The object is absent on the collaboration agent.
- `CreateAgent` with `role: collaboration`: `template` must be an enabled agent template whose `capabilities` contain
  `paperclip` (else `ValidationError`, `field: template`, naming it); a `paperclip` template is refused for a worker;
  `iamIdentity` defaults to and must be `dedicated` (`team_shared` → `ValidationError`); `volume.snapshotSchedule`
  defaults to `daily` with `snapshotRetain` 14 and `off` → `ValidationError`; `volume.onDelete` defaults to
  `snapshot_then_delete`; `collaboration` must be omitted. A team that already has a live collaboration agent →
  `409 Conflict`, `details.reason = collaboration_agent_exists`. In the same request `afs-api` generates the three boot
  secrets of COL-7 (`paperclip-better-auth-secret`, `paperclip-tool-action-signing-secret`, `paperclip-secrets-master-key`,
  32 random bytes each, hex) in Secrets Manager with `managedBy = afs`, binds them as `BETTER_AUTH_SECRET`,
  `PAPERCLIP_TOOL_ACTION_SIGNING_SECRET` and `PAPERCLIP_SECRETS_MASTER_KEY`, and creates the team's `TeamCollaboration`
  with its defaults; if Secrets Manager cannot be written the request fails `503 Unavailable` and creates nothing. It emits
  `agent.created` and one `secret.created` per boot secret. `DeleteAgent` of the collaboration agent disables both surfaces,
  removes the route and disconnects every agent (COL-8).
- `ListAgentTemplates` and `PutAgentTemplate` carry `capabilities[]` (values: `paperclip`); `ListAgentTemplates` accepts
  `?capability=`. The admin listing `ListAdminAgentTemplates` has **no** `capability` filter (decision RCF-1, v1.3.2): each
  item carries `capabilities[]`, so admin clients filter locally, and the CLI and PRD drop `afs admin list-agent-templates
  --capability`.

### 4.4a Swarms: stages and pools

Ephemeral teams only (`409 Conflict` on a persistent team). `afs-api` checks the team's `kind` before the write and returns `409`, because the database's guard would otherwise raise `23514` (`ValidationError`). Pool names share the team's agent names (v1.4.0: `orchestrator` is reserved, `name_reserved`). v1.4.0: `CreatePool` and `UpdatePool` take `models` with the rules of §4.4 *Models*; the gateway admits a pool worker's call only within the pool's effective set (service ∩ team ∩ pool ∩ callable).

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListStages` | `GET /v1/teams/{teamId}/stages` | `pools:read` | `pools list-stages` | `pools.stages.list()` | P4 | P2 |
| `CreateStage` | `POST /v1/teams/{teamId}/stages` (`name`, `position`) | `pools:write` | `pools create-stage` | `pools.stages.create()` | P4 | P2 |
| `UpdateStage` | `PATCH /v1/teams/{teamId}/stages/{stageId}` | `pools:write` | `pools update-stage` | `pools.stages.update()` | P4 | P2 |
| `DeleteStage` | `DELETE /v1/teams/{teamId}/stages/{stageId}` | `pools:write` | `pools delete-stage` | `pools.stages.delete()` | P4 | P2 |
| `ListPools` | `GET /v1/teams/{teamId}/pools` | `pools:read` | `pools list-pools` | `pools.list()` | P4 | P2 |
| `CreatePool` | `POST /v1/teams/{teamId}/pools` | `pools:write` | `pools create-pool` | `pools.create()` | P4 | P2 |
| `GetPool` | `GET /v1/teams/{teamId}/pools/{poolId}` | `pools:read` | `pools describe-pool` | `pools.get()` | P4 | P2 |
| `UpdatePool` | `PATCH /v1/teams/{teamId}/pools/{poolId}` | `pools:write` | `pools update-pool` | `pools.update()` | P4 | P2 |
| `DeletePool` | `DELETE /v1/teams/{teamId}/pools/{poolId}` | `pools:write` | `pools delete-pool` | `pools.delete()` | P4 | P2 |
| `ScalePool` | `POST /v1/teams/{teamId}/pools/{poolId}:scale` `{workers}` (0–200) | `pools:write` | `pools scale-pool` | `pools.scale()` | P4 | P2 |
| `PausePool` | `POST /v1/teams/{teamId}/pools/{poolId}:pause` | `pools:write` | `pools pause-pool` | `pools.pause()` | P4 | P2 |
| `ResumePool` | `POST /v1/teams/{teamId}/pools/{poolId}:resume` | `pools:write` | `pools resume-pool` | `pools.resume()` | P4 | P2 |
| `GetPoolStatus` | `GET /v1/teams/{teamId}/pools/{poolId}/status` (desired, reporting, working, idle, impaired workers) | `pools:read` | `pools get-pool-status` | `pools.getStatus()` | P4 | P2 |
| `ListPoolHandoffs` | `GET /v1/teams/{teamId}/pools/{poolId}/handoffs` | `pools:read` | `pools list-handoffs` | `pools.handoffs.list()` | P4 | P2 |
| `PutPoolHandoffs` | `PUT /v1/teams/{teamId}/pools/{poolId}/handoffs` `{toPoolIds[]}` | `pools:write` | `pools put-handoffs` | `pools.handoffs.put()` | P4 | P2 |
| `ListPoolSecretBindings` | `GET /v1/teams/{teamId}/pools/{poolId}/secret-bindings` | `pools:read` | `pools list-secret-bindings` | `pools.secretBindings.list()` | P4 | P2 |
| `PutPoolSecretBindings` | `PUT /v1/teams/{teamId}/pools/{poolId}/secret-bindings` | `pools:write` | `pools put-secret-bindings` | `pools.secretBindings.put()` | P4 | P2 |
| `ListPoolConfigValues` | `GET /v1/teams/{teamId}/pools/{poolId}/config-values` (resolved, with `source`) | `pools:read` | `pools list-config-values` | `pools.config.list()` | P4 | P2 |
| `PutPoolConfigValue` | `PUT /v1/teams/{teamId}/pools/{poolId}/config-values/{name}` | `pools:write` | `pools put-config-value` | `pools.config.put()` | P4 | P2 |
| `DeletePoolConfigValue` | `DELETE /v1/teams/{teamId}/pools/{poolId}/config-values/{name}` | `pools:write` | `pools delete-config-value` | `pools.config.delete()` | P4 | P2 |
| `ListPoolExternalSystems` | `GET /v1/teams/{teamId}/pools/{poolId}/external-systems` | `pools:read` | `pools list-external-systems` | `pools.externalSystems.list()` | P4 | P2 |
| `PutPoolExternalSystem` | `PUT /v1/teams/{teamId}/pools/{poolId}/external-systems/{systemId}` `{enabled}` | `pools:write` | `pools put-external-system` | `pools.externalSystems.put()` | P4 | P2 |
| `ListPoolRoleLinks` | `GET /v1/teams/{teamId}/pools/{poolId}/role-links` | `pools:read` | `pools list-role-links` | `pools.roleLinks.list()` | P4 | P2 |
| `CreatePoolRoleLink` | `POST /v1/teams/{teamId}/pools/{poolId}/role-links` (one `roleName` per pool, one link per account) | `pools:write` | `pools create-role-link` | `pools.roleLinks.create()` | P4 | P2 |
| `DeletePoolRoleLink` | `DELETE /v1/teams/{teamId}/pools/{poolId}/role-links/{linkId}` | `pools:write` | `pools delete-role-link` | `pools.roleLinks.delete()` | P4 | P2 |
| `VerifyPoolRoleLink` | `POST /v1/teams/{teamId}/pools/{poolId}/role-links/{linkId}:verify` | `pools:write` | `pools verify-role-link` | `pools.roleLinks.verify()` | P4 | P2 |

Pool secrets are team secrets with `scope: pool` and `poolId` (managed path
`<prefix>/<namespace>/<pool>/<name>`), created through `CreateSecret`. Pool usage, activity, traffic and
audit records carry `poolId` and the worker pod name. Each pool that receives handoffs has one SQS
inbox queue (`afs-<team ulid>-<short hash>`) that the reconciler creates. The receiver gets its URL as
`AFS_INBOX_QUEUE_URL`; each pool with a handoff to it gets the same URL as
`AFS_HANDOFF_<TO_POOL_NAME_UPPER_SNAKE>_QUEUE_URL` (for pool `fixer`: `AFS_HANDOFF_FIXER_QUEUE_URL`).
Only the roles of pools with a handoff to it may send (D22, D32).

### 4.4b Collaboration (Paperclip)

**v1.4.0:** `GetTeamCollaboration`, `PutTeamCollaboration` and `ListCollaborationEndpoints` are deprecated aliases of §4.4d
until 2.0.0 (mapping and headers there); the agent role and `CreateAgent` rules below are replaced by §4.4d; links and key
rotation stay. `collaboration_agent_missing` can occur only for a 1.0.1 team the backfill has not reached, and then reads
`orchestration_agent_pending`.

Requirements: [Paperclip PRD](../../paperclip-team-collaboration-PRD.md) COL-1 to COL-36, SEC-7. A team has at most one
collaboration agent (an agent with `role: collaboration`, created through `CreateAgent`); these operations configure
it. On a suspended team every mutation here → `409 Conflict` (`details.reason = team_suspended`). A team with no
collaboration agent has no collaboration resource: `GetTeamCollaboration`, `ListCollaborationLinks` →
`404 NotFound` (`details.reason = collaboration_agent_missing`); the mutations → `409 Conflict` with the same reason.

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `GetTeamCollaboration` | `GET /v1/teams/{teamId}/collaboration` | `collaboration:read` | `collaboration describe-collaboration` | `collaboration.get()` | T16, V3, T1 | P0 |
| `PutTeamCollaboration` | `PUT /v1/teams/{teamId}/collaboration` | `collaboration:manage` | `collaboration put-collaboration` | `collaboration.put()` | T16 | P0 |
| `ListCollaborationLinks` | `GET /v1/teams/{teamId}/collaboration/links?linkState&connect` | `collaboration:read` | `collaboration list-collaboration-links` | `collaboration.links.list()` | T16 | P0 (C1) |
| `ConnectAgentToCollaboration` | `POST /v1/teams/{teamId}/collaboration/links/{agentId}:connect` | `collaboration:manage` | `collaboration connect-agent` | `collaboration.connectAgent()` | T16 | P0 (C1) |
| `DisconnectAgentFromCollaboration` | `POST /v1/teams/{teamId}/collaboration/links/{agentId}:disconnect` | `collaboration:manage` | `collaboration disconnect-agent` | `collaboration.disconnectAgent()` | T16 | P0 (C1) |
| `RotateCollaborationKeys` | `POST /v1/teams/{teamId}/collaboration:rotate-keys` | `collaboration:manage` | `collaboration rotate-collaboration-keys` | `collaboration.rotateKeys()` | T16 | P1 |
| `ListCollaborationEndpoints` | `GET /v1/admin/collaboration-endpoints?teamId&state` *CSV* | `admin:settings` *svc* | `admin list-collaboration-endpoints` | `admin.collaborationEndpoints.list()` | S10 | P0 (C2) |

The local command `afs collaboration invoke-api` (CLI-US-25) calls `GetTeamCollaboration` once to learn the team's host (unless `--collab-url` is given) and then only the edge (§4.4c). `restApiUrl` is `https://<hostname>/api`, the base of Paperclip's REST API; the edge forwards paths unchanged, so a full path `/api/…` equals `restApiUrl` plus a suffix. The CLI's `--path` is the full path and the CLI uses only the origin of `restApiUrl` (v1.3.1).

**`TeamCollaboration`** (the resource; singleton per team; `ETag` is its `version`):

```json
{
  "teamId": "team_01J…", "agentId": "agt_01J…",
  "hostname": "payments-factory.collab.example.com",
  "webUiUrl": "https://payments-factory.collab.example.com/",
  "restApiUrl": "https://payments-factory.collab.example.com/api",
  "webUiEnabled": true, "restApiEnabled": false,
  "registrationMode": "manual",
  "effective": {"webUi": true, "restApi": false, "reasons": []},
  "unmanagedPaperclipAgents": [],
  "version": 4, "generation": 4, "status": { "...": "§1.1; conditions EndpointReady, PaperclipReady, RegistrationHealthy, UnmanagedPaperclipAgents" },
  "createdAt": "…", "createdBy": "…", "updatedAt": "…"
}
```

`hostname`, `webUiUrl` and `restApiUrl` are `null` while `collab.base_domain` is unset. `webUiEnabled` and
`restApiEnabled` are the team's stored choices (defaults on creating the agent: web UI on, REST API off);
`effective` is what the edge honours: a surface is effective only when its stored choice, the team's
`mayExposeCollaboration`, the service setting `collab.public_endpoints`, a set `collab.base_domain` and (web UI) a
collaboration client on the team's sign-in provider all hold. `effective.reasons[]` lists every failed condition from
`team_not_allowed`, `service_endpoints_off`, `base_domain_unset`, `provider_without_collab_client`.
`registrationMode` is `manual` or `managed` (`manual` by default in v1.3.0; `managed` becomes the default for new
collaboration agents when managed registration ships, PC-D8). `unmanagedPaperclipAgents` lists names (condition
`UnmanagedPaperclipAgents`).

**`PutTeamCollaboration`** body `{webUiEnabled, restApiEnabled, registrationMode?}` (`registrationMode` omitted keeps
the current value). `If-Match` optional (§1): a mismatch → `412 PreconditionFailed`. Returns the resource with
`status.phase` `Pending`. `ValidationError` (`details.fields[]`) when a surface is switched **on** while:
`collab.base_domain` is unset (`field: webUiEnabled|restApiEnabled`, `reason: collab_base_domain_unset`),
`collab.public_endpoints` is off (`collab_public_endpoints_off`), the team's `mayExposeCollaboration` is off
(`collaboration_not_allowed_for_team`), or, for the web UI, the team's provider has no `collabClientId`
(`provider_without_collab_client`); and for `registrationMode: managed` when the agent is not `Ready`
(`collaboration_agent_not_ready`). Switching a surface off is always accepted. Event `collaboration.updated`.
A viewer, collaborator, security operator or workload caller → `403 AccessDenied` and a denied audit record.

**`ListCollaborationLinks`** → `{items: [CollaborationLink], nextToken?}` with one item per live worker agent of the team
(the collaboration agent itself is not listed):

```json
{"agentId": "agt_01J…", "agentName": "reviewer-bot", "connect": true, "paperclipAgentId": "ag_8f2…",
 "linkState": "linked", "lastSyncedAt": "2026-11-22T10:00:00Z", "lastError": null, "version": 2}
```

`linkState` is `pending`, `linked`, `failed` or `disabled`; an agent never connected is `connect: false`,
`linkState: disabled`. `lastError` is a short reason written by the reconciler and never holds a Paperclip key or content.

**`ConnectAgentToCollaboration`** body `{paperclipAgentId?}`; returns the `CollaborationLink` (`200`). Sets
`connect: true` (idempotent: repeating it updates `paperclipAgentId`). `paperclipAgentId` is accepted only while
`registrationMode` is `manual` (`ValidationError`, `field: paperclipAgentId`, `reason: managed_registration`); in
`manual` mode the agent's key is a user secret named `paperclip-agent-api-key` (§4.6). The agent rolls. Errors: `404`
unknown agent; `ValidationError` for the collaboration agent itself (`field: agentId`) or a pool;
`409` `collaboration_agent_missing`. `DisconnectAgentFromCollaboration` sets `connect: false`; the reconciler revokes the
key (managed mode) and the agent rolls. Both accept `Idempotency-Key`, write the row and an audit record and no outbox row
(§4.12); the reconciler emits `agent.collaboration_linked` and `agent.collaboration_unlinked` when the link state changes.

**`RotateCollaborationKeys`** body (optional) `{names?: ["paperclip-better-auth-secret", "paperclip-tool-action-signing-secret"]}`;
default is both. `afs-api` writes a new random value (32 bytes, hex) to each with `PutSecretValue` and returns `200`
`{rotated: [{secretId, name, versionId}], signsOutEveryone: true, agentRolloutPending: true}`. Naming
`paperclip-secrets-master-key` → `400 ValidationError` (`field: names`, `reason: master_key_not_rotatable`): rotating it
would make Paperclip's stored identities undecryptable (COL-7). Emits `secret.rotated` per secret. `RotateSecret` on the master
key gives the same error.

**`ListCollaborationEndpoints`** → `{items: [CollaborationEndpoint]}` (paginated, *CSV*), service admins only; team callers
and security operators get `403`:

```json
{"teamId": "team_01J…", "slug": "payments-factory", "teamState": "active", "hostname": "payments-factory.collab.example.com",
 "agentId": "agt_01J…", "webUiEnabled": true, "restApiEnabled": false, "effective": {"webUi": true, "restApi": false},
 "mayExposeCollaboration": true, "phase": "Ready", "lastAccessAt": "2026-11-22T09:41:00Z"}
```

`lastAccessAt` is the newest `signed_in` or `allowed` bucket. Filters `teamId`, `state` (`Ready`, `Degraded`, `Failed`, `Pending`).

### 4.4c Collaboration edge (`afs-edge`): externally visible behaviour

Not a `/v1` operation: this is what a browser, a pipeline or `afs collaboration invoke-api` sees at a team's hostname
`https://<team-slug>.<collab.base_domain>`. The edge is the only public entry to a team's Paperclip (COL-15). It forwards
to Paperclip's own paths unchanged, except for the reserved paths below. Its errors use the §6 envelope
(`{code, message, requestId, details}`) for API traffic and an HTML page with the same `requestId` for browser
navigation, so the CLI renders them like any service error (`An error occurred (AccessDenied) when calling the
InvokeApi operation: …`, exit 254).

**Traffic classes.** A request carrying the cookie `__Host-afs-collab` is *web UI traffic*; one carrying
`X-Afs-Authorization: Bearer <token>` is *REST API traffic*; both → `400 ValidationError` (`details.reason =
ambiguous_credentials`). With neither, browser navigation (`Accept: text/html`, `Sec-Fetch-Mode: navigate`) is web UI
traffic and is redirected to sign in; anything else is API traffic.

**Credentials.** `X-Afs-Authorization` is an access token from the team's sign-in provider (same issuer and expiry rules as `afs-api`, §2; the audience rules are below, so the CLI's SSO token works) or a team API key (`afsk_…`) whose role holds
`collaboration:api`. The caller's Paperclip credential stays in `Authorization`. The edge validates and strips
`X-Afs-Authorization`, decides with `internal/authz` (cached at most 60 s), and forwards.

**Bearer audiences (v1.3.2).** The edge accepts a bearer token whose `aud` is one of three client ids of the team's provider: the console client id (`identity_providers.client_id`), the CLI client id (`cli_client_id`) or the collaboration client id (`collab_client_id`). The first two come from `afs.collab_routes()` (`-db` 000021). Any other audience is `401 token_invalid`.

**Exposure levels and hostnames (v1.4.0, ORC-13 to ORC-19).** The edge has two entries in front of the same pods: the
internet-facing ALB for **public** surfaces at `https://<slug>.<orchestration.public_base_domain>` (the 1.0.1
`collab.base_domain`) and an **internal ALB** in private subnets for **private** surfaces at
`https://<slug>.<orchestration.private_base_domain>`, resolvable only in a Route 53 private hosted zone associated with the
environment VPC (or delegated from corporate DNS), reachable from networks routed to the VPC and never from the internet.
Each ALB has its own certificate (apex and wildcard of its base domain) and listener rules that forward only its own base
domain's hosts; the internal ALB's WAF has the core rule set, known bad inputs and the per-IP rate rule, and
`collab.allowed_source_cidrs` applies to the public WAF only. The edge tells the level from `Host` alone and reads routes from
`afs.orchestration_routes()` (database contract §7.3.6). For a team host at level *L*:

| Case | Answer |
|---|---|
| No surface of the team is effective at *L* (including every Buzz host, a level switched off by a kill switch or the limit, and the other level's host) | `404 NotFound`, no detail |
| The request's surface is effective at *L* | Served as before |
| The request's surface is effective `off` and the other surface is effective at *L*, caller holds a binding | `403 AccessDenied`, `surface_disabled` (as 1.0.1) |
| The request's surface is effective at the other level | `404 NotFound`, no detail (wrong-level host, CD-38) |

Each base domain has its own apex callback `https://<base domain>/oauth2/callback` (identity providers register both
redirect URIs; PC-D21 per apex). Sessions are host-only, so a session on the private host never serves the public host.
Records and five-minute counts carry `exposure` (`public` or `private`, ORC-31). Exposure, limit and kill-switch changes take
effect within 60 s (p95). Paths, credentials, scanning, limits and errors are otherwise unchanged; the forward target is the
tool's Service and port from the route row (Paperclip `paperclip:3100`). `afs collaboration invoke-api` uses
`GetTeamOrchestration.webApi.url` and exits 254 with "Web API is off (N/A for Buzz)" when it is `null` for a tool without one.

**Hosts.** The ALB listener certificate covers `<collab.base_domain>` and `*.<collab.base_domain>`, Route 53 has an apex record and the wildcard, and listener rules forward only those host headers. The edge refuses any `Host` that is neither the apex nor a known team host (`404`); it does not check SNI, which the ALB terminates (G4).

**Sessions.** The edge keeps sessions server-side in the database (`-db` 000020, database contract §7.2.8). The cookie `__Host-afs-collab` (Secure, HttpOnly, SameSite=Lax, host-only) holds only an opaque 256-bit session id; the session row holds the person's issuer, subject, provider and groups, lives `collab.session_ttl_minutes`, and is revoked by sign-out. Turning the web UI off makes the edge refuse that team's sessions at once (no row is deleted). Decisions are cached at most 60 seconds per session.

**Reserved paths** (on team hosts; never forwarded): `GET /_afs/signin` (starts OIDC code + PKCE; sets nothing),
`GET /_afs/handoff?code=` (one-time code, 60 s, single use across all replicas; creates the session, sets the cookie and redirects to the original path) and `POST /_afs/signout`.
On the apex only: `GET https://<collab.base_domain>/oauth2/callback`, the one redirect URI per environment (the team is
carried in `state`); it ends by redirecting to the team host with the one-time code. Every other apex path is `404`.

**Authorization.** Safe methods (`GET`, `HEAD`) need `collaboration:access`; every other method, and every client-to-server
WebSocket frame, needs `collaboration:contribute` (PC-D17: a caller holding only `collaboration:access` may open a
WebSocket or SSE stream to watch but its client frames close the connection with code 1008). API traffic needs
`collaboration:api` as well, and `collaboration:contribute` for non-safe methods.

| Status | `code` | `details.reason` | When |
|---|---|---|---|
| `302` | none | none | Web UI traffic without a valid session: to the team's provider; after sign-in, to the handoff |
| `400` | `ValidationError` | `ambiguous_credentials`, `malformed_credential`, `body_too_large` (over `collab.max_body_mb`; as in §1 there is no `413`), `unsupported_encoding` (a request body with `Content-Encoding` other than identity while `collab.content_scan` is not `off`, because it cannot be scanned) | |
| `401` | `Unauthenticated` | `token_invalid`, `token_expired`, `key_expired`, `key_revoked`, `session_expired` (a non-navigation web UI request whose session ended) | Missing or unusable credential |
| `403` | `AccessDenied` | `no_team_role` (no binding on this team), `permission_required` (with `details.requiredPermission`: `collaboration:contribute` or `collaboration:api`), `surface_disabled` (the REST API or the web UI is off or not effective for this team while the other is on) | Valid caller, refused |
| `404` | `NotFound` | none | Unknown host (the apex serves only `/oauth2/callback`; every other apex path is `404`), a team without a collaboration agent, or both surfaces not effective (`collab.public_endpoints` off, `mayExposeCollaboration` off, base domain unset, both switches off). No team detail, ever. With exactly one surface not effective, a request for it from a caller who holds a binding on the team is `403 surface_disabled`, never `404` |
| `422` | `SecretDetected` | `details.patterns[]` (pattern types only, for example `aws_access_key`); `message` tells the person to store the credential as an AFS secret. Never the matched text | `collab.content_scan = block` and a body matched (COL-24) |
| `429` | `Throttled` | `Retry-After` | 20 requests per second per principal, burst 40 |
| `503` | `Unavailable` | `paused`, `team_suspended`, `starting`, `upstream_unavailable`; `Retry-After: 30`; the page names the reason | Pod down or not ready (COL-8); the route table has `ready = false` |

Other responses are Paperclip's own, passed through. A WAF block (managed rules, the per-IP rate rule or the
`collab.allowed_source_cidrs` list) answers `403` with the WAF's default body before the edge sees the request; it is
not in the AFS envelope. A surface switched off is refused within 60 seconds (p95).

**Headers.** The edge removes the AFS cookie, `X-Afs-Authorization`, `X-Amzn-*` and client-sent `X-Forwarded-*`, sets
`X-Forwarded-For`, `X-Forwarded-Proto` and `X-Request-Id`, and adds `Strict-Transport-Security`,
`X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `frame-ancestors 'none'` in
`Content-Security-Policy` to responses. No CORS headers are added.

**Scanning.** Text bodies (JSON, `text/*`, form and the text parts of multipart; binary is not scanned) and text WebSocket
frames are scanned for the pattern types `aws_access_key`, `github_token`, `slack_token`, `private_key`, `jwt`,
`okta_client_secret`, `entra_client_secret` and `generic_assignment`. `block` answers `422` above; `warn` forwards and
records outcome `secret_warned`; `off` skips. Each detection emits `collaboration.secret_detected` (no value or
fragment), at most once per team, person and pattern per five minutes with a `count`.

**Records** (COL-31, G5): the edge writes one structured JSON line per sign-in, sign-out and denied request to stdout with the field `afs.team_id` (and no body, header value, cookie or token). The node log collector that already feeds each team's aggregator (the per-team Fluent Bit aggregator of the log delivery, D17) also routes the records of `afs-system/afs-edge` by `afs.team_id` to that team's aggregator, with the redaction of OPS-4; the platform keeps every line in CloudWatch for 13 months. The routing rule is part of the `-restapi` Helm chart and its Fluent Bit configuration; no new network path exists. Edge records have the same shape as agent sign-in records and are delivered the same way; five-minute counts go to the reports (§4.9); a sign-in held only through a service admin role is marked
`serviceAdminEntry`. No request body, header value or raw path with ids is recorded. A Paperclip response that carries a once-only key is passed through unchanged, and the edge never logs bodies.

### 4.4d Orchestration (v1.4.0)

Requirements: [team orchestration agent PRD](../../team-orchestration-agent-PRD.md) ORC-1 to ORC-32 (decisions OR-D1 to
OR-D12, approved by J on 2026-11-23); database contract §7.3. Every team has exactly one **orchestration agent** (an agent
with `role: orchestration`), created with the team by `CreateTeam` (or `instantiate_work_order_team`) and deleted only
with the team. It runs the team's **collaboration tool** (`paperclip`, shown as *Paperclip AI*, or `buzz`, shown as
*Buzz*) and is the only thing in a team reachable from outside the cluster. Its agent configuration (models, persona,
resources, volume, configuration values, secret bindings, pause, resume, restart, snapshot) uses the §4.4 operations;
the operations here hold everything that belongs to the tool. Permissions are the `collaboration:*` ones (OR-D9). On a
suspended team every mutation here → `409 Conflict` (`team_suspended`).

A team created before 1.1.0 that the backfill has not reached yet has no orchestration agent: `GetTeamOrchestration` →
`404 NotFound` with `details.reason = orchestration_agent_pending` (the console shows "Orchestration agent being created");
the mutations → `409 Conflict` with the same reason (CD-26). After the backfill this cannot occur.

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `GetTeamOrchestration` | `GET /v1/teams/{teamId}/orchestration` | `collaboration:read` | `orchestration describe-orchestration` | `orchestration.get()` | T17, T18, V4, T1, T2 | P0 |
| `PutTeamOrchestration` | `PUT /v1/teams/{teamId}/orchestration` | `collaboration:manage` | `orchestration put-orchestration` | `orchestration.put()` | T17 | P0 |
| `ChangeCollaborationTool` | `POST /v1/teams/{teamId}/orchestration:change-tool` | `collaboration:manage` | `orchestration change-collaboration-tool` | `orchestration.changeTool()` | T17 | P1 (O3) |
| `ListOrchestrationEndpoints` | `GET /v1/admin/orchestration-endpoints?exposure&tool&teamId&state` *CSV* | `admin:settings` *svc* | `admin list-orchestration-endpoints` | `admin.orchestrationEndpoints.list()` | S12 | P0 |

**Exposure.** Each surface (`webUi`, `webApi`) has an **exposure**: `off` (inside the namespace only), `private` (through the
internal ALB from networks routed to the environment VPC) or `public` (through the internet-facing ALB). The team's
**limit** (`Team.maxCollaborationExposure`, service admins only) caps both. The stored choice and the **effective** level
can differ; the edge honours only the effective level. A surface is lowered, with these reasons in `reasons[]`:

| Reason | Effect |
|---|---|
| `tool_not_supported` | The tool has no such surface (Buzz: neither). Always `off` |
| `team_limit` | Choice above the limit: lowered to the limit |
| `service_switch_public`, `service_switch_private` | Kill switch `orchestration.public_endpoints` / `orchestration.private_endpoints` is off: that level becomes `off` |
| `base_domain_unset` | `orchestration.public_base_domain` / `orchestration.private_base_domain` unset: that level becomes `off` |
| `provider_without_collab_client` | Web UI only: the team's sign-in provider has no `collabClientId` (as 1.0.1, CD-28): `off` |

**`TeamOrchestration`** (singleton per team; `ETag` is its `version`):

```json
{
  "teamId": "team_01J…", "agentId": "agt_01J…", "agentName": "orchestrator",
  "phase": "Ready",
  "tool": "paperclip", "toolVersion": "2026.11.2", "registrationMode": "managed",
  "limit": "private",
  "webUi":  {"supported": true, "exposure": "public", "effective": "private", "reasons": ["team_limit"],
             "hostname": "payments-factory.collab.corp.example.com", "url": "https://payments-factory.collab.corp.example.com/"},
  "webApi": {"supported": true, "exposure": "off", "effective": "off", "reasons": [], "hostname": null, "url": null},
  "connectedAgents": 4,
  "unmanagedToolAgents": [],
  "version": 7, "generation": 7,
  "status": {"...": "§1.1; conditions EndpointReady, ToolReady, RegistrationHealthy, UnmanagedToolAgents, ToolChanging, SnapshotTaken"},
  "createdAt": "…", "createdBy": "…", "updatedAt": "…"
}
```

`phase` is the orchestration agent's phase (§1.1). `toolVersion` is reported by the reconciler from the running image and is
`null` until known. `supported` comes from the tool's profile (CD-28), so a client can render "N/A for Buzz" without
guessing. `hostname` and `url` are those of the **effective** level and `null` when it is `off`; the Web UI `url` is
`https://<hostname>/`, the Web API `url` is `https://<hostname>/api` for Paperclip. `connectedAgents` counts live workers
with `connect: true`. Hostnames are `<slug>.<orchestration.public_base_domain>` for `public` and
`<slug>.<orchestration.private_base_domain>` for `private` (ORC-16). The template and harness are never in this
resource (ORC-6).

**`PutTeamOrchestration`** body `{webUi: {exposure}, webApi: {exposure}, registrationMode?}` (both surfaces required;
`registrationMode` omitted keeps the current value). `If-Match` optional (§1); a mismatch → `412`. Returns the resource
with `status.phase` `Pending`; changes apply at the edge within 60 s (p95). Refusals, each `ValidationError` with
`details.fields[]`:

| `field` | `reason` | When |
|---|---|---|
| `webUi.exposure`, `webApi.exposure` | `exposure_above_limit` | Level above the team's limit (ORC-14) |
| `webUi.exposure`, `webApi.exposure` | `surface_not_supported_by_tool` | Any level other than `off` for a surface the tool lacks (ORC-15; Buzz both) |
| `registrationMode` | `registration_mode_not_supported` | `manual` for Buzz (ORC-10) |
| `registrationMode` | `collaboration_agent_not_ready` | `managed` while the agent is not `Ready` (as 1.0.1) |

An unset base domain, a kill switch that is off or a provider without a collaboration client does **not** refuse the
write: the choice is stored and the effective level and reasons say why it is not served (CD-27). Emits
`orchestration.updated`. A viewer, collaborator, security operator or workload caller → `403` and a denied audit record.

**`ChangeCollaborationTool`** body `{tool}` (`paperclip` or `buzz`); accepts `Idempotency-Key`. `202 Accepted` with the
`TeamOrchestration` (new `generation`, `status.phase` `Pending`, condition `ToolChanging=True`). In one transaction `afs-api`
inserts a `pre_tool_change` snapshot request (kept 90 days, labelled with the old tool), sets the tool, the orchestration
agent's template and image from `orchestration.tools`, sets every surface the new tool lacks to `off`, and writes the new
tool's boot secrets (generated in Secrets Manager first; failure → `503 Unavailable`, nothing changes). The reconciler then
takes the snapshot, starts the new tool with an empty data directory, revokes the old tool's agent keys and re-registers
connected workers (ORC-11). Errors: same tool → `ValidationError` (`field: tool`, `reason: tool_unchanged`); tool not enabled
→ `ValidationError` (`tool_not_enabled`); tool's runtime unset → `409` (`orchestration_runtime_unset`); a change already
running → `409` (`tool_change_in_progress`); suspended team → `409` (`team_suspended`). The console asks the person to type
the team slug first; the API does not. Emits `orchestration.tool_changed` and `agent.updated`.

**`ListOrchestrationEndpoints`** → `{items: [OrchestrationEndpoint], nextToken?, migration}` (paginated, *CSV* without
`migration`). One item per team and surface whose **stored** exposure is not `off`, so a surface lowered by the limit or a
kill switch is still listed with its reasons; a Buzz team never has one (US-39) (CD-33):

```json
{"teamId": "team_01J…", "slug": "payments-factory", "teamState": "active", "agentId": "agt_01J…", "tool": "paperclip",
 "surface": "webUi", "exposure": "public", "effective": "private", "reasons": ["team_limit"], "limit": "private",
 "hostname": "payments-factory.collab.corp.example.com", "phase": "Ready", "lastAccessAt": "2026-11-23T09:41:00Z"}
```

Filters: `exposure` (effective level, `private` or `public`), `tool`, `teamId`, `state` (`Ready`, `Degraded`, `Failed`,
`Pending`). `migration` reports the backfill for S12: `{state: running|complete|blocked, teamsTotal,
teamsWithOrchestration, teamsPending, blocked: [{teamId, reason: orchestration_runtime_unset|no_model}]}`.

**Agents and the orchestration agent** (changes to §4.4, ORC-2, ORC-4, ORC-6):

- `Agent.role` is `worker` or `orchestration` (read-only; 1.0.1 `collaboration` reads as `orchestration`). `CreateAgent`
  creates workers only: a body `role` other than `worker` (including `collaboration` and `orchestration`) →
  `ValidationError` (`field: role`, `reason: role_reserved`); name `orchestrator` → `ValidationError` (`field: name`,
  `reason: name_reserved`). The name rule also applies to `CreatePool`.
- `DeleteAgent` on the orchestration agent → `409 Conflict` (`orchestration_agent_required`) and a denied audit record.
- `UpdateAgent` on the orchestration agent changing `name`, `role`, `template`, `image` or `iamIdentity`, setting
  `volume.snapshotSchedule: off`, or sending `collaboration` → `ValidationError` (`field` named,
  `reason: orchestration_field_fixed`). Its `image` digest follows the template's current digest; the tool and runtime
  change only through `ChangeCollaborationTool`. `models`, `persona`, `resources`, `volume.sizeGiB`, `volume.snapshotRetain`,
  configuration values, secret bindings, pause, resume, restart and snapshots work as for any agent.
- `Agent.runtime†` is `"orchestration"` on the orchestration agent and absent on workers. For callers who are not service
  admins, the orchestration agent's `template` and `image` are `null` (CD-32); service admins see them. The tool's boot
  secrets are listed by `ListSecrets` with `managedBy: afs` like any agent secret.
- `ListAgents` returns every agent, including the orchestration agent; `?role=worker|orchestration` filters
  (`?role=collaboration` is read as `orchestration` until 2.0.0). `Team.orchestrationAgentId†` names it.
- `ListAgentTemplates` (team callers) never returns a template with the `orchestration` capability (AGT-11 change);
  `ListAdminAgentTemplates` and `PutAgentTemplate` carry `capabilities[]` with values `orchestration`, `tool:paperclip`,
  `tool:buzz` (a written `paperclip` is stored as `orchestration` + `tool:paperclip` until 2.0.0).
- A worker's `collaboration` object is unchanged, except that `connect` now defaults to `true` on create (every team has an
  orchestration agent, COL-10 change) and its `paperclipAgentId` is accepted only for Paperclip. A Buzz worker's link has no
  tool-side id the caller sets; its public key is `CollaborationLink.toolAgentId` (read-only).

**Deprecated aliases (kept until 2.0.0, ORC-27).** Each response of a deprecated operation, and each response to a request
that sends a deprecated field, carries `Deprecation: @<unix time of the 1.1.0 release>`, `Sunset: <HTTP-date of the planned
2.0.0 release>` and `Link: <successor>; rel="successor-version"` (CD-29).

| Deprecated | Behaviour in 1.1.0 | Successor |
|---|---|---|
| `GetTeamCollaboration` | `webUiEnabled` / `restApiEnabled` = stored exposure ≠ `off`; `effective.webUi` / `restApi` = effective ≠ `off`; `effective.reasons[]` mapped (`team_limit` → `team_not_allowed`, `service_switch_*` → `service_endpoints_off`, `base_domain_unset`, `provider_without_collab_client` as is, `tool_not_supported` added); `hostname` of the effective level, else the public hostname | `GetTeamOrchestration` |
| `PutTeamCollaboration` | `true` sets the highest level within the limit (`public` or `private`); `false` sets `off`. `true` with limit `off` → `ValidationError` (`collaboration_not_allowed_for_team`, as 1.0.1); `true` for a surface the tool lacks → `surface_not_supported_by_tool`. Emits `orchestration.updated` and `collaboration.updated` | `PutTeamOrchestration` |
| `ListCollaborationEndpoints` | One row per team with a surface not `off` (booleans only, as 1.0.1), `effective` true only for `public` | `ListOrchestrationEndpoints` |
| `Team.mayExposeCollaboration` | Read: limit ≠ `off`. Write (`CreateTeam`, `UpdateTeam`): `true` sets limit `public`, `false` sets `off`; with `maxCollaborationExposure` in the same body, that field wins | `Team.maxCollaborationExposure` |
| `Agent.modelId`, `Pool.modelId` | Read: `models.defaultModelId`. Write: §4.4 *Models* | `models` |
| `PutTeamModels` with only `modelIds` | Read as `{access: selected, modelIds}` | `{access, modelIds}` |
| `SwarmTemplate.spec.collaboration` | Read as `spec.orchestration {tool: paperclip}` | `spec.orchestration` |
| Event `collaboration.endpoint_state_changed` | Still emitted beside `orchestration.endpoint_state_changed` | `orchestration.endpoint_state_changed` |

Link operations (`ListCollaborationLinks`, `ConnectAgentToCollaboration`, `DisconnectAgentFromCollaboration`) and
`RotateCollaborationKeys` keep their names and paths (OR-D8) and now act on the orchestration agent's tool. A 1.0.1 client
keeps working for every operation except creating a collaboration agent and deleting it, both refused with the reasons above.

### 4.5 Agent identity and AWS access

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListAgentRoleLinks` | `GET /v1/teams/{teamId}/agents/{agentId}/role-links` | `identities:read` | `agents list-role-links` | `agents.roleLinks.list()` | T8 | P1 |
| `CreateAgentRoleLink` | `POST /v1/teams/{teamId}/agents/{agentId}/role-links` | `identities:write` | `agents create-role-link` | `agents.roleLinks.create()` | T8 | P1 |
| `GetAgentRoleLink` | `GET /v1/teams/{teamId}/agents/{agentId}/role-links/{linkId}` (includes `trustPolicy` JSON) | `identities:read` | `agents describe-role-link` | `agents.roleLinks.get()` | T8 | P1 |
| `DeleteAgentRoleLink` | `DELETE /v1/teams/{teamId}/agents/{agentId}/role-links/{linkId}` | `identities:write` | `agents delete-role-link` | `agents.roleLinks.delete()` | T8 | P1 |
| `VerifyAgentRoleLink` | `POST /v1/teams/{teamId}/agents/{agentId}/role-links/{linkId}:verify` | `identities:write` | `agents verify-role-link` | `agents.roleLinks.verify()` | T8 | P1 |
| `ListAgentIdentities` | `GET /v1/teams/{teamId}/agents/{agentId}/identities` | `identities:read` | `agents list-identities` | `agents.identities.list()` | T2 | P1 |
| `CreateAgentIdentity` | `POST /v1/teams/{teamId}/agents/{agentId}/identities` | `identities:write` | `agents create-identity` | `agents.identities.create()` | T2 | P1 |
| `DeleteAgentIdentity` | `DELETE /v1/teams/{teamId}/agents/{agentId}/identities/{identityId}` | `identities:write` | `agents delete-identity` | `agents.identities.delete()` | T2 | P1 |

### 4.6 Secrets

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListSecrets` | `GET /v1/teams/{teamId}/secrets` | `secrets:read-metadata` | `secrets list-secrets` | `secrets.list()` | T4, V2 | P0 |
| `CreateSecret` | `POST /v1/teams/{teamId}/secrets` | `secrets:write` | `secrets create-secret` | `secrets.create()` | T4 | P0 |
| `GetSecret` | `GET /v1/teams/{teamId}/secrets/{secretId}` | `secrets:read-metadata` | `secrets describe-secret` | `secrets.get()` | T4 | P0 |
| `UpdateSecret` | `PATCH /v1/teams/{teamId}/secrets/{secretId}` | `secrets:write` | `secrets update-secret` | `secrets.update()` | T4 | P0 |
| `DeleteSecret` | `DELETE /v1/teams/{teamId}/secrets/{secretId}` | `secrets:write` | `secrets delete-secret` | `secrets.delete()` | T4 | P0 |
| `PutSecretValue` | `PUT /v1/teams/{teamId}/secrets/{secretId}/value` → 204 | `secrets:write` | `secrets put-secret-value` | `secrets.putValue()` | T4 | P0 |
| `RotateSecret` | `POST /v1/teams/{teamId}/secrets/{secretId}:rotate` | `secrets:write` | `secrets rotate-secret` | `secrets.rotate()` | T4 | P1 |

`CreateSecret` body: `scope` (`team` \| `agent` \| `pool`), `agentId` (agent scope), `poolId` (pool scope), `name`, `description`,
`externalSystemId` (optional), `value`. The value is sent once, passed to Secrets Manager and dropped.
No response, error, log line or audit record contains it. The server sets `secretsManagerPath`.
`DeleteSecret` on a bound secret → `409 Conflict` listing the agents.

v1.3.0: `Secret.managedBy` (†) is `user` (created through `CreateSecret`), `afs` (generated by AFS: the Paperclip boot
secrets and, in `managed` registration, a connected agent's `paperclip-agent-api-key`) or `paperclip` (created by
Paperclip itself under the reserved prefix `<service>/<namespace>/<agent>/paperclip/`, SEC-7; listed with metadata, never a
value, refreshed hourly from Secrets Manager metadata; `name` is `paperclip-` plus the path suffix). `ListSecrets` accepts
`?managedBy=user|afs|paperclip`. For `afs` and `paperclip` secrets `UpdateSecret`, `DeleteSecret` and `PutSecretValue` →
`409 Conflict` (`details.reason = managed_secret`; **planned addition, RCF-2, not yet implemented in `-restapi`:** `details.managedBy = afs|paperclip`, so a client can name who manages the secret; clients must treat it as optional until `-restapi` ships it); `RotateSecret` on an `afs` secret takes no body (AFS generates the value) and is allowed except
`paperclip-secrets-master-key` (`ValidationError`, `reason: master_key_not_rotatable`) and a `paperclip` secret → `409 Conflict` (`managed_secret`). A
`paperclip` secret cannot be bound (`ValidationError`). `CreateSecret` refuses the name `paperclip` and any name starting
`paperclip-` except `paperclip-agent-api-key` on a worker agent (`ValidationError`, `reason: reserved_name`).

### 4.7 Configuration values

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListNamespaceConfigValues` | `GET /v1/teams/{teamId}/config-values` | `config:read` | `teams list-config-values` | `config.namespace.list()` | T7 | P0 |
| `PutNamespaceConfigValue` | `PUT /v1/teams/{teamId}/config-values/{name}` | `config:write` | `teams put-config-value` | `config.namespace.put()` | T7 | P0 |
| `DeleteNamespaceConfigValue` | `DELETE /v1/teams/{teamId}/config-values/{name}` | `config:write` | `teams delete-config-value` | `config.namespace.delete()` | T7 | P0 |
| `ListAgentConfigValues` | `GET /v1/teams/{teamId}/agents/{agentId}/config-values` (resolved, with `source`) | `config:read` | `agents list-config-values` | `config.agent.list()` | T7 | P0 |
| `PutAgentConfigValue` | `PUT /v1/teams/{teamId}/agents/{agentId}/config-values/{name}` body `{source: literal\|namespace, value?}` | `config:write` | `agents put-config-value` | `config.agent.put()` | T7 | P0 |
| `DeleteAgentConfigValue` | `DELETE /v1/teams/{teamId}/agents/{agentId}/config-values/{name}` | `config:write` | `agents delete-config-value` | `config.agent.delete()` | T7 | P0 |

### 4.8 External systems

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListTeamExternalSystems` | `GET /v1/teams/{teamId}/external-systems` (catalog + team-declared, with team state) | `external-systems:read` | `teams list-external-systems` | `externalSystems.team.list()` | T9 | P0 |
| `SetTeamExternalSystemState` | `PUT /v1/teams/{teamId}/external-systems/{systemId}/state` `{enabled}` | `external-systems:write` | `teams set-external-system-state` | `externalSystems.team.setState()` | T9 | P0 |
| `DeclareTeamExternalSystem` | `POST /v1/teams/{teamId}/external-systems` | `external-systems:write` + team `mayAddExternalSystems` | `teams declare-external-system` | `externalSystems.team.declare()` | T9 | P1 |
| `UpdateTeamExternalSystem` | `PATCH /v1/teams/{teamId}/external-systems/{systemId}` (team-declared only) | `external-systems:write` | `teams update-external-system` | `externalSystems.team.update()` | T9 | P1 |
| `DeleteTeamExternalSystem` | `DELETE /v1/teams/{teamId}/external-systems/{systemId}` (team-declared only) | `external-systems:write` | `teams delete-external-system` | `externalSystems.team.delete()` | T9 | P1 |
| `ListAgentExternalSystems` | `GET /v1/teams/{teamId}/agents/{agentId}/external-systems` (effective, with `disabledAt`) | `external-systems:read` | `agents list-external-systems` | `externalSystems.agent.list()` | T10 | P0 |
| `SetAgentExternalSystemState` | `PUT /v1/teams/{teamId}/agents/{agentId}/external-systems/{systemId}/state` | `external-systems:write` | `agents set-external-system-state` | `externalSystems.agent.setState()` | T10 | P0 |

### 4.9 Budget, usage and reports

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `GetBudget` | `GET /v1/teams/{teamId}/budget` | `budgets:read` | `budgets get-budget` | `budgets.get()` | T5 | P0 |
| `PutBudget` | `PUT /v1/teams/{teamId}/budget` (`onExhaust: notify` needs `teams:set-guardrails` and a persistent team, D33) | `budgets:write` | `budgets put-budget` | `budgets.put()` | T5 | P0 |
| `GetUsage` | `GET /v1/teams/{teamId}/usage?from&to&groupBy=agent\|model\|day` *CSV* | `usage:read` | `budgets get-usage` | `usage.get()` | T5 | P0 |
| `ListCosts` | `GET /v1/teams/{teamId}/costs?from&to` (gateway and CUR by day) *CSV* | `usage:read` | `budgets list-costs` | `usage.listCosts()` | T5 | P1 |
| `GetTeamReport` | `GET /v1/teams/{teamId}/reports/{usage\|costs\|activity\|traffic\|collaboration}` *CSV* | `reports:read` | `reports get-team-report` | `reports.team()` | T11 | P0 |
| `GetAgentReport` | `GET /v1/teams/{teamId}/agents/{agentId}/reports/{kind}` *CSV* | `reports:read` | `reports get-agent-report` | `reports.agent()` | T12 | P0 |
| `GetServiceReport` | `GET /v1/reports/{kind}` *CSV* | `admin:reports` *svc* | `reports get-service-report` | `reports.service()` | S8, S9 | P0 |
| `ListReportSchedules` | `GET /v1/teams/{teamId}/report-schedules` | `reports:read` | `reports list-report-schedules` | `reports.schedules.list()` | T11 | P1 |
| `CreateReportSchedule` | `POST /v1/teams/{teamId}/report-schedules` | `reports:schedule` | `reports create-report-schedule` | `reports.schedules.create()` | T11 | P1 |
| `UpdateReportSchedule` | `PATCH /v1/teams/{teamId}/report-schedules/{scheduleId}` | `reports:schedule` | `reports update-report-schedule` | `reports.schedules.update()` | T11 | P1 |
| `DeleteReportSchedule` | `DELETE /v1/teams/{teamId}/report-schedules/{scheduleId}` | `reports:schedule` | `reports delete-report-schedule` | `reports.schedules.delete()` | T11 | P1 |
| `ListCostTagStatus` | `GET /v1/admin/cost-tag-status` (per key: seen in CUR, first and last seen, AWS activation status) | `admin:cost-tag-status` *svc* | `admin list-cost-tag-status` | `admin.costTagStatus.list()` | S9 | P2 |

**Collaboration report** (v1.3.0, COL-36; kind `collaboration` on `GetTeamReport`, and on `GetServiceReport` with
`groupBy=team`). Usage only, never content. Measures per row: `signIns`, `uniquePeople` (distinct people and keys,
anonymous excluded), `requests` (allowed and warned requests), `deniedRequests` and `secretDetections` (blocked plus warned
by the edge). `groupBy` adds `surface` (`web_ui`, `rest_api`), `reason` (denied requests by `unauthenticated`, `forbidden`,
`surface_disabled`, `secret_detected`; also `bad_request` and `throttled` as separate non-denied reasons), `person`
(rows carry `principal{id, type: user|api_key, name?}`) and `day`; a row has `serviceAdminEntry` when the access was held only
as a service admin. T16's *Recent access* panel is `range=7d&groupBy=day,person`. The report is not schedulable
(`CreateReportSchedule` `report` stays `usage|costs|activity|traffic`, PC-D13).

The S9 activation banner (TAG-7) is acknowledged with `PutSetting` on key
`billing.tag_activation_acknowledged` = `true`; there is no separate acknowledge operation.

A report schedule has `name`, `report` (`usage` \| `costs` \| `activity` \| `traffic`), `query` (the
report query parameters below, stored), `cadence` (`daily` \| `weekly` \| `monthly`), `channelId` (one of
**the team's own** notification channels; a service-level channel → `ValidationError`), `format` (`csv`),
`enabled`, and read-only `nextRunAt`, `lastRunAt`, `lastStatus`, `lastError`.

Report query parameters: `from`, `to` (or `range=24h|7d|30d|90d|mtd`), `groupBy`
(`team|agent|model|system|day|hour|tag:<key>`, and for the collaboration report `surface|reason|person`), filters `teamId`, `agentId`, `modelId`, `systemId`,
`tag.<key>=<value>`, and in ephemeral teams `poolId` and `groupBy=pool`. Grouping by tag uses the tags in
effect when each record happened (TAG-6); a record missing a currently required key reports
`"(untagged)"` for that key. Traffic reports carry counts and bytes, never a cost. Response: `{"range": {...}, "groupBy": [...], "rows": [...], "totals": {...},
"notes": ["Spend with providers other than Bedrock is not included"]}`.
The cost measures differ by report: `usage` rows carry `costUsd` (the gateway's settled spend), `costs` rows carry
`estimatedUsd` (the gateway metering) and `billedUsd` (the Cost and Usage Report); a `costs` row never has `costUsd`.
The `tag.<key>=<value>` filters are accepted on every report operation although the OpenAPI document does not
declare them as parameters.

### 4.10 Delivery, notifications, audit and history

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListLogDestinations` | `GET /v1/teams/{teamId}/log-destinations` | `logs:read` | `teams list-log-destinations` | `logDestinations.list()` | T14 | P1 |
| `CreateLogDestination` | `POST /v1/teams/{teamId}/log-destinations` | `logs:write` | `teams create-log-destination` | `logDestinations.create()` | T14 | P1 |
| `GetLogDestination` | `GET /v1/teams/{teamId}/log-destinations/{id}` | `logs:read` | `teams describe-log-destination` | `logDestinations.get()` | T14 | P1 |
| `UpdateLogDestination` | `PATCH /v1/teams/{teamId}/log-destinations/{id}` | `logs:write` | `teams update-log-destination` | `logDestinations.update()` | T14 | P1 |
| `DeleteLogDestination` | `DELETE /v1/teams/{teamId}/log-destinations/{id}` | `logs:write` | `teams delete-log-destination` | `logDestinations.delete()` | T14 | P1 |
| `ListNotificationChannels` | `GET /v1/teams/{teamId}/notification-channels` | `notifications:read` | `teams list-notification-channels` | `notificationChannels.list()` | T14 | P1 |
| `CreateNotificationChannel` | `POST /v1/teams/{teamId}/notification-channels` | `notifications:write` | `teams create-notification-channel` | `notificationChannels.create()` | T14 | P1 |
| `GetNotificationChannel` | `GET /v1/teams/{teamId}/notification-channels/{id}` (with delivery status) | `notifications:read` | `teams describe-notification-channel` | `notificationChannels.get()` | T14 | P1 |
| `UpdateNotificationChannel` | `PATCH /v1/teams/{teamId}/notification-channels/{id}` | `notifications:write` | `teams update-notification-channel` | `notificationChannels.update()` | T14 | P1 |
| `DeleteNotificationChannel` | `DELETE /v1/teams/{teamId}/notification-channels/{id}` | `notifications:write` | `teams delete-notification-channel` | `notificationChannels.delete()` | T14 | P1 |
| `TestNotificationChannel` | `POST /v1/teams/{teamId}/notification-channels/{id}:test` (`200 NotificationTestResult` with `delivered`, or `202` with `pending: true` while the reconciler has not reported within 10 s; emits `notification.test_requested`) | `notifications:write` | `teams test-notification-channel` | `notificationChannels.test()` | T14 | P1 |
| `ListAuditEvents` | `GET /v1/teams/{teamId}/audit-events` *CSV* | `audit:read` | `audit list-audit-events` | `audit.list()` | T15 | P0 |
| `ListRevisions` | `GET /v1/teams/{teamId}/revisions` | `revisions:read` | `teams list-revisions` | `revisions.list()` | T15 | P0 |
| `GetRevision` | `GET /v1/teams/{teamId}/revisions/{rev}` | `revisions:read` | `teams describe-revision` | `revisions.get()` | T15 | P0 |
| `RestoreRevision` | `POST /v1/teams/{teamId}/revisions/{rev}:restore` (`dryRun` supported; answers `RestoreResult`) | `revisions:restore` | `teams restore-revision` | `revisions.restore()` | T15 | P1 |

Service-level notification channels (for example the outside-contact email channel named in
`work_orders.notification_channel_id`) are in §4.11.

### 4.10a Work orders and swarm templates

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `ListWorkOrders` | `GET /v1/work-orders?status&severity&slaState&teamId` *CSV* | `work-orders:read` (security operators and service admins: all; teams: assigned or own repos) | `work-orders list-work-orders` | `workOrders.list()` | P3 | P2 |
| `CreateWorkOrder` | `POST /v1/work-orders` | `work-orders:write` | `work-orders create-work-order` | `workOrders.create()` | P3 | P2 |
| `GetWorkOrder` | `GET /v1/work-orders/{workOrderId}` (with repositories, progress counts, `slaState` and notifications) | `work-orders:read` | `work-orders describe-work-order` | `workOrders.get()` | P6 | P2 |
| `UpdateWorkOrder` | `PATCH /v1/work-orders/{workOrderId}` (title, SLA, cap, add repositories) | `work-orders:write` | `work-orders update-work-order` | `workOrders.update()` | P6 | P2 |
| `CancelWorkOrder` | `POST /v1/work-orders/{workOrderId}:cancel` `{reason}` | `work-orders:write` | `work-orders cancel-work-order` | `workOrders.cancel()` | P6 | P2 |
| `ListWorkOrderRepos` | `GET /v1/work-orders/{workOrderId}/repos` | `work-orders:read` | `work-orders list-repos` | `workOrders.repos.list()` | P6 | P2 |
| `UpdateWorkOrderRepo` | `PATCH /v1/work-orders/{workOrderId}/repos/{host}/{owner}/{repo}` `{status, prUrl?, prNumber?, detail?}` (`If-Match` optional for a workload principal, §2, CD-12) | `work-orders:report` (assigned team) or `work-orders:write` | `work-orders update-repo` | `workOrders.repos.update()` | P6 | P2 |
| `ListTeamWorkOrders` | `GET /v1/teams/{teamId}/work-orders` (team members; security operators get `403` and use `ListWorkOrders?teamId=`) | `work-orders:read` | `work-orders list-team-work-orders` | `workOrders.listForTeam()` | T1 | P2 |
| `ListSwarmTemplates` | `GET /v1/admin/swarm-templates` | `swarm-templates:read` *svc* | `swarm-templates list-swarm-templates` | `swarmTemplates.list()` | P5, P3 | P2 |
| `CreateSwarmTemplate` | `POST /v1/admin/swarm-templates` | `admin:swarm-templates` *svc* | `swarm-templates create-swarm-template` | `swarmTemplates.create()` | P5 | P2 |
| `GetSwarmTemplate` | `GET /v1/admin/swarm-templates/{templateId}` | `swarm-templates:read` *svc* | `swarm-templates describe-swarm-template` | `swarmTemplates.get()` | P5, P3 | P2 |
| `UpdateSwarmTemplate` | `PATCH /v1/admin/swarm-templates/{templateId}` | `admin:swarm-templates` *svc* | `swarm-templates update-swarm-template` | `swarmTemplates.update()` | P5 | P2 |
| `DeleteSwarmTemplate` | `DELETE /v1/admin/swarm-templates/{templateId}` (refused with `409` while an open work order uses it) | `admin:swarm-templates` *svc* | `swarm-templates delete-swarm-template` | `swarmTemplates.delete()` | P5 | P2 |
| `EnableSwarmTemplate` | `POST /v1/admin/swarm-templates/{templateId}:enable` | `admin:swarm-templates` *svc* | `swarm-templates enable-swarm-template` | `swarmTemplates.enable()` | P5 | P2 |
| `DisableSwarmTemplate` | `POST /v1/admin/swarm-templates/{templateId}:disable` | `admin:swarm-templates` *svc* | `swarm-templates disable-swarm-template` | `swarmTemplates.disable()` | P5 | P2 |

A swarm template's optional `spec.collaboration` is `{templateId}`, an agent template with the `paperclip` capability (else `ValidationError`, `field: spec.collaboration.templateId`); the remediation team then gets a collaboration agent (dedicated identity, daily snapshots) with the web UI on and the REST API off, and its boot secrets are generated by the reconciler (database contract §7.2.9). Pools never connect to Paperclip. A swarm template's `spec.secrets` entry is `{pool, name, description?, delivery?, target?}`: `pool` names one pool of the
template, `name` matches `^[a-z][a-z0-9-]{0,62}$` and is unique in the template, `description` has at most
1000 characters, `delivery` is `env` (default) or `file`, and `target` is an environment variable name
(`^[A-Z_][A-Z0-9_]{0,127}$`, default the upper-case name with underscores) or an absolute path, unique per
pool. A template never holds a value. When the reconciler instantiates the remediation team it creates each
secret empty in Secrets Manager, at the managed path of a pool secret, and records it (a `scope: pool` secret
and its binding); the team sets the value with `PutSecretValue`.

`CreateWorkOrder` body: `cveId` (the `externalRef`, one open work order per CVE → `409 Conflict`),
`severity` (`critical` \| `high` \| `medium` \| `low`), `title`, `slaDueAt`, `templateId`, optional
`budgetUsd` (else the template's), and `repos[]` {`host`, `repo` (`owner/name`), `owningTeamId?`,
`owningContact?` (email)}. A repository may name its owner explicitly (team id, email, or both) or name
none. With none, `afs-api` resolves the owner from the teams' namespace configuration value
`AFS_OWNED_REPOS` through `afs.resolve_repo_owners(host, repo)`: an exact `host/owner/repo` entry wins
over a `host/owner/*` wildcard, matching is case-insensitive, and deleted or deleting teams are skipped.
An explicit owner always wins over the configuration value (D38). Errors, all refusing the whole request:

| Case | Response |
|---|---|
| Two or more teams at the best match | `400 ValidationError`, `details.ambiguousRepos[{repo, teamIds[]}]` |
| No team lists the repository and no owner is given | `400 ValidationError`, `details.unownedRepos[]`; the message tells the caller to give `=team_…` or `=email` |
| An explicit `owningTeamId` that is unknown or deleted | `400 ValidationError`, `details.fields[]` |

The `CreateWorkOrder` response gives each repository `ownerSource` (`explicit` \| `team_config`), which
`afs-api` knows at create time; it is not stored, so `GetWorkOrder` and `ListWorkOrderRepos` show the
resolved owners without it. The CLI shorthand is `--repos host/owner/repo[=<owner>[,<owner>]]`: without
`=` the owner is resolved from team configuration; an owner containing `@` is `owningContact` and one
starting `team_` is `owningTeamId`; there is no slug lookup in the CLI. The console pre-fills its owner
picker from the same resolution (the error details above) and resolves slugs with `ListTeamDirectory`. AFS creates the remediation team
from the template and returns the work order with `teamId` once assigned. The same work order can
arrive as an EventBridge event `afs.work_order.requested` with `source_event_id`; duplicates are
ignored (D22).

### 4.11 Service administration

All *svc*. Screen S10 unless stated.

| Op | Method and path | Perm | CLI | SDK | Screen | P |
|---|---|---|---|---|---|---|
| `GetSettings` | `GET /v1/admin/settings` (every key of root PRD §6.13 with value, default and range, including the seven `collab.*` keys of the Paperclip PRD §6.9 (`PutSetting` accepts the same seven keys in its key enum: `collab.base_domain`, `collab.public_endpoints`, `collab.session_ttl_minutes`, `collab.max_body_mb`, `collab.content_scan`, `collab.allowed_source_cidrs`, `collab.denied_spike_threshold`) (`collab.base_domain` unset reads `null`); the database-only keys `work_orders.cost_center` and `work_orders.owner_email` are not exposed, CD-8) | `admin:settings` | `admin get-settings` | `admin.settings.get()` | S10 | P0 |
| `PutSetting` | `PUT /v1/admin/settings/{key}` `{value}` | `admin:settings` | `admin put-setting` | `admin.settings.put()` | S10, S9 | P0 |
| `ListIdentityProviders` | `GET /v1/admin/identity-providers` | `admin:identity-providers` | `admin list-identity-providers` | `admin.identityProviders.list()` | S10 | P0 |
| `CreateIdentityProvider` | `POST /v1/admin/identity-providers` (v1.3.0: optional `collabClientId` and `collabClientSecretArn`, a Secrets Manager reference, never the value; `collabClientSecretArn` needs `collabClientId`; `ListSignInProviders` never returns them) | `admin:identity-providers` | `admin create-identity-provider` | `admin.identityProviders.create()` | S10 | P0 |
| `GetIdentityProvider` | `GET /v1/admin/identity-providers/{id}` (with the teams using it) | `admin:identity-providers` | `admin describe-identity-provider` | `admin.identityProviders.get()` | S10 | P0 |
| `UpdateIdentityProvider` | `PATCH /v1/admin/identity-providers/{id}` (disabling one in use → `409`) | `admin:identity-providers` | `admin update-identity-provider` | `admin.identityProviders.update()` | S10 | P0 |
| `DeleteIdentityProvider` | `DELETE /v1/admin/identity-providers/{id}` (in use → `409`) | `admin:identity-providers` | `admin delete-identity-provider` | `admin.identityProviders.delete()` | S10 | P0 |
| `ListServiceNotificationChannels` | `GET /v1/admin/notification-channels` | `admin:settings` | `admin list-notification-channels` | `admin.notificationChannels.list()` | S10 | P2 |
| `CreateServiceNotificationChannel` | `POST /v1/admin/notification-channels` | `admin:settings` | `admin create-notification-channel` | `admin.notificationChannels.create()` | S10 | P2 |
| `UpdateServiceNotificationChannel` | `PATCH /v1/admin/notification-channels/{id}` | `admin:settings` | `admin update-notification-channel` | `admin.notificationChannels.update()` | S10 | P2 |
| `DeleteServiceNotificationChannel` | `DELETE /v1/admin/notification-channels/{id}` (named in a setting → `409`) | `admin:settings` | `admin delete-notification-channel` | `admin.notificationChannels.delete()` | S10 | P2 |
| `ListServiceTags` | `GET /v1/admin/service-tags` | `admin:service-tags` | `admin list-service-tags` | `admin.serviceTags.list()` | S9 | P1 |
| `PutServiceTag` | `PUT /v1/admin/service-tags/{key}` | `admin:service-tags` | `admin put-service-tag` | `admin.serviceTags.put()` | S9 | P1 |
| `DeleteServiceTag` | `DELETE /v1/admin/service-tags/{key}` | `admin:service-tags` | `admin delete-service-tag` | `admin.serviceTags.delete()` | S9 | P1 |
| `ListModels` | `GET /v1/models` (not *svc*: models enabled for the service, for any signed-in caller; v1.4.0 adds `callable` and `priceState` per item) | `models:read` | `models list-models` | `models.list()` | T3 | P0 |
| `ListAdminModels` | `GET /v1/admin/models` | `admin:models` | `admin list-models` | `admin.models.list()` | S4 | P0 |
| `CreateModel` | `POST /v1/admin/models` (`maxOutputTokens` required, D28) | `admin:models` | `admin create-model` | `admin.models.create()` | S4 | P0 |
| `UpdateModel` | `PATCH /v1/admin/models/{modelId}` (v1.4.0: changing `enabled` while service access is `all` → `409`, `service_model_access_all`) | `admin:models` | `admin update-model` | `admin.models.update()` | S4 | P0 |
| `PutServiceModelAccess` | `PUT /v1/admin/model-access` `{access: all\|selected}` → `{access, enabledModels, callableModels, version}` (v1.4.0, MOD-1; `If-Match` optional on the setting's version) | `admin:models` | `admin put-service-model-access` | `admin.models.putServiceAccess()` | S4 | P0 |
| `ListModelPrices` | `GET /v1/admin/models/{modelId}/prices` | `admin:pricing` | `admin list-model-prices` | `admin.prices.list()` | S4 | P0 |
| `CreateModelPrice` | `POST /v1/admin/models/{modelId}/prices` | `admin:pricing` | `admin create-model-price` | `admin.prices.create()` | S4 | P0 |
| `ListModelPriceProposals` | `GET /v1/admin/model-price-proposals?status=pending` | `admin:pricing` | `admin list-price-proposals` | `admin.priceProposals.list()` | S4, S1 | P1 |
| `ApproveModelPriceProposal` | `POST /v1/admin/model-price-proposals/{proposalId}:approve` `{note}` | `admin:pricing` | `admin approve-price-proposal` | `admin.priceProposals.approve()` | S4 | P1 |
| `RejectModelPriceProposal` | `POST /v1/admin/model-price-proposals/{proposalId}:reject` `{note}` | `admin:pricing` | `admin reject-price-proposal` | `admin.priceProposals.reject()` | S4 | P1 |
| `ListCostReconciliations` | `GET /v1/admin/cost-reconciliations?status=over_threshold` | `admin:reports` | `admin list-cost-reconciliations` | `admin.reconciliations.list()` | S1, S8 | P1 |
| `ListCatalogSystems` | `GET /v1/admin/external-systems` | `admin:external-systems` | `admin list-external-systems` | `admin.externalSystems.list()` | S7 | P0 |
| `CreateCatalogSystem` | `POST /v1/admin/external-systems` | `admin:external-systems` | `admin create-external-system` | `admin.externalSystems.create()` | S7 | P0 |
| `GetCatalogSystem` | `GET /v1/admin/external-systems/{systemId}` | `admin:external-systems` | `admin describe-external-system` | `admin.externalSystems.get()` | S7 | P0 |
| `UpdateCatalogSystem` | `PATCH /v1/admin/external-systems/{systemId}` (endpoints, service-level switch) | `admin:external-systems` | `admin update-external-system` | `admin.externalSystems.update()` | S7 | P0 |
| `DeleteCatalogSystem` | `DELETE /v1/admin/external-systems/{systemId}` (built-ins → `409`) | `admin:external-systems` | `admin delete-external-system` | `admin.externalSystems.delete()` | S7 | P0 |
| `ListStorageClassesAdmin` | `GET /v1/admin/storage-classes` | `admin:storage-classes` | `admin list-storage-classes` | `admin.storageClasses.list()` | S10 | P0 |
| `PutStorageClass` | `PUT /v1/admin/storage-classes/{name}` | `admin:storage-classes` | `admin put-storage-class` | `admin.storageClasses.put()` | S10 | P0 |
| `DeleteStorageClass` | `DELETE /v1/admin/storage-classes/{name}` (in use → `409`) | `admin:storage-classes` | `admin delete-storage-class` | `admin.storageClasses.delete()` | S10 | P0 |
| `ListAdminAgentTemplates` | `GET /v1/admin/agent-templates` | `admin:agent-templates` | `admin list-agent-templates` | `admin.agentTemplates.list()` | S10 | P0 |
| `PutAgentTemplate` | `PUT /v1/admin/agent-templates/{name}` (harness, defaults, `allowedDigests[]`, `enabled`) | `admin:agent-templates` | `admin put-agent-template` | `admin.agentTemplates.put()` | S10 | P0 |
| `DeleteAgentTemplate` | `DELETE /v1/admin/agent-templates/{name}` (in use → `409`; disable instead) | `admin:agent-templates` | `admin delete-agent-template` | `admin.agentTemplates.delete()` | S10 | P0 |
| `ListOrgAccounts` | `GET /v1/admin/org-accounts` | `admin:accounts` | `admin list-org-accounts` | `admin.orgAccounts.list()` | S6 | P1 |
| `ListAdminAuditEvents` | `GET /v1/admin/audit-events` *CSV* | `admin:audit` | `audit list-service-audit-events` | `admin.audit.list()` | S5 | P0 |
| `CreateExport` | `POST /v1/exports` (asynchronous; `202` with the export in `pending`) | `admin:export` | `admin create-export` | `admin.exports.create()` | S11 | P1 |
| `GetExport` | `GET /v1/exports/{exportId}` (status, size, `sha256`, `downloadUrl` when `succeeded`) | `admin:export` | `admin describe-export` | `admin.exports.get()` | S11 | P1 |
| `ListExports` | `GET /v1/exports` | `admin:export` | `admin list-exports` | `admin.exports.list()` | S11 | P1 |
| `CreateImport` | `POST /v1/imports?dryRun=true\|false` (body: `{bundle, accountMap}`, `application/json`, up to 100 MB) | `admin:export` | `admin create-import` | `admin.imports.create()` | S11 | P1 |
| `GetHealth` | `GET /healthz` | none | none (not a CLI operation) | `health.get()` | none | P0 |
| `GetReadiness` | `GET /readyz` | none | none (not a CLI operation) | `health.getReadiness()` | none | P0 |

**Model catalog (v1.4.0, MOD-1).** With `models.service_access = all` (default on a fresh install, `selected` on an
upgraded one) every catalog model is enabled for the service, including models the reconciler's daily Bedrock sync adds
(`origin: bedrock_sync`, only models the account can invoke, OR-D17); the per-model `enabled` flag is then ignored and
read-only. With `selected` the flag decides and synced models start disabled. A model is **callable** while it is enabled for
the service and has an approved price. `ListAdminModels` items add `serviceEnabled†`, `callable†`, `priceState†`
(`approved`: a price is in effect; `pending`: none in effect but a pending proposal or a future-dated price; `none`),
`origin†` and `teamsWithAccess†` (count). `GetSettings` and `PutSetting` include the `orchestration.*` and `models.*` keys of
the orchestration PRD §6.8 (database contract §7.0); `PutSetting` on `collab.base_domain` or `collab.public_endpoints` →
`ValidationError` (`reason: setting_renamed`, naming the new key); `PutSetting` on `models.service_access` has the same
effect as `PutServiceModelAccess`. Service admins see `orchestration.tools` (tools, templates, image repositories) on S12;
nothing else exposes it.

Export and import (D49): `CreateExport` records the request; the reconciler builds the bundle, writes it
to S3, and stores the checksum and a pre-signed download URL (valid 7 days) on the export row; after that a new
export is needed. `afs-api` never reads or writes S3. `CreateImport`
takes `{bundle, accountMap}` as the request body (`accountMap` rewrites account ids, secret ARNs, identity and role-link references from the source environment to the target; a team whose account is not mapped fails alone), verifies its checksum and `schemaVersion`, and applies it
as ordinary desired-state writes, one transaction per team (each with its audit and outbox records),
returning a report `{dryRun, teams[{slug, result: created|updated|skipped|failed, changes[], error?}]}`.
With `dryRun=true` (the default) nothing is written. A bundle over 100 MB → `400 ValidationError`.

### 4.13 Model endpoints, routes and providers (v1.5.0)

Source: [model endpoint gateway PRD](../../model-endpoint-gateway-PRD.md) Appendix D.3.4 (approved by J, 2026-11-25).
Every mutation writes audit and outbox in one transaction; updates need `If-Match`; credential values are write-only
and never returned (`credentialState` only). Team-scoped paths take `teamId` from the path.

| Operation | Method and path | Permission | CLI | SDK | Screen | Pri |
|---|---|---|---|---|---|---|
| `ListModelEndpoints` | `GET /v1/admin/model-endpoints` (service endpoints and, with `includeTeams=true`, team-declared ones; filters `provider`, `location`, `pricingMode`) | `admin:model-endpoints` | `model-endpoints list-model-endpoints` | `admin.modelEndpoints.list()` | 56 | P0 |
| `CreateModelEndpoint` | `POST /v1/admin/model-endpoints` (created off) | `admin:model-endpoints` | `model-endpoints create-model-endpoint` | `admin.modelEndpoints.create()` | 57 | P0 |
| `GetModelEndpoint` | `GET /v1/admin/model-endpoints/{endpointId}` | `admin:model-endpoints` | `model-endpoints describe-model-endpoint` | `admin.modelEndpoints.get()` | 58 | P0 |
| `UpdateModelEndpoint` | `PATCH /v1/admin/model-endpoints/{endpointId}` (includes `enabled`) | `admin:model-endpoints` | `model-endpoints update-model-endpoint` | `admin.modelEndpoints.update()` | 56, 58 | P0 |
| `DeleteModelEndpoint` | `DELETE /v1/admin/model-endpoints/{endpointId}` (refused while enabled, `409 endpoint_enabled`) | `admin:model-endpoints` | `model-endpoints delete-model-endpoint` | `admin.modelEndpoints.delete()` | 58 | P0 |
| `PutEndpointModels` | `PUT /v1/admin/model-endpoints/{endpointId}/models` | `admin:model-endpoints` | `model-endpoints put-endpoint-models` | `admin.modelEndpoints.putModels()` | 57 | P0 |
| `CreateEndpointModelPrice` | `POST /v1/admin/model-endpoints/{endpointId}/prices` | `admin:pricing` | `model-endpoints create-endpoint-model-price` | `admin.modelEndpoints.createPrice()` | 57 | P0 |
| `ListEndpointPriceProposals` | `GET /v1/admin/endpoint-price-proposals` | `admin:pricing` | `model-endpoints list-endpoint-price-proposals` | `admin.endpointPriceProposals.list()` | 56 | P0 |
| `ApproveEndpointPriceProposal` | `POST /v1/admin/endpoint-price-proposals/{proposalId}:approve` | `admin:pricing` | `model-endpoints approve-endpoint-price-proposal` | `admin.endpointPriceProposals.approve()` | 56 | P0 |
| `RejectEndpointPriceProposal` | `POST /v1/admin/endpoint-price-proposals/{proposalId}:reject` | `admin:pricing` | `model-endpoints reject-endpoint-price-proposal` | `admin.endpointPriceProposals.reject()` | 56 | P0 |
| `ListEndpointTeams` | `GET /v1/admin/model-endpoints/{endpointId}/teams` (selection, `selectedBy`, `credentialState`, `federationState`, gateway version, spend) | `admin:model-endpoints` | `model-endpoints list-endpoint-teams` | `admin.modelEndpoints.teams()` | 58 | P0 |
| `PutModelEndpointCredential` | `PUT /v1/admin/model-endpoints/{endpointId}/credentials/{scope}` (`scope` = team id or `_shared`; body `{value}`; → `{state, rotatedAt}`) | `admin:model-endpoints` | `model-endpoints put-model-endpoint-credential` (value from file, stdin or prompt) | `admin.modelEndpoints.putCredential()` | 58 | P0 |
| `RotateModelEndpointCredential` | `POST /v1/admin/model-endpoints/{endpointId}/credentials/{scope}:rotate` | `admin:model-endpoints` | `model-endpoints rotate-model-endpoint-credential` | `admin.modelEndpoints.rotateCredential()` | 58 | P0 |
| `PutEndpointFederation` | `PUT /v1/admin/model-endpoints/{endpointId}/federations/{teamId}` (Azure `tenantId`, `clientId`; Google `projectNumber`, `poolId`, `providerId`, `projectId`, `location`); response adds `expectedIssuer`, `expectedSubject`, `expectedAudience` or `expectedRoleArn` | `admin:model-endpoints` | `model-endpoints put-endpoint-federation` | `admin.modelEndpoints.putFederation()` | 58 | P0 |
| `ImportProviderStatement` | `POST /v1/admin/model-endpoints/{endpointId}/statements` (`period`, `source`, `rows[]` or CSV body) | `admin:pricing` | `model-endpoints import-provider-statement` | `admin.modelEndpoints.importStatement()` | 59 | P1 |
| `ListInferenceProviders` | `GET /v1/admin/inference-providers` (each with hosts, enabled, teamSelection, routes, teams, direct systems) | `admin:model-endpoints` | `model-endpoints list-inference-providers` | `admin.inferenceProviders.list()` | 68 | P0 |
| `UpdateInferenceProvider` | `PATCH /v1/admin/inference-providers/{provider}` (`enabled`, `teamSelection`, `reason`; Bedrock off also `fallbackModel` = `<route>/<model>` and `selectForAllTeams`; `dryRun=true` returns the impact counts of screen 68a without writing) | `admin:inference-providers` | `model-endpoints update-inference-provider` | `admin.inferenceProviders.update()` | 68, 68a | P0 |
| `PutTeamGatewaySettings` | `PUT /v1/admin/teams/{teamId}/gateway-settings` (`deployment` `shared`\|`per_namespace`, `mayAddModelEndpoints`) | `teams:set-guardrails` | `teams put-team-gateway-settings` | `admin.teams.putGatewaySettings()` | 60 | P0 |
| `ListTeamModelEndpoints` | `GET /v1/teams/{teamId}/model-endpoints` (`selected`, `own`, `available`; per item `selectedBy`, `credentialState`, health, `offReason`) | `model-endpoints:read` | `model-endpoints list-team-model-endpoints` | `teams.modelEndpoints.list()` | 61, 66 | P0 |
| `PutTeamModelEndpoints` | `PUT /v1/teams/{teamId}/model-endpoint-selections` (`{endpointIds[]}`; extra-terms and service-selected providers need a service admin, else `403 needs_service_admin`) | `model-endpoints:write` | `teams put-team-model-endpoints` | `teams.modelEndpoints.putSelections()` | 60, 61 | P0 |
| `CreateTeamModelEndpoint` | `POST /v1/teams/{teamId}/model-endpoints` (needs `mayAddModelEndpoints`, else `403 team_endpoints_not_allowed`) | `model-endpoints:write` | `model-endpoints create-team-model-endpoint` | `teams.modelEndpoints.create()` | 62 | P0 |
| `UpdateTeamModelEndpoint` | `PATCH /v1/teams/{teamId}/model-endpoints/{endpointId}` | `model-endpoints:write` | `model-endpoints update-team-model-endpoint` | `teams.modelEndpoints.update()` | 61 | P0 |
| `DeleteTeamModelEndpoint` | `DELETE /v1/teams/{teamId}/model-endpoints/{endpointId}` | `model-endpoints:write` | `model-endpoints delete-team-model-endpoint` | `teams.modelEndpoints.delete()` | 61 | P0 |
| `PutTeamEndpointModels` | `PUT /v1/teams/{teamId}/model-endpoints/{endpointId}/models` (prices inline for team endpoints) | `model-endpoints:write` | `model-endpoints put-team-endpoint-models` | `teams.modelEndpoints.putModels()` | 62 | P0 |
| `PutTeamModelEndpointCredential` | `PUT /v1/teams/{teamId}/model-endpoints/{endpointId}/credential` (team endpoints, and the team's credential of a `per_team` service endpoint) | `model-endpoints:write` | `model-endpoints put-team-model-endpoint-credential` | `teams.modelEndpoints.putCredential()` | 61, 62 | P0 |
| `GetInferenceReport` | `GET /v1/reports/inference` (service) and `GET /v1/teams/{teamId}/reports/inference` (`groupBy=route\|model\|agent\|hour`, CSV) | `admin:reports` / `reports:read` | `reports get-inference-report`, `reports get-team-inference-report` | `reports.inference()` | 59, 64 | P0 |

Changes to existing operations: `PutAgentModels` and `PutPoolModels` (and `CreateAgent` / `CreatePool` model fields) gain
`routeAccess` (`all`\|`selected`), `routes[]` (endpoint slugs) and `defaultModel` may be `<route>/<model>`; responses add
`effectiveRoutes[]` and `env` (the `AFS_LLM_*` names the agent will receive). Usage and cost reports gain
`groupBy=route` and a `route` filter. `ListModels` items gain `providerState` (`on`\|`off`). `GetMe` permissions include
the new ones.

New permissions (§3): `admin:model-endpoints` and `admin:inference-providers` (service admin); `model-endpoints:read`
(viewer, team-admin, service admin) and `model-endpoints:write` (team-admin, service admin). `admin:pricing` and
`teams:set-guardrails` cover prices and gateway settings.

New error reasons (§6): `slug_taken` (409), `slug_reserved`, `slug_invalid`, `base_url_not_https`, `provider_disabled`,
`no_fallback_model`, `endpoint_base_url_placeholder` (400), `needs_service_admin`, `team_endpoints_not_allowed` (403),
`endpoint_enabled` (409).

New events (§7): `model_endpoint.created`, `model_endpoint.updated`, `model_endpoint.deleted`,
`model_endpoint.enabled`, `model_endpoint.disabled`, `model_endpoint.credential_set`, `model_endpoint.selected`,
`model_endpoint.deselected` (api); `model_endpoint.degraded`, `model_endpoint.recovered` (reconciler);
`inference_provider.enabled`, `inference_provider.disabled` (api).

### 4.14 MCP servers, policy, reviews, ledger and federated identities (v1.6.0)

Source: [MCP gateway PRD](../../mcp-gateway-PRD.md) Appendix A to D (approved by J, 2026-11-25; ships with `-restapi`
1.3.0). Every mutation writes the row, an `audit_log` record and its outbox event in one transaction; updates need
`If-Match`; credential values are write-only and never returned (`credentialState` only). Service paths
(`/v1/mcp-servers…`) act on any server and are for service admins and security operators; team members act on their
team's servers and switches only through `/v1/teams/{teamId}/…` paths, which take `teamId` from the path (CD-45).

| Operation | Method and path | Permission | CLI | SDK | Screen | Pri |
|---|---|---|---|---|---|---|
| `ListMcpServers` | `GET /v1/mcp-servers` (filters `scope` `enterprise`\|`team`\|`all`, `reviewStatus`, `riskTier`, `teamId`, `location`) | `mcp-servers:read` | `mcp-servers list-mcp-servers` | `mcpServers.list()` | 46 | P0 |
| `CreateMcpServer` | `POST /v1/mcp-servers` (enterprise; created `draft`) | `mcp-servers:write` | `mcp-servers create-mcp-server` | `mcpServers.create()` | 48 | P0 |
| `GetMcpServer` | `GET /v1/mcp-servers/{mcpServerId}` (any server; checks, risk, adoption) | `mcp-servers:read` | `mcp-servers describe-mcp-server` | `mcpServers.get()` | 47 | P0 |
| `UpdateMcpServer` | `PATCH /v1/mcp-servers/{mcpServerId}` (enterprise; a change of URL, image, location or auth returns it to `pending_review`) | `mcp-servers:write` | `mcp-servers update-mcp-server` | `mcpServers.update()` | 47 | P0 |
| `DeleteMcpServer` | `DELETE /v1/mcp-servers/{mcpServerId}` (refused while switched on for any team, `409 server_in_use`) | `mcp-servers:write` | `mcp-servers delete-mcp-server` | `mcpServers.delete()` | 47 | P0 |
| `ImportMcpServerManifest` | `POST /v1/mcp-servers:import` (body: MCP Registry `server.json`; → a `draft`) | `mcp-servers:write` | `mcp-servers import-mcp-server-manifest` | `mcpServers.importManifest()` | 48 | P1 |
| `PublishMcpServer`, `RetireMcpServer` | `POST /v1/mcp-servers/{mcpServerId}:publish`, `:retire` (publish needs `approved`) | `mcp-servers:write` | `mcp-servers publish-mcp-server`, `retire-mcp-server` | `mcpServers.publish()`, `.retire()` | 47 | P0 |
| `SuspendMcpServer`, `ResumeMcpServer` | `POST /v1/mcp-servers/{mcpServerId}:suspend`, `:resume` (`reason` required, ≤ 500) | `mcp-servers:suspend` | `mcp-servers suspend-mcp-server`, `resume-mcp-server` | `mcpServers.suspend()`, `.resume()` | 47, 49 | P0 |
| `RefreshMcpInventory` | `POST /v1/mcp-servers/{mcpServerId}:refresh` (→ `202`; the gateway refreshes within 2 minutes) | `mcp-servers:write` | `mcp-servers refresh-mcp-inventory` | `mcpServers.refresh()` | 47 | P1 |
| `ListMcpPrimitives` | `GET /v1/mcp-servers/{mcpServerId}/primitives` (filters `kind`, `status`) | `mcp-servers:read` | `mcp-servers list-mcp-primitives` | `mcpServers.primitives()` | 47 | P0 |
| `GetMcpPrimitiveChange` | `GET /v1/mcp-servers/{mcpServerId}/primitives/{primitiveId}/change` (approved and pending definitions, field diff, checks) | `mcp-servers:read` | `mcp-servers get-mcp-primitive-change` | `mcpServers.primitiveChange()` | 49 | P0 |
| `ApproveMcpPrimitive`, `RejectMcpPrimitive` | `POST /v1/mcp-servers/{mcpServerId}/primitives/{primitiveId}:approve`, `:reject` (`note`; reject → `blocked`) | `mcp-servers:review` | `mcp-servers approve-mcp-primitive`, `reject-mcp-primitive` | `mcpServers.approvePrimitive()`, `.rejectPrimitive()` | 49 | P0 |
| `PutMcpPolicy` | `PUT /v1/mcp-servers/{mcpServerId}/policy` (service rules, `driftPolicy`, `allowSampling`, `trustAnnotations`) | `mcp-servers:write` | `mcp-servers put-mcp-policy` | `mcpServers.putPolicy()` | 47 | P0 |
| `PutMcpServerCredential` | `PUT /v1/mcp-servers/{mcpServerId}/credentials/{scope}` (`scope` = team id or `_shared`; body `{value}` or OAuth client `{clientId, clientSecret}`; → `{state, rotatedAt}`) | `mcp-servers:write` | `mcp-servers put-mcp-server-credential` (value from file, stdin or prompt) | `mcpServers.putCredential()` | 47 | P0 |
| `ApproveMcpServer`, `RejectMcpServer`, `RequestMcpServerChanges` | `POST /v1/mcp-servers/{mcpServerId}:approve`, `:reject`, `:request-changes` (`note` required; optional `primitives[]` `{primitiveId, decision: approve\|block}`) | `mcp-servers:review` | `mcp-servers approve-mcp-server`, `reject-mcp-server`, `request-mcp-server-changes` | `mcpServers.approve()`, `.reject()`, `.requestChanges()` | 49 | P0 |
| `ListMcpReviews` | `GET /v1/mcp-reviews` (pending servers and held primitives of every team; filters `kind` `server`\|`primitive`, `riskTier`, `teamId`, `approver` `security`\|`team_admin`) | `mcp-servers:review` | `mcp-servers list-mcp-reviews` | `mcpReviews.list()` | 49 | P0 |
| `ListMcpEvents` | `GET /v1/mcp-events` (filters `from`, `to`, `teamId`, `agentId`, `poolId`, `serverId`, `method`, `name`, `outcome`, `reason`) *CSV* | `mcp-events:read` | `audit list-mcp-events` | `mcpEvents.list()` | 55 | P0 |
| `GetMcpUsageReport` | `GET /v1/reports/mcp` (`groupBy` `server`\|`primitive`\|`team`\|`agent`\|`day`; `view` `usage`\|`unused`\|`refusals`\|`discovery`\|`adoption`; `window` days for `unused`, 30 or 90) *CSV* | `mcp-events:read` | `reports get-mcp-usage-report` | `reports.mcp()` | 55 | P0 |
| `StartMcpQueryCapture`, `StopMcpQueryCapture` | `POST /v1/mcp-settings/query-capture:start` (`days` ≤ `mcp.query_capture_max_days`, `reason` required), `:stop` | `mcp-settings:write` | `mcp-servers start-mcp-query-capture`, `stop-mcp-query-capture` | `mcpSettings.startQueryCapture()`, `.stopQueryCapture()` | 55 | P1 |
| `GetMcpQueryCapture` | `GET /v1/mcp-settings/query-capture` (the open window, if any) | `mcp-events:read` | `mcp-servers get-mcp-query-capture` | `mcpSettings.getQueryCapture()` | 55 | P1 |
| `ListTeamMcpServers` | `GET /v1/teams/{teamId}/mcp-servers` (published enterprise servers with the team's switch and mode, and the team's own servers; filter `state` `on`\|`off`\|`pending`) | `team-mcp-servers:read` | `teams list-team-mcp-servers` | `teams.mcpServers.list()` | 50, 54 | P0 |
| `GetTeamMcpServer` | `GET /v1/teams/{teamId}/mcp-servers/{mcpServerId}` (an enterprise server the team can see, or a team server) | `team-mcp-servers:read` | `teams describe-team-mcp-server` | `teams.mcpServers.get()` | 51, 54 | P0 |
| `PutTeamMcpServer` | `PUT /v1/teams/{teamId}/mcp-servers/{mcpServerId}` (`{enabled, mode: full\|read_only}`; enterprise needs `published`; `409 team_gateway_required` while the team gateway is `shared`, CD-44) | `team-mcp-servers:write` | `teams put-team-mcp-server` | `teams.mcpServers.put()` | 50 | P0 |
| `CreateTeamMcpServer` | `POST /v1/teams/{teamId}/mcp-servers` (needs `mayAddMcpServers`, else `403 team_servers_not_allowed`; created `draft`) | `team-mcp-servers:write` | `teams create-team-mcp-server` | `teams.mcpServers.create()` | 53 | P0 |
| `UpdateTeamMcpServer`, `DeleteTeamMcpServer` | `PATCH`/`DELETE /v1/teams/{teamId}/mcp-servers/{mcpServerId}` (team servers only; an enterprise id → `403 not_team_server`) | `team-mcp-servers:write` | `teams update-team-mcp-server`, `delete-team-mcp-server` | `teams.mcpServers.update()`, `.delete()` | 51 | P0 |
| `SubmitMcpServerForReview` | `POST /v1/teams/{teamId}/mcp-servers/{mcpServerId}:submit` (runs the automatic checks; → `pending_review` with `riskTier` and `approver`) | `team-mcp-servers:write` | `teams submit-mcp-server-for-review` | `teams.mcpServers.submit()` | 53 | P0 |
| `ApproveTeamMcpServer`, `RejectTeamMcpServer` | `POST /v1/teams/{teamId}/mcp-servers/{mcpServerId}:approve`, `:reject` (`note` required; refused with `403 approval_not_permitted_by_policy` when `mcp.team_server_approval` sends this tier to security) | `team-mcp-servers:approve` | `teams approve-team-mcp-server`, `reject-team-mcp-server` | `teams.mcpServers.approve()`, `.reject()` | 51 | P0 |
| `RefreshTeamMcpInventory` | `POST /v1/teams/{teamId}/mcp-servers/{mcpServerId}:refresh` | `team-mcp-servers:write` | `teams refresh-team-mcp-inventory` | `teams.mcpServers.refresh()` | 51 | P1 |
| `ListTeamMcpPrimitives` | `GET /v1/teams/{teamId}/mcp-servers/{mcpServerId}/primitives` (with the team's and service's decision per primitive) | `team-mcp-servers:read` | `teams list-team-mcp-primitives` | `teams.mcpServers.primitives()` | 51 | P0 |
| `ApproveTeamMcpPrimitive`, `RejectTeamMcpPrimitive` | `POST /v1/teams/{teamId}/mcp-servers/{mcpServerId}/primitives/{primitiveId}:approve`, `:reject` (team servers; a change that crosses a risk line needs security, `403 approval_not_permitted_by_policy`) | `team-mcp-servers:approve` | `teams approve-team-mcp-primitive`, `reject-team-mcp-primitive` | `teams.mcpServers.approvePrimitive()`, `.rejectPrimitive()` | 51 | P0 |
| `PutTeamMcpPolicy` | `PUT /v1/teams/{teamId}/mcp-servers/{mcpServerId}/policy` (team rules) | `team-mcp-servers:write` | `teams put-team-mcp-policy` | `teams.mcpServers.putPolicy()` | 51 | P0 |
| `PutTeamMcpServerCredential` | `PUT /v1/teams/{teamId}/mcp-servers/{mcpServerId}/credential` (team servers and the team's credential of a `per_team` enterprise server) | `team-mcp-servers:write` | `teams put-team-mcp-server-credential` | `teams.mcpServers.putCredential()` | 51 | P0 |
| `StartMcpOAuthConnect` | `POST /v1/teams/{teamId}/mcp-servers/{mcpServerId}/oauth:start` (→ `{authorizationUrl, state, expiresAt}`; `oauth_user` only) | `team-mcp-servers:write` | `teams start-mcp-oauth-connect` (prints the URL) | `teams.mcpServers.startOAuth()` | 51 | P0 |
| `CompleteMcpOAuthConnect` | `GET /v1/mcp-oauth/callback?code&state&iss` (no bearer; bound to `state`, single use, 10 minutes; redirects to the console) | none (state-bound) | none | none | 51 | P0 |
| `DisconnectMcpOAuth` | `POST /v1/teams/{teamId}/mcp-servers/{mcpServerId}/oauth:disconnect` | `team-mcp-servers:write` | `teams disconnect-mcp-oauth` | `teams.mcpServers.disconnectOAuth()` | 51 | P0 |
| `TestMcpFederation` | `POST /v1/teams/{teamId}/mcp-servers/{mcpServerId}/federation:test` (→ `202`; the gateway tries the exchange and records `federationState`) | `team-mcp-servers:write` | `teams test-mcp-federation` | `teams.mcpServers.testFederation()` | 51 | P1 |
| `GetEffectiveAgentMcp` | `GET /v1/teams/{teamId}/agents/{agentId}/mcp` and `GET /v1/teams/{teamId}/pools/{poolId}/mcp` (`GetEffectivePoolMcp`): servers, effective primitives with the deciding level and reason, URLs, env names | `team-mcp-servers:read` | `agents get-effective-agent-mcp`, `pools get-effective-pool-mcp` | `agents.mcp()`, `pools.mcp()` | 52 | P0 |
| `ListTeamMcpEvents` | `GET /v1/teams/{teamId}/mcp-events` (filters as `ListMcpEvents` without `teamId`) *CSV* | `mcp-events:read` | `audit list-team-mcp-events` | `teams.mcpEvents.list()` | 54 | P0 |
| `GetTeamMcpUsageReport` | `GET /v1/teams/{teamId}/reports/mcp` (as `GetMcpUsageReport` without `team`, `adoption`) *CSV* | `mcp-events:read` | `reports get-team-mcp-usage-report` | `teams.reports.mcp()` | 54 | P0 |
| `ListAgentFederatedIdentities` | `GET /v1/teams/{teamId}/agents/{agentId}/federated-identities` | `agents:read` | `agents list-agent-federated-identities` | `agents.federatedIdentities.list()` | 52 | P0 |
| `PutAgentFederatedIdentity`, `DeleteAgentFederatedIdentity` | `PUT`/`DELETE /v1/teams/{teamId}/agents/{agentId}/federated-identities/{name}` (`{audience, expirationSeconds, filePath?, envVar?}`) | `agent-federated-identities:write` | `agents put-agent-federated-identity`, `delete-agent-federated-identity` | `agents.federatedIdentities.put()`, `.delete()` | 52 | P0 |
| `ListPoolFederatedIdentities` | `GET /v1/teams/{teamId}/pools/{poolId}/federated-identities` | `pools:read` | `pools list-pool-federated-identities` | `pools.federatedIdentities.list()` | 52 | P1 |
| `PutPoolFederatedIdentity`, `DeletePoolFederatedIdentity` | `PUT`/`DELETE /v1/teams/{teamId}/pools/{poolId}/federated-identities/{name}` (as for agents) | `agent-federated-identities:write` | `pools put-pool-federated-identity`, `delete-pool-federated-identity` | `pools.federatedIdentities.put()`, `.delete()` | 52 | P1 |

That is 55 operations (counting each named operation once, including `GetEffectivePoolMcp`).

Changes to existing operations:

- `CreateAgent`, `UpdateAgent`, `CreatePool`, `UpdatePool` take `mcp`: `{access: all|selected, serverIds[], mode:
  search|full, blocks[]}` where `blocks[]` are agent-level rules `{serverId, name|class, effect: block}`. Responses add
  `mcp.effectiveServers[]` and `mcp.env` (the `AFS_MCP_*` names). Defaults: `access: all`, `mode` from
  `mcp.default_agent_mode`. A `serverIds` entry outside the team's effective set → `400 mcp_server_not_enabled_for_team`.
- `PutTeamGatewaySettings` (§4.13) takes `mayAddMcpServers` and `mcpPlacement` (`combined`|`separate`, default from
  `gateway.mcp.placement_default`).
- `PutSetting` accepts the keys of PRD Appendix B (`mcp.*`, `gateway.mcp.placement_default`,
  `federation.allowed_audiences`); writing them needs `mcp-settings:write`, and the setting values are validated as
  listed there. `mcp.team_server_approval` changes emit `mcp_settings.changed`.
- `CreateTeam` switches on every published enterprise server with `defaultForNewTeams` (mode as stored on the server).
- `ListExternalSystems` items gain `category` (`mcp` among others) and, for `mcp`, `governedBy` (the MCP server id
  that covers the host, after which the system cannot be granted: `409 governed_by_mcp_server`).
- `GetMe` permissions include the new ones; `team-mcp-servers:approve` is reported per team as the policy allows.

Mutations added to the §4.12 exemption list (audit only, no event; the matrix test list grows by five):
`RefreshMcpInventory`, `RefreshTeamMcpInventory`, `TestMcpFederation` (each sets a request timestamp the gateway acts
on), `StartMcpOAuthConnect` (writes a single-use state) and `StopMcpQueryCapture` when the window had already expired.

Resource shapes (§5): `McpServer` `{id, teamId|null, scope: enterprise|team, slug, displayName, owner, purpose, docUrl,
dataClassification, transport: streamable_http|hosted, location: external|corporate|in_namespace, url?, hosted?:
{imageDigest, args[], envNames[], instanceMode: per_team|per_agent, egressHosts[], resources}, authKind,
federation?: {issuer, audience, subjectTemplate, tokenEndpoint}, protocolVersion, reviewStatus:
draft|pending_review|approved|rejected|suspended, published, riskTier, riskReasons[], checks[], driftPolicy:
hold|auto_approve_compatible, defaultForNewTeams, allowSampling, trustAnnotations, limits, credentialState:
set|missing|expired|not_required, adoption?: {teams, agents}, version, createdAt, createdBy, updatedAt}`.
`McpPrimitive` `{id, serverId, kind, name, title, annotations, digest, approvedDigest, status, firstSeen, lastSeen,
decision?: {allowed, level, reason}}`. `McpEvent` as database contract §7.5.5 in camelCase. Credentials expose
`{state, subject?, scopes[], expiresAt?, rotatedAt?}` only.

New permissions (§3):

| Permission | Viewer | Team-admin | Service admin | Security operator | Notes |
|---|:-:|:-:|:-:|:-:|---|
| `mcp-servers:read` | | | ✓ | ✓ | Service paths only |
| `mcp-servers:write` | | | ✓ | | Enterprise servers, service policy, shared credentials |
| `mcp-servers:review` | | | ✓ | ✓ | Review queue, server and primitive decisions |
| `mcp-servers:suspend` | | | ✓ | ✓ | Any server, takes effect within 60 s |
| `team-mcp-servers:read` | ✓ | ✓ | ✓ | | Credential metadata only |
| `team-mcp-servers:write` | | ✓ | ✓ | | Switches, team servers, team policy, team credentials, OAuth connect |
| `team-mcp-servers:approve` | | ✓ when the policy allows | ✓ | | Condition `mcpApprovalPolicy`, decided in the use case from `mcp.team_server_approval` and the tier |
| `mcp-settings:write` | | | ✓ | | Appendix B keys and the query capture window |
| `mcp-events:read` | ✓ | ✓ | ✓ | ✓ | Team paths for team roles; service paths for service admins and security operators |
| `agent-federated-identities:write` | | ✓ | ✓ | | Agent and pool federated identities |

New error reasons (§6): `slug_invalid`, `slug_reserved`, `url_not_https`, `host_denied`, `image_not_pinned`,
`image_unsigned`, `registry_not_allowed`, `auth_kind_unsupported`, `service_account_required`, `audience_not_allowed`,
`audience_reserved`, `capture_window_too_long`, `mcp_server_not_enabled_for_team`, `invalid_policy_rule` (400);
`team_servers_not_allowed`, `approval_not_permitted_by_policy`, `not_team_server` (403); `slug_taken`, `server_in_use`,
`server_not_published`, `server_not_approved`, `team_gateway_required`, `invalid_review_transition`,
`federated_identity_limit`, `governed_by_mcp_server`, `capture_active` (409).

New events (§7):

| Type | Emitted by | When |
|---|---|---|
| `mcp_server.created`, `.updated`, `.deleted`, `.published`, `.retired` | api | Catalog mutations; `data` `{serverId, teamId?, slug, scope}` plus `changes` on update |
| `mcp_server.submitted`, `.approved`, `.rejected`, `.changes_requested` | api | Review; `data` `{serverId, teamId?, riskTier, approver, decidedBy?, note?}`; `submitted` is delivered to reviewers' channels, decisions to the owning team |
| `mcp_server.suspended`, `.resumed` | api | `data` `{serverId, teamId?, reason}`; delivered to every team that has it on |
| `mcp_server.primitive_changed` | gateway | A new or changed primitive is held; `data` `{serverId, teamId?, primitiveId, kind, name, change: new\|changed, digest, previousDigest?, autoApproved}`; at most one per server per refresh |
| `mcp_server.primitive_approved`, `.primitive_rejected` | api | `data` `{serverId, primitiveId, digest, decidedBy}` |
| `mcp_server.credential_set`, `.credential_removed` | api | `PutMcpServerCredential`, `PutTeamMcpServerCredential`, `CompleteMcpOAuthConnect`, `DisconnectMcpOAuth`; `data` `{serverId, scope, kind, state, subject?}`; never a value |
| `mcp_server.policy_changed` | api | `PutMcpPolicy`, `PutTeamMcpPolicy`; `data` `{serverId, level, teamId?, rules: {added, removed}}` |
| `mcp_server.auth_expired`, `.auth_restored` | gateway | `oauth_user` refresh failed or was restored; `data` `{serverId, teamId}` |
| `mcp_server.degraded`, `.recovered` | reconciler | Condition `McpServerReady` changed; `data` `{serverId, teamId, reason}` |
| `team_mcp_server.enabled`, `.disabled` | api, db function | `PutTeamMcpServer` and the `defaultForNewTeams` switch-on in `CreateTeam`; `data` `{teamId, serverId, mode}` |
| `mcp_settings.changed` | api | An `mcp.*` setting changed; `data` `{key, from, to}` |
| `mcp_query_capture.started`, `.stopped` | api, reconciler | `data` `{captureId, startedBy, until, reason}`; the reconciler emits `stopped` at expiry |
| `agent.federated_identity_changed` | api | Put or delete; `data` `{teamId, agentId?, poolId?, name, audience, change: put\|delete}` |

| # | Decision |
|---|---|
| CD-45 | Service paths serve service admins and security operators for every server; team roles use mirrored team paths only, because the authorization middleware takes `teamId` from the path and security operators are denied team routes. A team-admin's right to approve is a conditional grant decided in the use case from `mcp.team_server_approval` and the tier |
| CD-48 | `CompleteMcpOAuthConnect` is the only unauthenticated `/v1` route: it is bound to a single-use `state` (10 minutes, sealed with the caller and server) and to `iss` (RFC 9207), writes the refresh token to Secrets Manager and redirects; it carries no permission and is listed in the matrix's public-route list |
| CD-49 | `PutSetting` for `mcp.*` keys is audited and emits `mcp_settings.changed`, unlike other settings (§4.12), because a change of the approval policy changes who may approve |

### 4.12 Mutations without an outbox event (exemption from the outbox rule)

(v1.4.0 adds `PutServiceModelAccess`, 72 operations; v1.3.0 adds `ConnectAgentToCollaboration` and `DisconnectAgentFromCollaboration`, 71 operations; `PutTeamCollaboration` emits `collaboration.updated` and `RotateCollaborationKeys` emits `secret.rotated`, so neither is listed.)

These mutations have no event type in §7. Each writes the row and one audit record in one transaction and
no `outbox_events` row (CD-7). Adding an event type for one of them is a contract change (§8) and removes it
from this list. The `-restapi` test `internal/adapter/httpapi/matrix/outbox_exemptions_test.go` embeds the
list and fails when the two diverge.

| Category | Operations |
|---|---|
| members | `AddMember`, `UpdateMember`, `RemoveMember`, `AddSecurityOperator`, `RemoveSecurityOperator` |
| api-keys | `CreateApiKey`, `DeleteApiKey`, `CreateServiceApiKey`, `DeleteServiceApiKey` |
| settings | `PutSetting`, `PutServiceModelAccess` (v1.4.0), `PutBudget`, `PutServiceTag`, `DeleteServiceTag`, `CreateIdentityProvider`, `UpdateIdentityProvider`, `DeleteIdentityProvider`, `CreateLogDestination`, `UpdateLogDestination`, `DeleteLogDestination`, `CreateNotificationChannel`, `UpdateNotificationChannel`, `DeleteNotificationChannel`, `CreateServiceNotificationChannel`, `UpdateServiceNotificationChannel`, `DeleteServiceNotificationChannel` |
| catalog | `CreateCatalogSystem`, `UpdateCatalogSystem`, `DeleteCatalogSystem`, `DeclareTeamExternalSystem`, `UpdateTeamExternalSystem`, `DeleteTeamExternalSystem`, `SetTeamExternalSystemState`, `SetAgentExternalSystemState`, `PutPoolExternalSystem`, `CreateModel`, `UpdateModel`, `CreateModelPrice`, `CreateStage`, `UpdateStage`, `DeleteStage`, `PutAgentTemplate`, `DeleteAgentTemplate`, `PutStorageClass`, `DeleteStorageClass` |
| config | `ConnectAgentToCollaboration`, `DisconnectAgentFromCollaboration`, `PutNamespaceConfigValue`, `DeleteNamespaceConfigValue`, `PutAgentConfigValue`, `DeleteAgentConfigValue`, `PutPoolConfigValue`, `DeletePoolConfigValue`, `CreateAgentIdentity`, `DeleteAgentIdentity`, `CreateAgentRoleLink`, `DeleteAgentRoleLink`, `VerifyAgentRoleLink`, `CreatePoolRoleLink`, `DeletePoolRoleLink`, `VerifyPoolRoleLink`, `CreateAgentSnapshot`, `CreateExport`, `UpdateSecret` |
| schedules | `CreateReportSchedule`, `UpdateReportSchedule`, `DeleteReportSchedule` |
| swarm-templates | `CreateSwarmTemplate`, `UpdateSwarmTemplate`, `DeleteSwarmTemplate`, `EnableSwarmTemplate`, `DisableSwarmTemplate` |

Secret create, rotate and delete keep their `secret.*` events (§7). `UpdateWorkOrder` emits
`work_order.repo_updated` only when it adds repositories; a change of title, SLA or budget emits nothing.

## 5. Resource shapes (wire, abbreviated)

Fields marked † are read-only. All resources also carry `id`†, `version`†, `createdAt`†, `createdBy`†,
`updatedAt`† and, for desired state, `generation`† and `status`†.

```yaml
Team:        {slug, displayName, description, kind: persistent|ephemeral, state†: active|suspended|deleting|deleted,
              namespace†, costCenter, ownerEmail, maxBudgetUsd, ceilingOverrideUsd?, ceilingOverrideExpiresAt?,
              effectiveCeilingUsd†, quotas{cpu,memory,pods,storage},
              isolationTier: shared|dedicated, viewerKubectl, expiresAt?, teardownRule?: at_expiry|at_cap_or_expiry|at_work_complete,
              tornDownAt†?, teardownReason†?, identityProviderId?, allowedAwsAccounts[], mayManageAwsAccounts,
              mayAddExternalSystems, mayExposeCollaboration (deprecated), maxCollaborationExposure: off|private|public,
              modelAccess: all|selected, orchestrationAgentId†}
TeamTags:    {tags: [{key, value?, source†: system|service|team|service_default|missing}],
              requestMetadataDroppedKeys†[]}       # PUT sends team-owned keys only; the read-only list names tag keys left out of the Bedrock requestMetadata because of its pair limit (D27)
Member:      {role: team_admin|team_collaborator|team_viewer, subjectType: user|group, userId?, email†?, groupName?}
ApiKey:      {name, role: team_admin|team_collaborator|team_viewer|security_operator, keyPrefix†, expiresAt (≤ 90 days), lastUsedAt†, secretKey† (create response only)}
Agent:       {name, role†: worker|orchestration (v1.4.0; create accepts only worker), runtime†?: orchestration,
              models{access: all|selected, modelIds[], defaultModelId, effectiveModelIds†}, modelId (deprecated alias of models.defaultModelId), collaboration?{connect, paperclipAgentId?, linkState†, lastSyncedAt†?, lastError†?}, description, template (null for non-admins on the orchestration agent), image, resources{cpu,memory}, iamIdentity, iamRoleArn†,
              volume{sizeGiB, storageClass, onDelete: snapshot_then_delete|delete|retain, snapshotSchedule: off|daily|weekly, snapshotRetain},
              secretBindings[{secretId, delivery: env|file, target}], paused†}
RoleLink:    {profileName, roleArn, accountId†, maxSessionSeconds, externalId†, status†: pending_trust|verified|failed|disabled,
              lastVerifiedAt†, trustPolicy†}
Stage:       {name, position (1..50), description}
Pool:        {name, description, stageId?, template, image, persona?, models{access, modelIds[], defaultModelId, effectiveModelIds†}, modelId (deprecated), workers (0..200), resources{cpu,memory},
              scratchSizeGiB (1..500), sharedVolume: none|efs, state†: enabled|paused|deleting|deleted, handoffs†[poolId]}
PoolRoleLink: {roleName, accountId, profileName, roleArn†, externalId†, maxSessionSeconds, status†, lastVerifiedAt†}
SwarmTemplate: {name, description, kind: cve_remediation|general, spec{stages[], pools[], handoffs[], secrets[{pool, name, description?, delivery?, target?}], configValues[], orchestration?{tool?, webUiExposure?, webApiExposure?, defaultModelId?}, collaboration?{templateId} (deprecated)},
              budgetUsd, ttlHours, teardownRule, enabled}
WorkOrder:   {kind: cve_remediation|general, cveId?, severity, title, source†: api|eventbridge, templateId, budgetUsd, slaDueAt,
              status†: open|assigned|in_progress|pr_opened|done|failed|cancelled, teamId†?, assignedAt†?, completedAt†?,
              slaState†: on_track|due_soon|breached|met|missed, progress†{repos, prsOpen, prsMerged, notAffected, failedOrSkipped, remaining}}
WorkOrderRepo: {host, repo, owningTeamId?, owningContact?, ownerSource†? (CreateWorkOrder response only): explicit|team_config, status: pending|in_progress|pr_opened|merged|not_affected|failed|skipped,
              prUrl?, prNumber?, detail?, mergedAt†?, notifications†[{kind, recipient, sentAt}]}
ReportSchedule: {name, report, query{}, cadence, channelId, format: csv, enabled, nextRunAt†, lastRunAt†, lastStatus†, lastError†}
CostTagStatus: {key, seenInCur, firstSeenAt, lastSeenAt, awsStatus: active|inactive|unknown, checkedAt}
Secret:      {managedBy†: user|afs|paperclip|buzz, scope: team|agent|pool, agentId?, poolId?, name, description, externalSystemId?, secretsManagerPath†, secretsManagerArn†,
              versionId†, lastChangedAt†, rotation{enabled, days}, boundTo†[agentId]}               # no value field, ever
ConfigValue: {name, value?, source: literal|namespace, resolvedValue†, usedBy†[agentId]}
ExternalSystem: {slug, name, category, description, docsUrl, endpoints[], enabled, builtin†, owner?, purpose?,
              scope†: service|team, teamState†: on|off, disabledAt†: service|team|agent|pool|null, unmetered†}      # pool only in pool views (CD-11)
Budget:      {period: monthly|lifetime, amountUsd, thresholds[], onExhaust: block|notify (notify: persistent teams, service admins only), expiresAt?,
              spentUsd†, reservedUsd†, percentUsed†, forecastUsd†, projectedCapAt†}
ServiceTag:  {key, mode: fixed|required, value?, defaultValue?, allowedValues?[], description}
IdentityProvider: {kind: okta|entra_id, name, issuerUrl, consoleClientId, cliClientId, collabClientId?, collabClientSecretArn?, clientSecretArn?, groupsClaim, isDefault, enabled}
Model:       {id, provider, displayName, enabled, contextWindow, maxOutputTokens, callable†, priceState†: approved|pending|none,
              serviceEnabled†, origin†: manual|bedrock_sync, teamsWithAccess†? (admin list only)}
TeamModels:  {access: all|selected, modelIds[], items[Model + inferenceProfileState†: creating|active|retiring|null]}
ServiceModelAccess: {access: all|selected, enabledModels†, callableModels†, version†}
AgentTemplate: {name, description, harness, capabilities[]: orchestration|tool:paperclip|tool:buzz, defaultResources{cpu,memory}, allowedDigests[], currentDigest, enabled}
Export:      {status†: pending|running|succeeded|failed, schemaVersion†, sha256†, sizeBytes†, downloadUrl†?, finishedAt†?}
TeamCollaboration: {teamId†, agentId†, hostname†?, webUiUrl†?, restApiUrl†?, webUiEnabled, restApiEnabled, registrationMode: manual|managed,
              effective†{webUi, restApi, reasons[]}, unmanagedPaperclipAgents†[]}     # §4.4b
CollaborationLink: {agentId†, agentName†, connect, paperclipAgentId?, toolAgentId†? (Buzz public key), linkState†: pending|linked|failed|disabled, lastSyncedAt†?, lastError†?}
TeamOrchestration: {teamId†, agentId†, agentName†, phase†, tool†: paperclip|buzz, toolVersion†?, registrationMode: manual|managed,
              limit†: off|private|public, webUi{supported†, exposure, effective†, reasons†[], hostname†?, url†?}, webApi{…},
              connectedAgents†, unmanagedToolAgents†[]}     # §4.4d
OrchestrationEndpoint: {teamId, slug, teamState, agentId, tool, surface: webUi|webApi, exposure, effective, reasons[], limit, hostname?, phase, lastAccessAt?}
CollaborationEndpoint: {teamId, slug, teamState, hostname?, agentId, webUiEnabled, restApiEnabled, effective{webUi, restApi}, mayExposeCollaboration, phase, lastAccessAt?}
TeamDirectoryEntry: {id, slug, displayName}
PriceProposal: {modelId, effectiveFrom, inputUsdPerMtok, outputUsdPerMtok, cacheReadUsdPerMtok, cacheWriteUsdPerMtok,
              status†: pending|approved|rejected|superseded, fetchedAt†, decidedBy†, decidedAt†, note}
NotificationTestResult: {testId, delivered?, pending?, error?: string|null}     # 200: delivered true|false (+ error); 202: pending true
RestoreResult: {dryRun, revision, changes[RestoreChange], skipped?[RestoreChange]}         # skipped: bindings or references that no longer resolve
RestoreChange: {resourceKind, resourceId, action: create|update|delete|skip, fields?[{field, ...}], reason?}
Snapshot:    {id†, agentId†, reason†: scheduled|manual|pre_delete|pre_change|pre_tool_change, tool†?, retainUntil†?, state†: requested|pending|completed|failed|deleted,
              sizeGiB†?, requestedAt†, requestedBy†?, completedAt†?}
StorageClass: {name, description?, volumeType: gp3|io2, enabled, version†, createdAt†, updatedAt†}
AuditEvent:  {id (integer), occurredAt, actorType: user|api_key|system|agent|pool, actorId, action, outcome: allowed|denied|failed,
              resourceKind?, resourceId?, teamId?, requestId?, sourceIp?, userAgent?, workerPod?, before?{}, after?{}}   # no secret values
Revision:    {revision (integer), resourceKind, resourceId, operation: INSERT|UPDATE|DELETE, version?, changedBy, changedAt, snapshot?{}}
CostRow:     {day, source: gateway|cur_bedrock|cur_eks|cur_other, agentId?, poolId?, modelId?, costUsd, calls?}
Me:          {principal{id, type: user|api_key|workload, email?, name?}, serviceAdmin, securityOperator, servicePermissions[], teams[{teamId, slug, role, permissions[]}]}
              # servicePermissions: every service-level permission for a service admin, the security operator column
              # for a security operator, empty otherwise; clients use it for UX only
```

## 6. Errors

Body: `{"code": "<Code>", "message": "<text>", "requestId": "<id>", "details": {}}`. `message` is for people and may
change; clients branch on `code` only. No error contains a secret value, token or API key.

| HTTP | `code` | When | Database origin (see [database contract](database-contract.md) §6) |
|---|---|---|---|
| 400 | `ValidationError` | Malformed or out-of-range input, a body over the size limit (`field: body`; there is no `413`), a page token issued to another caller or for another operation; `details.fields[]` lists `{field, reason}`. v1.4.0 reasons: `role_reserved`, `name_reserved`, `orchestration_field_fixed`, `exposure_above_limit`, `surface_not_supported_by_tool`, `registration_mode_not_supported`, `tool_not_enabled`, `tool_unchanged`, `no_model_available`, `model_not_enabled_for_service`, `model_not_enabled_for_team`, `models_selection_empty`, `default_model_not_allowed`, `model_ids_not_allowed_for_all`, `conflicting_fields`, `setting_renamed` | `23514`, `23502`, `22xxx` |
| 401 | `Unauthenticated` | Missing, expired or unknown credential | — |
| 403 | `AccessDenied` | Valid caller without the permission, including a signed-in caller with no team binding on a route not scoped to one team (CD-10); `details.requiredPermission` | `42501` |
| 404 | `NotFound` | No such resource, **or** a resource in a team the caller cannot see (never reveals existence), **or** an unknown path or `:verb` (error envelope, not a bare 404) | RLS hides the row |
| 402 | `BudgetExceeded` | Returned by `afs-gateway` to agents only, never by `afs-api`; not retryable (see [gateway contract](gateway-contract.md)) | `reserve_spend_held` returned NULL |
| 409 | `Conflict` | Duplicate name, idempotency body mismatch or in progress (these two are `details.reason` values, `idempotency_mismatch` and `idempotency_in_progress`, not separate codes; v1.3.0 adds `collaboration_agent_exists`, `collaboration_agent_missing`, `managed_secret` and `hostname_held`; v1.4.0 adds `orchestration_agent_required`, `orchestration_agent_exists`, `orchestration_agent_pending`, `orchestration_runtime_unset`, `tool_change_in_progress` and `service_model_access_all`), dependent objects exist, invalid state transition (for example a suspended team) | `23505`, `23503` (on delete), `55000` |
| 412 | `PreconditionFailed` | `If-Match` does not match `version` | — |
| 428 | `PreconditionRequired` | `If-Match` missing on `PATCH` or `DELETE` of an existing resource (optional on `PUT`) | — |
| 429 | `Throttled` | Rate limit; `Retry-After` | — |
| 500 | `InternalError` | Anything else; logged with `requestId` | `40001` after retries, others |
| 502 | `UpstreamError` | Returned by `afs-gateway` only, when Bedrock returns a server error; the reservation is released; retry with backoff | — |
| 503 | `Unavailable` | Dependency down (database, Secrets Manager); safe to retry with backoff | — |
| 422 | `SecretDetected` | Returned by the collaboration edge only (§4.4c), never by `afs-api`; `details.patterns[]` | — |

CLI rendering: `An error occurred (<code>) when calling the <operationId> operation: <message>`, exit
254. SDK: `AfsError` with `code`, `status`, `requestId`, `details`, and getters `isValidationError`,
`isUnauthenticated`, `isAccessDenied`, `isNotFound`, `isConflict`, `isPreconditionFailed`,
`isPreconditionRequired`, `isThrottled`, `isUnavailable` and `isInternalError` (`BudgetExceeded` and `UpstreamError` come only from the gateway, so the SDK has no helper for them).

Client handling:
- **428.** Clients never rely on it: the CLI and SDK send `If-Match` on every `PATCH`, `DELETE` and, when they hold the resource's version, `PUT`
  of an existing resource (`--if-match <version>` or a resource carrying `version` is the safe mode;
  otherwise the CLI reads first, then writes, and prints to stderr the version it applied to). A `428`
  is a client bug: CLI exit 254, SDK `isPreconditionRequired`, never retried (D40).
- **Retries.** SDK and CLI retry `429` and `503` with jittered backoff honouring `Retry-After`, and `409`
  with `details.reason = idempotency_in_progress` after `Retry-After: 1`, only for requests that are
  idempotent or carry an `Idempotency-Key`. They never retry `402`, `412` or `428`.
- **401.** The CLI exits 255 without a call when its cached token is known to be expired, with the
  `afs sso login` hint, and exits 254 on a server `401`; it refreshes silently when a refresh token exists
  (D41).

## 7. Events (EventBridge, from the outbox)

Source `afs`, `detail-type` = event type, `detail` = `{id, occurredAt, teamId, agentId?, poolId?, actor, data}`.
Delivery is at least once and ordered per resource; consumers must be idempotent on `id`. The reconciler
delivers events to each team's notification channels through one EventBridge rule and target per channel
(D46).

| Type | Emitted by | When |
|---|---|---|
| `team.created`, `team.updated`, `team.suspended`, `team.resumed`, `team.deleted`, `team.tags_changed`, `team.ceiling_override_set` | api | Team mutations |
| `team.status_changed`, `agent.status_changed` | reconciler | Phase change |
| `agent.created`, `agent.updated`, `agent.deleted`, `agent.paused`, `agent.resumed`, `agent.restarted` | api | Agent mutations |
| `secret.created`, `secret.rotated`, `secret.deleted` | api | No value in the event |
| `budget.threshold_crossed`, `budget.exhausted` | gateway | Once per period per threshold |
| `budget.reset` | reconciler | A new period's spend counter is opened (D31) |
| `budget.ceiling_override_expired` | reconciler | A ceiling override expired; `data` has the base ceiling and whether the budget was lowered (D34) |
| `cost.variance_detected`, `pricing.proposal_created`, `pricing.proposal_decided` | billing, api | BUD-6, BUD-9 |
| `drift.corrected` | reconciler | Manual change reverted |
| `egress.denied_spike` | reconciler collectors | More than `egress.denied_spike_threshold` denials from one agent or pool in 5 minutes (RPT-6). Agent sign-ins go to team log delivery only, never to events (D17) |
| `role_link.verified`, `role_link.failed` | reconciler | AGT-10 |
| `team.teardown_started`, `team.torn_down` | reconciler | TEAM-7; `data.reason` = `expired` \| `cap_reached` \| `work_complete` \| `manual` |
| `pool.created`, `pool.updated`, `pool.scaled`, `pool.deleted`, `pool.status_changed` | api, reconciler | AGT-8 |
| `report.delivered`, `report.failed` | billing | RPT-8 |
| `work_order.created`, `work_order.assigned`, `work_order.repo_updated`, `work_order.pr_opened`, `work_order.merged`, `work_order.sla_warning`, `work_order.sla_breached`, `work_order.completed`, `work_order.failed`, `work_order.cancelled` | api, reconciler | WO-1 to WO-5; owners notified once per event |
| `cost_tags.not_seen` | billing | TAG-7 |
| `export.completed`, `export.failed`, `import.applied` | reconciler, api | CFG-5 |
| `collaboration.updated` | api | `PutTeamCollaboration`; `data` `{teamId, agentId, changes: {webUiEnabled?: {from, to}, restApiEnabled?: {from, to}, registrationMode?: {from, to}}}` |
| `collaboration.endpoint_state_changed` | reconciler | `EndpointReady` changed; `data` `{teamId, agentId, hostname, ready, previousReady, reason: Ready\|Starting\|Paused\|TeamSuspended\|ServiceOff\|GuardrailOff\|BaseDomainUnset\|RouteNotPublished, effective: {webUi, restApi}}` |
| `collaboration.secret_detected` | edge, reconciler | A credential pattern was found. No value or fragment ever. `data` `{teamId, source: edge\|reconciler, pattern, action: blocked\|warned\|detected, count, surface?: web_ui\|rest_api, principal?: {id, type: user\|api_key}, paperclipItemId?, authorAgentId?}` (`surface` and `principal` from the edge; `paperclipItemId` and `authorAgentId` from the reconciler's scan of agent writes, COL-25). The edge emits at most one per team, person and pattern per 5 minutes |
| `collaboration.denied_spike` | reconciler | More than `collab.denied_spike_threshold` denied requests (`401`, `403`, `422`) on one team in 5 minutes, read from `collaboration_access`; `data` `{teamId, windowStart, windowEnd, denied, threshold, byReason: {unauthenticated, forbidden, surface_disabled, secret_detected}}`; delivered to the team's channels and the service admins' channels; at most one per team per 15 minutes |
| `collaboration.unmanaged_agents_found` | reconciler | COL-13, once per new name; `data` `{teamId, agentId, paperclipAgentName, paperclipAgentId, reason: not_linked\|local_adapter}` |
| `collaboration.bootstrap_completed` | reconciler | Paperclip's first-admin bootstrap finished; `data` `{teamId, agentId, mode: manual\|managed}`. Never the invite URL: the invite is a credential and goes only as a direct notification to the creating team-admin's email through the reconciler's notification delivery, never into events, audit or logs (G8) |
| `agent.collaboration_linked`, `agent.collaboration_unlinked` | reconciler | An agent's `linkState` became `linked`, or left `linked`; `data` `{agentId, linkState, paperclipAgentId?, registrationMode}` (`unlinked` adds `reason`: `disconnected`, `agent_deleted`, `collaboration_agent_deleted`, `failed`) |
| `orchestration.created` | api, db function | The team's orchestration agent was created (`CreateTeam`, backfill, work order team); `data` `{teamId, agentId, agentName, tool, limit, webUiExposure, webApiExposure}` |
| `orchestration.updated` | api | `PutTeamOrchestration` (and the deprecated `PutTeamCollaboration`); `data` `{teamId, agentId, changes: {webUiExposure?: {from, to}, webApiExposure?: {from, to}, registrationMode?: {from, to}}}`. A limit change is in `team.updated` |
| `orchestration.tool_changed` | api | `ChangeCollaborationTool`; `data` `{teamId, agentId, fromTool, toTool, snapshotId}` |
| `orchestration.endpoint_state_changed` | reconciler | A surface's effective level or readiness changed; `data` `{teamId, agentId, surface: webUi\|webApi, exposure, effective, previousEffective, reasons[], hostname, ready}`. Replaces `collaboration.endpoint_state_changed`, which is still emitted until 2.0.0 |
| `team.models_changed` | api | `PutTeamModels`; `data` `{teamId, from: {access, modelIds}, to: {access, modelIds}}` |
| `agent.model_unavailable` | reconciler | An agent's or pool's default model left its effective set, or the set became empty; once per transition; `data` `{teamId, agentId?, poolId?, defaultModelId, reason: model_not_enabled_for_service\|model_not_enabled_for_team\|model_not_enabled_for_agent\|model_not_priced, effectiveModelIds}`; delivered to the team's channels |
| `notification.delivery_failed` | reconciler | A channel's EventBridge target failed (D46) |
| `notification.test_requested` | api | `TestNotificationChannel`; `data` has `channelId` and `testId`. The reconciler tests the channel and reports the result on the channel's status (condition `DeliveryHealthy`, `lastTestId`), which the API returns (CD-3) |

v1.4.0: `collaboration.updated` is deprecated and emitted only by the deprecated `PutTeamCollaboration` (CD-34). `CreateTeam` emits `secret.created` for each boot secret of the orchestration agent. `CreateAgent` for a collaboration agent (1.0.1 only) also emits `secret.created` for each boot secret and `RotateCollaborationKeys` emits `secret.rotated` for each secret rotated (no value). A mutation with no type in this table (the list is §4.12) emits no event; its audit record is its trace (CD-7).

Events written by database functions use the same envelope (`afs.event_detail`, database contract §4.2): `afs.instantiate_work_order_team` writes `team.created` (`data` `{teamId, kind: ephemeral, workOrderId}`), `pool.created` (`poolId` in the envelope, `data` `{poolId, teamId}`) and `work_order.assigned` (`data` `{workOrderId, teamId}`), and `afs.record_work_order_secret` writes `secret.created` (`data` `{secretId, name, scope: pool}`, `poolId` in the envelope). Rows written before `-db` 000019 hold the older flat payload; the reconciler's outbox publisher wraps them into the envelope (database contract §4.2), so a consumer never sees the flat shape. `PausePool` and `ResumePool` emit `pool.updated`. A future type is an additive change.

## 8. Change process

1. Propose the change in this document (or directly in `openapi.yaml` once it holds the operation) in
   a `-restapi` PR, labelled `contract`.
2. Additive changes merge with the handler. Breaking changes need a new path version and an ADR in the
   root.
3. After merge, `-website` regenerates `packages/sdk/src/generated/` and `-cli` regenerates its client,
   each in its own PR referencing the `-restapi` tag.
4. A contract test in each consumer replays the OpenAPI examples; CI fails on drift.

## 9. Changelog

- **v1.6.0 (2026-11-25), governed MCP servers ([PRD](../../mcp-gateway-PRD.md), approved by J):** §4.14: 55
  operations for the enterprise catalog, import, publish, suspend, inventory and primitive review, service and team
  policy, credentials and OAuth connect for service accounts, federation tests, team switches (full or read-only) and
  team servers with a three-level approval policy, the review queue, effective agent and pool MCP, the MCP ledger and
  usage reports, query capture windows, and agent and pool federated identities; `mcp` on agent and pool bodies;
  `mayAddMcpServers` and `mcpPlacement` on team gateway settings; ten permissions; new error reasons and `mcp_server.*`,
  `team_mcp_server.*`, `mcp_settings.changed`, `mcp_query_capture.*` and `agent.federated_identity_changed` events;
  CD-45, CD-48, CD-49. Minor (1.3.0).

- **v1.5.0 (2026-11-25), team model gateway ([PRD](../../model-endpoint-gateway-PRD.md), approved by J):** §4.13: 26
  operations for model endpoints, route models and prices, credentials, federation, statements, provider switches
  (including Bedrock off with a fallback), team gateway settings, team selections and team endpoints, and the inference
  report; route fields on agent and pool model access; four permissions; new error reasons and `model_endpoint.*` and
  `inference_provider.*` events. Minor (1.2.0).

- **v1.4.0 (2026-11-23), team orchestration agent and model access ([PRD](../../team-orchestration-agent-PRD.md) ORC-1 to ORC-32, MOD-1 to MOD-8, approved by J):** §4.4d (`GetTeamOrchestration`, `PutTeamOrchestration`, `ChangeCollaborationTool`, `ListOrchestrationEndpoints`); every team gets its orchestration agent from `CreateTeam` (`orchestration`, `maxCollaborationExposure`, `modelAccess`, `Team.orchestrationAgentId`); `CreateAgent` creates workers only (`role_reserved`, `name_reserved`), `DeleteAgent` refuses the orchestration agent (`orchestration_agent_required`), `UpdateAgent` refuses its fixed fields (`orchestration_field_fixed`); exposure levels `off`/`private`/`public` within a team limit, with `exposure_above_limit` and `surface_not_supported_by_tool` (Buzz: both surfaces N/A); the edge's private entry and wrong-level `404` (§4.4c); `Agent.models` and `Pool.models`; `ListTeamModels`/`PutTeamModels` with `access`; `ListModels` `callable` and `priceState`; `PutServiceModelAccess`; the `orchestration.*` and `models.*` settings; events `orchestration.created`, `orchestration.updated`, `orchestration.tool_changed`, `orchestration.endpoint_state_changed`, `team.models_changed`, `agent.model_unavailable`; permission scope changes in §3 (no new permission). Deprecated until 2.0.0 with `Deprecation` and `Sunset` headers: `GetTeamCollaboration`, `PutTeamCollaboration`, `ListCollaborationEndpoints`, `Team.mayExposeCollaboration`, `Agent.modelId`, `Pool.modelId`, `SwarmTemplate.spec.collaboration`, the event `collaboration.endpoint_state_changed` and `collaboration.updated`. Minor (1.1.0, OR-D10): refusing to create or delete a collaboration agent is accepted as a minor change because 1.0.1 has no production users.

Contract decisions recorded with v1.4.0 (the database contract holds CD-13 to CD-25 and the gateway contract CD-39 and CD-40):

| # | Decision |
|---|---|
| CD-26 | A 1.0.1 team the backfill has not reached answers `GetTeamOrchestration` with `404 orchestration_agent_pending` and the mutations with `409` and the same reason, so the console can show "being created" instead of an error |
| CD-27 | `PutTeamOrchestration` refuses only what the team cannot change (limit, tool support, registration mode). An unset base domain, a kill switch or a provider without a collaboration client stores the choice and shows up in `effective` and `reasons[]`. The deprecated `PutTeamCollaboration` keeps the 1.0.1 refusal for limit `off` |
| CD-28 | Each surface carries `supported`, so clients render "N/A for <tool>" from data. `provider_without_collab_client` stays a reason for the Web UI, as in 1.0.1, although the PRD's list omits it. `unmanagedPaperclipAgents` becomes `unmanagedToolAgents`; `CollaborationLink.toolAgentId` carries the Buzz public key shown on T17 |
| CD-29 | Deprecation headers: `Deprecation: @<unix time of the 1.1.0 release>`, `Sunset: <HTTP-date>` and `Link: <successor path>; rel="successor-version"`. The Sunset date is a `-restapi` Helm value (`api.deprecationSunset`), default twelve months after the 1.1.0 release, until J sets the 2.0.0 date |
| CD-30 | A team's model access changes only through `CreateTeam` and `PutTeamModels` (`UpdateTeam` does not take it). `models[]` without `modelAccess`, and `PutTeamModels` with only `modelIds`, mean `selected`, which is what a 1.0.1 client meant. `PutTeamModels` with `all` keeps the stored selection for a later switch back |
| CD-31 | Agent and pool `models`: create defaults to `all`; the default model comes from the body, then `modelId`, then `models.default_model_id`, then the team's first callable model by id. On update, sent members replace stored ones and omitted members are kept; `modelId` alone changes only the default. A model without an approved price may be selected (the gateway refuses it as `model_not_priced`) |
| CD-32 | `Agent.runtime` is `orchestration` on the orchestration agent and absent on workers; its `template` and `image` are `null` for callers who are not service admins, so no team screen or API response names the harness (ORC-6) |
| CD-33 | `ListOrchestrationEndpoints` lists one row per team and surface whose stored exposure is not `off` (a lowered surface stays visible with its reasons), never a Buzz row, and carries the backfill's `migration` progress for S12 outside the CSV |
| CD-34 | `PutTeamOrchestration` emits only `orchestration.updated`; `collaboration.updated` comes only from the deprecated `PutTeamCollaboration`, which emits both. `collaboration.endpoint_state_changed` is emitted beside its successor until 2.0.0 (ORC-28) |
| CD-35 | `PutServiceModelAccess` writes a service setting, so like `PutSetting` it emits no event (§4.12). While service access is `all`, `UpdateModel` refuses a change of `enabled` (`409 service_model_access_all`) |
| CD-36 | `CreateTeam.orchestration` takes `models` like any agent; `modelId` is shorthand for `models.defaultModelId` (the PRD names `modelId` in ORC-26 and `models` in MOD-3). Default exposure levels are lowered to the team's limit, never refused; explicit levels above it are refused |
| CD-37 | Team callers never see orchestration templates in `ListAgentTemplates`; a worker's `collaboration.connect` defaults to `true` on create because every team now has an orchestration agent (COL-10 change) |
| CD-38 | At the edge, a surface effective at the other level answers `404` on this host (wrong-level host, ORC-16); `403 surface_disabled` stays only for a surface that is `off` while the other surface is served on this host |

- **v1.3.2 (2026-11-22), build of the Paperclip collaboration change:** `PutSetting` key enum includes the seven `collab.*` keys; `afs-edge` bearer audience rules (console, CLI or collaboration client id); RCF-1 decided (`ListAdminAgentTemplates` has no `capability` filter, `ListAgentTemplates` keeps `?capability=`; the CLI and PRD drop `afs admin list-agent-templates --capability`); RCF-2 planned (`details.managedBy` on the `managed_secret` error, not yet implemented in `-restapi`).
- **v1.3.1 (2026-11-22), contract corrections (PC-D20 to PC-D31):** edge callback only on the apex, certificate and DNS cover apex and wildcard, every other apex path `404`; server-side edge sessions and single-use handoff (cookie holds an opaque id); SNI check dropped (`host_mismatch` removed); edge records go to stdout with `afs.team_id` and the node collector routes them to the team's log delivery; event `collaboration.bootstrap_completed`; `CreateTeam` `409 hostname_held`; `team_collaborator` inherits the conditional viewer grants; `RotateSecret` on an `afs` secret takes no body; priorities of `ListCollaborationLinks`, connect and disconnect (P0, C1) and `ListCollaborationEndpoints` (P0, C2); `SwarmTemplate.spec.collaboration`; `invoke-api` reads `GetTeamCollaboration` once and `restApiUrl` is `https://<hostname>/api`; 404 versus 403 `surface_disabled` stated; once-only Paperclip keys pass through unchanged. All additive or clarifying.
- **v1.3.0 (2026-11-22), Paperclip team collaboration ([PRD](../../paperclip-team-collaboration-PRD.md), PC-D11 to PC-D19):** §4.4b (seven operations: `GetTeamCollaboration`, `PutTeamCollaboration`, `ListCollaborationLinks`, `ConnectAgentToCollaboration`, `DisconnectAgentFromCollaboration`, `RotateCollaborationKeys`, `ListCollaborationEndpoints`); §4.4c the edge's externally visible behaviour; role `team_collaborator` and permissions `collaboration:read|manage|access|contribute|api`; `Agent.role` and `Agent.collaboration`; `Team.mayExposeCollaboration`; `Secret.managedBy` and its filter; `AgentTemplate.capabilities`; identity provider `collabClientId` and `collabClientSecretArn`; seven `collab.*` settings; collaboration report kind; seven events; new `details.reason` values. All additive (minor).
- **v1.2.0 (2026-11-21), `-restapi` M0 (CD-1 to CD-10):** delete status codes (`202` with body for resources that carry a status, `204` otherwise); `If-Match` optional on `PUT`; `NotificationTestResult` and event `notification.test_requested`; `RestoreResult`, `Snapshot`, `StorageClass`, `AuditEvent`, `Revision` and `CostRow` shapes; `TeamTags.requestMetadataDroppedKeys`; mutations without a §7 event type write no outbox row; identity provider client id mapping recorded as open; `403` for a signed-in caller with no binding on a route not scoped to one team; error notes: oversized body is `400`, unknown path or verb is `404` in the envelope, page tokens bound to operation and caller, CSV formula cells prefixed with `'`.
- **v1.2.0 amendment (2026-11-22), `-restapi` feature complete (CD-7, CD-9, CD-11, CD-12, CQ-4):** the CD-7 exemption list is now §4.12 (69 operations, checked by a `-restapi` test); a workload principal may omit `If-Match` on `UpdateWorkOrderRepo` (CD-12); `ExternalSystem.disabledAt` adds `pool` for pool views (CD-11); swarm template `spec.secrets` shape and how the reconciler records those secrets; `Me.servicePermissions` and `Stage.position` 1..50 in §5; `CreateImport` body `{bundle, accountMap}`; report cost measures; idempotency conditions named as `details.reason` values; database-function events share the §7 envelope. The two client ids stay `consoleClientId` and `cliClientId` on the wire (CD-9). Not yet reflected in the OpenAPI document and left to `-restapi`: `IfMatchPut` description, the `If-Match` parameter of `DeleteApiKey` and `DeleteServiceApiKey` (an API key has no `version`), duplicate path parameters on `GetTeamReport` and `GetAgentReport`, `info.version`.
- **v1.1.0:** revised after the `review-prd` pass (root PRD D23 to D57).
