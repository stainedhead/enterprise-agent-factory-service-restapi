# Technical details

## Toolchain

Go 1.27.4, golangci-lint v2.15.0, govulncheck v1.8.0, PostgreSQL 17.

## Package dependency rule

`cmd/*` → `internal/adapter/*` → `internal/usecase` → `internal/domain`; `internal/authz` is imported by
`usecase` and `adapter/httpapi`. `domain` imports only the standard library.

## Configuration

Environment variables, prefixed `AFS_`. `afs-api`: `AFS_API_ADDR` (default `:8080`), `AFS_DATABASE_URL`,
`AFS_DATABASE_OWNER_URL`, `AFS_DB_IAM_AUTH` (RDS IAM tokens; needs `AFS_AWS_REGION`), `AFS_METRICS_ADDR`,
`AFS_API_REPLICAS`, `AFS_TRUSTED_PROXY_HOPS`. `afs-reconciler` adds `AFS_EVENT_BUS_NAME`, `AFS_CLUSTER_NAME`
and `AFS_RECONCILER_USERS`; see `user-docs/reconciler.md`.

## Request pipeline and idempotency

Outer: metrics (RED). Then authentication, rate limit, CORS, If-Match, authorization and the suspended-team
guard. Idempotency is not middleware: each mutating use case claims and completes its `Idempotency-Key` inside
its own transaction (`acting.Idem`), so a crash cannot leave a mutation without its stored response.

## Reconciler

Engine (queue, backoff, leader-only jobs) with team environment, admission, lifecycle finalizer
(`team.Hooks.Deleting`) and housekeeping (outbox publisher, partitions, idempotency purge). Status goes to
`resource_status` through `StatusWriter`; `ConditionStore` adapts it for the finalizer.

## Database

Schema and migrations come from `enterprise-agent-factory-service-db` (Go module, tagged releases).
Integration tests use `AFS_TEST_DATABASE_URL`. The module is pinned to -db a6ecc3c (migrations 000001-000017);
`afs.schema_version()` is checked at startup. Envtest suites run with `-tags envtest` and `KUBEBUILDER_ASSETS`.
