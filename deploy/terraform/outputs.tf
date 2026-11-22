output "database_endpoint" {
  description = "RDS endpoint (the chart's database.host)."
  value       = module.rds.endpoint
}

output "database_master_secret_arn" {
  description = "Secrets Manager secret RDS manages the master password in; used once to bootstrap roles and never mounted in the cluster."
  value       = module.rds.master_user_secret_arn
}

output "export_bucket" {
  description = "AFS_EXPORT_BUCKET."
  value       = module.buckets.export_bucket_name
}

output "reports_bucket" {
  description = "AFS_REPORTS_BUCKET."
  value       = module.buckets.reports_bucket_name
}

output "buckets_kms_key_id" {
  description = "AFS_EXPORT_KMS_KEY_ID and AFS_REPORTS_KMS_KEY_ID."
  value       = module.buckets.kms_key_id
}

output "permissions_boundary_arn" {
  description = "AFS_PERMISSIONS_BOUNDARY_ARN."
  value       = module.boundary.policy_arn
}

output "gateway_role_arn" {
  description = "AFS_GATEWAY_ROLE_ARN: the only role every team's Bedrock role trusts."
  value       = module.pod_identity.role_arns["gateway"]
}

output "reconciler_role_arn" {
  description = "AFS_RECONCILER_ROLE_ARN: the role agent roles trust so the reconciler can verify role links."
  value       = module.pod_identity.role_arns["reconciler"]
}

output "role_arns" {
  description = "Pod Identity role of each binary."
  value       = module.pod_identity.role_arns
}

output "backup_vault_arn" {
  value = module.backup.vault_arn
}
