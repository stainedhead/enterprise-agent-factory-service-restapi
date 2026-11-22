# Costs, reconciliation and model prices

`afs-billing` is a set of Kubernetes `CronJob`s (`afs-billing --job <name>`). `afs-api` serves what they
write. This page covers the cost and reconciliation reads, the CUR and price-sync jobs, and the price
proposal decisions.

## Reads

| Operation | Path | Needs |
|---|---|---|
| `ListCosts` | `GET /v1/teams/{teamId}/costs?from&to` | `usage:read` on the team |
| `ListCostReconciliations` | `GET /v1/admin/cost-reconciliations?status=` | `admin:reports` (service administrators) |

`ListCosts` returns one row per day, source (`gateway`, `cur_bedrock`, `cur_eks`, `cur_other`), agent or
pool, and model, summed over the day's tag sets. The default range is the last 7 days. Send
`Accept: text/csv` for the same rows as CSV; the next page's token is then in `X-Afs-Next-Token`.
A service administrator can read a deleted team's costs.

`ListCostReconciliations` compares, per team and day, what the gateway estimated with what AWS billed for
Bedrock. `status` is `ok`, `over_threshold` (the variance exceeds `billing.variance_alert_pct`) or
`missing_billing`; `variancePct` reads `0.00` when nothing was billed. A day the CUR has no rows for yet is
not reconciled, so metered days are not reported as unbilled before AWS delivers the report.

## Jobs

| Job | Does | Settings |
|---|---|---|
| `cur-ingest` | Reads the last `AFS_BILLING_LOOKBACK_DAYS` days (default 3, ending yesterday) from the CUR through Athena into `cost_daily`. Rerunning a day replaces that day's CUR rows. Failed queries are retried with backoff; three failed days raise an alert. | `AFS_CUR_DATABASE`, `AFS_CUR_TABLE`, `AFS_ATHENA_WORKGROUP`, `AFS_ATHENA_OUTPUT`, `AFS_CUR_TAG_PREFIX` (default `user_`) |
| `reconcile` | Rolls the day's gateway usage into `cost_daily`, writes the reconciliation and publishes one `cost.variance_detected` when a team-day turns `over_threshold`. Run it after `cur-ingest`. | |
| `price-sync` | Reads the AWS Price List for Bedrock and proposes new model prices (below). | `AFS_PRICING_REGION` (Price List API region, default `us-east-1`), `AFS_AWS_REGION` (region whose prices are read, default `us-east-1`), `AFS_PRICING_PRICE_LIST_SYNC` (see below) |
| `cost-tags` | Daily at 06:30. For each AFS tag key (`afs-team-id`, `afs-agent-id`, `afs-pool-id`, the service tags and the teams' tags) records whether the CUR has shown it and whether AWS Billing has it active as a cost allocation tag. A key active for 3 days without the CUR showing it raises one `cost_tags.not_seen` event. Read the result with `ListCostTagStatus` (service admins). Needs `ce:ListCostAllocationTags` on the billing role. | |

Add `--day YYYY-MM-DD` to run one completed UTC day.

The CUR export must be the standard data export with resource ids and split cost allocation data for EKS,
and the AFS tags (`afs-team-id`, `afs-agent-id`, `afs-pool-id`, the service and team tags) activated as
cost allocation tags.

## Model price proposals

With `pricing.price_list_sync` on (the default), `price-sync` creates one `pending` proposal per model whose
on-demand price differs from its current price. A newer proposal supersedes the older pending one; an
unchanged price creates nothing. A model the list prices only partly (input or output missing) is skipped,
and a cache price the list lacks keeps the current value.

| Operation | Path | Needs |
|---|---|---|
| `ListModelPriceProposals` | `GET /v1/admin/model-price-proposals?status=` | `admin:pricing` |
| `ApproveModelPriceProposal` | `POST /v1/admin/model-price-proposals/{proposalId}:approve` | `admin:pricing` |
| `RejectModelPriceProposal` | `POST /v1/admin/model-price-proposals/{proposalId}:reject` | `admin:pricing` |

Both decisions take `{"note": "..."}` (at most 500 characters), accept `Idempotency-Key`, and answer `409`
when the proposal is no longer `pending`. Approval makes the price effective from the later of the
proposal's `effectiveFrom` and the approval time, so usage before that moment is never repriced; the
gateway uses it for calls from then on. Proposal ids look like `mpp_...`.

`pricing.price_list_sync` is read by the billing job from the database. Until the billing database role can
read that setting, set `AFS_PRICING_PRICE_LIST_SYNC=true|false` on the job; without either the job fails
rather than guess.
