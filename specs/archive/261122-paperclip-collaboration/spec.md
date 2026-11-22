# Spec: Paperclip Team Collaboration

Source: [paperclip-collaboration-PRD.md](paperclip-collaboration-PRD.md).

## Scope
In: collaboration API and domain, reconciler adapters and render, admission policy, `afs-edge`, Helm, Terraform, user-docs.
Out: see PRD non-goals and the Parked table in implementation-notes.md.

## Requirements and acceptance criteria
See PRD AC-1..AC-8. Traceability: RA-17..22 -> internal/usecase/collab, adapter/httpapi/h_collab.go; RC-21..30 -> adapter/k8s/render, admission.go, adapter/aws; EDGE-1..7 -> usecase/edge, adapter/edge*; DP-2..4 -> deploy/.

## Edge decision order
Host, reserved paths, class (cookie=web, X-Afs-Authorization=API, both=400), surface, identify, `collab.Decide`, limiter (20 rps, burst 40), body limits and scan. 60 s decision cache, 10 s negative cache, route table stale after 5 min (503).

## Edge cases
Unknown host, empty sign-in state, expired session, compressed bodies (refused), WebSocket oversize and protocol errors, access-count buffer overflow (dropped, counted).
