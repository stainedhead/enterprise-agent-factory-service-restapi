# Permissions boundary of every role the reconciler creates for teams and agents
# (AFS_PERMISSIONS_BOUNDARY_ARN). A role created with it can never exceed this policy, whatever
# the reconciler attaches: no Bedrock (only the gateway's per-team role has it), no IAM and no
# Organizations. Which secrets, queues and buckets a role may touch is granted by the role's own
# policy, scoped to its team; the boundary only caps what that policy can ever grant.

data "aws_iam_policy_document" "boundary" {
  statement {
    sid       = "AllowWorkloadBasics"
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret", "kms:Decrypt", "s3:GetObject", "s3:PutObject", "s3:ListBucket", "ec2:DescribeSnapshots", "elasticfilesystem:Client*", "logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["*"]
  }

  # Pool inbox and handoff queues (M5-C2): the reconciler creates one queue per receiving pool, named
  # afs-<team ulid>-<short hash>, and a team or agent role may use only queues with that prefix. Which of
  # them a role may send to or receive from is granted by the role's own policy, naming the queue.
  statement {
    sid       = "AllowAFSQueues"
    effect    = "Allow"
    actions   = ["sqs:*"]
    resources = ["arn:*:sqs:*:*:afs-*"]
  }

  statement {
    sid       = "DenyBedrock"
    effect    = "Deny"
    actions   = ["bedrock:*"]
    resources = ["*"]
  }

  statement {
    sid       = "DenyIdentityAndOrganization"
    effect    = "Deny"
    actions   = ["iam:*", "organizations:*", "account:*"]
    resources = ["*"]
  }
}

resource "aws_iam_policy" "boundary" {
  name        = "${var.name}-agent-boundary"
  description = "Permissions boundary for AFS team and agent roles"
  policy      = data.aws_iam_policy_document.boundary.json
}
