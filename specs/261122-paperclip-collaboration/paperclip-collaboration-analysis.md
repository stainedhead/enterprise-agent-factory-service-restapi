# Process analysis

Flow: C1 to C4 delivered in four milestones, each with a green gate and a push. The edge milestone was the largest (usecase 98.5% coverage, adapters, WebSocket policing).
Friction: lint (gosec, noctx, bodyclose) cost several rounds; race conditions in edgehttp tests; CI envtest exposed a policy gap that local unit tests could not (fixed by running envtest locally).
Lessons: run envtest before pushing admission changes; add MaxHeaderBytes by default on every http.Server; keep decisions in implementation-notes as they are made.
Numbers: domain 91.4%, authz 99.2%, domain/collab 97.6%, usecase/edge 98.5%.
