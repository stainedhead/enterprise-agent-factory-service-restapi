# Deploying the control plane

Two pieces: Terraform creates the AWS resources, the Helm chart runs the four binaries on the EKS
cluster.

## 1. Terraform (`deploy/terraform`)

Needs Terraform 1.9 or later and credentials for the target account. The root module composes:

| Module | Creates |
|---|---|
| `rds` | RDS PostgreSQL 17: encrypted with its own KMS key, IAM database authentication, TLS required, automated backups for point-in-time recovery (default 14 days, 1 to 35). The master password is generated and held by RDS in Secrets Manager; it never appears in configuration or state. |
| `backup` | AWS Backup: a daily plan with one copy to a vault in the disaster-recovery region and one to a vault in another account (`cross_account_backup_vault_arn`; that vault's access policy must allow this account to copy into it). |
| `buckets` | The export-bundle bucket and the reports bucket: private, versioned, SSE-KMS with one customer-managed key, TLS only, lifecycle expiry. |
| `boundary` | The permissions boundary `AFS_PERMISSIONS_BOUNDARY_ARN` for every team and agent role the reconciler creates: it denies `bedrock:*`, `iam:*` and Organizations. |
| `pod-identity` | One role per binary (`afs-api`, `afs-reconciler`, `afs-gateway`, `afs-billing`) and its EKS Pod Identity association. Each role has only what its binary does; none can invoke a model (only a team's Bedrock role, assumed by the gateway, can). |

```bash
cd deploy/terraform
cp terraform.tfvars.example terraform.tfvars   # fill in your VPC, subnets, cluster, CUR and Athena names
terraform init
terraform plan
terraform apply
```

Nothing is hardcoded: account ids come from the caller identity, regions and names from variables.

### After the first apply

1. Create the database roles once, as a DBA, using the master secret from the `database_master_secret_arn`
   output: run `afs-migrate bootstrap-roles`, then create the five LOGIN users (`afs_owner`, `afs_app`,
   `afs_reconciler`, `afs_gateway`, `afs_billing`) with `GRANT rds_iam TO <user>` so they sign in with IAM.
2. Note the outputs the chart and the reconciler need: `database_endpoint`, `gateway_role_arn`
   (`AFS_GATEWAY_ROLE_ARN`), `permissions_boundary_arn` (`AFS_PERMISSIONS_BOUNDARY_ARN`), `export_bucket`,
   `reports_bucket` and `buckets_kms_key_id`.

## 2. Helm chart (`deploy/helm/afs`)

Needs cert-manager (the gateway's TLS certificate) and, for metrics, the Prometheus Operator CRDs
(`ServiceMonitor`, `PrometheusRule`).

```bash
helm upgrade --install afs deploy/helm/afs --namespace afs-system --create-namespace \
  --set global.image.registry=<registry> --set global.image.tag=<tag> \
  --set global.region=<region> --set global.clusterName=<cluster> \
  --set database.host=<database_endpoint> \
  --set gateway.tls.issuerRef.name=<cert-manager issuer> \
  -f my-values.yaml
```

Required values fail fast with a message when missing. Defaults per binary live in
`values/<component>.yaml`; anything you set under `api:`, `reconciler:`, `gateway:` or `billing:` in your
values wins. Highlights:

- `afs-api` and `afs-gateway` run 3 replicas spread across availability zones (hard constraint), with a
  `PodDisruptionBudget` of `minAvailable: 2`. The reconciler runs 2 replicas: one leader and a standby.
- Every container runs as non-root with a read-only root filesystem. No password is stored anywhere: the
  pods sign in to PostgreSQL with IAM (`AFS_DB_IAM_AUTH`).
- The billing jobs are `CronJob`s, one per entry under `billing.cronjobs` (`afs-billing --job <name>`).
- Set the environment each binary needs under its `env:` map (reconciler: event bus, boundary and gateway
  role ARNs, OIDC provider, export bucket and key; billing: CUR database and table, Athena workgroup and
  output, reports bucket). `deploy/helm/afs/ci/default-values.yaml` shows a complete example.

- `GetTeamKubeconfig` needs `AFS_CLUSTER_ENDPOINT` (the cluster's API server URL, `aws eks describe-cluster`)
  and `AFS_CLUSTER_CA_DATA` (its base64 certificate authority): set `global.cluster.endpoint` and
  `global.cluster.caData`; without them the operation answers `503`. The kubeconfig it returns holds no token: `kubectl` runs `aws eks get-token` as the viewer.
  Viewer access itself is set up by the reconciler: set `reconciler.viewerKubectl.roles` (rendered as
  `AFS_VIEWER_KUBECTL_ROLES`, see `reconciler.md`).
- Agents and pools call the API with their projected ServiceAccount token (audience `afs-api`). `afs-api`
  validates it offline against the cluster's service account issuer: set `global.cluster.oidcIssuer`
  (`AFS_CLUSTER_OIDC_ISSUER`, for EKS `https://oidc.eks.<region>.amazonaws.com/id/<cluster id>`). It fetches
  only the issuer's public keys (cached ten minutes) and never calls the Kubernetes API. Tokens that name
  another audience, live longer than an hour or come from another issuer are refused with `401`; with no issuer
  set, workload tokens are refused.
- Pools with a shared volume use the EFS storage class `reconciler.efsStorageClass` (`AFS_EFS_STORAGE_CLASS`,
  default `efs-sc`). The reconciler creates and deletes pool queues named `afs-<team>-<hash>`; the Terraform
  `pod-identity` module grants it queue administration on `afs-*` queues only, and the permissions boundary lets
  team and agent roles use `afs-*` queues and no other.

Verify with `helm lint deploy/helm/afs -f deploy/helm/afs/ci/default-values.yaml --strict`.
