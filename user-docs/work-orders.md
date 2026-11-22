# Work orders and swarm templates

A work order is one unit of remediation work, for example a CVE across a list of repositories. The service turns
it into a short-lived team (a swarm) built from a swarm template, tracks the pull requests the workers open, and
tears the team down when the work is complete.

## Who can do what

| Caller | Swarm templates | Work orders |
|---|---|---|
| Service admin | create, update, enable, disable, delete, list, get | everything |
| Security operator (person or service key) | list, get | create, list, get, update, cancel |
| Team admin or viewer | none | the work orders assigned to the team or that touch its repositories: list and get |
| Worker (workload token) | none | report progress on its own team's work order |

Add a security operator with `AddSecurityOperator` (service admins only). A service key for automation is created
with `CreateServiceApiKey` (role `security_operator` only); the secret is returned once.

## Swarm templates

`POST /v1/admin/swarm-templates` takes a name, `kind` (`cve_remediation`), a lifetime budget (`budgetUsd`), a time
to live (`ttlHours`), a `teardownRule` and a `spec` with `stages`, `pools`, `handoffs`, `configValues` and
`secrets`. A template with an open work order cannot be deleted (`409`).

Each entry of `spec.secrets` names a secret the remediation team gets, created empty:

```json
{"pool": "fixers", "name": "scanner-token", "description": "scanner API token", "delivery": "env", "target": "SCANNER_TOKEN"}
```

- `pool` is one of the template's pools; `name` is lower-case letters, digits and hyphens, unique in the template.
- `delivery` is `env` (default) or `file`. An `env` target is an upper-case variable name (default: the name in
  upper case with underscores); a `file` target is an absolute path. A target is unique within its pool.
- A template never holds a value. When the team is created the service creates the secret in Secrets Manager with
  no value and binds it to the pool; the team sets the value with `PutSecretValue`.

## Work orders

`POST /v1/work-orders` (with `Idempotency-Key`) takes `cveId`, `severity`, `title`, `slaDueAt`, `templateId` and
`repos`. A repository without an owner is resolved from the teams' `AFS_OWNED_REPOS` configuration value (an exact
entry beats a wildcard); an ambiguous or unowned repository is refused with `400` and nothing is written. A second
open work order for the same CVE answers `409`.

A work order moves `open`, `assigned`, `in_progress`, `pr_opened`, `done` (or `failed`, `cancelled`). The
reconciler derives the status from the repositories; `UpdateWorkOrderRepo` changes only a repository's progress.

### Workers

A worker (an agent or a pool of the remediation team) calls the API with its projected ServiceAccount token
(audience `afs-api`, see `deployment.md`). Its only permission is `work-orders:report` on its own team's work
order: `PATCH /v1/work-orders/{id}/repos/{host}/{owner}/{repo}` with `{status, prUrl?, prNumber?, detail?}`;
`pr_opened` needs a pull request link. Another team's work order answers `404`.

A worker cannot read the repository's version, so it may omit `If-Match` on this one operation: the update then
applies to the current version. A version it does send is compared (`412` when stale). Everyone else must send
`If-Match`, and the response carries the new `ETag`.

## Operating it

Set `AFS_WORK_ORDER_QUEUE_URL` and `AFS_GITHUB_APPS_FILE` on the reconciler (see `reconciler.md`). Intake takes one
work order per `sourceEventId` even when an event is delivered twice. Owners are notified once per event and
recipient: assigned, pull request opened, merged, SLA warning (`work_orders.sla_warning_hours` before the due
time), SLA breached, completed.
