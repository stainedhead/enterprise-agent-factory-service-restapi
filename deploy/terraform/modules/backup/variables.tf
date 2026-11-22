variable "name" { type = string }

variable "resource_arns" {
  description = "Resources the plan backs up (the RDS instance)."
  type        = list(string)
}

variable "cross_account_backup_vault_arn" {
  description = "Vault in the backup account that receives the cross-account copy."
  type        = string
}

variable "retention_days" {
  type    = number
  default = 35
}

variable "copy_retention_days" {
  type    = number
  default = 90
}
