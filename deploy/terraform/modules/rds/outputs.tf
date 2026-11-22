output "instance_arn" {
  value = aws_db_instance.db.arn
}

output "resource_id" {
  description = "DbiResourceId, used in rds-db:connect ARNs."
  value       = aws_db_instance.db.resource_id
}

output "endpoint" {
  value = aws_db_instance.db.address
}

output "master_user_secret_arn" {
  value = aws_db_instance.db.master_user_secret[0].secret_arn
}

output "kms_key_arn" {
  value = aws_kms_key.db.arn
}
