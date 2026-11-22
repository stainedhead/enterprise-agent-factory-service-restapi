# One IAM role per AFS binary, assumed through EKS Pod Identity (the Helm chart creates the
# ServiceAccounts; the associations are created here). Each role gets only what its binary does:
#   afs-api         write-only secret values, database login
#   afs-reconciler  team and agent roles under /afs/ bounded by the permissions boundary, Pod Identity
#                   and access entries, inference profiles, secret deletion, events, exports, snapshots
#   afs-gateway     assume each team's Bedrock role (and nothing else), database login
#   afs-billing     Athena over the CUR, Price List, Cost Explorer, report writes, database login
#   afs-edge        read of the collaboration sign-in client secrets, database login (no table privilege, ten functions)
# No role here has bedrock:InvokeModel: only a team's own Bedrock role, assumed by the gateway, does.

locals {
  roles = toset(["api", "reconciler", "gateway", "billing", "edge"])

  db_connect_arn = {
    for k, user in var.db_users :
    k => "arn:${var.partition}:rds-db:${var.region}:${var.account_id}:dbuser:${var.db_resource_id}/${user}"
  }

  secret_arn_pattern = "arn:${var.partition}:secretsmanager:${var.region}:${var.account_id}:secret:${var.secrets_path_prefix}/*"
}

