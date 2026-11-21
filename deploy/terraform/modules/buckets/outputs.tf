output "export_bucket_arn" {
  value = aws_s3_bucket.this["export"].arn
}

output "export_bucket_name" {
  value = aws_s3_bucket.this["export"].id
}

output "reports_bucket_arn" {
  value = aws_s3_bucket.this["reports"].arn
}

output "reports_bucket_name" {
  value = aws_s3_bucket.this["reports"].id
}

output "kms_key_arn" {
  value = aws_kms_key.buckets.arn
}

output "kms_key_id" {
  value = aws_kms_key.buckets.key_id
}
