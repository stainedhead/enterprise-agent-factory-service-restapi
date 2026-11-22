# Tasks

- [x] C1 collaboration API, domain, authz, audit/outbox, openapi
- [x] C2 reconciler adapters, Paperclip render, admission, IAM, WAF sync, swarm collaboration
- [x] C3 afs-edge usecase, adapters, command, metrics
- [x] C4 Helm edge chart, Terraform collab-edge, user-docs, ADR-023..027
- [x] CI envtest fix (selector-less Service)
- [x] Review: MaxHeaderBytes on the edge server (P2)
- [x] C5.1 Paperclip HTTP client adapter (`internal/adapter/paperclip`) behind `collab.Paperclip`, fake `papertest`, contract tests
- [x] C5.2 Managed registration (RC-25, COL-12): `collab.Registrar`, Secrets Manager `KeyStore`, `ManagedSource`, RegistrationHealthy, log-capture test
- [x] C5.3 Roster guard (RC-26) and content scan (COL-25) wired every 10 min with dedupe; `Guard` reuses `domain/collab.Scan`
- [x] C5.4 PaperclipReady probe with OpenSignUp (COL-27)
- [x] C5.5 First admin invite by email and `bootstrap_completed` (COL-32); member sync (COL-33)
- [x] C5.6 Terraform: reconciler writes worker keys only at `*/paperclip-agent-api-key-??????`
- [x] C5.7 `TestStreamFailsPartWay` flake fixed (wire-level test)
- [x] C5.8 Collaboration report kind (RA-21, COL-36)
- [x] C5.9 Export/import of collaboration settings (CFG-5, COL-26)
- [ ] C5.10 Review, archive, analysis, pr-body
- [ ] Parked (real AWS only): see implementation-notes.md
