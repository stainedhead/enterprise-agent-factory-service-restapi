output "target_group_arn" {
  description = "The Helm chart's edge.privateTargetGroupArn."
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

output "private_zone_id" {
  description = "The private hosted zone of private_base_domain."
  value       = aws_route53_zone.private.zone_id
}

output "web_acl_arn" {
  value = aws_wafv2_web_acl.this.arn
}
