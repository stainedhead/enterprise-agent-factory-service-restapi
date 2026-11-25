# Model routes and the team gateway (1.2.0)

Teams can reach OpenAI-compatible model endpoints (OpenAI, OpenRouter, Nous Portal, OpenCode, Azure, Google Vertex AI,
corporate and in-namespace servers) through a metered, capped gateway in their own namespace. Bedrock keeps working as in
1.1.0. Every provider, Bedrock included, is a service-admin switch.

## Turning it on for a team

1. A service admin creates the route (`CreateModelEndpoint`, `admin:model-endpoints`), lists its models and, for
   `per_token` routes, prices them (`CreateEndpointModelPrice`). A route starts disabled.
2. The route's credential is written once (`PutModelEndpointCredential`, body `{"value": "..."}`); AFS stores only the
   Secrets Manager ARN. Azure and Google routes use federation instead (`PutEndpointFederation`, metadata only).
3. The team gets its own gateway: `PUT /v1/admin/teams/{teamId}/gateway-settings` with `{"deployment": "per_namespace"}`
   (`teams:set-guardrails`). The reconciler then runs `afs-gateway` in the team's namespace.
4. The team selects service routes (`PutTeamModelEndpoints`) or declares its own (when
   `mayAddModelEndpoints` is true).
5. Agents and pools take routes in their `models` object: `routeAccess` (`all` or `selected`), `routes`, and
   `defaultRoute` or `defaultModelId: "<route>/<model>"`.

Routes are served only by team gateways. A team on the shared gateway keeps Bedrock and gets no route variables.

## What an agent receives

| Variable or file | Value |
|---|---|
| `AFS_GATEWAY_URL` | The team gateway (`https://afs-gateway.<namespace>.svc:8443`) |
| `AFS_LLM_ROUTES` | Comma-separated route slugs, the default first (Bedrock is not listed) |
| `AFS_LLM_<ROUTE>_BASE_URL` | `$(AFS_GATEWAY_URL)/r/<route>/v1`; `AFS_LLM_BEDROCK_BASE_URL` while Bedrock is on |
| `OPENAI_BASE_URL`, `OPENAI_API_KEY` | `$(AFS_GATEWAY_URL)/openai/v1` (send `"model": "<route>/<model>"`); a placeholder key (the gateway authenticates the pod, never the key) |
| `AFS_LLM_DEFAULT_ROUTE`, `AFS_LLM_DEFAULT_MODEL` | Set when the default model is a route model |
| `/etc/afs/llm-routes.json` | Version 1: every route with `baseUrl`, `protocols`, `models` (`name`, `maxOutputTokens`) and `pricing` |

Calls: `POST $(AFS_GATEWAY_URL)/r/<route>/v1/chat/completions` (also `/responses`, `/embeddings` where the route lists the
protocol), or `/openai/v1/...` with `"model": "<route>/<model>"`. `GET .../v1/models` lists the agent's models on the
route. `/r/bedrock/v1/chat/completions` is translated to Bedrock Converse.

## Route health

`ListModelEndpoints` and `ListTeamModelEndpoints` show each route's health for the team: `Ready`, `Degraded` or
`Unavailable`, with a reason: `team_gateway_not_deployed`, `credential_missing`, `federation_not_ready` or
`federation_pending`. A change publishes `model_endpoint.degraded` or `model_endpoint.recovered` once.

## Provider switches

`UpdateInferenceProvider` turns a provider off or on. Off removes its hosts from every team gateway's egress, and for
Bedrock removes `bedrock:Invoke*` from the team roles; agents whose default was on that provider move to the
service's fallback model. `ListModels` shows `providerState` and `callable: false` while Bedrock is off.

## Reports

Usage and cost reports group by `route` (`groupBy=route`) and filter on it (`route=<slug>` or `route=bedrock`).
`GetInferenceReport` (service) and `GetTeamInferenceReport` give per-route calls, errors, tokens, cost and latency
percentiles, in JSON or CSV. See [Reports and usage](reports.md).

## Troubleshooting

| Symptom | Check |
|---|---|
| Agent has no `AFS_LLM_*` variables | The team is `shared`; set `per_namespace` |
| Route `Unavailable`, `credential_missing` | Write the route credential (per team for `per_team` routes) |
| Route `Unavailable`, `federation_not_ready` | `PutEndpointFederation` status and the Azure or Google trust for the team's ServiceAccount |
| Reason `provider_disabled` | The provider is switched off |
| Reason `model_not_on_route` | The model is not listed on the route, or the agent cannot use the route |
