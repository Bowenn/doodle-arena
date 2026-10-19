#!/usr/bin/env bash
# Shared by scripts/aws/*.sh (ADR 0003, docs/setup.md "AWS"). Sourced, not run.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TF="$ROOT/infra/terraform"
export AWS_PROFILE="${AWS_PROFILE:-doodle-arena}"
export AWS_REGION="${AWS_REGION:-us-west-2}"

# Fails early, with the fix, when the SSO session has expired.
require_aws() {
  if ! aws sts get-caller-identity >/dev/null 2>&1; then
    echo "No AWS session for profile $AWS_PROFILE. Sign in: aws sso login --profile $AWS_PROFILE" >&2
    exit 1
  fi
}

account_id() { aws sts get-caller-identity --query Account --output text; }

# The stack root's settings: the domain (required), the team's addresses (default: this
# machine's public address) and the image tag (default: HEAD, which push-images.sh pushed).
stack_vars() {
  : "${DOMAIN:?set DOMAIN to the Route 53 domain, for example DOMAIN=example.com}"
  TEAM_CIDRS="${TEAM_CIDRS:-$(curl -fsS https://checkip.amazonaws.com)/32}"
  IMAGE_TAG="${IMAGE_TAG:-$(git -C "$ROOT" rev-parse --short=12 HEAD)}"
  local cidrs
  cidrs="$(python3 -c 'import json, sys; print(json.dumps(sys.argv[1].split(",")))' "$TEAM_CIDRS")"
  # shellcheck disable=SC2034 # used by the scripts that source this file
  TF_STACK_VARS=(-var "domain_name=$DOMAIN" -var "team_cidrs=$cidrs" -var "image_tag=$IMAGE_TAG")
}

# Points the stack root at the state bucket the bootstrap root made.
init_stack() {
  printf 'bucket = "doodle-arena-tfstate-%s"\n' "$(account_id)" > "$TF/stack/backend.hcl"
  terraform -chdir="$TF/stack" init -input=false -reconfigure -backend-config=backend.hcl >/dev/null
}

tf_output() { terraform -chdir="$TF/stack" output -raw "$1"; }
