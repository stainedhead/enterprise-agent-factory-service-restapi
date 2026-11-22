# Getting started

This repository builds four binaries. You run them on EKS in production (see [Deployment](deployment.md)); on a
workstation you build them, point `afs-api` at a PostgreSQL 17 database and call the API with curl.

| Binary | Purpose |
|---|---|
| `afs-api` | The REST API: authentication, role-based access, audit. Every client uses it. |
| `afs-reconciler` | Applies desired state to EKS and AWS, reverts drift, runs housekeeping jobs. |
| `afs-gateway` | Bedrock proxy that meters tokens and enforces hard budget caps. |
| `afs-billing` | Cron jobs: CUR ingest, reconciliation, price sync, scheduled reports, showback. |

## Prerequisites

- Go 1.27 (the version is pinned in `go.mod`; an older Go downloads the right toolchain) and `make`.
- PostgreSQL 17 reachable with two logins: the schema owner (`afs_owner`) and the API login (`afs_app`).
  The roles come from the database repository: run `afs-migrate bootstrap-roles` once as a DBA, then create the
  LOGIN users. See [Deployment](deployment.md).
- For the reconciler and gateway: an EKS cluster, an AWS account and the environment in
  [Configuration](configuration.md). `afs-api` alone needs only the database.

## Build and run the API

```bash
make build                      # binaries in ./bin
export AFS_DATABASE_URL='postgres://afs_app@localhost:5432/afs?sslmode=disable'
export AFS_DATABASE_OWNER_URL='postgres://afs_owner@localhost:5432/afs?sslmode=disable'
./bin/afs-api --addr :8080
```

At startup `afs-api` runs the schema migrations it embeds, under an advisory lock, as the owner login, then
serves as `afs_app`. It refuses to start on a schema it does not know. Check it:

```bash
curl -s localhost:8080/healthz
curl -s localhost:8080/v1/sign-in-providers      # public; lists the enabled identity providers
curl -s localhost:8080/v1/openapi.yaml           # public; the contract
```

Every other call needs a bearer token: an identity provider JWT, or an API key that starts with `afsk_`
(see [Authentication, limits and CORS](authentication-and-limits.md)). Examples of calls are in
[Usage examples](usage-examples.md).

## Run the checks

```bash
make gate        # tidy, fmt-check, vet, openapi and contract checks, lint, race tests, build
make cover       # coverage floor of 90% on domain, usecase and authz
export AFS_TEST_DATABASE_URL='postgres://postgres@localhost:5432/afs_test?sslmode=disable'
make test-db     # integration tests against a real PostgreSQL (they skip when the variable is unset)
make envtest     # Kubernetes adapter tests; needs KUBEBUILDER_ASSETS
```

No test talks to a real AWS account. The `e2e`, `load` and `conformance` suites are build-tagged and run on a
development cluster (`make e2e`, `make load`, `make conformance`).

## Next

- [Configuration reference](configuration.md): every environment variable and service setting.
- [Usage examples](usage-examples.md): create a team, an agent, a secret and a budget; read a report.
- [Deployment](deployment.md): Terraform and the Helm chart.
