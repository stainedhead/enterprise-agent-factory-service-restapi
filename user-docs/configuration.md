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
| `AFS_API_REPLICAS` | `1` | Replica count; the per-principal rate limit is divided by it. Above 1, `AFS_PAGE_TOKEN_KEY` and `AFS_IDEMPOTENCY_HMAC_KEY` are required and afs-api refuses to start without them. |
| `AFS_TRUSTED_PROXY_HOPS` | `0` | Trusted proxies in front of the API, for client addresses in `X-Forwarded-For`. |
| `AFS_PAGE_TOKEN_KEY` | random per process (one replica only) | At least 32 bytes, the same on every replica, so a page token from one replica works on another. Required when `AFS_API_REPLICAS` is above 1. The chart injects it from the Secret named by `api.keysSecret.name` (default `afs-api-keys`), key `pageTokenKey`, by `secretKeyRef`; it is never a chart value. |
| `AFS_IDEMPOTENCY_HMAC_KEY` | random per process (one replica only) | At least 32 bytes, the same on every replica. Keys the HMAC that fingerprints secret values in idempotency claims. Required when `AFS_API_REPLICAS` is above 1. From the same Secret, key `idempotencyHmacKey`. |
| `AFS_HTTP_READ_TIMEOUT` | `60s` | Time allowed to read a whole request, body included; a slower client is disconnected. `CreateImport` (bundles up to 100 MB) extends its own limit to 10 minutes. |
| `AFS_HTTP_IDLE_TIMEOUT` | `120s` | How long an idle keep-alive connection stays open. |
| `AFS_CLUSTER_NAME`, `AFS_CLUSTER_ENDPOINT`, `AFS_CLUSTER_CA_DATA` | none | Cluster identity for `GetTeamKubeconfig`; without endpoint and CA data it answers `503`. The endpoint must be an https URL and the CA data base64; a bad value stops startup. |
| `AFS_CLUSTER_OIDC_ISSUER` | none | Issuer of projected ServiceAccount tokens (workload principal). Unset: workload tokens are refused. |
| `AFS_MCP_OAUTH_CLIENT_ID`, `AFS_MCP_OAUTH_REDIRECT_URI`, `AFS_CONSOLE_URL` | none | MCP OAuth connect for `oauth_user` servers: the https URL of the environment's OAuth Client ID Metadata Document, the public https URL of `GET /v1/mcp-oauth/callback`, and the console's base URL the callback redirects to. Set all three or none; unset, `StartMcpOAuthConnect` answers `409 oauth_not_configured`. The PKCE verifier is derived from `AFS_PAGE_TOKEN_KEY`, so every replica must share that key (it already must). |
| `AFS_AWS_ACCOUNT_ID` | none | The platform account (12 digits) used to derive role ARNs for identity bindings. A malformed value stops startup. |

Startup validates every variable above; a bad value stops afs-api with exit code 2 and the variable's name.
The request body limit is fixed per route in the API contract (1 MiB, 100 MB for `CreateImport`); it is not a
setting. `/readyz` on the public listener answers `{"status":"not ready","checks":{"database":"unavailable"}}`
and logs the cause with the request id; the detailed checks are on the metrics address (`AFS_METRICS_ADDR`).

### Database authentication combinations

| `AFS_DB_IAM_AUTH` | `AFS_DATABASE_URL` login | `AFS_DATABASE_OWNER_URL` login | Result |
|---|---|---|---|
| `true` | `afs_app` (no password) | `afs_owner` (no password), or unset | The pool signs an RDS IAM token for `afs_app` on every new connection. The startup migration signs one for `afs_owner` with the same credentials. Both roles need `rds_iam` granted. With no owner URL, migrations run out of band with `afs-migrate`. |
| `false` | any, password in the URL if the database wants one | any, password in the URL | Passwords come from the connection strings only, injected from a Secret in the deployment. Not for production RDS. |
| `true` | any | owner URL with a password | The IAM token replaces the URL's password. |

