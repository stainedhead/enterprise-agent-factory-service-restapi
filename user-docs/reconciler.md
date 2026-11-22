# Running afs-reconciler

`afs-reconciler` applies the desired state in PostgreSQL to EKS and AWS. Run two or more replicas: one
leads (a `Lease` named `afs-reconciler`), the others stand by and take over within 30 seconds.

## What it does for a team

For each active team it applies, and keeps correct:

- the namespace `afs-t-<slug>` with Pod Security `restricted` enforced, audited and warned
- a ResourceQuota and LimitRange from the team's quotas
- default-deny ingress and egress NetworkPolicies, and Cilium policies that allow DNS and `afs-gateway`
- an External Secrets `SecretStore` bound to the team's IAM role, and a copy of the gateway CA bundle
- the team's IAM roles (`afs-<ulid>` under `/afs/<namespace>/`, the Bedrock role trusting only the
  gateway) and one Bedrock application inference profile per enabled model

It also applies, once per cluster, the Cilium policy on `afs-gateway` that admits only pods of team
namespaces, and the admission policies that refuse, inside team namespaces, network policy changes by
anyone but the reconciler, `LoadBalancer` and `NodePort` services, host networking, host PID and IPC,
`hostPath` volumes, privileged containers, changes to `afs.stainedhead.io/*` labels and another team's
dedicated nodes.

An object edited or deleted by someone else is put back within 60 seconds, and the correction is
recorded in the audit log and as a `drift.corrected` event. Team status (`Ready`, `Degraded`, `Failed`)
is in `resource_status` with the conditions `NamespaceReady`, `IamReady` and `Ready`.

## Configuration

| Variable | Required | Meaning |
|---|---|---|
| `AFS_DATABASE_URL` | yes | Login as `afs_reconciler`; on RDS without a password (IAM authentication) |
| `AFS_AWS_REGION` | yes | Region of the cluster's account |
| `AFS_GATEWAY_ROLE_ARN` | yes | The only principal the team Bedrock roles trust; also names the account |
| `AFS_RECONCILER_USERS` | yes | Comma-separated Kubernetes usernames of this service (the EKS access entry's username) |
| `AFS_PERMISSIONS_BOUNDARY_ARN` | no | Boundary attached to every role it creates |
| `AFS_CLUSTER_NAME` | yes | The EKS cluster; narrows the Pod Identity trust and names the cluster for access entries |
| `AFS_EVENT_BUS_NAME` | yes | EventBridge bus the outbox publisher writes to |
| `AFS_DB_IAM_AUTH` | no | `true` to log in to RDS with IAM tokens instead of a password |
| `AFS_ESO_ROLE_ARN` | no | External Secrets Operator's role, trusted by team roles |
| `AFS_SECRETS_PREFIX` | no | Secrets Manager path prefix; default `afs` |
| `AFS_LEADER_LEASE_NAMESPACE` | no | Namespace of the Lease; default `afs-system` |
| `AFS_GATEWAY_NAMESPACE`, `AFS_GATEWAY_NAME`, `AFS_GATEWAY_CA_CONFIGMAP` | no | Where `afs-gateway` and its CA bundle are; defaults `afs-system`, `afs-gateway`, `afs-gateway-ca` |
| `AFS_POLL_INTERVAL`, `AFS_RESYNC_INTERVAL` | no | Backstop poll (30s) and full resync (10m) |
| `AFS_METRICS_ADDR` | no | Probe and metrics address; default `:9090` |
| `POD_NAME` | no | Leader election identity; the hostname if unset |

The service refuses to start on a database whose schema is older than the build expects.

## Probes and metrics

On `AFS_METRICS_ADDR`: `/healthz` (process up), `/readyz` (database reachable; the body shows whether this
replica leads) and `/metrics` (leader, reconciles by result, queue depth, lag, last poll and resync).

## Troubleshooting

- A team stays `Degraded`: read the condition on the team (`GET /v1/teams/{teamId}`); its reason is the AWS
  or Kubernetes error code. `GatewayCAMissing` means the CA ConfigMap is not in the gateway namespace.
- Nothing reconciles: check `/readyz` on each replica and that one reports `"leader":true`.
