# RDS PostgreSQL 17 for the AFS schema (-db). Encrypted with a customer-managed key, IAM database
# authentication on, TLS required, automated backups for point-in-time recovery. The master
# password is generated and held by RDS in Secrets Manager (manage_master_user_password), so no
# password appears in configuration or state.

resource "aws_kms_key" "db" {
  description             = "${var.name} RDS storage, backups and Performance Insights"
  enable_key_rotation     = true
  deletion_window_in_days = 30
}

resource "aws_kms_alias" "db" {
  name          = "alias/${var.name}-rds"
  target_key_id = aws_kms_key.db.key_id
}

resource "aws_security_group" "db" {
  name_prefix = "${var.name}-rds-"
  description = "PostgreSQL for ${var.name}"
  vpc_id      = var.vpc_id

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "postgres" {
  for_each = toset(var.client_security_group_ids)

  security_group_id            = aws_security_group.db.id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  description                  = "PostgreSQL from AFS workloads"
}

resource "aws_db_subnet_group" "db" {
  name       = "${var.name}-db"
  subnet_ids = var.subnet_ids
}

resource "aws_db_parameter_group" "db" {
  name   = "${var.name}-pg17"
  family = "postgres17"

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }

  parameter {
    name  = "log_min_duration_statement"
    value = "1000"
  }
}

resource "aws_db_instance" "db" {
  identifier     = "${var.name}-db"
  engine         = "postgres"
  engine_version = "17"
  instance_class = var.instance_class

  db_name  = "afs"
  username = "afs_master"

  manage_master_user_password   = true
  master_user_secret_kms_key_id = aws_kms_key.db.arn

  allocated_storage     = var.allocated_storage_gib
  max_allocated_storage = var.allocated_storage_gib * 4
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = aws_kms_key.db.arn

  iam_database_authentication_enabled = true
  multi_az                            = var.multi_az
  db_subnet_group_name                = aws_db_subnet_group.db.name
  vpc_security_group_ids              = [aws_security_group.db.id]
  parameter_group_name                = aws_db_parameter_group.db.name
  publicly_accessible                 = false

  backup_retention_period  = var.backup_retention_days
  backup_window            = "03:00-04:00"
  maintenance_window       = "sun:05:00-sun:06:00"
  copy_tags_to_snapshot    = true
  delete_automated_backups = false
  deletion_protection      = true
  skip_final_snapshot      = false

  final_snapshot_identifier = "${var.name}-db-final"

  performance_insights_enabled          = true
  performance_insights_kms_key_id       = aws_kms_key.db.arn
  performance_insights_retention_period = 7
  enabled_cloudwatch_logs_exports       = ["postgresql", "upgrade"]
  auto_minor_version_upgrade            = true

  tags = {
    "afs-component" = "database"
  }
}
