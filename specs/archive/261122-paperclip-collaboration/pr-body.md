## Paperclip team collaboration (restapi) 1.0.1

Adds the collaboration API (RA-17..22), the collaboration report kind (RA-21), export/import of collaboration settings (CFG-5), the Paperclip client with managed registration, roster guard, readiness probe, agent-write scan, first-admin invite and member sync (RC-25, RC-26, COL-12, COL-13, COL-25, COL-27, COL-32, COL-33), the `CollaborationKeyMissing` condition (RC-22), reconciler support (render, admission policy, IAM, WAF sync), the new `afs-edge` proxy (host/class/identity/role decisions, OIDC sign-in, WebSocket policing), Helm edge chart and Terraform `collab-edge`.

Spec: `specs/archive/261122-paperclip-collaboration/`.

**Release:** lockstep with `-db`. J must push tag **v1.0.1** on `-db`; then switch `go.mod` from the pseudo-version `v0.0.0-20261122194543-4281d057a571` to `v1.0.1`.

Tests: race tests, lint, cover (domain 91.4, authz 99.2, usecase/edge 98.4, domain/collab 97.8), envtest, helm and terraform tests, contract-check and openapi-check green. Only real AWS/EKS validation and live Paperclip calls are parked (see implementation-notes.md); the Paperclip API paths are defined by the test fake.

OpenAPI: `api/openapi.yaml` regenerated for the `collaboration` report kind (enum entries on GetTeamReport and GetServiceReport, `Report.kind`, the `GroupBy` pattern for `surface`, `reason`, `person`); no other change.

Security review of `afs-edge`: 14 findings fixed with tests (details and decisions RD-24 to RD-27 in `implementation-notes.md`, "Security review fixes"): cross-origin check, 60 s stream re-authorization, WebSocket and body scan bypasses, upstream handshake validation, bearer audience, bounded caches, sign-in binding, redirect, cookie, method-override and Host hardening. Open: `-db` must return the provider client id from `collab_routes()` (RD-27); J should register `collab.base_domain` on the Public Suffix List or use a dedicated domain. Also: `PutSetting` key enum lists the `collab.*` settings.

Root follow-ups: link software-factory-manager and agentic-teams in root docs.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01ScLWHEMWuWjKHYTmosrfTn
