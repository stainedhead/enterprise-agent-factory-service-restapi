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
- for a team set to `per_namespace` (1.2.0): its own `afs-gateway` (ServiceAccount, Deployment of 2 replicas, Service on
  8443, PodDisruptionBudget, serving Certificate), ExternalSecrets `afs-gateway-credentials` (one key per route) and
  `afs-gateway-db`, and the CiliumNetworkPolicy `afs-gateway-policy` that lets only that gateway reach the route hosts;
  agents' gateway traffic goes to it. Route health is in `resource_status` (kinds `team_route`, `team_gateway`). Back to
  `shared`, these objects are removed. Needs `AFS_TEAM_GATEWAY_*` (see [configuration](configuration.md)) and, per team,
  the gateway ServiceAccount's Pod Identity association (Terraform) while Bedrock is used. See
  [model routes](model-routes.md).

Tags on AWS resources follow the team's effective tag set (system tags, service tags, team tags; at most 40
keys). Roles, Pod Identity associations, inference profiles, snapshots and NodePools are tagged by their own
controllers. The `tag_sync` resource of each team keeps the team's Secrets Manager secrets and its pool inbox
queues (`afs-<team>-<hash>`) in step: a changed team or service tag, or a renamed agent or pool, is applied
(`TagResource`, `TagQueue`), and a tag AFS applied earlier and the team no longer sets is removed (`UntagResource`,
`UntagQueue`). Tags that someone else put on a resource are left alone. A secret scheduled for deletion and a
queue that does not exist (a pool that receives no handoffs has none) are skipped. A set over the cap applies
nothing to that resource and shows `TagsApplied` false with `TagLimitExceeded`. The reconciler's role needs
`secretsmanager:DescribeSecret`, `TagResource` and `UntagResource` on the secrets path, and
`sqs:ListQueueTags`, `TagQueue` and `UntagQueue` on `afs-*` queues; the Terraform `pod-identity` module grants them.

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
| `AFS_NODECLASS_JSON` | no | Base `EC2NodeClass` spec (role, subnet and security group selectors, AMI family) that every dedicated-tier team's node class copies, as JSON; the team's tags are merged in |
| `AFS_WORK_ORDER_QUEUE_URL` | no | SQS queue that receives `afs.work_order.requested` events from EventBridge; without it work order intake is off (see [work-orders.md](work-orders.md)) |
| `AFS_GITHUB_APPS_FILE` | no | JSON file naming the GitHub Apps (host, organization, app id, installation id, private key file) the reconciler uses to read pull request state; without it repository tracking is off |
| `AFS_POLL_INTERVAL`, `AFS_RESYNC_INTERVAL` | no | Backstop poll (30s) and full resync (10m) |
| `AFS_METRICS_ADDR` | no | Probe and metrics address; default `:9090` |
| `POD_NAME` | no | Leader election identity; the hostname if unset |

### Viewer kubectl

A service admin can turn on `viewerKubectl` for a team. Its viewers then get a kubeconfig from
`GetTeamKubeconfig` and can run read-only `kubectl` in the team's namespace. Which IAM role the team's people
arrive with is configuration, because it depends on how your identity provider federates into AWS:

| Variable | Required | Meaning |
|---|---|---|
| `AFS_EFS_STORAGE_CLASS` | no | Storage class of a pool's shared EFS volume (default `efs-sc`) |
| `AFS_VIEWER_KUBECTL_ROLES` | to enable | JSON object from an identity provider id (`idp_...`), or `default` for the default provider, to the ARN of the IAM role that provider federates to for kubectl: `{"default":"arn:aws:iam::111122223333:role/afs-viewers"}`. Unset: no access entry is created for anyone |

For each team with the flag on, the reconciler keeps a `Role` named `afs-viewer` in the team namespace that
can `get`, `list` and `watch` pods, pod logs, events, services, workloads and the agents' custom resources,
and nothing else (no secrets, no `exec`, `attach` or `port-forward`, no write), and a `RoleBinding` to a group
that is specific to the federated role. It also keeps one EKS access entry per role, tagged
`afs-managed-by=viewer-kubectl`, mapped to that group with no access policy. Turning the flag off deletes the
`RoleBinding` and `Role` on the next pass (within seconds) and removes the access entry when no other team
uses the role. An access entry for the same role that AFS did not create is never changed: the team shows an
`AccessEntryNotManaged` error until you remove it or use another role.

An access entry belongs to a role, not to a person, so everyone who signs in through a role can reach every
team namespace whose `RoleBinding` names that role's group. Give teams that must not see each other
different identity providers and roles. The EKS access entry takes the role ARN without an IAM path (for an
SSO permission set, the real role ARN under `aws-reserved/sso.amazonaws.com`, minus that path). The
reconciler needs `eks:CreateAccessEntry`, `UpdateAccessEntry`, `DeleteAccessEntry`, `DescribeAccessEntry`,
`ListAccessEntries` and `eks:TagResource`, and the chart's `afs-reconciler-viewer-kubectl` ClusterRole
(`reconciler.viewerKubectl.enabled`, on by default).

### Notification channels

Each notification channel becomes one EventBridge rule on `AFS_EVENT_BUS_NAME`, filtered on the channel's team and event types. Delivery is off, with a warning, until `AFS_NOTIFY_TARGET_ROLE_ARN` is set.

| Variable | Required | Meaning |
|---|---|---|
| `AFS_NOTIFY_TARGET_ROLE_ARN` | to enable | Role EventBridge assumes to invoke the API destinations (Slack, webhook, Jira) and event bus targets |
| `AFS_NOTIFY_EMAIL_QUEUE_ARN` | for email channels | FIFO queue (content-based deduplication on) that email rules target; the reconciler drains it and sends with SES once per event id. Its policy must let `events.amazonaws.com` send |
| `AFS_NOTIFY_DLQ_ARN` | recommended | Dead-letter queue of the rule targets; failed deliveries after EventBridge's retries (20 attempts or 2 hours) show on the channel as `DeliveryHealthy=False` and raise `notification.delivery_failed` |
| `AFS_NOTIFY_EMAIL_FROM` | for email channels | Verified SES sender address; also used by *Send test* on an email channel |

The reconciler's role needs `events:PutRule`, `PutTargets`, `RemoveTargets`, `DeleteRule`, `ListRules`, `ListTargetsByRule`, the connection and API destination actions on `afs-nch_*` objects, `sqs:ReceiveMessage` and `DeleteMessage` on the two queues, `ses:SendEmail`, and `secretsmanager:GetSecretValue` on the team secret path only: it reads a channel's credential into memory to build the connection and to run a test, and never logs or stores it. A Slack webhook URL is that credential; it is handed to EventBridge as the API destination endpoint and is never stored in the database.

A channel with no event types receives every event family of its team except `notification.*`; `report.*` events reach only the channel a schedule addresses. A service-level channel with no event types has no rule (it is an address book entry, for example for work order mail).

The service refuses to start on a database whose schema is older than the build expects.

## Probes and metrics

On `AFS_METRICS_ADDR`: `/healthz` (process up), `/readyz` (database reachable; the body shows whether this
replica leads) and `/metrics` (leader, reconciles by result, queue depth, lag, last poll and resync).

## Troubleshooting

- A team stays `Degraded`: read the condition on the team (`GET /v1/teams/{teamId}`); its reason is the AWS
  or Kubernetes error code. `GatewayCAMissing` means the CA ConfigMap is not in the gateway namespace.
- Nothing reconciles: check `/readyz` on each replica and that one reports `"leader":true`.
