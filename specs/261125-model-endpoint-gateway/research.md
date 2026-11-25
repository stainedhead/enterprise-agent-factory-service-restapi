# Research: team model gateway (1.2.0)

## Provider cost sources (MEG-BL-1, MEG-OQ-11, MEG-OQ-14)

| Provider | Source | Shape used | Attribution |
|---|---|---|---|
| OpenAI | `GET /v1/organization/costs?start_time&end_time&bucket_width=1d&group_by=project_id` (admin key), pages by `next_page`; `GET /v1/organization/projects` for names | `data[].results[].amount.value`, `project_id` | project name contains the team id |
| OpenRouter | `GET /api/v1/keys` (provisioning key), `offset` paging | `data[].usage_daily`, `name`, `label` | key name or label contains the team id |
| Azure | `POST <scope>/providers/Microsoft.CostManagement/query?api-version=2023-11-01`, `ActualCost`, custom day, grouping `ResourceGroupName` and tag `afs-team-id` | `properties.columns`, `properties.rows`, `nextLink` | tag value, else resource group name (case-folded) |
| Google | BigQuery `POST /bigquery/v2/projects/<p>/queries` on the Cloud Billing export, Vertex AI, credits included | `rows[].f[].v`, `jobComplete` | resource label `afs-team-id`, else project label (case-folded) |
| Nous Portal, OpenCode | statements imported by service admins (`ImportProviderStatement`, `provider_statements`) | `rows[] {teamId, model, amountUsd, tokens?}` | the statement names the team |

- OpenRouter's `usage_daily` is the current UTC day only: there is no history endpoint for a key, so the pull runs late in the
  day (CronJob `30 23 * * *`) and a past day cannot be pulled (`ErrDayNotAvailable`).
- Azure's Cost Management query API and Google's billing export query are not confirmed against live tenants (PRD MEG-OQ-14);
  the fixtures are shaped from the published references and are replaced by recordings once J has cloud access.
- Recorded fixtures: `internal/adapter/billingpull/testdata/*.json`. Ids and subscription are placeholders
  (`00000000-...`), no real account.

## Price list (MEG-BL-3, MEG-D2)

LiteLLM's `model_prices_and_context_window.json` (MIT data): `input_cost_per_token`, `output_cost_per_token`,
`litellm_provider`. Mapped providers: `openai`, `text-completion-openai` → openai; `openrouter`; `azure`, `azure_ai` →
azure; `vertex_ai*` → google. Keys lose their provider prefix (`openrouter/x/y` → `x/y`) so they match a route's model
names; `routebill` also matches `<provider>/<model>`.

## Kubernetes and federation (MEG-RC-1..4)

- Azure workload identity federation needs a projected service account token with audience `api://AzureADTokenExchange`;
  Google workload identity federation accepts a configurable audience (the provider's
  `//iam.googleapis.com/projects/<n>/locations/global/workloadIdentityPools/<pool>/providers/<provider>`). One projected
  token per gateway pod, so a team with both must allow the Azure audience on its Google provider (implementation notes).
- Cilium `toFQDNs` egress needs the DNS rule in the same policy (kept from the 1.1.0 team policy).
