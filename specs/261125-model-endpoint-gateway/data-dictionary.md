# Data dictionary: team model gateway (1.2.0)

Schema: `-db` migration 000023 (database contract §7.4). This repository adds no table; it reads and writes these.

## Tables and columns used

| Table | Used by | How |
|---|---|---|
| `inference_providers` | api, gateway, reconciler | provider switches; `afs.provider_enabled(p)` |
| `model_endpoints`, `endpoint_models`, `endpoint_model_prices` | api (write), gateway, reconciler, billing (read) | routes, their models, effective-dated prices |
| `endpoint_price_proposals` | api (decide), billing (insert, supersede) | `source price_list`; one pending per (endpoint, model) |
| `endpoint_credentials` | api (ARN and status), reconciler (read), gateway (read) | scope `_shared` or the team id; never a value |
| `endpoint_federations` | api, reconciler, billing (read) | Azure and Google federation metadata |
| `endpoint_capacity_costs` | api (write), billing (read) | monthly or hourly cost, `by_tokens` or `by_agent` |
| `provider_statements` | api (import), billing (read) | Nous and OpenCode monthly rows |
| `team_model_endpoints`, `agent_model_endpoints`, `pool_model_endpoints` | api, reconciler, gateway | selections and members |
| `teams.gateway_deployment`, `teams.may_add_model_endpoints` | api, reconciler | `shared` or `per_namespace` |
| `agents` / `agent_pools` `.route_access`, `.default_route` | api, reconciler | `all` or `selected`; NULL default route is Bedrock |
| `usage_events` route columns | gateway (write), api reports, billing | `endpoint_id`, `model_name`, `usage_source`, timings, `http_status` |
| `cost_daily` sources `route`, `route_capacity` | billing | `model_id` `<route>/<model>` (or the route alone for capacity) |
| `resource_status` kinds `team_route`, `team_gateway` | reconciler (write), api (read) | see below |

## Status rows written by the reconciler

| Kind | Id | Phase | Condition |
|---|---|---|---|
| `team_route` | `<teamId>/<endpointId>` | Ready, Degraded, Failed (route health Ready, Degraded, Unavailable) | `RouteHealth`, reason `Ready`, `team_gateway_not_deployed`, `credential_missing`, `federation_not_ready`, `federation_pending` |
| `team_gateway` | `<teamId>` | Ready | `Ready`, reason `Applied` |

## Go types

| Type | Package | Purpose |
|---|---|---|
| `TeamGateway`, `GatewayEndpoint`, `Federation`, `RouteStatus`, `RouteReporter` | `usecase/reconcile/team` | the team's route set and its health |
| `LLMRoutes`, `RouteSpec`, `RouteModel` | `usecase/reconcile/agent` | routes delivered to an agent or pool |
| `Endpoint`, `Figure`, `Statement`, `Capacity`, `Share`, `CostRow`, `ListedPrice`, `RouteModel`, `Proposal`, `Variance`, `Result`, `Puller`, `Store`, `Job` | `usecase/billing/routebill` | route billing |
| `OpenAI`, `OpenRouter`, `Azure`, `Google`, `LiteLLM` | `adapter/billingpull` | provider pulls and the price list |
| `RouteBillStore`, `RouteStatusWriter` | `adapter/postgres` | stores |

## Events

`model_endpoint.degraded` and `.recovered` (aggregate `model_endpoints`, id the endpoint, data `endpointId`, `route`,
`teamId`, `health`, `previousHealth`, `reason`), once per transition. `cost.variance_detected` for routes (aggregate
`route_reconciliation`, id `<endpointId>/<teamId>/<period>`, data `route`, `endpointId`, `period`, `source`, `gatewayUsd`,
`billedUsd`, `variancePct` (empty when billed is zero), `status`).
