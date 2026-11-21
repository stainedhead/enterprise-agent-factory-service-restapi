output "vault_arn" {
  value = aws_backup_vault.primary.arn
}

output "dr_vault_arn" {
  value = aws_backup_vault.dr.arn
}