Needs `AFS_AWS_REGION` (or `AWS_REGION`) when `AFS_DB_IAM_AUTH` is `true`.

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
| `AFS_MODEL_SYNC` | on | `off` disables the daily Bedrock model catalogue sync (new models arrive disabled; an admin enables and prices them). |
| `AFS_EFS_STORAGE_CLASS` | `efs-sc` | Storage class for pools with a shared volume. |
| `AFS_NODECLASS_JSON` | none | Base `EC2NodeClass` spec for dedicated-tier teams. |
| `AFS_WORK_ORDER_QUEUE_URL` | none | SQS queue fed by the `afs.work_order.requested` EventBridge rule. Unset: work order intake is off. |
| `AFS_GITHUB_APPS_FILE` | none | GitHub App credentials file for tracking pull requests of work orders. |
| `AFS_NOTIFY_TARGET_ROLE_ARN`, `AFS_NOTIFY_DLQ_ARN`, `AFS_NOTIFY_EMAIL_QUEUE_ARN`, `AFS_NOTIFY_EMAIL_FROM` | no | Notification delivery (EventBridge targets, dead-letter queue, email queue, SES sender). |
| `AFS_HUBBLE_ADDR`, `AFS_HUBBLE_TLS` | no | Hubble relay for external traffic samples. |
| `AFS_LOG_AGGREGATOR_IMAGE`, `AFS_LOG_ROUTE_NAMESPACE`, `AFS_LOG_REDACTION_KEY` | no | Log destination aggregator. |
| `AFS_TEAM_GATEWAY_IMAGE` | for per_namespace teams | Image of the team gateway (`afs-gateway`, pinned by digest). |
| `AFS_TEAM_GATEWAY_DB_SECRET_ARN`, `AFS_TEAM_GATEWAY_DB_HOST`, `AFS_TEAM_GATEWAY_DB_PORT` | for per_namespace teams | Secret holding the team gateway's database URL (synced into the namespace by an ExternalSecret), the database host for its egress rule, and its port (default 5432). |
| `AFS_TEAM_GATEWAY_TLS_ISSUER` | for per_namespace teams | cert-manager issuer of the team gateway's serving certificate. |
| `AFS_TEAM_GATEWAY_MCP_SESSION_KEYS` | none | MCP (1.3.0): one or two comma-separated base64 32-byte master keys, new first while rotating. Each team gateway gets `HMAC-SHA256(master, "afs-mcp-session/" + teamId)` in its `afs-gateway-mcp` Secret. Unset: team gateways serve no `/mcp`. Comes from a Kubernetes Secret (Helm `mcpSessionKeys`), never a value. |
| `AFS_MCP_BRIDGE_IMAGE` | when a team hosts a server | Image of `afs-mcp-bridge` (pinned by digest), copied into each hosted server's pod by an init container. |
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
| `AFS_GATEWAY_TEAM_NAMESPACE` (`--team-namespace`), `AFS_GATEWAY_TEAM_ID` | none | Team gateway mode (1.2.0): serve this namespace only, with model routes. Both or neither; the reconciler sets them. |
| `AFS_GATEWAY_CREDENTIALS_DIR` | `/etc/afs/gateway-credentials` | Team gateway: one file per route slug holding the route credential, reread on change. |
| `AFS_GATEWAY_FEDERATION_TOKEN` | none | Team gateway: the projected ServiceAccount token file used for Azure and Google federation. |
| `AFS_GATEWAY_MCP_SEALING_KEYS` | none | Team gateway (1.3.0): comma-separated base64 32-byte keys that seal MCP session ids and list cursors; the reconciler sets it from the team's `afs-gateway-mcp` Secret. Unset: no `/mcp`. MCP server credentials are files `mcp.<slug>` in `AFS_GATEWAY_CREDENTIALS_DIR`; federation tokens are under `/var/run/secrets/afs/gateway/federation/<audience>/token`. |

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
| `AFS_BILLING_AZURE_SCOPES` | empty | `route-pull` (1.2.0): `route=/subscriptions/<id>[/resourceGroups/<rg>],...`, the Cost Management scope of each Azure route. Empty: Azure is not pulled. |
| `AFS_BILLING_GOOGLE_EXPORTS` | empty | `route-pull`: `route=<project>:<dataset>.<table>,...`, the Cloud Billing export of each Google route. Empty: Google is not pulled. |
| `AFS_ROUTE_PRICE_LIST_URL` | LiteLLM's published list | `route-price-sync`. |