data "aws_iam_policy_document" "trust" {
  statement {
    sid     = "PodIdentityTrust"
    actions = ["sts:AssumeRole", "sts:TagSession"]

    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "this" {
  for_each = local.roles

  name               = "${var.name}-${each.key}"
  assume_role_policy = data.aws_iam_policy_document.trust.json

  tags = {
    "afs-component" = each.key
  }
}

resource "aws_eks_pod_identity_association" "this" {
  for_each = local.roles

  cluster_name    = var.cluster_name
  namespace       = var.namespace
  service_account = var.service_accounts[each.key]
  role_arn        = aws_iam_role.this[each.key].arn
}

# ---- every binary: connect to the database as its own login ----

data "aws_iam_policy_document" "db" {
  for_each = local.roles

  statement {
    sid       = "ConnectAsOwnLogin"
    actions   = ["rds-db:connect"]
    resources = [local.db_connect_arn[each.key]]
  }
}

resource "aws_iam_role_policy" "db" {
  for_each = local.roles

  name   = "database"
  role   = aws_iam_role.this[each.key].id
  policy = data.aws_iam_policy_document.db[each.key].json
}

# ---- afs-api ----

data "aws_iam_policy_document" "api" {
  statement {
    sid = "WriteOnlySecretValues"
    actions = [
      "secretsmanager:CreateSecret",
      "secretsmanager:PutSecretValue",
      "secretsmanager:TagResource",
      "secretsmanager:DescribeSecret",
    ]
    resources = [local.secret_arn_pattern]
  }

  statement {
    sid       = "EncryptSecretValues"
    actions   = ["kms:GenerateDataKey", "kms:Encrypt", "kms:DescribeKey"]
    resources = [var.secrets_kms_key_arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${var.region}.amazonaws.com"]
    }
  }

  statement {
    sid       = "DenyReadingSecretValues"
    effect    = "Deny"
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:BatchGetSecretValue"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "api" {
  name   = "api"
  role   = aws_iam_role.this["api"].id
  policy = data.aws_iam_policy_document.api.json
}

# ---- afs-reconciler ----

data "aws_iam_policy_document" "reconciler" {
  # iam:CreateRole is deliberately absent here: it is granted only with the permissions boundary,
  # in the next statement.
  statement {
    sid = "ManageTeamAndAgentRoles"
    actions = [
      "iam:DeleteRole",
      "iam:GetRole",
      "iam:TagRole",
      "iam:UntagRole",
      "iam:UpdateAssumeRolePolicy",
      "iam:PutRolePolicy",
      "iam:DeleteRolePolicy",
      "iam:GetRolePolicy",
      "iam:ListRolePolicies",
      "iam:ListAttachedRolePolicies",
      "iam:AttachRolePolicy",
      "iam:DetachRolePolicy",
      "iam:ListRoles",
    ]
    resources = ["arn:${var.partition}:iam::${var.account_id}:role/afs/*"]
  }

  statement {
    sid       = "CreateRolesOnlyWithTheBoundary"
    actions   = ["iam:CreateRole", "iam:PutRolePermissionsBoundary"]
    resources = ["arn:${var.partition}:iam::${var.account_id}:role/afs/*"]

    condition {
      test     = "StringEquals"
      variable = "iam:PermissionsBoundary"
      values   = [var.permissions_boundary_arn]
    }
  }

  statement {
    sid       = "PassTeamRolesToPodIdentity"
    actions   = ["iam:PassRole"]
    resources = ["arn:${var.partition}:iam::${var.account_id}:role/afs/*"]

    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["pods.eks.amazonaws.com"]
    }
  }

  statement {
    sid = "PodIdentityAndAccessEntries"
    actions = [
      "eks:CreatePodIdentityAssociation",
      "eks:DeletePodIdentityAssociation",
      "eks:DescribePodIdentityAssociation",
      "eks:ListPodIdentityAssociations",
      "eks:UpdatePodIdentityAssociation",
      "eks:CreateAccessEntry",
      "eks:DeleteAccessEntry",
      "eks:DescribeAccessEntry",
      "eks:ListAccessEntries",
      "eks:UpdateAccessEntry",
      "eks:TagResource",
    ]
    resources = ["*"]
  }

  statement {
    sid = "TeamInferenceProfiles"
    actions = [
      "bedrock:CreateInferenceProfile",
      "bedrock:DeleteInferenceProfile",
      "bedrock:GetInferenceProfile",
      "bedrock:ListInferenceProfiles",
      "bedrock:TagResource",
      "bedrock:UntagResource",
      "bedrock:ListTagsForResource",
    ]
    resources = ["*"]
  }

  statement {
    sid = "ScheduleSecretDeletionSweepOrphansAndSyncTags"
    actions = [
      "secretsmanager:DeleteSecret",
      "secretsmanager:DescribeSecret",
      "secretsmanager:TagResource",
      "secretsmanager:UntagResource",
    ]
    resources = [local.secret_arn_pattern]
  }

  statement {
    sid       = "ListSecrets"
    actions   = ["secretsmanager:ListSecrets"]
    resources = ["*"]
  }

  statement {
    sid       = "PublishOutboxEvents"
    actions   = ["events:PutEvents"]
    resources = ["arn:${var.partition}:events:${var.region}:${var.account_id}:event-bus/${var.event_bus_name}"]
  }

  statement {
    sid = "ExportBundles"
    actions = [
      "s3:PutObject",
      "s3:GetObject",
      "s3:ListBucket",
      "s3:AbortMultipartUpload",
    ]
    resources = [var.export_bucket_arn, "${var.export_bucket_arn}/*"]
  }

  statement {
    sid       = "ExportBundleKey"
    actions   = ["kms:GenerateDataKey", "kms:Decrypt", "kms:DescribeKey"]
    resources = [var.buckets_kms_key_arn]
  }

  statement {
    sid = "AgentVolumeSnapshots"
    actions = [
      "ec2:CreateSnapshot",
      "ec2:DeleteSnapshot",
      "ec2:DescribeSnapshots",
      "ec2:DescribeVolumes",
      "ec2:CreateTags",
    ]
    resources = ["*"]
  }

  statement {
    sid       = "OrganizationAccountMirror"
    actions   = ["organizations:ListAccounts", "organizations:DescribeAccount"]
    resources = ["*"]
  }

  # Pool inbox and handoff queues (M5-C2): one SQS queue per receiving pool, named afs-<team ulid>-<short hash>.
  # Administration is limited to queues with the afs- prefix; the queue policy it sets lets only the senders'
  # roles send and only the receiver's role receive. No ReceiveMessage or SendMessage here.
  statement {
    sid = "PoolQueues"
    actions = [
      "sqs:CreateQueue",
      "sqs:DeleteQueue",
      "sqs:GetQueueUrl",
      "sqs:GetQueueAttributes",
      "sqs:SetQueueAttributes",
      "sqs:TagQueue",
      "sqs:UntagQueue",
      "sqs:ListQueueTags",
    ]
    resources = ["arn:${var.partition}:sqs:${var.region}:${var.account_id}:afs-*"]
  }

  # Verifying an agent role link: the reconciler assumes the agent's own role (which trusts it), and that
  # session assumes the linked role in the other account. Only the first hop is here, and only on roles
  # under /afs/.
  statement {
    sid       = "VerifyAgentRoleLinks"
    actions   = ["sts:AssumeRole", "sts:TagSession"]
    resources = ["arn:${var.partition}:iam::${var.account_id}:role/afs/*"]
  }
}

data "aws_iam_policy_document" "reconciler_queue" {
  count = var.work_order_queue_arn == "" ? 0 : 1

  statement {
    sid       = "WorkOrderQueue"
    actions   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes", "sqs:ChangeMessageVisibility"]
    resources = [var.work_order_queue_arn]
  }
}

resource "aws_iam_role_policy" "reconciler" {
  name   = "reconciler"
  role   = aws_iam_role.this["reconciler"].id
  policy = data.aws_iam_policy_document.reconciler.json
}

resource "aws_iam_role_policy" "reconciler_queue" {
  count = var.work_order_queue_arn == "" ? 0 : 1

  name   = "work-order-queue"
  role   = aws_iam_role.this["reconciler"].id
  policy = data.aws_iam_policy_document.reconciler_queue[0].json
}

# ---- afs-gateway ----

data "aws_iam_policy_document" "gateway" {
  statement {
    sid       = "AssumeTeamBedrockRoles"
    actions   = ["sts:AssumeRole", "sts:TagSession"]
    resources = ["arn:${var.partition}:iam::${var.account_id}:role/afs/*/afs-*-bedrock"]
  }
}

resource "aws_iam_role_policy" "gateway" {
  name   = "gateway"
  role   = aws_iam_role.this["gateway"].id
  policy = data.aws_iam_policy_document.gateway.json
}

# ---- afs-billing ----

data "aws_iam_policy_document" "billing" {
  statement {
    sid = "QueryTheCostAndUsageReport"
    actions = [
      "athena:StartQueryExecution",
      "athena:GetQueryExecution",
      "athena:GetQueryResults",
      "athena:StopQueryExecution",
    ]
    resources = ["arn:${var.partition}:athena:${var.region}:${var.account_id}:workgroup/${var.athena_workgroup}"]
  }

  statement {
    sid = "ReadTheCurCatalog"
    actions = [
      "glue:GetDatabase",
      "glue:GetTable",
      "glue:GetTables",
      "glue:GetPartition",
      "glue:GetPartitions",
    ]
    resources = [
      "arn:${var.partition}:glue:${var.region}:${var.account_id}:catalog",
      "arn:${var.partition}:glue:${var.region}:${var.account_id}:database/${var.cur_glue_database}",
      "arn:${var.partition}:glue:${var.region}:${var.account_id}:table/${var.cur_glue_database}/*",
    ]
  }

  statement {
    sid       = "ReadTheReport"
    actions   = ["s3:GetObject", "s3:ListBucket", "s3:GetBucketLocation"]
    resources = [var.cur_bucket_arn, "${var.cur_bucket_arn}/*"]
  }

  statement {
    sid       = "AthenaResults"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:ListBucket", "s3:GetBucketLocation", "s3:AbortMultipartUpload"]
    resources = [var.athena_results_bucket_arn, "${var.athena_results_bucket_arn}/*"]
  }

  statement {
    sid       = "WriteReports"
    actions   = ["s3:PutObject", "s3:GetObject", "s3:ListBucket"]
    resources = [var.reports_bucket_arn, "${var.reports_bucket_arn}/*"]
  }

  statement {
    sid       = "ReportsKey"
    actions   = ["kms:GenerateDataKey", "kms:Decrypt", "kms:DescribeKey"]
    resources = [var.buckets_kms_key_arn]
  }

  statement {
    sid       = "PriceListAndCostExplorer"
    actions   = ["pricing:GetProducts", "pricing:DescribeServices", "pricing:GetAttributeValues", "ce:GetCostAndUsage", "ce:ListCostAllocationTags"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "billing" {
  name   = "billing"
  role   = aws_iam_role.this["billing"].id
  policy = data.aws_iam_policy_document.billing.json
}

# ---- afs-edge (RC-28, DP-4) ----
# The edge reads the client secret of a team's collaboration sign-in client for one token exchange and
# holds it in memory only. It reads nothing else: not the agents' secrets, not the board keys. Its sealing
# keys arrive through a Kubernetes Secret, so no statement here covers them.

data "aws_iam_policy_document" "edge" {
  statement {
    sid       = "ReadCollaborationClientSecrets"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = ["arn:${var.partition}:secretsmanager:${var.region}:${var.account_id}:secret:${var.collab_client_secret_name_pattern}"]
  }

  statement {
    sid       = "DecryptCollaborationClientSecrets"
    actions   = ["kms:Decrypt"]
    resources = [var.secrets_kms_key_arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${var.region}.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "edge" {
  name   = "edge"
  role   = aws_iam_role.this["edge"].id
  policy = data.aws_iam_policy_document.edge.json
}

# ---- afs-reconciler: collaboration (RC-28, DP-4) ----

data "aws_iam_policy_document" "reconciler_collab" {
  # The board key is written by the collaboration pod's bootstrap; the reconciler reads only secrets with
  # exactly this name (the ARN suffix is the six random characters Secrets Manager appends) and records them.
  statement {
    sid       = "ReadBoardKeys"
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
    resources = ["arn:${var.partition}:secretsmanager:${var.region}:${var.account_id}:secret:${var.secrets_path_prefix}/*/paperclip-board-api-key-??????"]
  }

  # Managed registration (COL-12) writes a connected worker's Paperclip key straight to Secrets Manager at the
  # worker's own path, and schedules it for deletion on disconnect (DeleteSecret is in the reconciler statement
  # above). Nothing else under the prefix is writable through this statement.
  statement {
    sid = "WriteWorkerPaperclipKeys"
    actions = [
      "secretsmanager:CreateSecret",
      "secretsmanager:PutSecretValue",
      "secretsmanager:TagResource",
      "secretsmanager:DescribeSecret",
    ]
    resources = ["arn:${var.partition}:secretsmanager:${var.region}:${var.account_id}:secret:${var.secrets_path_prefix}/*/paperclip-agent-api-key-??????"]
  }

  statement {
    sid       = "EncryptWorkerPaperclipKeys"
    actions   = ["kms:GenerateDataKey", "kms:Encrypt", "kms:DescribeKey"]
    resources = [var.secrets_kms_key_arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${var.region}.amazonaws.com"]
    }
  }

  statement {
    sid       = "DecryptBoardKeys"
    actions   = ["kms:Decrypt"]
    resources = [var.secrets_kms_key_arn]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${var.region}.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "reconciler_collab" {
  name   = "collaboration"
  role   = aws_iam_role.this["reconciler"].id
  policy = data.aws_iam_policy_document.reconciler_collab.json
}

# The allow-list sync: the two IP sets of the collaboration web ACL, and nothing else in WAF.
data "aws_iam_policy_document" "reconciler_waf" {
  count = length(var.collab_waf_ip_set_arns) == 0 ? 0 : 1

  statement {
    sid       = "SyncCollaborationAllowList"
    actions   = ["wafv2:GetIPSet", "wafv2:UpdateIPSet"]
    resources = var.collab_waf_ip_set_arns
  }
}

resource "aws_iam_role_policy" "reconciler_waf" {
  count = length(var.collab_waf_ip_set_arns) == 0 ? 0 : 1

  name   = "collaboration-waf"
  role   = aws_iam_role.this["reconciler"].id
  policy = data.aws_iam_policy_document.reconciler_waf[0].json
}
