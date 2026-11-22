# The public entry of the Paperclip collaboration feature (DP-3): an internet-facing ALB in front of afs-edge,
# a wildcard certificate, Route 53 alias records, and a regional WAF. The ALB forwards only the base domain and
# its subdomains to the afs-edge pods; every other host gets a fixed 404. The allow-list IP sets are created
# open (0.0.0.0/0 and ::/0) and owned by the reconciler afterwards (collab.allowed_source_cidrs), so Terraform
# never resets them.

locals {
  hosts = [var.base_domain, "*.${var.base_domain}"]
}

data "aws_caller_identity" "current" {}

# ---- certificate (PC-D21) ----

resource "aws_acm_certificate" "this" {
  domain_name               = var.base_domain
  subject_alternative_names = ["*.${var.base_domain}"]
  validation_method         = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "validation" {
  for_each = {
    for o in aws_acm_certificate.this.domain_validation_options : o.domain_name => {
      name   = o.resource_record_name
      type   = o.resource_record_type
      record = o.resource_record_value
    }
  }

  zone_id         = var.route53_zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "this" {
  certificate_arn         = aws_acm_certificate.this.arn
  validation_record_fqdns = [for r in aws_route53_record.validation : r.fqdn]
}

# ---- access log bucket ----

resource "aws_s3_bucket" "logs" {
  bucket = "${var.name}-collab-edge-logs-${data.aws_caller_identity.current.account_id}"

  tags = {
    "afs-component" = "collab-edge"
  }
}

resource "aws_s3_bucket_public_access_block" "logs" {
  bucket                  = aws_s3_bucket.logs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "logs" {
  bucket = aws_s3_bucket.logs.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

# ALB access logs support only SSE-S3 (AES256).
resource "aws_s3_bucket_server_side_encryption_configuration" "logs" {
  bucket = aws_s3_bucket.logs.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "logs" {
  bucket = aws_s3_bucket.logs.id

  rule {
    id     = "expire"
    status = "Enabled"

    filter {}

    expiration {
      days = var.log_retention_days
    }
  }
}

data "aws_iam_policy_document" "logs" {
  statement {
    sid       = "DeliverAlbAccessLogs"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.logs.arn}/alb/*"]

    principals {
      type        = "Service"
      identifiers = ["logdelivery.elasticloadbalancing.amazonaws.com"]
    }
  }

  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.logs.arn, "${aws_s3_bucket.logs.arn}/*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "logs" {
  bucket = aws_s3_bucket.logs.id
  policy = data.aws_iam_policy_document.logs.json

  depends_on = [aws_s3_bucket_public_access_block.logs]
}

# ---- load balancer ----

resource "aws_security_group" "alb" {
  name_prefix = "${var.name}-collab-edge-alb-"
  description = "Internet-facing ALB of the collaboration edge"
  vpc_id      = var.vpc_id

  tags = {
    "afs-component" = "collab-edge"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "https_v4" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from anywhere; the WAF decides"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_ingress_rule" "https_v6" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from anywhere (IPv6); the WAF decides"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv6         = "::/0"
}

resource "aws_vpc_security_group_ingress_rule" "http_v4" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTP, only to redirect to HTTPS"
  ip_protocol       = "tcp"
  from_port         = 80
  to_port           = 80
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_ingress_rule" "http_v6" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTP (IPv6), only to redirect to HTTPS"
  ip_protocol       = "tcp"
  from_port         = 80
  to_port           = 80
  cidr_ipv6         = "::/0"
}

resource "aws_vpc_security_group_egress_rule" "edge_https" {
  for_each = toset(var.edge_security_group_ids)

  security_group_id            = aws_security_group.alb.id
  description                  = "To the afs-edge pods only"
  ip_protocol                  = "tcp"
  from_port                    = var.edge_port
  to_port                      = var.edge_port
  referenced_security_group_id = each.value
}

resource "aws_vpc_security_group_egress_rule" "edge_health" {
  for_each = toset(var.edge_security_group_ids)

  security_group_id            = aws_security_group.alb.id
  description                  = "Health checks of the afs-edge pods"
  ip_protocol                  = "tcp"
  from_port                    = var.health_check_port
  to_port                      = var.health_check_port
  referenced_security_group_id = each.value
}

resource "aws_lb" "this" {
  name                       = "${var.name}-collab-edge"
  load_balancer_type         = "application"
  internal                   = false
  ip_address_type            = "dualstack"
  subnets                    = var.public_subnet_ids
  security_groups            = [aws_security_group.alb.id]
  drop_invalid_header_fields = true
  enable_deletion_protection = true
  # A WebSocket or event stream is closed by the edge after 10 minutes without traffic; the ALB waits longer.
  idle_timeout = 660

  access_logs {
    bucket  = aws_s3_bucket.logs.id
    prefix  = "alb"
    enabled = true
  }

  tags = {
    "afs-component" = "collab-edge"
  }

  depends_on = [aws_s3_bucket_policy.logs]
}

resource "aws_lb_target_group" "edge" {
  name_prefix = "afse-"
  vpc_id      = var.vpc_id
  target_type = "ip"
  port        = var.edge_port
  protocol    = "HTTPS"

  health_check {
    protocol            = "HTTP"
    port                = tostring(var.health_check_port)
    path                = "/readyz"
    matcher             = "200"
    interval            = 10
    healthy_threshold   = 2
    unhealthy_threshold = 2
  }

  deregistration_delay = 30

  tags = {
    "afs-component" = "collab-edge"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.this.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "redirect"

    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate_validation.this.certificate_arn

  default_action {
    type = "fixed-response"

    fixed_response {
      content_type = "text/plain"
      message_body = "Not found"
      status_code  = "404"
    }
  }
}

resource "aws_lb_listener_rule" "edge" {
  listener_arn = aws_lb_listener.https.arn
  priority     = 10

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.edge.arn
  }

  condition {
    host_header {
      values = local.hosts
    }
  }
}

# ---- DNS ----

resource "aws_route53_record" "alias" {
  for_each = {
    for p in setproduct(local.hosts, ["A", "AAAA"]) : "${p[0]}-${p[1]}" => { name = p[0], type = p[1] }
  }

  zone_id = var.route53_zone_id
  name    = each.value.name
  type    = each.value.type

  alias {
    name                   = aws_lb.this.dns_name
    zone_id                = aws_lb.this.zone_id
    evaluate_target_health = true
  }
}

# ---- WAF ----

# The two IP sets start open, so the allow rule holds with no list set. The reconciler owns the addresses
# after creation (collab.allowed_source_cidrs); Terraform ignores them.
resource "aws_wafv2_ip_set" "allow_v4" {
  name               = "${var.name}-collab-allow-v4"
  scope              = "REGIONAL"
  ip_address_version = "IPV4"
  addresses          = ["0.0.0.0/0"]

  lifecycle {
    ignore_changes = [addresses]
  }
}

resource "aws_wafv2_ip_set" "allow_v6" {
  name               = "${var.name}-collab-allow-v6"
  scope              = "REGIONAL"
  ip_address_version = "IPV6"
  addresses          = ["::/0"]

  lifecycle {
    ignore_changes = [addresses]
  }
}

resource "aws_wafv2_web_acl" "this" {
  name  = "${var.name}-collab-edge"
  scope = "REGIONAL"

  default_action {
    allow {}
  }

  rule {
    name     = "allow-list"
    priority = 0

    action {
      block {}
    }

    statement {
      not_statement {
        statement {
          or_statement {
            statement {
              ip_set_reference_statement {
                arn = aws_wafv2_ip_set.allow_v4.arn
              }
            }
            statement {
              ip_set_reference_statement {
                arn = aws_wafv2_ip_set.allow_v6.arn
              }
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "allow-list"
      sampled_requests_enabled   = true
    }
  }

  rule {
    name     = "rate-per-ip"
    priority = 1

    action {
      block {}
    }

    statement {
      rate_based_statement {
        limit              = var.rate_limit_per_5_minutes
        aggregate_key_type = "IP"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "rate-per-ip"
      sampled_requests_enabled   = true
    }
  }

  dynamic "rule" {
    for_each = {
      "common"           = { priority = 10, group = "AWSManagedRulesCommonRuleSet" }
      "known-bad-inputs" = { priority = 11, group = "AWSManagedRulesKnownBadInputsRuleSet" }
      "ip-reputation"    = { priority = 12, group = "AWSManagedRulesAmazonIpReputationList" }
      "anonymous-ip"     = { priority = 13, group = "AWSManagedRulesAnonymousIpList" }
    }

    content {
      name     = rule.key
      priority = rule.value.priority

      override_action {
        none {}
      }

      statement {
        managed_rule_group_statement {
          name        = rule.value.group
          vendor_name = "AWS"
        }
      }

      visibility_config {
        cloudwatch_metrics_enabled = true
        metric_name                = rule.key
        sampled_requests_enabled   = true
      }
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${var.name}-collab-edge"
    sampled_requests_enabled   = true
  }

  tags = {
    "afs-component" = "collab-edge"
  }
}

resource "aws_wafv2_web_acl_association" "this" {
  resource_arn = aws_lb.this.arn
  web_acl_arn  = aws_wafv2_web_acl.this.arn
}

resource "aws_cloudwatch_log_group" "waf" {
  # The name must start with aws-waf-logs-.
  name              = "aws-waf-logs-${var.name}-collab-edge"
  retention_in_days = var.log_retention_days
}

resource "aws_wafv2_web_acl_logging_configuration" "this" {
  resource_arn            = aws_wafv2_web_acl.this.arn
  log_destination_configs = [aws_cloudwatch_log_group.waf.arn]

  redacted_fields {
    single_header {
      name = "authorization"
    }
  }

  redacted_fields {
    single_header {
      name = "x-afs-authorization"
    }
  }

  redacted_fields {
    single_header {
      name = "cookie"
    }
  }
}

# ---- sealing keys of the sign-in state ----

# The container only: the operator writes two 32-byte base64 keys (current and previous) and syncs them to the
# Kubernetes Secret the chart reads (edge.stateKey.secretName). No value is created or stored by Terraform, so
# none reaches its state. Rotating every 30 days is an operator procedure: move current to previous, write a new
# current (see user-docs/collaboration-edge.md).
resource "aws_secretsmanager_secret" "state_key" {
  name                    = "${var.name}/edge/state-key"
  description             = "Sealing keys of the afs-edge sign-in state (current and previous)"
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = 30

  tags = {
    "afs-component" = "collab-edge"
  }
}
