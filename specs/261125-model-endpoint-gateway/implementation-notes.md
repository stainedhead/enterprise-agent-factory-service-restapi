# Implementation notes

## Contract notes

- **GetInferenceReport has two paths.** API contract §4.13 lists one row, `GetInferenceReport`, with a service path and a
  team path and two CLI commands. OpenAPI needs one operationId per path, so the team form is
  `GetTeamInferenceReport` (`GET /v1/teams/{teamId}/reports/inference`, `reports:read`, team scope), named after its CLI
  command `reports get-team-inference-report`; both keep the SDK method `reports.inference()`. The contract's
  "26 operations" are 27 OpenAPI operations (221 in total). `contractgen` derives the split from the row.
- **Scopes in §4.13.** The table has no `*svc*` markers. `/v1/admin/...` paths and `admin:*` permissions are service
  routes; `PutTeamGatewaySettings` (`/v1/admin/teams/{teamId}/gateway-settings`, `teams:set-guardrails`) is a team route,
  like `PutTeamModels`, so an unbound caller gets 404 and a team-admin 403. `PutEndpointFederation` names a team in its
  path but is a service route (`admin:model-endpoints`).
- **Permissions follow the contract, not PRD D.3.4,** where they differ: `ListEndpointPriceProposals` and
  `ImportProviderStatement` need `admin:pricing` (the PRD table says `admin:model-endpoints`).
- **Events.** The v1.5.0 event types are in a §4.13 paragraph, not the §7 table; `contractgen` reads that paragraph.
  Event per operation (`x-afs-events`): endpoint price and model changes, federation and statement imports emit
  `model_endpoint.updated`; proposal decisions keep `pricing.proposal_decided`; `PutTeamGatewaySettings` emits
  `team.updated`; credentials emit `model_endpoint.credential_set` (no value); `UpdateInferenceProvider` emits
  `inference_provider.enabled`/`.disabled` and, with `selectForAllTeams`, `model_endpoint.selected` per team.
- **`PutAgentModels` and `PutPoolModels` do not exist.** Agent and pool model access is the `models` object of
  `CreateAgent`, `UpdateAgent`, `CreatePool`, `UpdatePool` (and `CreateTeam.orchestration.models`). The route members
  `routeAccess`, `routes`, `defaultRoute` go there (`AgentModelsInput`), and `AgentModels` adds `effectiveRoutes` and
  `env`. A route default model is `defaultModelId = <route>/<model>` or `defaultRoute` plus the model name.
- **Usage and cost reports.** `groupBy=route` is in the shared `GroupBy` pattern and `route` is a new filter
  (`RouteFilter`) on `GetTeamReport`, `GetAgentReport` and `GetServiceReport`; it applies to the usage and costs kinds.
- **Team fields.** `Team` gains read-only `gatewayDeployment` and `mayAddModelEndpoints`; they change only through
  `PutTeamGatewaySettings`.
- **Rotate takes a value.** `RotateModelEndpointCredential` takes `{value}` like `PutModelEndpointCredential` (an
  upstream key is issued by the provider, so AFS cannot generate it).
- **Request schemas are open** (no `additionalProperties: false`), as in the rest of the API, so the MCP 1.3.0 change
  can add `mayAddMcpServers` and `mcpPlacement` to `PutTeamGatewaySettingsRequest`.

## PR 1 interim (removed by PR 2)

- `httpapi.PendingModelEndpointOps` binds the 27 operations to the `501 not_implemented` answer so `afs-api` starts
  (it refuses unbound operations) and the authorization matrix skips them (`pendingV150`, which may only shrink).
- The agent and pool `models` route members are decoded and refused with `models.routeAccess: not_implemented`,
  never ignored.
