# Both buckets (design doc D13): round-logs belongs to the game servers (assets reads it, the
# claim check), drawings to assets. Neither is public: browsers get presigned URLs. They are
# emptied and deleted with the stack.

data "aws_caller_identity" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id
  buckets = {
    round-logs = "doodle-arena-round-logs-${local.account_id}"
    drawings   = "doodle-arena-drawings-${local.account_id}"
    config     = "doodle-arena-config-${local.account_id}" # the data host's compose bundle
  }
}

resource "aws_s3_bucket" "bucket" {
  for_each      = local.buckets
  bucket        = each.value
  force_destroy = true
}

resource "aws_s3_bucket_public_access_block" "bucket" {
  for_each                = aws_s3_bucket.bucket
  bucket                  = each.value.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "bucket" {
  for_each = aws_s3_bucket.bucket
  bucket   = each.value.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Stroke logs are needed only until assets has stored the replay: 7 days, as SeaweedFS's TTL
# does locally (infra/seaweedfs/create-buckets.sh).
resource "aws_s3_bucket_lifecycle_configuration" "round_logs" {
  bucket = aws_s3_bucket.bucket["round-logs"].id
  rule {
    id     = "expire-after-7-days"
    status = "Enabled"
    filter {}
    expiration {
      days = 7
    }
  }
}
