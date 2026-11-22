# Configuration reference

Every binary reads environment variables prefixed `AFS_`; the Helm chart sets them from values and Terraform
outputs. Booleans accept `true` and `false`; durations are Go durations (`30s`, `5m`). A required variable that
is missing stops the binary at start with a message naming it. Settings that operators change at runtime live in
the database as service settings (last section), not in the environment.

## Common to all binaries

| Variable | Default | Meaning |
|---|---|---|
| `AFS_DATABASE_URL` | none (required) | Connection string of the binary's own database login (`afs_app`, `afs_reconciler`, `afs_gateway` or `afs_billing`). No password: use IAM authentication in AWS. |
| `AFS_DB_IAM_AUTH` | `false` | Sign an RDS IAM token as the password. Needs `AFS_AWS_REGION`. |
| `AFS_AWS_REGION` | none | AWS region. |
| `AFS_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON lines; secret values and `Authorization` headers are never logged. |
| `AFS_METRICS_ADDR` | `:9090` | Prometheus metrics and probe address. |
| `AFS_OTLP_ENDPOINT` | empty | OTLP/HTTP base URL for traces, for example `http://collector:4318`. Empty turns tracing off. |
| `AFS_AWS_ENDPOINT_URL` | empty | Points every AWS client at one endpoint. For local fakes only. |
| `AFS_ALLOW_STATIC_CREDENTIALS` | `false` | Allows static AWS credentials. Off in production. |

## afs-api

| Variable | Default | Meaning |
|---|---|---|
| `AFS_API_ADDR` (`--addr`) | `:8080` | Listen address. |
| `AFS_DATABASE_OWNER_URL` | none | Owner login used only to run migrations at start. |
| `AFS_MIGRATION_LOCK_TIMEOUT` | `10s` | How long to wait for the migration advisory lock. |
| `AFS_API_REPLICAS` | `1` | Replica count; the per-principal rate limit is divided by it. |
| `AFS_TRUSTED_PROXY_HOPS` | `0` | Trusted proxies in front of the API, for client addresses in `X-Forwarded-For`. |
| `AFS_PAGE_TOKEN_KEY` | random per process | At least 32 bytes, shared by every replica, so a page token from one replica works on another. Set it. |
| `AFS_CLUSTER_NAME`, `AFS_CLUSTER_ENDPOINT`, `AFS_CLUSTER_CA_DATA` | none | Cluster identity for `GetTeamKubeconfig`; without endpoint and CA data it answers `503`. |
| `AFS_CLUSTER_OIDC_ISSUER` | none | Issuer of projected ServiceAccount tokens (workload principal). Unset: workload tokens are refused. |
| `AFS_AWS_ACCOUNT_ID` | none | Account used to derive role ARNs for identity bindings. |

## afs-reconciler

