# Architecture: team model gateway (1.2.0)

```
agent pod ──$(AFS_GATEWAY_URL)/r/<route>/v1──► team afs-gateway (per_namespace) ──► provider (egress: route hosts only)
            /etc/afs/llm-routes.json            │ route_admission (60 s cache, change feed)
                                                 │ reserve → call → settle (usage_events, endpoint_id)
                                                 ▼
                                             PostgreSQL ◄── afs-billing route-pull / statements / capacity / price sync
                                                 ▲
afs-api (27 §4.13 operations) ── desired state ──┘──► afs-reconciler: team gateway objects, egress, ExternalSecrets,
                                                       projected federation token, agent route env, route health
```

## afs-gateway router shape

`gatewayhttp.Mux` is a composable router of route families (`Family.Mount(*Mux)`): exact paths and prefixes, longest match
wins, the rest is the contract's 404. Families today: Bedrock and Anthropic (1.1.0 paths) and `RoutesFamily` (`/r/` and
`/openai/`). The MCP 1.3.0 change adds its family without touching these. A team gateway (`--team-namespace`,
`AFS_GATEWAY_TEAM_ID`) serves one namespace and mounts the route family; the shared gateway is unchanged.

## afs-reconciler

- Team: `team.Desired.Gateway` (from `loadTeamGateway`) renders, when `per_namespace`, ServiceAccount, Deployment (2
  replicas, 8443), Service, PodDisruptionBudget, CiliumNetworkPolicy `afs-gateway-policy`, ExternalSecrets
  `afs-gateway-credentials` and `afs-gateway-db`, Certificate `afs-gateway-tls`; removes them when the team returns to
  shared. The team CiliumNetworkPolicy sends agents' gateway egress to the in-namespace gateway. `RouteReporter` writes
  route health after apply.
- Agent and pool: `LLMRoutes` adds route variables (after `AFS_GATEWAY_URL`, which they reference) and
  `/etc/afs/llm-routes.json` for per_namespace teams; the pod-template hash changes only when routes are delivered or a
  route default is set.
- `engine.GatewayRoutes` sends changes of the gateway tables to the team, agent and pool controllers.

## afs-billing

`routebill.Job` is the use case; `billingpull` holds the provider adapters behind `routebill.Puller`; `postgres.RouteBillStore`
the store. Network calls happen outside transactions; each comparison and its events are written in one transaction.
