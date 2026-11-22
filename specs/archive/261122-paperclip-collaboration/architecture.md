# Architecture

Clean Architecture holds: `usecase/collab` and `usecase/edge` define ports; adapters `edgedb` (pgx, role afs_edge), `edgehttp` (proxy, WebSocket), `edgesession` (sealing), `edgelog`, `oidc/codeflow` implement them; `cmd/afs-edge` wires.
Flow: ALB -> afs-edge (HTTPS 8443) -> team Paperclip Service. Reconciler writes route data and Paperclip resources; the admission CEL policy `afs-protect-paperclip-service` blocks tampering.
Deploy: Helm edge Deployment, PDB, NetworkPolicy, TargetGroupBinding, Certificate, ServiceMonitor; Terraform `collab-edge` and pod-identity roles.
