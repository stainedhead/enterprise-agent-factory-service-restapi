# Spec: team model gateway, routes and provider switches (release 1.2.0)

Source: root [model-endpoint-gateway-PRD.md](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/model-endpoint-gateway-PRD.md)
(approved by J, 2026-11-25), Appendix D.3; root contracts on main: gateway contract v1.2.0 §12, API contract v1.5.0
§3 and §4.13, database contract v1.5.0 §7.4 (migration 000023, `-db` 1.2.0, lockstep).

## Goal

Teams reach OpenAI-compatible model endpoints (OpenAI, OpenRouter, Nous Portal, OpenCode, Azure, Google Vertex AI,
corporate and in-namespace servers) through a metered, capped team gateway, with every provider (Bedrock included) a
service-admin switch, without changing anything an existing 1.1.0 agent relies on.

## Requirements (copied from PRD D.3)

### afs-gateway

| ID | Requirement | Proves |
|---|---|---|
| MEG-GW-1 | Team mode (`--team-namespace`): one binary serves the shared or a team gateway | TC-GW-22, TC-P-03 |
| MEG-GW-2 | Route resolution (path and body forms), admission from `afs.route_admission` with 60 s cache and change-feed invalidation | TC-GW-02, 09, 18, 26 |
| MEG-GW-3 | OpenAI pass-through for Chat Completions, Responses, Embeddings, SSE; reservation cap written into the request; `include_usage` injected and stripped; usage parsing; provider `usage.cost` settles to the larger | TC-GW-01, 03 to 07, 24, 27 |
| MEG-GW-4 | Chat Completions → Bedrock Converse on `/r/bedrock`, `unsupported_parameter` for unmapped fields | TC-GW-10, 23 |
| MEG-GW-5 | Upstream auth from files (bearer, header), Azure and Google federation (§11.1), rotation without restart; agent `Authorization` replaced and redacted | TC-GW-11 to 13, 28, 29 |
| MEG-GW-6 | Error mapping: `credential_missing`, `federation_not_ready`, `provider_balance_exhausted`, `provider_budget_exhausted`, upstream 429/5xx/timeout, fail closed on database loss | TC-GW-12, 16, 17 |
| MEG-GW-7 | `GET /v1/models` per route from the agent's effective models; unsupported paths 404 | TC-GW-14, 15 |
| MEG-GW-8 | Telemetry: GenAI spans and metrics, no agent label, `traceparent` rules, timing columns | TC-GW-19 to 21 |

### afs-reconciler

| ID | Requirement | Proves |
|---|---|---|
| MEG-RC-1 | Render the team gateway per namespace behind `teams.gateway_deployment`; ring rollout | TC-RC-01, 10, US-76 |
| MEG-RC-2 | Egress: route hosts only from the team gateway; agents never; providers off removed | TC-RC-02 to 04, 09, 12 |
| MEG-RC-3 | Agent delivery: `AFS_LLM_*`, `OPENAI_BASE_URL`, `/etc/afs/llm-routes.json`, route default model | TC-RC-07, 08 |
| MEG-RC-4 | Credentials and federation: `_gateway` secrets, projected token for Azure, nothing mounted into agents | TC-RC-06, 14 |
| MEG-RC-5 | Provider switches incl. Bedrock off (remove `bedrock:Invoke*`, fallback defaults) | TC-RC-13 |
| MEG-RC-6 | Route health conditions and `model_endpoint.degraded` / `recovered` events | TC-RC-11 |

### afs-billing

| ID | Requirement | Proves |
|---|---|---|
| MEG-BL-1 | Daily provider pulls (OpenAI, OpenRouter, Azure Cost Management, Google billing export) behind an interface with recorded fixtures; variance → `cost.variance_detected` | TC-BL-01, 04 |
| MEG-BL-2 | Statement imports (Nous, OpenCode) and *Not reconciled* state | TC-BL-05, 06 |
| MEG-BL-3 | Capacity allocation by token share; endpoint price proposals | TC-BL-02, 03 |

### afs-api (contract v1.5.0 §4.13, 26 rows, 27 OpenAPI operations)

| ID | Requirement | Proves |
|---|---|---|
| MEG-API-1 | Every operation with allowed and denied tests per role; team scope from the path | TC-API-01, 05, 07 |
| MEG-API-2 | Row, audit and outbox in one transaction; If-Match on updates | TC-API-02, 03 |
| MEG-API-3 | Credential values never stored, returned or logged; federation is metadata only | TC-API-04 |
| MEG-API-4 | Provider rules: disabled-provider host refused; Bedrock fallback guard | TC-API-08 |
| MEG-API-5 | Inference report percentiles from the ledger, JSON and CSV | TC-API-06 |

## Non-goals

Real-cloud checks (EKS, Azure and Google federation against live tenants, provider billing APIs) wait for J's cloud
access; everything is proved against fakes, envtest and recorded fixtures. Responses and Embeddings on `/r/bedrock`
(404 in 1.2.0). Token quotas on capacity routes.

## Delivery

1. PR 1, contract first: `api/openapi.yaml` v1.5.0, permissions, routes; handlers answer `501 not_implemented`
   (`httpapi.PendingModelEndpointOps`).
2. PR 2, implementation of every requirement above, pinned to the `-db` 000023 commit.
