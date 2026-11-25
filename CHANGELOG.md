# Changelog

All notable changes to this repository. `-db` and `-restapi` share version numbers (lockstep).

## v1.2.0

Team model gateway, model routes and provider switches (spec `specs/261125-model-endpoint-gateway/`, change PRD
`model-endpoint-gateway-PRD.md`). Adopts `-db` migration 000023 (pinned to the pseudo-version of `-db` commit `debc7d9`
until `-db` v1.2.0 is tagged; see `user-docs/upgrading-to-1.2.0.md`). Nothing changes for a 1.1.0 agent on the shared gateway.

- API contract v1.5.0 §4.13 (27 operations; 221 with the 1.1.0 set): service model endpoints, their models, prices and
  price proposals, credentials (`{value}` in, metadata out), federations, provider statements (JSON and CSV), inference
  providers (`UpdateInferenceProvider`, Bedrock fallback guard), `PutTeamGatewaySettings`, team route selections and
  team-declared endpoints, `GetInferenceReport` and `GetTeamInferenceReport`. Agent and pool `models` gain `routeAccess`,
  `routes`, `defaultRoute`, `effectiveRoutes` and `env`; usage and cost reports group and filter by `route`; `ListModels`
  adds `providerState`. The MCP fields of contract v1.6.0 on `PutTeamGatewaySettings` are decoded and refused until 1.3.0.
- Gateway: team mode (`--team-namespace`, `AFS_GATEWAY_TEAM_ID`); route admission from `afs.route_admission` (60 s cache,
  change-feed invalidation); OpenAI pass-through for Chat Completions, Responses, Embeddings and SSE with the output cap
  written into the request and `include_usage` injected and stripped; provider cost settles to the larger;
  `/r/bedrock` Chat Completions to Converse (`unsupported_parameter`); upstream auth from mounted files and Azure and
  Google federation; `GET /v1/models` per route; GenAI telemetry without agent labels. The router is a Mux of route families.
- Reconciler: the team gateway per namespace (ServiceAccount, Deployment, Service, PodDisruptionBudget,
  CiliumNetworkPolicy, ExternalSecrets, Certificate) behind `teams.gateway_deployment`; route-host egress from the team
  gateway only; route variables, `OPENAI_BASE_URL` and `/etc/afs/llm-routes.json` for agents and pools; route defaults
  checked with `afs.route_admission`; route health (`team_route`, `team_gateway` status) with `model_endpoint.degraded`
  and `.recovered`. Settings `AFS_TEAM_GATEWAY_*`.
