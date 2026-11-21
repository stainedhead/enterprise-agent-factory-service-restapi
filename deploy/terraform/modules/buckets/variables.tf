variable "name" { type = string }

variable "export_retention_days" {
  description = "Agent export bundles expire after this many days; pre-signed download links last 7 days."
  type        = number
  default     = 14
}

variable "reports_retention_days" {
  description = "Scheduled report and showback CSVs expire after this many days."
  type        = number
  default     = 400
}
