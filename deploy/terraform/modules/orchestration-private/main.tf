# The private entry of the team orchestration agent (ORC-13..15): an internal ALB in front of afs-edge, a private
# Route 53 zone, and a regional WAF. It serves the private hostnames (orchestration.private_base_domain) that
# afs-edge answers at level private; the public entry is modules/collab-edge. The two ALBs share the afs-edge
# pods; afs-edge decides by hostname which surfaces a request may reach, so a surface exposed at one level is a
# 404 at the other. The certificate is supplied (var.certificate_arn); no credential or account id is written here.

locals {
  hosts = [var.private_base_domain, "*.${var.private_base_domain}"]
}

# ---- DNS ----

resource "aws_route53_zone" "private" {
  name = var.private_base_domain

  vpc {
    vpc_id = var.vpc_id
  }

  # Further VPCs are associated below; Terraform must not fight the associations.
  lifecycle {
    ignore_changes = [vpc]
  }

  tags = {
    "afs-component" = "orchestration-private"
  }
}

resource "aws_route53_zone_association" "extra" {
  for_each = { for v in var.additional_zone_vpc_ids : v.vpc_id => v }

  zone_id    = aws_route53_zone.private.zone_id
  vpc_id     = each.value.vpc_id
  vpc_region = each.value.region
}

resource "aws_route53_record" "alias" {
  for_each = {
    for p in setproduct([var.private_base_domain, "*.${var.private_base_domain}"], ["A"]) : "${p[0]}-${p[1]}" => { name = p[0], type = p[1] }
  }

  zone_id = aws_route53_zone.private.zone_id
  name    = each.value.name
  type    = each.value.type

  alias {
    name                   = aws_lb.this.dns_name
    zone_id                = aws_lb.this.zone_id
    evaluate_target_health = true
  }
}

# ---- load balancer ----

resource "aws_security_group" "alb" {
  name_prefix = "${var.name}-orch-private-alb-"
  description = "Internal ALB of the private orchestration entry"
  vpc_id      = var.vpc_id

  tags = {
    "afs-component" = "orchestration-private"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "https" {
  for_each = toset(var.allowed_ingress_cidrs)

  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from an internal network"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = each.value
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
  name                       = "${var.name}-orch-private"
  load_balancer_type         = "application"
  internal                   = true
  ip_address_type            = "ipv4"
  subnets                    = var.private_subnet_ids
  security_groups            = [aws_security_group.alb.id]
  drop_invalid_header_fields = true
  enable_deletion_protection = true
  # A WebSocket or event stream is closed by the edge after 10 minutes without traffic; the ALB waits longer.
  idle_timeout = 660

  dynamic "access_logs" {
    for_each = var.access_log_bucket == "" ? [] : [var.access_log_bucket]

    content {
      bucket  = access_logs.value
      prefix  = "orch-private"
      enabled = true
    }
  }

  tags = {
    "afs-component" = "orchestration-private"
  }
}

resource "aws_lb_target_group" "edge" {
  name_prefix = "afsp-"
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
    "afs-component" = "orchestration-private"
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.certificate_arn

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

# ---- WAF ----

resource "aws_wafv2_web_acl" "this" {
  name  = "${var.name}-orch-private"
  scope = "REGIONAL"

  default_action {
    allow {}
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
    metric_name                = "${var.name}-orch-private"
    sampled_requests_enabled   = true
  }

  tags = {
    "afs-component" = "orchestration-private"
  }
}

resource "aws_wafv2_web_acl_association" "this" {
  resource_arn = aws_lb.this.arn
  web_acl_arn  = aws_wafv2_web_acl.this.arn
}

resource "aws_cloudwatch_log_group" "waf" {
  # The name must start with aws-waf-logs-.
  name              = "aws-waf-logs-${var.name}-orch-private"
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
