output "state_bucket" {
  description = "The S3 bucket that holds both roots' Terraform state."
  value       = aws_s3_bucket.state.bucket
}

output "ecr_registry" {
  description = "The account's ECR registry host (images are <registry>/doodle-arena/<name>:<git sha>)."
  value       = split("/", aws_ecr_repository.image["lobby"].repository_url)[0]
}

output "backup_bucket" {
  description = "Where aws-down writes each session's backup and aws-up restores the latest (ADR 0016)."
  value       = aws_s3_bucket.backups.bucket
}
