# Product details

Per-component behavior as built. How it is implemented is in [technical-details.md](technical-details.md); how to
run and call it is in [user-docs/](../user-docs/README.md).

## afs-api

- **Contract.** 182 operations under `/v1`, camelCase JSON, prefixed ids, `maxResults` and `nextToken` pagination
  (signed page tokens), errors as `{code, message, requestId}`. `/healthz`, `/readyz`, `/v1/openapi.yaml` and
  `/v1/sign-in-providers` are public. Every other route declares the permission it needs; the generated route table
  and the authorization matrix test (every operation for every role) fail on a route without one.
- **Authentication.** Identity provider JWTs (Okta and Entra ID, JWKS cached), API keys (`afsk_…`, hashed), and
  workload tokens (projected ServiceAccount tokens, audience `afs-api`, checked offline against the cluster issuer).
  A workload holds only `work-orders:report`, bounded to its assigned team.
- **Access rules.** A team-scoped route takes `teamId` from the path. A caller who cannot see a resource gets `404`,
  one who can see it but lacks the permission gets `403`. Row-level security applies in every transaction.
- **Mutations.** One transaction writes the row, an `audit_log` entry and an `outbox_events` entry. Creating calls
  accept `Idempotency-Key` (claimed and completed in the mutation's own transaction); updates need `If-Match`
  (`428` missing, `412` stale; a workload may omit it on `UpdateWorkOrderRepo`).
- **Secrets.** Values go to Secrets Manager (`PutSecretValue`) and are never stored, returned or logged.
  `RotateSecret` writes a new version.
- **Limits.** 20 requests per second per principal with a burst of 40, divided across replicas; `429` with
  `Retry-After`.

## afs-reconciler

- **Engine.** One leader-elected process, a work queue keyed by kind and id with per-team ordering, backoff, a
  change feed (`LISTEN afs_desired_state`) with a poll and a resync as backstops, and status in `resource_status`.
- **Controllers.** `team` (namespace, quota, default-deny and Cilium policies, SecretStore, IAM roles, inference
  profiles), `platform` and `admission` (cluster-wide), `agent`, `pool`, `egress` (per-agent network allowances),
  `kubectl` (viewer access), `tag_sync` (tags of secrets and pool queues), plus log destinations, notification
  channels and role links.
- **Lifecycle.** Ordered team delete: secrets scheduled for deletion, agents and pools, IAM, inference profiles,
  access entry, namespace. Suspend scales workloads down. Drift is corrected within 60 seconds and recorded.
- **Tags.** The effective set (system, service and team tags, at most 40) is applied to roles, Pod Identity
  associations, inference profiles, snapshots, NodePools, secrets and pool queues; tags AFS applied earlier and no
  longer wanted are removed, foreign tags are kept.
- **Jobs.** Outbox publisher to EventBridge, partition maintenance, idempotency purge, report rollups, orphan
  secret sweep, budget periods and backstop, AWS Organizations account mirror, export bundles, work order intake
  (EventBridge queue), assignment, template secrets, tracking and notifications.

## afs-gateway

TLS server for the Bedrock Runtime paths and `/anthropic/v1/messages`. The caller is its source IP, mapped to
pod, team, agent or pool. Spend is reserved before the call and settled on success, error or abort; a team with no
budget gets `402`; database trouble gives `503` within 2 seconds and nothing reaches Bedrock. Usage rows are batched
and carry the team's tag set; no prompt or completion content is stored or logged.

## afs-billing

`cur-ingest`, `reconcile` (variance against the metered spend, alert and `cost.variance_detected`), `price-sync`
(proposals for service admins to accept), `report-schedules` (CSV to the reports bucket, `report.delivered` or
`report.failed`) and `showback` (monthly, refuses a CSV whose dimension does not add up to the total).
