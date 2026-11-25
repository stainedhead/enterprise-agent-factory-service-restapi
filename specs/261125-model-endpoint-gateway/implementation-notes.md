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

## PR 2 decisions (1.2.0)

- **Route variables reference the gateway URL.** `AFS_LLM_<ROUTE>_BASE_URL`, `OPENAI_BASE_URL` and the rest are rendered
  as `$(AFS_GATEWAY_URL)/r/<route>/v1` after `AFS_GATEWAY_URL`, so a gateway move changes one variable. The file
  `/etc/afs/llm-routes.json` cannot expand variables and carries the resolved URL.
- **Delivered only on a team gateway.** Route variables, `llm-routes.json` and the API's `AgentModels.env` are produced
  only for `per_namespace` teams (routes are served only by team gateways, gateway contract §12.1). The pod-template hash
  of a shared-gateway agent with no route default is unchanged from 1.1.0.
- **`AFS_LLM_ROUTES` excludes `bedrock`.** Bedrock keeps its 1.1.0 variables (`AFS_DEFAULT_MODEL`, `AFS_MODELS`);
  `/r/bedrock` serves Chat Completions only (Responses and Embeddings are 404 in 1.2.0).
- **Inline prices are refused on service endpoints** in create and update bodies; service endpoint prices go through
  `CreateEndpointModelPrice` (append-only, effective-dated). Team endpoints may carry them inline.
- **Credential bodies.** `PutModelEndpointCredential` and `RotateModelEndpointCredential` take `{value}`; the response
  is metadata (`secretArn`, `status`, `rotatedAt`, `version`); the value is written to Secrets Manager only.
- **Route health in `resource_status`.** Kinds `team_route` (id `<teamId>/<endpointId>`, condition `RouteHealth`) and
  `team_gateway` (id `<teamId>`); reasons `Ready`, `team_gateway_not_deployed`, `credential_missing`,
  `federation_not_ready`, `federation_pending`. `model_endpoint.degraded` and `.recovered` are published once per
  transition (aggregate `model_endpoints`); an unchanged health keeps its transition time.
- **One federation token per gateway pod.** The projected token's audience is `api://AzureADTokenExchange` when the team
  has an Azure route, else the first Google provider audience. A team with both must allow the Azure audience on its
  Google workload identity provider.
- **Team gateway credentials.** ExternalSecret `afs-gateway-credentials` maps each route's ARN to a key named after the
  route slug (mounted at `/etc/afs/gateway-credentials`); `afs-gateway-db` carries the gateway's database URL. Nothing is
  mounted into agent pods.
- **MCP fields on `PutTeamGatewaySettings`** (v1.6.0, merged from main): decoded so the strict decoder accepts them;
  `mayAddMcpServers: false` is accepted, `true` or any `mcpPlacement` is a `400 ValidationError` until release 1.3.0
  stores them (`-db` 000024). They are never silently dropped.
- **Provider attribution convention.** A provider figure belongs to a team when the provider side names the team: an
  OpenAI project or OpenRouter key whose name contains the team id, an Azure `afs-team-id` tag or a resource group whose
  name contains it, a Google `afs-team-id` resource or project label. Anything else is reported as unattributed per
  route, never guessed.
- **Route variance events have no table.** `cost.variance_detected` for a route uses aggregate `route_reconciliation`,
  id `<endpointId>/<teamId>/<period>`; a rerun of the same day or month publishes again (the 1.1.0 Bedrock
  reconciliation keeps its per-transition rule through `cost_reconciliations`).
- **Capacity rows carry cost only.** `route_capacity` rows of `cost_daily` have zero tokens: the route's tokens are in
  its `route` rows, and counting them twice would double the reports' token totals.
- **OpenRouter is pulled for the current UTC day only** (`usage_daily`); `route-pull` runs at 23:30 UTC.
- **Billing keys** are read by name from `<prefix>/_service/billing/<route>` (`secretsmanager.Reader.ReadBillingKey`,
  which refuses any other name); the billing role may `GetSecretValue` on that path only (Terraform `pod-identity`).

## Deferred (need cloud access)

- Pod Identity association for the team gateway ServiceAccount (`bedrock:Invoke*` on the team's profiles while Bedrock
  is on) is made by Terraform per team; not rendered by the reconciler.
- Live Azure and Google token acquisition for `afs-billing` (the pulls take a bearer token from the billing key path)
  and live federation for the gateway against real tenants.
- Recording the Azure Cost Management and BigQuery fixtures from live tenants (MEG-OQ-14); today's fixtures follow the
  published references.
- envtest runs of the reconciler (assets not available here; envtest-tagged tests compile and lint).
- OpenRouter historical days (no provider API).
- `endpoint_credentials.status` written by the reconciler from the ExternalSecret's sync state.

## PR 1 interim (removed by PR 2)

- `httpapi.PendingModelEndpointOps` binds the 27 operations to the `501 not_implemented` answer so `afs-api` starts
  (it refuses unbound operations) and the authorization matrix skips them (`pendingV150`, which may only shrink).
- The agent and pool `models` route members are decoded and refused with `models.routeAccess: not_implemented`,
  never ignored.

## Root contract pin

CI checks the root repository out at the commit in `api/contract/ROOT_REF` (root PR #28 merge
`ef3af265`, API contract v1.5.0) instead of root `main`, which moved on to v1.6.0 (the MCP release). A later
release moves the pin together with `contractVersion` and the extracted contract.
