# API contract: `-restapi` ↔ `-cli`, `@afs/sdk` and the console

| | |
|---|---|
| **Status** | Contract v1.2.0, draft for implementation (2026-11-21); v1.2.0 records the decisions of `-restapi` milestone M0 (CD-1 to CD-10, see §9); v1.1.0 was revised after the `review-prd` pass (root PRD D23 to D57) |
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
| Concurrency | Every mutable resource has `version` (integer) and is returned with `ETag: "<version>"`. `PATCH` and `DELETE` on an existing resource require `If-Match` (absent → `428`); on `PUT` it is optional, because `PUT` is an upsert: with it, a mismatch → `412 PreconditionFailed`; without it, the write applies to the current version (CD-2) |
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
the team has none). Team API keys carry one team and one role (`team_admin` or `team_viewer`). Service-level API keys
carry no team and only the `security_operator` role. Security operators (people bound to the role, or
those keys) can use only the work order operations, `ListSwarmTemplates`, `GetSwarmTemplate`,
`ListTeamDirectory` and `GetMe`; every team-scoped route (`/v1/teams/{teamId}/…`), including
`ListTeamWorkOrders`, returns `403 AccessDenied` to them, and they list one team's work orders with
`ListWorkOrders?teamId=`. `afs-api` sets `afs.security_operator` for them so the database lets them
read and write work orders across teams.

