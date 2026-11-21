variable "name" {
  description = "Name prefix of every resource (for example afs-dev)."
  type        = string
  default     = "afs"
}

variable "region" {
  description = "Primary AWS region."
  type        = string
}

variable "dr_region" {
  description = "Region that receives the cross-region backup copy; must differ from region."
  type        = string
}

variable "cross_account_backup_vault_arn" {
  description = "ARN of the AWS Backup vault in the backup account that receives the cross-account copy. That vault's access policy must allow this account to copy into it."
  type        = string
}

variable "cluster_name" {
  description = "EKS cluster the Pod Identity associations are created in."
  type        = string
}

variable "namespace" {
  description = "Kubernetes namespace of the AFS control plane."
  type        = string
  default     = "afs-system"
}

variable "vpc_id" {
  description = "VPC of the database."
  type        = string
}

variable "database_subnet_ids" {
  description = "Private subnets (at least two availability zones) for the RDS subnet group."
  type        = list(string)
}

variable "database_client_security_group_ids" {
  description = "Security groups allowed to reach PostgreSQL (the EKS node or pod security groups)."
  type        = list(string)
}

variable "database_instance_class" {
  type    = string
  default = "db.m7g.large"
}

variable "database_allocated_storage_gib" {
  type    = number
  default = 100
}

variable "database_multi_az" {
  type    = bool
  default = true
}

variable "database_backup_retention_days" {
  description = "Point-in-time recovery window, 1 to 35 days."
  type        = number
  default     = 14
}

variable "secrets_path_prefix" {
  description = "Secrets Manager name prefix of every AFS-managed secret (service setting secrets.path_prefix)."
  type        = string
  default     = "afs"
}

variable "work_order_queue_arn" {
  description = "SQS queue that delivers work orders to the reconciler; empty disables the permission."
  type        = string
  default     = ""
}

variable "cur_bucket_arn" {
  description = "Bucket holding the Cost and Usage Report that afs-billing reads through Athena."
  type        = string
}

variable "cur_glue_database" {
  description = "Glue database of the CUR table."
  type        = string
}

variable "athena_workgroup" {
  description = "Athena workgroup afs-billing queries in."
  type        = string
}

variable "athena_results_bucket_arn" {
  description = "Bucket the Athena workgroup writes results to."
  type        = string
}

variable "event_bus_name" {
  description = "EventBridge bus the outbox publishes to."
  type        = string
  default     = "default"
}

variable "tags" {
  description = "Tags added to every resource, on top of afs-component."
  type        = map(string)
  default     = {}
}
