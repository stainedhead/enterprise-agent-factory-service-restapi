# Plan: team model gateway (1.2.0)

Two pull requests, contract first (spec.md, Delivery).

| Step | Layer | What | PR |
|---|---|---|---|
| 1 | tools, api | `contractgen` reads contract v1.5.0; `api/src` gains §4.13 (27 operations), route members, report route filter | 1 |
| 2 | authz, domain | permissions and role tests; `domain` endpoints, providers, routes, events, route env | 1 |
| 3 | adapter/gatewayhttp, usecase/gateway | team mode, route admission (60 s cache, change feed), OpenAI pass-through, Chat to Converse on `/r/bedrock`, upstream auth from files and federation, `/v1/models`, telemetry | 2 |
| 4 | usecase/modelendpoints, adapter/postgres, adapter/httpapi | the 27 operations; 000023 error mapping; agent and pool route members; reports by route; `providerState` | 2 |
| 5 | usecase/reconcile/team, adapter/k8s/render | team gateway objects per namespace, egress, credentials ExternalSecret, Azure projected token, route health and events | 2 |
| 6 | usecase/reconcile/agent, pool, render | `AFS_LLM_*`, `OPENAI_BASE_URL`, `/etc/afs/llm-routes.json`, route default model, hash rules | 2 |
| 7 | usecase/billing/routebill, adapter/billingpull, adapter/postgres, cmd/afs-billing | provider pulls with recorded fixtures, statements, capacity allocation, price proposals, four CronJobs | 2 |
| 8 | deploy, docs | Helm CronJobs, billing key read in Terraform, versions 1.2.0, docs and user-docs | 2 |

Decisions are recorded in `implementation-notes.md` (contract notes and choices) and `docs/architectural-decision-record.md`
(ADR-031 to ADR-034).

## Dependencies

- `-db` 000023 at commit `debc7d9` (pseudo-version `v0.0.0-20261125011205-debc7d9d28fd`); moves to `-db` v1.2.0 once tagged.
- Root contracts at `api/contract/ROOT_REF` (root `b528c73`, API contract v1.6.0 after the MCP contract change merged into
  main; the v1.5.0 §4.13 operations are unchanged in it).
