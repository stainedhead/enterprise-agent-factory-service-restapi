# enterprise-agent-factory-service-restapi

Control plane for the Enterprise Agent Factory Service, in Go: the REST API with role-based access, the
reconciler that applies desired state to EKS and AWS, the Bedrock metering gateway that enforces hard
budget caps, and daily billing reconciliation. Owns the API contract `api/openapi.yaml`.

Part of [enterprise-agent-factory-service](https://github.com/stainedhead/enterprise-agent-factory-service);
requirements in its initial PRD and architecture in its `docs/solution-design.md`.

**Status:** scaffolded. Binaries start and stop; the role/permission matrix (`internal/authz`) is
implemented and tested. No API resource is built yet.

## Binaries

| Binary | Purpose |
|---|---|
| `afs-api` | REST API, RBAC enforcement, audit |
| `afs-reconciler` | Applies desired state to EKS and AWS, reverts drift |
| `afs-gateway` | Bedrock proxy: per-team token metering and hard caps |
| `afs-billing` | Daily CUR reconciliation and EKS cost attribution |

## Build and test

Requires Go 1.27 (pinned in `go.mod`; older Go fetches it) and `make`.

```bash
make gate     # tidy, fmt-check, vet, golangci-lint, race tests, build
make build    # binaries in ./bin
./bin/afs-api --addr :8080
curl localhost:8080/healthz
```

Contributor and agent rules: [AGENTS.md](AGENTS.md). Why this repository exists: [INTENT.md](INTENT.md).
