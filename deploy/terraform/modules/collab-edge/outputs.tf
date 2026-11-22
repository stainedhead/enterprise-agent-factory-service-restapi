output "target_group_arn" {
  description = "The Helm chart's edge.targetGroupArn."
  value       = aws_lb_target_group.edge.arn
}

output "alb_arn" {
  value = aws_lb.this.arn
}

output "alb_dns_name" {
  value = aws_lb.this.dns_name
}

output "alb_security_group_id" {
  description = "Allow this group to reach the afs-edge pods' security groups on the edge and health check ports."
  value       = aws_security_group.alb.id
}

output "certificate_arn" {
  value = aws_acm_certificate_validation.this.certificate_arn
}

output "web_acl_arn" {
  value = aws_wafv2_web_acl.this.arn
}

output "waf_ip_set_arns" {
  description = "The two IP sets the reconciler syncs collab.allowed_source_cidrs into (AFS_COLLAB_WAF_IPSET_V4_ARN and AFS_COLLAB_WAF_IPSET_V6_ARN)."
  value       = [aws_wafv2_ip_set.allow_v4.arn, aws_wafv2_ip_set.allow_v6.arn]
}

output "waf_ip_set_v4_arn" {
  value = aws_wafv2_ip_set.allow_v4.arn
}

output "waf_ip_set_v6_arn" {
  value = aws_wafv2_ip_set.allow_v6.arn
}

output "state_key_secret_arn" {
  description = "The secret that holds the sealing keys (written by the operator)."
  value       = aws_secretsmanager_secret.state_key.arn
}
