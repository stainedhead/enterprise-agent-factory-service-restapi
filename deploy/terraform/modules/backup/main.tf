# AWS Backup: a daily plan with a copy to a vault in another region (disaster recovery) and a copy
# to a vault in another account (protection from a compromised or deleted account). Both vaults
# are encrypted with their own keys.

resource "aws_kms_key" "vault" {
  description             = "${var.name} backup vault"
  enable_key_rotation     = true
  deletion_window_in_days = 30
}

resource "aws_kms_key" "dr_vault" {
  provider = aws.dr

  description             = "${var.name} backup vault (disaster recovery region)"
  enable_key_rotation     = true
  deletion_window_in_days = 30
}

resource "aws_backup_vault" "primary" {
  name        = "${var.name}-primary"
  kms_key_arn = aws_kms_key.vault.arn
}

resource "aws_backup_vault" "dr" {
  provider = aws.dr

  name        = "${var.name}-dr"
  kms_key_arn = aws_kms_key.dr_vault.arn
}

resource "aws_backup_plan" "daily" {
  name = "${var.name}-daily"

  rule {
    rule_name         = "daily"
    target_vault_name = aws_backup_vault.primary.name
    schedule          = "cron(0 5 * * ? *)"
    start_window      = 60
    completion_window = 360

    lifecycle {
      delete_after = var.retention_days
    }

    copy_action {
      destination_vault_arn = aws_backup_vault.dr.arn

      lifecycle {
        delete_after = var.copy_retention_days
      }
    }

    copy_action {
      destination_vault_arn = var.cross_account_backup_vault_arn

      lifecycle {
        delete_after = var.copy_retention_days
      }
    }
  }
}

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["backup.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "backup" {
  name               = "${var.name}-backup"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

resource "aws_iam_role_policy_attachment" "backup" {
  role       = aws_iam_role.backup.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSBackupServiceRolePolicyForBackup"
}

resource "aws_iam_role_policy_attachment" "restore" {
  role       = aws_iam_role.backup.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSBackupServiceRolePolicyForRestores"
}

resource "aws_backup_selection" "resources" {
  name         = "${var.name}-resources"
  plan_id      = aws_backup_plan.daily.id
  iam_role_arn = aws_iam_role.backup.arn
  resources    = var.resource_arns
}