See [Costs, reconciliation and prices](billing.md).

## afs-edge

The collaboration edge. Every variable, the sealing keys and the `collab.*` settings are in
[Paperclip collaboration: the edge](collaboration-edge.md). It needs `AFS_DATABASE_URL` (the `afs_edge` login),
`AFS_EDGE_TLS_CERT`, `AFS_EDGE_TLS_KEY` and `AFS_EDGE_STATE_KEY`.

The reconciler's collaboration jobs read `AFS_EDGE_NAMESPACE` (default `afs-system`), `AFS_EDGE_NAME` (default `afs-edge`),
`AFS_KMS_KEY_ARN` (the environment key, for the collaboration agent's role policy) and, for the WAF allow-list sync,
`AFS_COLLAB_WAF_IPSET_V4_ARN` and `AFS_COLLAB_WAF_IPSET_V6_ARN`.

Buzz worker registration (managed teams whose orchestration agent runs Buzz) needs `AFS_BUZZ_RELAY_ADMIN_KEY_FILE`, the path of a
mounted file that holds the relay's admin key. Without it the job is off, with a warning, and Buzz workers stay unregistered.

### Orchestration and models (1.1.0)

`afs-api` also reads `AFS_DEPRECATION_SINCE` and `AFS_DEPRECATION_SUNSET` (RFC 3339; defaults 2026-11-23 and
2027-11-23): the `Deprecation` and `Sunset` headers of the deprecated 1.0.1 operations. `afs-api orchestration-backfill`
takes the same environment as `afs-api` (see [upgrading to 1.1.0](upgrading-to-1.1.0.md)).

## Service settings

Service administrators change these with the settings operations; they are cached for 30 seconds. Examples:
`api.cors_origins` (browser origins allowed to call the API), `secrets.path_prefix`, `billing.variance_alert_pct`,
`pricing.price_list_sync`. `GET /v1/admin/settings` lists every key with its default and current value.

Orchestration and model settings (1.1.0), all in `admin:settings` scope:

| Key | Meaning |
|---|---|
| `orchestration.tools` | Per tool (`paperclip`, `buzz`): `enabled`, `default` (exactly one), `templateId` (an agent template with `orchestration` and the tool capability), `imageRepository`, and the fixed profile (`webUi`, `webApi`, `registration`, `inNamespacePort`). A tool without a template and an image repository is "not set up": `CreateTeam` answers `orchestration_runtime_unset`. |
| `orchestration.public_base_domain`, `orchestration.private_base_domain` | Apexes of the public and private hostnames (`<team>.<domain>`, `api.<team>.<domain>`). The 1.0.1 key `collab.base_domain` is a read-only alias of the public one. |
| `orchestration.public_endpoints`, `orchestration.private_endpoints` | Whether each level is offered; a level that is off cannot be chosen. |
| `orchestration.default_limit`, `orchestration.default_web_ui_exposure`, `orchestration.default_web_api_exposure` | Defaults for a new team's limit and the two levels (a default is capped by the limit and by the tool's profile). |
| `orchestration.default_resources`, `orchestration.default_volume_gib` | Defaults of the orchestration agent. |
| `models.default_model_id` | Default model of an agent created without one. |

Model access is changed with `PutServiceModelAccess` (service level), `PutTeamModels` (team) and the `models` member of
an agent or pool; see [usage examples](usage-examples.md).
