# Implementation notes

## Decisions
- Edge serves HTTPS :8443 with a cert-manager certificate; health `/readyz` on ops port 9090.
- Sealing keys come from a K8s Secret; Terraform creates only an empty Secrets Manager container (no value in state). 30-day rotation is an operator procedure.
- Collaboration client secrets live under `<prefix>/idp/*` (or `collab_client_secret_name_pattern`).
- X-Forwarded-For is set from the peer address; compressed bodies refused; WebSocket frame policing in-house.
- Fluent Bit routing shipped as a ConfigMap rule; ALB subnet health-check rule in the NetworkPolicy.
- EndpointReady derives from pod readiness plus route checks; unlink detection is in memory.
- PAPERCLIP_HOME=/data/paperclip; KMS statement only when AFS_KMS_KEY_ARN is set; board key policy allows PutSecretValue only; connected-worker ingress uses a pod label.
- Admission policy allows Services with no selector.

## Root contract follow-ups
- -db: default-partition blocking; reconciler ensures partitions before edge writes.
- Switch go.mod to -db v1.0.1 when J pushes that tag; keep the pseudo-version meanwhile.
- Root docs should link https://github.com/stainedhead/software-factory-manager and https://github.com/stainedhead/agentic-teams (root repo not edited here).
- Board key CreateSecret question; DP-2/DP-3 HTTPS 8443 followed; PRD state key in Secrets Manager with rotation implemented as container only; /readyz on 9090.

## Parked
| Item | Reason |
|---|---|
| Paperclip HTTP client, managed registration (RC-25), board key finder, roster guard (RC-26), PaperclipReady probe | ports only; needs live Paperclip |
| CollaborationKeyMissing condition | needs the client |
| Export/import of collaboration resources | P2 |
| Collaboration report kind (RA-21), member sync | P2 |
| Sealing key rotation automation | operator procedure for now |
| Real AWS/EKS validation | needs cluster |

## Release
Version 1.0.1 in lockstep with -db; J must push tag v1.0.1.
