# Architectural decision record

Decisions local to this repository. Cross-repo decisions live in the root repository's
`docs/architectural-decision-record.md`. Status values: Accepted, Proposed, Superseded.

## ADR-001 One Go module for all control-plane services

- Status: Accepted (J, 2026-11-20)
- Context: the API, reconciler, gateway and billing job share domain types, authz and the store. The CLI
  and the schema have their own repositories.
- Decision: one module, four `cmd/` entry points, shared `internal/` packages.
- Consequences: one CI and release pipeline; binaries version together.

## ADR-002 Standard library HTTP routing

- Status: Proposed
- Context: Go's `net/http` ServeMux supports method and path-parameter patterns.
- Decision: use `http.ServeMux` with a thin middleware chain (request id, auth, authz, logging) instead
  of a router framework.
- Consequences: fewer dependencies; route permissions are declared in our own route table.
