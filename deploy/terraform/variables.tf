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

variable "collab_base_domain" {
  description = "collab.base_domain: the apex of the collaboration hostnames. Empty leaves the collaboration edge (ALB, certificate, WAF, DNS) out."
  type        = string
  default     = ""
}

variable "collab_route53_zone_id" {
  description = "Hosted zone of collab_base_domain. Required when collab_base_domain is set."
  type        = string
  default     = ""
}

variable "collab_public_subnet_ids" {
  description = "Public subnets (at least two availability zones) of the collaboration ALB. Required when collab_base_domain is set."
  type        = list(string)
  default     = []
}

variable "collab_edge_security_group_ids" {
  description = "Security groups of the afs-edge pods; the ALB's only egress. Required when collab_base_domain is set."
  type        = list(string)
  default     = []
}

variable "collab_client_secret_name_pattern" {
  description = "Secrets Manager name pattern, below secrets_path_prefix, of the collaboration sign-in client secrets afs-edge may read. Empty means idp/*."
  type        = string
  default     = ""
}

variable "orchestration_private_base_domain" {
  description = "orchestration.private_base_domain: the apex of the private orchestration hostnames. Empty leaves the private entry (internal ALB, private zone, WAF) out."
  type        = string
  default     = ""
}

variable "orchestration_private_subnet_ids" {
  description = "Private subnets (at least two availability zones) of the internal ALB. Required when orchestration_private_base_domain is set."
  type        = list(string)
  default     = []
}

variable "orchestration_private_certificate_arn" {
  description = "Certificate for the private base domain and its wildcards. Required when orchestration_private_base_domain is set."
  type        = string
  default     = ""
}

variable "orchestration_private_ingress_cidrs" {
  description = "Internal CIDR blocks allowed to reach the internal ALB. Required when orchestration_private_base_domain is set."
  type        = list(string)
  default     = []
}

variable "orchestration_private_extra_vpcs" {
  description = "Further VPCs that resolve the private zone."
  type        = list(object({ vpc_id = string, region = string }))
  default     = []
}

variable "orchestration_private_access_log_bucket" {
  description = "S3 bucket for the internal ALB's access logs. Empty turns them off."
  type        = string
  default     = ""
}
