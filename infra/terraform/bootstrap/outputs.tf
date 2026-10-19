output "state_bucket" {
  description = "The S3 bucket that holds both roots' Terraform state."
  value       = aws_s3_bucket.state.bucket
}

output "ecr_registry" {
  description = "The account's ECR registry host (images are <registry>/doodle-arena/<name>:<git sha>)."
  value       = split("/", aws_ecr_repository.image["lobby"].repository_url)[0]
}
