# Paperclip Team Collaboration (restapi): Change PRD

Refines the living PRD `specs/archive/261121-restapi-feature-complete/enterprise-agent-factory-service-restapi-PRD.md`
(RA-17..22, RC-21..30, EDGE-1..7, DP-2..4, INF-US-1, RD-1..23). Schema from `-db` migration 000020 (`-db` v1.0.1).

## Problem
Teams need a shared Paperclip collaboration surface per team, reachable from a browser and by API, with access decided by AFS roles, not by Paperclip.

## Goals
1. Collaboration API: settings, links, access, endpoint status, swarm collaboration (RA-17..22), RBAC in `internal/authz`, audit and outbox in the same transaction.
2. Reconciler: renders Paperclip per team, admission policy protecting its Service, IAM for board key and edge, WAF sync (RC-21..30).
3. `afs-edge`: one edge service deciding every collaboration request (host, class, identity, role, limits, scan), OIDC sign-in, WebSocket policing (EDGE-1..7).
4. Deployment: Helm edge workload, ALB target group binding, cert-manager certificate, Terraform `collab-edge` module (DP-2..4).

## Non-goals
Real AWS/EKS validation, Paperclip HTTP client and managed registration (RC-25/26 ports only), collaboration report kind (RA-21 P2), member sync, export/import of collaboration resources.

## Acceptance criteria
- AC-1 Every collaboration route declares a permission with allowed and denied tests.
- AC-2 `afs-edge` connects as `afs_edge` and uses only the ten SECURITY DEFINER functions.
- AC-3 Ambiguous credentials (cookie and API header) are refused with 400.
- AC-4 Access-only callers cannot send WebSocket data frames (close 1008); frames over 1 MiB close 1009.
- AC-5 No secret value is logged, stored or returned; cookies are `__Host-`, Secure, HttpOnly.
- AC-6 Admission policy refuses unprotected changes to the Paperclip Service (envtest).
- AC-7 Helm and Terraform tests pass; `make gate`, cover (>=90% domain/usecase/authz), contract-check and openapi-check pass.
- AC-8 Release 1.0.1 in lockstep with `-db`.

## Milestones
C1 API and domain, C2 reconciler adapters, C3 edge, C4 deploy and docs.
