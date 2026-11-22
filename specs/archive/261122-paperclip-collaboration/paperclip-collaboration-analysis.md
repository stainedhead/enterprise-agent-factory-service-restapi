# Process analysis

Flow: C1 to C4 delivered in four milestones, each with a green gate and a push. The edge milestone was the largest (usecase 98.5% coverage, adapters, WebSocket policing).
Friction: lint (gosec, noctx, bodyclose) cost several rounds; race conditions in edgehttp tests; CI envtest exposed a policy gap that local unit tests could not (fixed by running envtest locally).
Lessons: run envtest before pushing admission changes; add MaxHeaderBytes by default on every http.Server; keep decisions in implementation-notes as they are made.
Numbers: domain 91.4%, authz 99.2%, domain/collab 97.6%, usecase/edge 98.5%.

## C5 follow-up (parked in-scope items)
Flow: eight items closed in three commits (client and registration, report and export/import, key-missing condition), each with a green gate. The Paperclip API surface had no source, so the fake defines it; that is the main risk to check against a live Paperclip.
Friction: golangci-lint (gosec, staticcheck QF1008) and gofmt cost one round; the local PostgreSQL 16 needed a reconciler role setting to exercise the registration guard; RC-22's CollaborationKeyMissing was missed in the first pass and found by re-reading the PRD for named conditions.
Lessons: grep the PRD for every named condition and test before declaring an item done; reproduce flakes at the wire first (the Bedrock flake was a test race, not product code).
Numbers: collab usecase package 91.0%, all gated packages at or above 90%.
