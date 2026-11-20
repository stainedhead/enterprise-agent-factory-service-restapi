# Technical details

## Toolchain

Go 1.27.4, golangci-lint v2.15.0, govulncheck v1.8.0, PostgreSQL 17.

## Package dependency rule

`cmd/*` → `internal/adapter/*` → `internal/usecase` → `internal/domain`; `internal/authz` is imported by
`usecase` and `adapter/httpapi`. `domain` imports only the standard library.

## Configuration

Environment variables, prefixed `AFS_`. `afs-api`: `AFS_API_ADDR` (default `:8080`).

## Database

Schema and migrations come from `enterprise-agent-factory-service-db` (Go module, tagged releases).
Integration tests use `AFS_TEST_DATABASE_URL`.
