provider "aws" {
  region = var.region

  default_tags {
    tags = merge(var.tags, { "afs-managed" = "terraform" })
  }
}

provider "aws" {
  alias  = "dr"
  region = var.dr_region

  default_tags {
    tags = merge(var.tags, { "afs-managed" = "terraform" })
  }
}

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

module "rds" {
  source = "./modules/rds"

  name                      = var.name
  vpc_id                    = var.vpc_id
  subnet_ids                = var.database_subnet_ids
  client_security_group_ids = var.database_client_security_group_ids
  instance_class            = var.database_instance_class
  allocated_storage_gib     = var.database_allocated_storage_gib
  multi_az                  = var.database_multi_az
  backup_retention_days     = var.database_backup_retention_days
}

module "backup" {
  source = "./modules/backup"

  providers = {
    aws    = aws
    aws.dr = aws.dr
  }

  name                           = var.name
  resource_arns                  = [module.rds.instance_arn]
  cross_account_backup_vault_arn = var.cross_account_backup_vault_arn
}

module "buckets" {
  source = "./modules/buckets"

  name = var.name
}

module "boundary" {
  source = "./modules/boundary"

  name = var.name
}

module "collab_edge" {
  count  = var.collab_base_domain == "" ? 0 : 1
  source = "./modules/collab-edge"

  name                    = var.name
  vpc_id                  = var.vpc_id
  public_subnet_ids       = var.collab_public_subnet_ids
  base_domain             = var.collab_base_domain
  route53_zone_id         = var.collab_route53_zone_id
  edge_security_group_ids = var.collab_edge_security_group_ids
  kms_key_arn             = module.buckets.kms_key_arn
}

# The private entry of the orchestration agent (ORC-13): an internal ALB, a private zone and a WAF in front of
# afs-edge. Optional: leave orchestration_private_base_domain empty to serve public hostnames only.
module "orchestration_private" {
  count  = var.orchestration_private_base_domain == "" ? 0 : 1
  source = "./modules/orchestration-private"

  name                    = var.name
  vpc_id                  = var.vpc_id
  private_subnet_ids      = var.orchestration_private_subnet_ids
  private_base_domain     = var.orchestration_private_base_domain
  certificate_arn         = var.orchestration_private_certificate_arn
  allowed_ingress_cidrs   = var.orchestration_private_ingress_cidrs
  additional_zone_vpc_ids = var.orchestration_private_extra_vpcs
  edge_security_group_ids = var.collab_edge_security_group_ids
  access_log_bucket       = var.orchestration_private_access_log_bucket
}

module "pod_identity" {
  source = "./modules/pod-identity"

  name                      = var.name
  cluster_name              = var.cluster_name
  namespace                 = var.namespace
  region                    = var.region
  account_id                = data.aws_caller_identity.current.account_id
  partition                 = data.aws_partition.current.partition
  db_resource_id            = module.rds.resource_id
  secrets_path_prefix       = var.secrets_path_prefix
  secrets_kms_key_arn       = module.buckets.kms_key_arn
  permissions_boundary_arn  = module.boundary.policy_arn
  export_bucket_arn         = module.buckets.export_bucket_arn
  reports_bucket_arn        = module.buckets.reports_bucket_arn
  buckets_kms_key_arn       = module.buckets.kms_key_arn
  event_bus_name            = var.event_bus_name
  work_order_queue_arn      = var.work_order_queue_arn
  cur_bucket_arn            = var.cur_bucket_arn
  cur_glue_database         = var.cur_glue_database
  athena_workgroup          = var.athena_workgroup
  athena_results_bucket_arn = var.athena_results_bucket_arn

  collab_client_secret_name_pattern = var.collab_client_secret_name_pattern == "" ? "${var.secrets_path_prefix}/idp/*" : "${var.secrets_path_prefix}/${var.collab_client_secret_name_pattern}"
  collab_waf_ip_set_arns            = flatten(module.collab_edge[*].waf_ip_set_arns)
}