- Billing: jobs `route-pull` (OpenAI, OpenRouter, Azure Cost Management, Google billing export, recorded fixtures),
  `route-statements` (Nous, OpenCode), `route-capacity` (cost_daily `route_capacity` by token share) and
  `route-price-sync` (LiteLLM's list into endpoint price proposals); gateway rollup writes route calls as source `route`.
- Deploy: four billing CronJobs; the billing role reads `<prefix>/_service/billing/*`.

## v1.1.0

Team orchestration agent and model access (spec `specs/261123-orchestration-and-model-access/`, change PRD `team-orchestration-agent-PRD.md`). Adopts `-db` migration 000022 (one-way upgrade; see `user-docs/upgrading-to-1.1.0.md`):

- API contract v1.4.0 (`api/openapi.yaml`, 194 operations): `GetTeamOrchestration`, `PutTeamOrchestration`, `ChangeCollaborationTool`, `ListOrchestrationEndpoints`, `PutServiceModelAccess`; `CreateTeam` creates the orchestration agent (`orchestration`, `maxCollaborationExposure`, `modelAccess`); `Team.orchestrationAgentId`; exposure levels `off`, `private`, `public` within a team limit; `Agent.models` and `Pool.models`; `ListTeamModels`/`PutTeamModels` with `access`; `ListModels` `callable` and `priceState`; `orchestration.*` and `models.*` settings; six new events. `CreateAgent` creates workers only (`role_reserved`, `name_reserved`); `DeleteAgent` refuses the orchestration agent (`orchestration_agent_required`).
- Deprecated until 2.0.0, with `Deprecation` and `Sunset` headers (`AFS_DEPRECATION_SINCE`, `AFS_DEPRECATION_SUNSET`): `GetTeamCollaboration`, `PutTeamCollaboration`, `ListCollaborationEndpoints`, `Team.mayExposeCollaboration`, `Agent.modelId`, `Pool.modelId`, `SwarmTemplate.spec.collaboration`, the `collab.*` settings, the events `collaboration.updated` and `collaboration.endpoint_state_changed`.
- No new permission; scope changes: `collaboration:manage` also covers `PutTeamOrchestration` and `ChangeCollaborationTool`, `teams:set-guardrails` the exposure limit and team model access, `pools:write` pool models, `admin:models` `PutServiceModelAccess`, `admin:settings` `ListOrchestrationEndpoints` and the `orchestration.*` and `models.*` settings.
- `afs-api orchestration-backfill`: creates the orchestration agent of every existing team, in batches of 20, as `system:migration`; idempotent and resumable; run by a Helm post-install and post-upgrade hook.
- Reconciler: renders the orchestration agent by tool (Paperclip, Buzz), tool-neutral `AFS_COLLAB_TOOL` and `AFS_COLLAB_URL`, model delivery (`AFS_DEFAULT_MODEL`, `AFS_MODELS`, `/etc/afs/models.json`), daily Bedrock model sync (new models arrive disabled), one inference profile per callable model with a 7-day retirement grace, `ModelUnavailable` condition and `agent.model_unavailable` event.
- Gateway: model admission (service, team, agent and pool levels, and a price) with the reasons `model_not_enabled_for_service`, `model_not_enabled_for_team`, `model_not_enabled_for_agent` and `model_not_priced`; fails closed; 60 s cache refreshed on the change feed; `503 model_provisioning` with `Retry-After: 30`; metric `afs_gateway_model_denials_total`.
- Edge: private and public hostnames from `orchestration_routes()`; a surface offered at one level is a `404` at the other; access records carry `exposure`.
- Deploy: Terraform module `orchestration-private` (internal ALB, private zone, WAF); Helm `edge.privateTargetGroupArn`, `api.deprecation`, `api.orchestrationBackfill`.
- Buzz worker registration: the reconciler generates each worker's keypair and registers its public key at the relay (`AFS_BUZZ_RELAY_ADMIN_KEY_FILE`); the tool-change state machine reports `ToolChanging`, `SnapshotTaken` and `ToolReady`.
- OpenAPI follow-up (CLI team findings, same 194 operations): the `PutSetting` key enum lists the 14 new `orchestration.*` and `models.*` keys and drops the read-only aliases `collab.base_domain` and `collab.public_endpoints` (a write to either is `400 ValidationError`, `reason: setting_renamed`, `newKey`; the operation description says so); `Setting.type` gains `object` (`orchestration.tools`, `orchestration.default_resources`); the `ListAgents` `role` filter gains `orchestration`; `CreateTeamRequest.orchestration` gains `volume {sizeGiB, onDelete}`, `modelId` and the flat `webUiExposure` and `webApiExposure` of contract section 4.4d, and `afs-api` now accepts the flat levels (alongside the nested `webUi.exposure`; two different values for one level are refused); `UpdateAgentRequest.role` is documented (immutable). New guards: `TestSettingsMatchRegistry`, `TestAgentRoleFilterMatchesDomain` (`api/`) and `TestRequestBodiesMatchSpec`, `TestResponseBodiesMatchSpec` (`internal/adapter/httpapi/`), which compare the handlers' strictly decoded and emitted JSON members with the schemas.

## v1.0.1

Paperclip team collaboration (spec `specs/archive/261122-paperclip-collaboration/`, change PRD `paperclip-collaboration-PRD.md`):

- API contract v1.3.1: seven collaboration operations (`GetTeamCollaboration`, `PutTeamCollaboration`, `ListCollaborationLinks`,
  `ConnectAgentToCollaboration`, `DisconnectAgentFromCollaboration`, `RotateCollaborationKeys`, `ListCollaborationEndpoints`), 189 operations in all.
- `afs-edge` security review fixes: cross-origin writes, upgrades and streams refused (`cross_origin`); open streams re-authorized every 60 s; WebSocket scan bypasses closed (no compression, reserved bits and opcodes, binary and fragmented messages); body scan decodes JSON escapes, forms, multipart parts and UTF-16, and refuses unreadable media types (415); upstream WebSocket handshake validated; bearer audience follows `afs-api` and accepts the team IdP's console, CLI and collaboration client ids (RD-27, resolved by `-db` 000021); bounded caches and a per-source limit on unknown credentials; sign-in bound to the starting browser; redirect, `Set-Cookie`, method-override and Host hardening; group-only sessions re-authenticate after 60 minutes.
- `PutSetting` key list includes the seven `collab.*` settings.
- Role `team_collaborator` and permissions `collaboration:read|manage|access|contribute|api`.
- Collaboration agent (`role: collaboration`) with AFS-generated boot secrets; managed secrets (`managedBy`, `managed_secret`, reserved names).
- `AgentTemplate.capabilities`, `Team.mayExposeCollaboration`, identity provider `collabClientId` and `collabClientSecretArn`.
- Reconciler: Paperclip client, managed registration (agent create, key to Secrets Manager, pause, revoke), roster guard, readiness probe with open sign-up, agent-write scan, first-admin invite by email, member sync, `CollaborationKeyMissing`.
- Reports: `collaboration` kind (sign-ins, people, requests, denied requests, secret detections). Export/import carries collaboration settings and connect choices, never Paperclip data or managed keys.
- Adopts `-db` migrations 000001-000021 (000021: `collab_routes()` returns the provider's console and CLI client ids; pseudo-version for now; move to `-db` v1.0.1 once tagged).

## v1.0.0

Feature complete: `afs-api`, `afs-reconciler`, `afs-gateway` and `afs-billing` on `-db` migrations 000001 to 000019.
