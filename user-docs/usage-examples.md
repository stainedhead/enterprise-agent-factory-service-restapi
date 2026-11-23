# Usage examples

Examples use `curl` against `https://afs.example.com` with a bearer token in `$TOKEN` (an identity provider JWT or
an API key that starts with `afsk_`). Ids are prefixed ULIDs (`team_…`, `agt_…`); account ids in the examples are
the documentation account `111122223333`. The contract is `GET /v1/openapi.yaml`; the `afs` CLI wraps the same
operations (each operation in the contract names its CLI command in `x-afs-cli`).

Conventions: JSON is camelCase; lists take `maxResults` and `nextToken`; errors are
`{"code": "...", "message": "...", "requestId": "..."}`; creating calls accept an `Idempotency-Key`; updates need
`If-Match` with the last `ETag` (`428` when missing, `412` when stale).

## Who am I

```bash
curl -s -H "Authorization: Bearer $TOKEN" https://afs.example.com/v1/me
```

Returns the caller, the bound teams, the role in each and the permissions. It is for display: the API decides on
every call.

## Create a team (service admin)

```bash
curl -s -X POST https://afs.example.com/v1/teams \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: $(uuidgen)" -H 'Content-Type: application/json' \
  -d '{
    "slug": "payments",
    "displayName": "Payments",
    "kind": "persistent",
    "maxBudgetUsd": 5000,
    "firstAdmin": {"subjectType": "user", "email": "owner@example.com"},
    "budget": {"period": "monthly", "amountUsd": 1000, "thresholds": [50, 80, 100]},
    "tags": {"cost-center": "CC100"}
  }'
```

The row is written with its audit and outbox entries in one transaction. `afs-reconciler` then creates the
namespace, roles and inference profiles; poll `GET /v1/teams/{teamId}/status` until the phase is `Ready`.

Every team gets its orchestration agent with the team. The optional `orchestration` member chooses the tool and the
starting levels; `maxCollaborationExposure` (`off`, `private`, `public`) is the limit of the team:

```bash
  -d '{ ..., "maxCollaborationExposure": "private",
        "orchestration": {"tool": "paperclip", "webUiExposure": "private", "webApiExposure": "off"},
        "modelAccess": "all" }'
```

## Orchestration: read it, change a level, change the tool

```bash
curl -s https://afs.example.com/v1/teams/$TEAM/orchestration -H "Authorization: Bearer $TOKEN"

# raise the web UI to public (needs collaboration:manage; the limit must allow it)
curl -s -X PUT https://afs.example.com/v1/teams/$TEAM/orchestration -H "Authorization: Bearer $TOKEN" \
  -H 'If-Match: 3' -H 'Content-Type: application/json' -d '{"webUiExposure": "public"}'

# change the tool (a snapshot is taken first; the data of the old tool is not carried over)
curl -s -X POST https://afs.example.com/v1/teams/$TEAM/orchestration:change-tool -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"tool": "buzz"}'
```

Errors name the reason in `details.reason`: `exposure_above_limit`, `surface_not_supported_by_tool`,
`tool_not_enabled`, `tool_unchanged`, `tool_change_in_progress`. The orchestration agent cannot be created or deleted
through `CreateAgent` or `DeleteAgent` (`role_reserved`, `name_reserved`, `orchestration_agent_required`).

## Models

```bash
# a team's models: access "all" means every model the service enables, now and later
curl -s -X PUT https://afs.example.com/v1/teams/$TEAM/models -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"access": "selected", "modelIds": ["anthropic.example-model-v1:0"]}'

# an agent's models: a default and, optionally, a selection within the team's
curl -s -X PATCH https://afs.example.com/v1/teams/$TEAM/agents/$AGENT -H "Authorization: Bearer $TOKEN" \
  -H 'If-Match: 2' -H 'Content-Type: application/json' \
  -d '{"models": {"access": "selected", "modelIds": ["anthropic.example-model-v1:0"], "defaultModelId": "anthropic.example-model-v1:0"}}'
```

A call to a model the agent may not use is refused by the gateway with `AccessDenied` and `details.reason` one of
`model_not_enabled_for_service`, `model_not_enabled_for_team`, `model_not_enabled_for_agent` or `model_not_priced`.
`ListModels` shows `callable` and `priceState` per model.

## Create an agent

```bash
curl -s -X POST https://afs.example.com/v1/teams/$TEAM/agents \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name": "triage", "template": "hermes", "image": "ghcr.io/example/hermes@sha256:<digest>", "modelId": "claude-sonnet"}'
```

The image must be pinned by digest and allowed by the template. Pause, resume and restart are `:verb` actions on
the agent, for example `POST /v1/teams/$TEAM/agents/$AGENT:pause`.

## Store a secret

```bash
curl -s -X POST https://afs.example.com/v1/teams/$TEAM/secrets \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"scope": "team", "name": "scanner-token", "value": "<secret value>"}'
```

The value goes straight to Secrets Manager; the database keeps the ARN and metadata, and no endpoint or log line
returns the value. `RotateSecret` takes a new `{value}` and writes a new version. Scope `agent` needs `agentId`,
scope `pool` needs `poolId`.

## Set a budget

```bash
curl -s -X PUT https://afs.example.com/v1/teams/$TEAM/budget \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'If-Match: "3"' \
  -d '{"period": "monthly", "amountUsd": 1500, "onExhaust": "block", "thresholds": [50, 80, 100]}'
```

The gateway refuses calls with `402 BudgetExceeded` once the budget is spent (`onExhaust: block`); `notify`
needs `teams:set-guardrails` and a persistent team.

## Read a report

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://afs.example.com/v1/teams/$TEAM/reports/usage?range=30d&groupBy=agent"
curl -s -H "Authorization: Bearer $TOKEN" -H 'Accept: text/csv' \
  "https://afs.example.com/v1/teams/$TEAM/reports/costs?from=2026-11-01&to=2026-11-30"
```

Kinds are `usage`, `costs`, `activity` and `traffic`; `tag.<key>=<value>` filters by a cost tag. See
[Reports and usage](reports.md).

## Pagination and errors

```bash
curl -s -H "Authorization: Bearer $TOKEN" "https://afs.example.com/v1/teams?maxResults=50"
# the response carries nextToken when more remain; pass it back unchanged:
curl -s -H "Authorization: Bearer $TOKEN" "https://afs.example.com/v1/teams?maxResults=50&nextToken=<token>"
```

A caller without a permission gets `403` on a resource it can see and `404` on one it cannot; `401` means the
credential is missing or expired; `429` carries `Retry-After`.

## More

[Work orders and swarm templates](work-orders.md), [Costs and prices](billing.md),
[Export, import and restore](export-import-restore.md).
