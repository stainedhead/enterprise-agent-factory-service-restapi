variable "name" { type = string }
variable "vpc_id" { type = string }
variable "subnet_ids" { type = list(string) }
variable "client_security_group_ids" { type = list(string) }

variable "instance_class" { type = string }
variable "allocated_storage_gib" { type = number }
variable "multi_az" { type = bool }

variable "backup_retention_days" {
  description = "Point-in-time recovery window; RDS uploads transaction logs about every five minutes, so the recovery point objective is five minutes within this window."
  type        = number

  validation {
    condition     = var.backup_retention_days >= 1 && var.backup_retention_days <= 35
    error_message = "backup_retention_days must be between 1 and 35; PITR needs automated backups."
  }
}
