# What outlives every session (ADR 0003): the Terraform state, the image repositories and the
# third-party secrets. The per-session stack (../stack) is created and destroyed around it. The
# budget alarm (doodle-arena-monthly, $35) was created by hand before anything else and stays
# outside Terraform, so no destroy can remove it.

data "aws_caller_identity" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id
  # One repository per image the stack runs (webhook-receiver is local only).
  images = ["lobby", "game-server", "notifier", "auth", "ai", "assets", "marketplace", "gateway", "web"]
}

# ---------------------------------------------------------------- Terraform state

resource "aws_s3_bucket" "state" {
  bucket = "doodle-arena-tfstate-${local.account_id}"
}

resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id
  versioning_configuration {
    status = "Enabled" # an overwritten or deleted state can be restored
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket                  = aws_s3_bucket.state.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# The state holds generated passwords and signing keys, so it travels only over TLS.
resource "aws_s3_bucket_policy" "state" {
  bucket = aws_s3_bucket.state.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TLSOnly"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.state.arn, "${aws_s3_bucket.state.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })
}

resource "aws_s3_bucket_lifecycle_configuration" "state" {
  bucket = aws_s3_bucket.state.id
  rule {
    id     = "old-versions"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
  }
}

# ---------------------------------------------------------------- images

resource "aws_ecr_repository" "image" {
  for_each = toset(local.images)

  name = "doodle-arena/${each.key}"
  # Tags are git SHAs (scripts/aws/push-images.sh): a tag always names the same image.
  image_tag_mutability = "IMMUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_lifecycle_policy" "image" {
  for_each   = aws_ecr_repository.image
  repository = each.value.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the last 5 images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 5 }
      action       = { type = "expire" }
    }]
  })
}

# ---------------------------------------------------------------- third-party secrets

# Created empty; a human puts each value (docs/setup.md, "AWS"):
#   aws ssm put-parameter --profile doodle-arena --overwrite --type SecureString --name <name> --value ...
# Terraform never reads the real values back into its state: this root ignores changes to them,
# and the stack root reads them without decryption, for their ARNs only.
resource "aws_ssm_parameter" "third_party" {
  for_each = {
    "google/client-id"          = "Google OAuth client id (auth)"
    "google/client-secret"      = "Google OAuth client secret (auth)"
    "stripe/secret-key"         = "Stripe test-mode secret key, sk_test_ (marketplace)"
    "stripe/webhook-secret"     = "Stripe webhook signing secret for the AWS endpoint, whsec_ (marketplace)"
    "discord/alert-webhook-url" = "Discord channel webhook for Grafana alerts (otel-lgtm)"
  }

  name        = "/doodle-arena/${each.key}"
  description = each.value
  type        = "SecureString"
  value       = "unset"

  lifecycle {
    ignore_changes = [value]
  }
}