| Variable | Required | Meaning |
|---|---|---|
| `AFS_DATABASE_URL`, `AFS_GATEWAY_ROLE_ARN`, `AFS_EVENT_BUS_NAME`, `AFS_CLUSTER_NAME`, `AFS_RECONCILER_USERS` | yes | Database login; the gateway's IAM role (the only principal the team Bedrock roles trust, and the source of the account id); the EventBridge bus the outbox publishes to; the EKS cluster name; comma-separated Kubernetes usernames of the reconciler that the admission policies exempt. |
| `AFS_PERMISSIONS_BOUNDARY_ARN` | no | Boundary applied to every role the reconciler creates. |
| `AFS_ESO_ROLE_ARN` | no | External Secrets Operator role trusted by the team role. |
| `AFS_SECRETS_PREFIX` | `afs` | Secrets Manager path prefix. |
| `AFS_GATEWAY_NAMESPACE`, `AFS_GATEWAY_NAME`, `AFS_GATEWAY_CA_CONFIGMAP` | `afs-system`, `afs-gateway`, `afs-gateway-ca` | Where the gateway runs. |
| `AFS_LEADER_LEASE_NAMESPACE` | `afs-system` | Namespace of the leader `Lease`. |
| `AFS_POLL_INTERVAL`, `AFS_RESYNC_INTERVAL` | built-in | Stale-generation poll and full resync intervals. |
| `AFS_RECONCILER_ROLE_ARN` | no | The reconciler's role, trusted by agent roles for role-link verification. |
| `AFS_EXPORT_BUCKET`, `AFS_EXPORT_KMS_KEY_ID` | no | Turn on the export bundle builder; both are then required. |
| `AFS_VIEWER_KUBECTL_ROLES` | no | Federated role per identity provider (or `default`) for viewer kubectl access. Unset: no access entries. |
| `AFS_ORG_MIRROR` | on | `off` disables the hourly AWS Organizations account mirror. |
| `AFS_EFS_STORAGE_CLASS` | `efs-sc` | Storage class for pools with a shared volume. |
| `AFS_NODECLASS_JSON` | none | Base `EC2NodeClass` spec for dedicated-tier teams. |
| `AFS_WORK_ORDER_QUEUE_URL` | none | SQS queue fed by the `afs.work_order.requested` EventBridge rule. Unset: work order intake is off. |
| `AFS_GITHUB_APPS_FILE` | none | GitHub App credentials file for tracking pull requests of work orders. |
| `AFS_NOTIFY_TARGET_ROLE_ARN`, `AFS_NOTIFY_DLQ_ARN`, `AFS_NOTIFY_EMAIL_QUEUE_ARN`, `AFS_NOTIFY_EMAIL_FROM` | no | Notification delivery (EventBridge targets, dead-letter queue, email queue, SES sender). |
| `AFS_HUBBLE_ADDR`, `AFS_HUBBLE_TLS` | no | Hubble relay for external traffic samples. |
| `AFS_LOG_AGGREGATOR_IMAGE`, `AFS_LOG_ROUTE_NAMESPACE`, `AFS_LOG_REDACTION_KEY` | no | Log destination aggregator. |
| `AFS_KUBECONFIG`, `POD_NAME` | no | Out-of-cluster development; leader election identity (default hostname). |

More detail on each is in [Running afs-reconciler](reconciler.md).

## afs-gateway

| Variable | Default | Meaning |
|---|---|---|
| `AFS_GATEWAY_ROLE_ARN` | required | The gateway's own role; names the account. |
| `AFS_GATEWAY_TLS_CERT`, `AFS_GATEWAY_TLS_KEY` | required | Certificate and key files; reloaded on change. |
| `AFS_GATEWAY_ADDR` | `:8443` | Listen address. |
| `AFS_BEDROCK_VPCE_URL` | regional endpoint | Bedrock Runtime VPC endpoint URL. |
| `AFS_GATEWAY_MAX_BODY` | 20 MB | Request body limit in bytes. |
| `AFS_GATEWAY_CACHE_TTL` | `1m` | Cache lifetime for team, model, price and budget data. |
| `AFS_GATEWAY_CILIUM_CHECK` | `true` | Cross-check the caller's IP against its Cilium endpoint identity. |

## afs-billing

Run one job per CronJob: `afs-billing --job <name>`.

| Variable | Default | Used by |
|---|---|---|
| `AFS_CUR_DATABASE`, `AFS_CUR_TABLE`, `AFS_ATHENA_WORKGROUP`, `AFS_ATHENA_OUTPUT` | none | `cur-ingest`. |
| `AFS_CUR_TAG_PREFIX` | `user_` | CUR tag column prefix. |
| `AFS_BILLING_LOOKBACK_DAYS` | `3` | Days ending yesterday a CUR job covers (1 to 31). |
| `AFS_PRICING_REGION` | `us-east-1` | Price List API region (`price-sync`). |
| `AFS_REPORTS_BUCKET`, `AFS_REPORTS_KMS_KEY_ID` | none | `report-schedules` and `showback`. |
| `AFS_SHOWBACK_TAG_KEY` | empty | Tag key of the second showback dimension. |

See [Costs, reconciliation and prices](billing.md).

## Service settings

Service administrators change these with the settings operations; they are cached for 30 seconds. Examples:
`api.cors_origins` (browser origins allowed to call the API), `secrets.path_prefix`, `billing.variance_alert_pct`,
`pricing.price_list_sync`. `GET /v1/admin/settings` lists every key with its default and current value.
