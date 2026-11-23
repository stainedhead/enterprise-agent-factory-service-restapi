# Changelog

All notable changes to this repository. `-db` and `-restapi` share version numbers (lockstep).

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
