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

output "edge_role_arn" {
  description = "Pod Identity role of afs-edge."
  value       = module.pod_identity.role_arns["edge"]
}

output "collab_target_group_arn" {
  description = "The Helm chart's edge.targetGroupArn; empty when the collaboration edge is not deployed."
  value       = one(module.collab_edge[*].target_group_arn)
}

output "collab_alb_security_group_id" {
  description = "Allow this group to reach the afs-edge pods on 8443 and the health check port."
  value       = one(module.collab_edge[*].alb_security_group_id)
}

output "collab_waf_ip_set_v4_arn" {
  description = "AFS_COLLAB_WAF_IPSET_V4_ARN."
  value       = one(module.collab_edge[*].waf_ip_set_v4_arn)
}

output "collab_waf_ip_set_v6_arn" {
  description = "AFS_COLLAB_WAF_IPSET_V6_ARN."
  value       = one(module.collab_edge[*].waf_ip_set_v6_arn)
}

output "collab_state_key_secret_arn" {
  description = "Secret holding the edge's sign-in state sealing keys (written by the operator)."
  value       = one(module.collab_edge[*].state_key_secret_arn)
}
