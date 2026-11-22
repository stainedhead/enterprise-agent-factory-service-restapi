variable "name" { type = string }

variable "vpc_id" {
  description = "VPC of the ALB and the EKS pods."
  type        = string
}

variable "public_subnet_ids" {
  description = "Public subnets (at least two availability zones) of the internet-facing ALB."
  type        = list(string)
}

variable "base_domain" {
  description = "collab.base_domain: the apex of the collaboration hostnames. The certificate covers it and *.<base_domain>."
  type        = string
}

variable "route53_zone_id" {
  description = "Hosted zone of base_domain, for the certificate validation and the alias records."
  type        = string
}

variable "edge_security_group_ids" {
  description = "Security groups of the afs-edge pods: the only egress of the ALB."
  type        = list(string)
}

variable "edge_port" {
  description = "HTTPS port of afs-edge."
  type        = number
  default     = 8443
}

variable "health_check_port" {
  description = "Port of afs-edge's /readyz (the operations port)."
  type        = number
  default     = 9090
}

variable "log_retention_days" {
  description = "Retention of the ALB access logs and the WAF logs: 13 months (COL-31)."
  type        = number
  default     = 395
}

variable "rate_limit_per_5_minutes" {
  description = "Requests per source IP in five minutes before the WAF blocks it."
  type        = number
  default     = 2000
}

variable "kms_key_arn" {
  description = "Environment KMS key that encrypts the state-key secret."
  type        = string
}
