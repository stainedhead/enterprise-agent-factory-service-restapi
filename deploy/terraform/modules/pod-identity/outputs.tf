output "role_arns" {
  description = "Role ARN of each binary, keyed api, reconciler, gateway, billing."
  value       = { for k, r in aws_iam_role.this : k => r.arn }
}
