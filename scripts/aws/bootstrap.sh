#!/usr/bin/env bash
# Creates (or updates) the bootstrap root: the state bucket, the ECR repositories and the empty
# third-party secrets (ADR 0003). The first run has no bucket for its own state yet: it applies
# with local state, then moves the state into the bucket it just made.
source "$(dirname "$0")/common.sh"
require_aws

dir="$TF/bootstrap"
bucket="doodle-arena-tfstate-$(account_id)"
if aws s3api head-bucket --bucket "$bucket" 2>/dev/null; then
  terraform -chdir="$dir" init -input=false -reconfigure -backend-config="bucket=$bucket" >/dev/null
  terraform -chdir="$dir" apply
else
  echo "First run: applying with local state, then moving it into $bucket"
  printf 'terraform {\n  backend "local" {}\n}\n' > "$dir/backend_override.tf"
  trap 'rm -f "$dir/backend_override.tf"' EXIT
  terraform -chdir="$dir" init -input=false -reconfigure >/dev/null
  terraform -chdir="$dir" apply
  rm -f "$dir/backend_override.tf"
  terraform -chdir="$dir" init -input=false -migrate-state -force-copy -backend-config="bucket=$bucket"
  rm -f "$dir/terraform.tfstate" "$dir/terraform.tfstate.backup"
fi
terraform -chdir="$dir" output
