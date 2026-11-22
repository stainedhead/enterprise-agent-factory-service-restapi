# enterprise-agent-factory-service-restapi

Control plane for the Enterprise Agent Factory Service, in Go: the REST API with role-based access, the
reconciler that applies desired state to EKS and AWS, the Bedrock metering gateway that enforces hard
budget caps, and daily billing reconciliation. Owns the API contract `api/openapi.yaml`.

Part of [enterprise-agent-factory-service](https://github.com/stainedhead/enterprise-agent-factory-service);
requirements in its initial PRD and architecture in its `docs/solution-design.md`.

**Status:** feature complete for the scope of the restapi spec (milestones M0 to M6): all 182 operations of the
API contract are implemented behind the permission matrix, with the reconciler, gateway and billing jobs.
Checks that need a development cluster or an AWS account (end-to-end run, restore drill, full-scale load,
`govulncheck` in this environment) run in CI and on the cluster, not here. `-db` is pinned to an untagged
pseudo-version until it is released. The code review of the first pass (restore guardrails, key handling, metrics,
startup checks for unbound operations) is closed; see `docs/architectural-decision-record.md` ADR-017 to ADR-022.

## Binaries

| Binary | Purpose |
|---|---|
| `afs-api` | REST API, RBAC enforcement, audit, idempotency, optimistic concurrency |
| `afs-reconciler` | Applies desired state to EKS and AWS, reverts drift, tags resources, runs housekeeping and billing-adjacent jobs |
| `afs-gateway` | Bedrock proxy: per-team token metering and hard caps, fails closed |
| `afs-billing` | CUR ingest and reconciliation, price sync, scheduled reports, showback |

What each does, in detail: [docs/product-summary.md](docs/product-summary.md) and
[docs/product-details.md](docs/product-details.md).

## Build and test

Requires Go 1.27 (pinned in `go.mod`; older Go fetches it) and `make`.

```bash
make gate     # tidy, fmt-check, vet, openapi and contract checks, golangci-lint, race tests, build
make cover    # at least 90% on domain, usecase and authz
make build    # binaries in ./bin
./bin/afs-api --addr :8080
curl localhost:8080/healthz
```

Integration tests need PostgreSQL 17: set `AFS_TEST_DATABASE_URL` and run `make test-db`. Kubernetes adapter
tests run with `make envtest` (needs `KUBEBUILDER_ASSETS`). `make e2e`, `make load` and `make conformance` run
the build-tagged suites on a cluster.

## Documentation

For people who deploy, configure and call the service ([user-docs/](user-docs/README.md)):

- [Getting started](user-docs/getting-started.md)
- [Configuration reference](user-docs/configuration.md)
- [Usage examples](user-docs/usage-examples.md)
- [Authentication, limits and CORS](user-docs/authentication-and-limits.md)
- [Deployment](user-docs/deployment.md)
- [Observability](user-docs/observability.md)
- [Running afs-reconciler](user-docs/reconciler.md)
- [Reports and usage](user-docs/reports.md)
- [Costs, reconciliation and prices](user-docs/billing.md)
- [Work orders and swarm templates](user-docs/work-orders.md)
- [Export, import and restore](user-docs/export-import-restore.md)

For contributors: [docs/technical-details.md](docs/technical-details.md) (how it is built),
[docs/architectural-decision-record.md](docs/architectural-decision-record.md) (why), the API contract
[api/openapi.yaml](api/openapi.yaml), and the cross-repo design in the root repository.

Contributor and agent rules: [AGENTS.md](AGENTS.md). Why this repository exists: [INTENT.md](INTENT.md).
