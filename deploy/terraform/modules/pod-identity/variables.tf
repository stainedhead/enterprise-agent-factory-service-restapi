variable "name" { type = string }
variable "cluster_name" { type = string }
variable "namespace" { type = string }
variable "region" { type = string }
variable "account_id" { type = string }
variable "partition" { type = string }

variable "service_accounts" {
  description = "ServiceAccount name of each binary (the Helm chart's serviceAccount.name values)."
  type        = map(string)
  default = {
    api        = "afs-api"
    reconciler = "afs-reconciler"
    gateway    = "afs-gateway"
    billing    = "afs-billing"
  }
}

variable "db_resource_id" {
  description = "DbiResourceId of the RDS instance, for rds-db:connect."
  type        = string
}

variable "db_users" {
  description = "Database login of each binary (-db roles)."
  type        = map(string)
  default = {
    api        = "afs_app"
    reconciler = "afs_reconciler"
    gateway    = "afs_gateway"
    billing    = "afs_billing"
  }
}

variable "secrets_path_prefix" { type = string }
variable "secrets_kms_key_arn" { type = string }
variable "permissions_boundary_arn" { type = string }
variable "export_bucket_arn" { type = string }
variable "reports_bucket_arn" { type = string }
variable "buckets_kms_key_arn" { type = string }
variable "event_bus_name" { type = string }
variable "work_order_queue_arn" { type = string }
variable "cur_bucket_arn" { type = string }
variable "cur_glue_database" { type = string }
variable "athena_workgroup" { type = string }
variable "athena_results_bucket_arn" { type = string }
