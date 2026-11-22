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

## C5 decisions (parked in-scope items)
- Paperclip API surface is not specified by the PRD: the client and the `papertest` fake define it (bearer board key; paths in docs/technical-details.md). Errors carry operation and status only, never a body.
- Registration order: create the Paperclip agent, record its id, write the key to Secrets Manager, then `afs.record_collab_agent_key`. A failure after create revokes the Paperclip agent and the link is `failed` with the id cleared. Teardown skips the Paperclip revoke when the collaboration agent is gone (Paperclip died with the pod).
- Keys, the board key and invite URLs are `collab.Key` (redacting); a log-capture test proves none is printed.
- Guard: first scan looks back two intervals; items are deduped by team, id and update time; events carry pattern, count, item id and author, never text.
- Member sync wants role-binding users plus edge sign-ins of the last 30 days; admin-role invites are never touched. The 30-day window is a choice, not in the PRD.
- `collaboration` is a report kind outside the schedulable `ReportKind` enum; it always reads 5-minute rows of `afs.collaboration_access`.
- Export carries `collaboration` (team settings and the collaboration agent's name) and each worker's `connect`; secrets and bindings with `managed_by <> 'user'` are left out; import creates the collaboration agent from the template of the same name and the link starts pending (or disabled).
- `CollaborationKeyMissing`: a `PAPERCLIP_AGENT_API_KEY` binding whose secret has no version is not delivered (render, ExternalSecret and hash), and the agent controller reports the condition for connected workers. Creating the key changes the hash and rolls the agent.
- `TestStreamFailsPartWay` flake: the SDK client raced with the fake closing the connection after the exception frame; the test now reads frames from a raw HTTP response and the SDK test tolerates the closed connection. Fixed with `-count=50 -race`.

## Root contract follow-ups
- -db: default-partition blocking; reconciler ensures partitions before edge writes.
- Switch go.mod to -db v1.0.1 when J pushes that tag; keep the pseudo-version meanwhile.
- Root docs should link https://github.com/stainedhead/software-factory-manager and https://github.com/stainedhead/agentic-teams (root repo not edited here).
- Board key CreateSecret question; DP-2/DP-3 HTTPS 8443 followed; PRD state key in Secrets Manager with rotation implemented as container only; /readyz on 9090.

## Parked
| Item | Reason |
|---|---|
| Paperclip API paths against a live Paperclip | real Paperclip only; the surface is defined by the fake |
| Managed registration, guard, probe, scan, first admin and member sync against a real cluster | real AWS/EKS only (Secrets Manager, SES, pods) |
| Sealing key rotation automation | operator procedure for now |
| Real AWS/EKS validation | needs cluster |

## Release
Version 1.0.1 in lockstep with -db; J must push tag v1.0.1.