A **workload principal** (`type: workload`, `id` = the agent or pool id, team from the token's namespace)
holds only `work-orders:report` on work orders assigned to its own team, and may call `GetMe`. Everything
else returns `403`. Its actions are audited with the agent or pool id and, for pools, the worker pod name.

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
the provider (CD-9).

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

**Security operator** (service-level role, D18): only the permissions in its column. It cannot be
granted by team-admins and never appears in a team binding. **Workload principal**: `work-orders:report`
on its own team's assigned work orders, nothing else.

Service admins hold every permission on every team. `GET /v1/me` returns, per bound team, the
effective permission list; for service admins it returns `"serviceAdmin": true` and the full list;
for security operators `"securityOperator": true` with their column's permissions.

## 4. Operations

Columns: **Op** = OpenAPI `operationId`; **Perm** = permission (team-scoped unless marked *svc*);
**CLI** = `afs <service> <operation>`; **SDK** = `@afs/sdk` method; **Screen** = console screen in the
root PRD §5.3. Every mutation writes the row and an audit record in one transaction, and, when it has an event type in §7, an outbox event in the same transaction. Mutations with no §7 event type (members, API keys, settings, identities, config values and the like) write no outbox row (CD-7).
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

`CreateTeam` body includes the first team-admin binding, the team's models, required tag values and,
when more than one provider exists, `identityProviderId`. A team missing a required tag is refused
with `ValidationError` naming the keys. An ephemeral team (`kind: ephemeral`) also needs `expiresAt`
(at most `teams.ephemeral_max_days` ahead), `teardownRule` (`at_expiry` \| `at_cap_or_expiry` \|
`at_work_complete`) and a lifetime `budget` in the same request; `kind` cannot change later.
`isolationTier`, `viewerKubectl`, `maxBudgetUsd`, `quotas`, `ceilingOverrideUsd` and
`ceilingOverrideExpiresAt` are guardrail fields (`teams:set-guardrails`). An override needs both fields
and a `ceilingOverrideReason` (write-only, stored in the audit record, not on the team), an expiry in the future and an amount above `maxBudgetUsd`; while it is unexpired it is
the effective ceiling. At expiry the reconciler clears it, lowers a budget above `maxBudgetUsd` to it and
emits `budget.ceiling_override_expired` (D34). `maxBudgetUsd` below the current budget amount →
`ValidationError`. A change to `kind` → `ValidationError`. `SuspendTeam` and `DeleteTeam` act as the
root PRD's TEAM-9 table says; on a suspended team every mutation except `ResumeTeam` and `DeleteTeam`
→ `409 Conflict` (`details.reason = team_suspended`).

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

### 4.4a Swarms: stages and pools

Ephemeral teams only (`409 Conflict` on a persistent team). `afs-api` checks the team's `kind` before the write and returns `409`, because the database's guard would otherwise raise `23514` (`ValidationError`). Pool names share the team's agent names.

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
| `GetTeamReport` | `GET /v1/teams/{teamId}/reports/{usage\|costs\|activity\|traffic}` *CSV* | `reports:read` | `reports get-team-report` | `reports.team()` | T11 | P0 |
| `GetAgentReport` | `GET /v1/teams/{teamId}/agents/{agentId}/reports/{kind}` *CSV* | `reports:read` | `reports get-agent-report` | `reports.agent()` | T12 | P0 |
| `GetServiceReport` | `GET /v1/reports/{kind}` *CSV* | `admin:reports` *svc* | `reports get-service-report` | `reports.service()` | S8, S9 | P0 |
| `ListReportSchedules` | `GET /v1/teams/{teamId}/report-schedules` | `reports:read` | `reports list-report-schedules` | `reports.schedules.list()` | T11 | P1 |
| `CreateReportSchedule` | `POST /v1/teams/{teamId}/report-schedules` | `reports:schedule` | `reports create-report-schedule` | `reports.schedules.create()` | T11 | P1 |
| `UpdateReportSchedule` | `PATCH /v1/teams/{teamId}/report-schedules/{scheduleId}` | `reports:schedule` | `reports update-report-schedule` | `reports.schedules.update()` | T11 | P1 |
| `DeleteReportSchedule` | `DELETE /v1/teams/{teamId}/report-schedules/{scheduleId}` | `reports:schedule` | `reports delete-report-schedule` | `reports.schedules.delete()` | T11 | P1 |
| `ListCostTagStatus` | `GET /v1/admin/cost-tag-status` (per key: seen in CUR, first and last seen, AWS activation status) | `admin:cost-tag-status` *svc* | `admin list-cost-tag-status` | `admin.costTagStatus.list()` | S9 | P2 |

The S9 activation banner (TAG-7) is acknowledged with `PutSetting` on key
`billing.tag_activation_acknowledged` = `true`; there is no separate acknowledge operation.

A report schedule has `name`, `report` (`usage` \| `costs` \| `activity` \| `traffic`), `query` (the
report query parameters below, stored), `cadence` (`daily` \| `weekly` \| `monthly`), `channelId` (one of
**the team's own** notification channels; a service-level channel → `ValidationError`), `format` (`csv`),
`enabled`, and read-only `nextRunAt`, `lastRunAt`, `lastStatus`, `lastError`.

Report query parameters: `from`, `to` (or `range=24h|7d|30d|90d|mtd`), `groupBy`
(`team|agent|model|system|day|hour|tag:<key>`), filters `teamId`, `agentId`, `modelId`, `systemId`,
`tag.<key>=<value>`, and in ephemeral teams `poolId` and `groupBy=pool`. Grouping by tag uses the tags in
effect when each record happened (TAG-6); a record missing a currently required key reports
`"(untagged)"` for that key. Traffic reports carry counts and bytes, never a cost. Response: `{"range": {...}, "groupBy": [...], "rows": [...], "totals": {...},
"notes": ["Spend with providers other than Bedrock is not included"]}`.

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
| `UpdateWorkOrderRepo` | `PATCH /v1/work-orders/{workOrderId}/repos/{host}/{owner}/{repo}` `{status, prUrl?, prNumber?, detail?}` | `work-orders:report` (assigned team) or `work-orders:write` | `work-orders update-repo` | `workOrders.repos.update()` | P6 | P2 |
| `ListTeamWorkOrders` | `GET /v1/teams/{teamId}/work-orders` (team members; security operators get `403` and use `ListWorkOrders?teamId=`) | `work-orders:read` | `work-orders list-team-work-orders` | `workOrders.listForTeam()` | T1 | P2 |
| `ListSwarmTemplates` | `GET /v1/admin/swarm-templates` | `swarm-templates:read` *svc* | `swarm-templates list-swarm-templates` | `swarmTemplates.list()` | P5, P3 | P2 |
| `CreateSwarmTemplate` | `POST /v1/admin/swarm-templates` | `admin:swarm-templates` *svc* | `swarm-templates create-swarm-template` | `swarmTemplates.create()` | P5 | P2 |
| `GetSwarmTemplate` | `GET /v1/admin/swarm-templates/{templateId}` | `swarm-templates:read` *svc* | `swarm-templates describe-swarm-template` | `swarmTemplates.get()` | P5, P3 | P2 |
| `UpdateSwarmTemplate` | `PATCH /v1/admin/swarm-templates/{templateId}` | `admin:swarm-templates` *svc* | `swarm-templates update-swarm-template` | `swarmTemplates.update()` | P5 | P2 |
| `DeleteSwarmTemplate` | `DELETE /v1/admin/swarm-templates/{templateId}` (refused with `409` while an open work order uses it) | `admin:swarm-templates` *svc* | `swarm-templates delete-swarm-template` | `swarmTemplates.delete()` | P5 | P2 |
| `EnableSwarmTemplate` | `POST /v1/admin/swarm-templates/{templateId}:enable` | `admin:swarm-templates` *svc* | `swarm-templates enable-swarm-template` | `swarmTemplates.enable()` | P5 | P2 |
| `DisableSwarmTemplate` | `POST /v1/admin/swarm-templates/{templateId}:disable` | `admin:swarm-templates` *svc* | `swarm-templates disable-swarm-template` | `swarmTemplates.disable()` | P5 | P2 |

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
| `GetSettings` | `GET /v1/admin/settings` (every key of root PRD §6.13 with value, default and range) | `admin:settings` | `admin get-settings` | `admin.settings.get()` | S10 | P0 |
| `PutSetting` | `PUT /v1/admin/settings/{key}` `{value}` | `admin:settings` | `admin put-setting` | `admin.settings.put()` | S10, S9 | P0 |
| `ListIdentityProviders` | `GET /v1/admin/identity-providers` | `admin:identity-providers` | `admin list-identity-providers` | `admin.identityProviders.list()` | S10 | P0 |
| `CreateIdentityProvider` | `POST /v1/admin/identity-providers` | `admin:identity-providers` | `admin create-identity-provider` | `admin.identityProviders.create()` | S10 | P0 |
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
| `ListModels` | `GET /v1/models` (not *svc*: enabled models for any signed-in caller) | `models:read` | `models list-models` | `models.list()` | T3 | P0 |
| `ListAdminModels` | `GET /v1/admin/models` | `admin:models` | `admin list-models` | `admin.models.list()` | S4 | P0 |
| `CreateModel` | `POST /v1/admin/models` (`maxOutputTokens` required, D28) | `admin:models` | `admin create-model` | `admin.models.create()` | S4 | P0 |
| `UpdateModel` | `PATCH /v1/admin/models/{modelId}` | `admin:models` | `admin update-model` | `admin.models.update()` | S4 | P0 |
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

Export and import (D49): `CreateExport` records the request; the reconciler builds the bundle, writes it
to S3, and stores the checksum and a pre-signed download URL (valid 7 days) on the export row; after that a new
export is needed. `afs-api` never reads or writes S3. `CreateImport`
takes the bundle itself as the request body, verifies its checksum and `schemaVersion`, and applies it
as ordinary desired-state writes, one transaction per team (each with its audit and outbox records),
returning a report `{dryRun, teams[{slug, result: created|updated|skipped|failed, changes[], error?}]}`.
With `dryRun=true` (the default) nothing is written. A bundle over 100 MB → `400 ValidationError`.

## 5. Resource shapes (wire, abbreviated)

Fields marked † are read-only. All resources also carry `id`†, `version`†, `createdAt`†, `createdBy`†,
`updatedAt`† and, for desired state, `generation`† and `status`†.

```yaml
Team:        {slug, displayName, description, kind: persistent|ephemeral, state†: active|suspended|deleting|deleted,
              namespace†, costCenter, ownerEmail, maxBudgetUsd, ceilingOverrideUsd?, ceilingOverrideExpiresAt?,
              effectiveCeilingUsd†, quotas{cpu,memory,pods,storage},
              isolationTier: shared|dedicated, viewerKubectl, expiresAt?, teardownRule?: at_expiry|at_cap_or_expiry|at_work_complete,
              tornDownAt†?, teardownReason†?, identityProviderId?, allowedAwsAccounts[], mayManageAwsAccounts,
              mayAddExternalSystems}
TeamTags:    {tags: [{key, value?, source†: system|service|team|service_default|missing}],
              requestMetadataDroppedKeys†[]}       # PUT sends team-owned keys only; the read-only list names tag keys left out of the Bedrock requestMetadata because of its pair limit (D27)
Member:      {role: team_admin|team_viewer, subjectType: user|group, userId?, email†?, groupName?}
ApiKey:      {name, role, keyPrefix†, expiresAt (≤ 90 days), lastUsedAt†, secretKey† (create response only)}
Agent:       {name, description, template, image, modelId, resources{cpu,memory}, iamIdentity, iamRoleArn†,
              volume{sizeGiB, storageClass, onDelete: snapshot_then_delete|delete|retain, snapshotSchedule: off|daily|weekly, snapshotRetain},
              secretBindings[{secretId, delivery: env|file, target}], paused†}
RoleLink:    {profileName, roleArn, accountId†, maxSessionSeconds, externalId†, status†: pending_trust|verified|failed|disabled,
              lastVerifiedAt†, trustPolicy†}
Stage:       {name, position, description}
Pool:        {name, description, stageId?, template, image, persona?, modelId, workers (0..200), resources{cpu,memory},
              scratchSizeGiB (1..500), sharedVolume: none|efs, state†: enabled|paused|deleting|deleted, handoffs†[poolId]}
PoolRoleLink: {roleName, accountId, profileName, roleArn†, externalId†, maxSessionSeconds, status†, lastVerifiedAt†}
SwarmTemplate: {name, description, kind: cve_remediation|general, spec{stages[], pools[], handoffs[], secrets[], configValues[]},
              budgetUsd, ttlHours, teardownRule, enabled}
WorkOrder:   {kind: cve_remediation|general, cveId?, severity, title, source†: api|eventbridge, templateId, budgetUsd, slaDueAt,
              status†: open|assigned|in_progress|pr_opened|done|failed|cancelled, teamId†?, assignedAt†?, completedAt†?,
              slaState†: on_track|due_soon|breached|met|missed, progress†{repos, prsOpen, prsMerged, notAffected, failedOrSkipped, remaining}}
WorkOrderRepo: {host, repo, owningTeamId?, owningContact?, ownerSource†? (CreateWorkOrder response only): explicit|team_config, status: pending|in_progress|pr_opened|merged|not_affected|failed|skipped,
              prUrl?, prNumber?, detail?, mergedAt†?, notifications†[{kind, recipient, sentAt}]}
ReportSchedule: {name, report, query{}, cadence, channelId, format: csv, enabled, nextRunAt†, lastRunAt†, lastStatus†, lastError†}
CostTagStatus: {key, seenInCur, firstSeenAt, lastSeenAt, awsStatus: active|inactive|unknown, checkedAt}
Secret:      {scope: team|agent|pool, agentId?, poolId?, name, description, externalSystemId?, secretsManagerPath†, secretsManagerArn†,
              versionId†, lastChangedAt†, rotation{enabled, days}, boundTo†[agentId]}               # no value field, ever
ConfigValue: {name, value?, source: literal|namespace, resolvedValue†, usedBy†[agentId]}
ExternalSystem: {slug, name, category, description, docsUrl, endpoints[], enabled, builtin†, owner?, purpose?,
              scope†: service|team, teamState†: on|off, disabledAt†: service|team|agent|null, unmetered†}
Budget:      {period: monthly|lifetime, amountUsd, thresholds[], onExhaust: block|notify (notify: persistent teams, service admins only), expiresAt?,
              spentUsd†, reservedUsd†, percentUsed†, forecastUsd†, projectedCapAt†}
ServiceTag:  {key, mode: fixed|required, value?, defaultValue?, allowedValues?[], description}
IdentityProvider: {kind: okta|entra_id, name, issuerUrl, consoleClientId, cliClientId, clientSecretArn?, groupsClaim, isDefault, enabled}
Model:       {id, provider, displayName, enabled, contextWindow, maxOutputTokens}
AgentTemplate: {name, description, harness, defaultResources{cpu,memory}, allowedDigests[], currentDigest, enabled}
Export:      {status†: pending|running|succeeded|failed, schemaVersion†, sha256†, sizeBytes†, downloadUrl†?, finishedAt†?}
TeamDirectoryEntry: {id, slug, displayName}
PriceProposal: {modelId, effectiveFrom, inputUsdPerMtok, outputUsdPerMtok, cacheReadUsdPerMtok, cacheWriteUsdPerMtok,
              status†: pending|approved|rejected|superseded, fetchedAt†, decidedBy†, decidedAt†, note}
NotificationTestResult: {testId, delivered?, pending?, error?: string|null}     # 200: delivered true|false (+ error); 202: pending true
RestoreResult: {dryRun, revision, changes[RestoreChange], skipped?[RestoreChange]}         # skipped: bindings or references that no longer resolve
RestoreChange: {resourceKind, resourceId, action: create|update|delete|skip, fields?[{field, ...}], reason?}
Snapshot:    {id†, agentId†, reason†: scheduled|manual|pre_delete|pre_change, state†: requested|pending|completed|failed|deleted,
              sizeGiB†?, requestedAt†, requestedBy†?, completedAt†?}
StorageClass: {name, description?, volumeType: gp3|io2, enabled, version†, createdAt†, updatedAt†}
AuditEvent:  {id (integer), occurredAt, actorType: user|api_key|system|agent|pool, actorId, action, outcome: allowed|denied|failed,
              resourceKind?, resourceId?, teamId?, requestId?, sourceIp?, userAgent?, workerPod?, before?{}, after?{}}   # no secret values
Revision:    {revision (integer), resourceKind, resourceId, operation: INSERT|UPDATE|DELETE, version?, changedBy, changedAt, snapshot?{}}
CostRow:     {day, source: gateway|cur_bedrock|cur_eks|cur_other, agentId?, poolId?, modelId?, costUsd, calls?}
Me:          {principal{id, type: user|api_key|workload, email?, name?}, serviceAdmin, securityOperator, teams[{teamId, slug, role, permissions[]}]}
```

## 6. Errors

Body: `{"code": "<Code>", "message": "<text>", "requestId": "<id>", "details": {}}`. `message` is for people and may
change; clients branch on `code` only. No error contains a secret value, token or API key.

| HTTP | `code` | When | Database origin (see [database contract](database-contract.md) §6) |
|---|---|---|---|
| 400 | `ValidationError` | Malformed or out-of-range input, a body over the size limit (`field: body`; there is no `413`), a page token issued to another caller or for another operation; `details.fields[]` lists `{field, reason}` | `23514`, `23502`, `22xxx` |
| 401 | `Unauthenticated` | Missing, expired or unknown credential | — |
| 403 | `AccessDenied` | Valid caller without the permission, including a signed-in caller with no team binding on a route not scoped to one team (CD-10); `details.requiredPermission` | `42501` |
| 404 | `NotFound` | No such resource, **or** a resource in a team the caller cannot see (never reveals existence), **or** an unknown path or `:verb` (error envelope, not a bare 404) | RLS hides the row |
| 402 | `BudgetExceeded` | Returned by `afs-gateway` to agents only, never by `afs-api`; not retryable (see [gateway contract](gateway-contract.md)) | `reserve_spend_held` returned NULL |
| 409 | `Conflict` | Duplicate name, idempotency body mismatch or in progress, dependent objects exist, invalid state transition (for example a suspended team) | `23505`, `23503` (on delete), `55000` |
| 412 | `PreconditionFailed` | `If-Match` does not match `version` | — |
| 428 | `PreconditionRequired` | `If-Match` missing on `PATCH` or `DELETE` of an existing resource (optional on `PUT`) | — |
| 429 | `Throttled` | Rate limit; `Retry-After` | — |
| 500 | `InternalError` | Anything else; logged with `requestId` | `40001` after retries, others |
| 502 | `UpstreamError` | Returned by `afs-gateway` only, when Bedrock returns a server error; the reservation is released; retry with backoff | — |
| 503 | `Unavailable` | Dependency down (database, Secrets Manager); safe to retry with backoff | — |

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
| `notification.delivery_failed` | reconciler | A channel's EventBridge target failed (D46) |
| `notification.test_requested` | api | `TestNotificationChannel`; `data` has `channelId` and `testId`. The reconciler tests the channel and reports the result on the channel's status (condition `DeliveryHealthy`, `lastTestId`), which the API returns (CD-3) |

A mutation with no type in this table (member and API key changes, settings, identities, config values, role links and so on) emits no event; its audit record is its trace (CD-7). A future type is an additive change.

## 8. Change process

1. Propose the change in this document (or directly in `openapi.yaml` once it holds the operation) in
   a `-restapi` PR, labelled `contract`.
2. Additive changes merge with the handler. Breaking changes need a new path version and an ADR in the
   root.
3. After merge, `-website` regenerates `packages/sdk/src/generated/` and `-cli` regenerates its client,
   each in its own PR referencing the `-restapi` tag.
4. A contract test in each consumer replays the OpenAPI examples; CI fails on drift.

## 9. Changelog

- **v1.2.0 (2026-11-21), `-restapi` M0 (CD-1 to CD-10):** delete status codes (`202` with body for resources that carry a status, `204` otherwise); `If-Match` optional on `PUT`; `NotificationTestResult` and event `notification.test_requested`; `RestoreResult`, `Snapshot`, `StorageClass`, `AuditEvent`, `Revision` and `CostRow` shapes; `TeamTags.requestMetadataDroppedKeys`; mutations without a §7 event type write no outbox row; identity provider client id mapping recorded as open; `403` for a signed-in caller with no binding on a route not scoped to one team; error notes: oversized body is `400`, unknown path or verb is `404` in the envelope, page tokens bound to operation and caller, CSV formula cells prefixed with `'`.
- **v1.1.0:** revised after the `review-prd` pass (root PRD D23 to D57).
