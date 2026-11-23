variable "name" { type = string }

variable "vpc_id" {
  description = "VPC of the internal ALB and the EKS pods. The private zone is associated with it."
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnets (at least two availability zones) of the internal ALB."
  type        = list(string)
}

variable "private_base_domain" {
  description = "orchestration.private_base_domain: the apex of the private orchestration hostnames (<team>.<domain>, api.<team>.<domain>). It resolves only inside the associated VPCs."
  type        = string
}

variable "certificate_arn" {
  description = "Certificate for private_base_domain and *.private_base_domain, in the ALB's region (for example issued by a private certificate authority). A public ACM certificate cannot be validated for a private-only name, so the operator supplies it."
  type        = string
}

variable "additional_zone_vpc_ids" {
  description = "Further VPCs (for example the corporate network's) that resolve the private zone. Each is a map {vpc_id, region}."
  type        = list(object({ vpc_id = string, region = string }))
  default     = []
}

variable "allowed_ingress_cidrs" {
  description = "Internal CIDR blocks (corporate network, VPN) allowed to reach the internal ALB on 443. There is deliberately no default: an internal ALB is still reachable from every routed network."
  type        = list(string)
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

variable "access_log_bucket" {
  description = "S3 bucket for the ALB access logs (its policy must allow ALB log delivery). Empty turns access logs off."
  type        = string
  default     = ""
}

variable "log_retention_days" {
  description = "Retention of the WAF logs: 13 months (COL-31)."
  type        = number
  default     = 395
}

variable "rate_limit_per_5_minutes" {
  description = "Requests per source IP in five minutes before the WAF blocks it."
  type        = number
  default     = 5000
}
