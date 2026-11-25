# Upgrading to 1.2.0

1.2.0 adds model routes, team gateways and provider switches (see [Model routes and the team gateway](model-routes.md)).
It needs `-db` migration 000023 (`afs-api` runs it at start with `AFS_DATABASE_OWNER_URL`, or run `afs-migrate up`).

## Before you start

- Upgrade from 1.1.0. The migration adds tables and nullable or defaulted columns; it rewrites no table.
- Nothing changes for existing agents: every team stays on the shared gateway (`gateway_deployment = shared`), Bedrock
  stays on, and a shared-gateway agent's pod template is unchanged, so no pod restarts.

## New settings

| Binary | Settings | Needed when |
|---|---|---|
| `afs-reconciler` | `AFS_TEAM_GATEWAY_IMAGE`, `AFS_TEAM_GATEWAY_DB_SECRET_ARN`, `AFS_TEAM_GATEWAY_DB_HOST`, `AFS_TEAM_GATEWAY_DB_PORT`, `AFS_TEAM_GATEWAY_TLS_ISSUER` | a team is set to `per_namespace` |
| `afs-billing` | `AFS_BILLING_AZURE_SCOPES`, `AFS_BILLING_GOOGLE_EXPORTS`, `AFS_ROUTE_PRICE_LIST_URL` | Azure or Google routes; a price list other than LiteLLM's |

The Helm chart adds four billing CronJobs (`route-pull`, `route-statements`, `route-capacity`, `route-price-sync`).
Terraform's `pod-identity` module lets the billing role read `<prefix>/_service/billing/*` (provider admin keys).

## After the upgrade

1. `ListInferenceProviders` (`GET /v1/admin/inference-providers`): Bedrock, OpenAI, OpenRouter, Nous and OpenCode on; Azure and Google off.
2. `ListModels` shows `providerState: on`.
3. Create a route, set one test team to `per_namespace`, and check the team gateway Deployment and the route's health.

## Deferred in 1.2.0

The team gateway's Pod Identity association is made by Terraform per team; live Azure and Google token acquisition for
`afs-billing` is not built (the pulls read a bearer token from the billing key path).
