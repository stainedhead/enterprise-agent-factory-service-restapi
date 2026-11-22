# Reports and usage

Four report kinds answer how much agents used, what it cost, whether they were working, and what they
reached on the network. Each is available at three levels.

| Level | Operation | Path | Needs |
|---|---|---|---|
| Team | `GetTeamReport` | `GET /v1/teams/{teamId}/reports/{kind}` | `reports:read` on the team |
| Agent | `GetAgentReport` | `GET /v1/teams/{teamId}/agents/{agentId}/reports/{kind}` | `reports:read` on the team |
| Service | `GetServiceReport` | `GET /v1/reports/{kind}` | `admin:reports` (service administrators) |
| Team usage | `GetUsage` | `GET /v1/teams/{teamId}/usage?from&to&groupBy=agent\|model\|day` | `usage:read` on the team |

`kind` is `usage`, `costs`, `activity` or `traffic`. A viewer of team A gets `404` for team B and `403`
for service reports. A service administrator can read the reports of a deleted team.

## Query parameters

| Parameter | Meaning |
|---|---|
| `from`, `to` | RFC 3339 range, `from` inclusive and `to` exclusive. `from` after `to`, or a range over 400 days, is `400`. |
| `range` | `24h`, `7d`, `30d`, `90d` or `mtd`, instead of `from` and `to`. With none of the three, a report covers the last 7 days (`GetUsage`: the month so far). |
| `groupBy` | Comma-separated: `team`, `agent`, `model` (usage, costs), `system` (traffic), `day`, `hour` (ranges of two days or less), `pool` (ephemeral teams), `tag:<key>`. Default `day`. |
| `teamId`, `agentId`, `modelId`, `systemId`, `poolId` | Filters. `teamId` applies to service reports. |
| `tag.<key>=<value>` | Keep records that carried the tag value; `(untagged)` keeps those without it. |
| `maxResults`, `nextToken` | Paging over the grouped rows (1 to 100, default 50). Totals always cover every row. |

Grouping by a tag uses the tags in effect when each record happened: one row per value plus a row for
`(untagged)`.

## Response

```json
{
  "kind": "usage",
  "range": {"from": "2026-11-15T00:00:00Z", "to": "2026-11-22T00:00:00Z"},
  "groupBy": ["agent", "model"],
  "rows": [{"agent": "agt_01JD…", "model": "anthropic.claude-sonnet-4-5", "calls": 1200, "inputTokens": 5400000, "outputTokens": 610000, "costUsd": "25.35"}],
  "totals": {"calls": 1200, "inputTokens": 5400000, "outputTokens": 610000, "costUsd": "25.35"},
  "notes": ["Spend with providers other than Bedrock is not included"]
}
```

Measures by kind:

| Kind | Measures |
|---|---|
| `usage` | `calls`, `inputTokens`, `outputTokens`, `costUsd` (calls sent to the model: outcome ok or error) |
| `costs` | `estimatedUsd` (gateway metering), `billedUsd` (AWS Cost and Usage Report, daily ranges, once reconciled) |
| `activity` | `workingSeconds`, `idleSeconds`, `offSeconds` (paused, stopped), `impairedSeconds` (unhealthy, budget blocked), `modelCalls`, `utilizationPct` (working share of working and idle time) |
| `traffic` | `connections`, `deniedConnections`, `requests`, `errors`, `bytesOut`, `bytesIn`. Counts and bytes, never a cost. `requests` and `errors` are `null` where no egress proxy sees HTTP. |

Ranges of two days or less read per-call and 5-minute data; longer ranges read daily rollups and cover
every UTC day the range touches. Usage and cost rollups are written by the billing job, so the most recent
day of a long range can be missing until it runs; use a range of two days or less for today.

## CSV

Send `Accept: text/csv` for the same rows as CSV: the groupBy columns, then the measures. A next page is
announced in the `X-Afs-Next-Token` response header (there is no `nextToken` field in a CSV body).

## Example

```bash
curl -s -H "Authorization: Bearer $TOKEN" -H "Accept: text/csv" \
  "$AFS/v1/teams/$TEAM/reports/costs?range=30d&groupBy=model,tag:business-unit"
```

## Activity samples

The reconciler writes one activity sample per running agent, and per worker of a pool, every five minutes.
An agent is working in a window when it made a model call through the gateway, reported itself busy on its
status endpoint, or used more CPU than `reports.idle_cpu_millicores` (default 50) on average. Operators:
`AFS_AGENT_STATUS=off` skips the status endpoint, `AFS_AGENT_STATUS_PORT` sets its pod port. The cluster
needs metrics-server for the CPU signal; without it, model calls and the status endpoint decide.

## Scheduled reports

A team admin (or service admin) can have a report delivered on a schedule; a viewer can list schedules but
not change them.

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  "$AFS/v1/teams/$TEAM/report-schedules" -d '{
    "name": "weekly-costs", "report": "costs", "cadence": "weekly", "channelId": "'$CHANNEL'",
    "query": {"range": "30d", "groupBy": ["model", "tag:business-unit"], "filters": {"tag.env": "prod"}}
  }'
```

- `cadence` is `daily`, `weekly` or `monthly`. Schedules run at 06:00 UTC: every day, every Monday, or the
  first of the month. A query without a range reports the period the cadence repeats (24 hours, 7 days or
  30 days).
- `channelId` must be one of the team's own notification channels. A service-level channel, another team's
  channel or an unknown one is a `400`.
- `query` takes the report query parameters in the stored shape (`range` or `from` and `to`, `groupBy` as a
  list, `filters` keyed `agentId`, `modelId`, `systemId`, `poolId` and `tag.<key>`). A schedule reports on
  its own team only.
- Update with `PATCH` and `If-Match`; delete with `DELETE` and `If-Match` (`204`). A channel that a schedule
  uses cannot be deleted until the schedule is.
- `nextRunAt`, `lastRunAt`, `lastStatus` (`succeeded` or `failed`) and `lastError` are read-only. The
  version (ETag) changes each time a schedule runs (the runner writes `lastRunAt` and `lastStatus` on the same row), so an update or delete with an ETag read before a run answers `412 PreconditionFailed`: list the schedules again and retry with the new ETag.

When a schedule is due, the billing job computes the same report you get from the API with
`Accept: text/csv`, stores the CSV in the reports bucket under `reports/<team>/<schedule>/`, and publishes
a `report.delivered` event whose data carries `channelId`, `scheduleId`, `downloadUrl` (a pre-signed link
valid for 7 days, or until the job's temporary credentials expire if that is sooner) and `downloadExpiresAt`.
The schedule's channel delivers the event; channels never receive another channel's report events. If the
report cannot be computed, stored or signed, the schedule records `failed` with the error, a
`report.failed` event is published, and the next cycle tries again.

Operators: `afs-billing` needs `AFS_REPORTS_BUCKET` and, for a customer-managed key,
`AFS_REPORTS_KMS_KEY_ID`; set `AFS_SHOWBACK_TAG_KEY` to add a tag dimension to the monthly showback CSV
(`showback/<YYYY-MM>/`, cost centers and the tag's values with a total row each, an `(untagged)` row for
spend without the tag).

