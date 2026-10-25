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

# The Route 53 hosted zone that holds $1: the longest of its suffixes Route 53 has a public zone
# for (doodle-arena.icanfly.click, then icanfly.click). Prints nothing when none matches.
find_zone() {
  local name="$1" found
  while [[ "$name" == *.* ]]; do
    found="$(aws route53 list-hosted-zones-by-name --dns-name "$name" --max-items 1 \
      --query "HostedZones[?Name==\`$name.\` && Config.PrivateZone==\`false\`].Name | [0]" --output text)"
    if [[ "$found" == "$name." ]]; then
      echo "$name"
      return
    fi
    name="${name#*.}"
  done
}

# The stack root's settings: the domain (required), its hosted zone (ZONE, default: found from
# DOMAIN), who may reach the load balancer (ALLOWED_CIDRS, default everyone; TEAM_CIDRS is its old
# name) and the image tag (default: HEAD, which push-images.sh pushed).
stack_vars() {
  : "${DOMAIN:?set DOMAIN to the host name of the app, for example DOMAIN=doodle-arena.example.com}"
  ZONE="${ZONE:-$(find_zone "$DOMAIN")}"
  : "${ZONE:?no Route 53 hosted zone found for $DOMAIN, or an AWS error above; set ZONE to the name of the zone}"
  ALLOWED_CIDRS="${ALLOWED_CIDRS:-${TEAM_CIDRS:-0.0.0.0/0}}"
  IMAGE_TAG="${IMAGE_TAG:-$(git -C "$ROOT" rev-parse --short=12 HEAD)}"
  local cidrs
  cidrs="$(python3 -c 'import json, sys; print(json.dumps(sys.argv[1].split(",")))' "$ALLOWED_CIDRS")"
  # shellcheck disable=SC2034 # used by the scripts that source this file
  TF_STACK_VARS=(-var "domain_name=$DOMAIN" -var "zone_name=$ZONE" -var "allowed_cidrs=$cidrs" -var "image_tag=$IMAGE_TAG")
}

# Points the stack root at the state bucket the bootstrap root made.
init_stack() {
  printf 'bucket = "doodle-arena-tfstate-%s"\n' "$(account_id)" > "$TF/stack/backend.hcl"
  terraform -chdir="$TF/stack" init -input=false -reconfigure -backend-config=backend.hcl >/dev/null
}

tf_output() { terraform -chdir="$TF/stack" output -raw "$1"; }

# ---------------------------------------------------------------- backups (ADR 0016)

# The bootstrap root's backup bucket (BACKUP_BUCKET overrides it, for the drill that makes a
# backup fail) and the stack's drawings bucket.
backup_bucket() { echo "${BACKUP_BUCKET:-doodle-arena-backups-$(account_id)}"; }
drawings_bucket() { echo "doodle-arena-drawings-$(account_id)"; }

# The image tag (git SHA) of a service's current task definition, running or not.
task_image_tag() {
  local definition image
  definition="$(aws ecs describe-services --cluster doodle-arena --services "$1" \
    --query 'services[0].taskDefinition' --output text)"
  image="$(aws ecs describe-task-definition --task-definition "$definition" \
    --query 'taskDefinition.containerDefinitions[0].image' --output text)"
  echo "${image##*:}"
}

# The data host's state ("running", "stopped"...; nothing when there is none). An AWS error fails,
# so it is never mistaken for "no data host" (aws-down would then destroy without a backup).
data_host_state() {
  local state
  state="$(aws ec2 describe-instances --filters Name=tag:Name,Values=doodle-data-host Name=tag:Root,Values=stack \
    Name=instance-state-name,Values=pending,running,stopping,stopped \
    --query 'Reservations[0].Instances[0].State.Name' --output text)" || return 1
  [[ "$state" == None ]] || echo "$state"
}

# The data host's instance id while it runs, else nothing; an AWS error fails.
data_host_id() {
  local id
  id="$(aws ec2 describe-instances --filters Name=tag:Name,Values=doodle-data-host Name=tag:Root,Values=stack \
    Name=instance-state-name,Values=running --query 'Reservations[0].Instances[0].InstanceId' --output text)" || return 1
  [[ "$id" == None ]] || echo "$id"
}

# has_manifest <bucket> <backup id>: 0 when the backup is complete, 1 when it has no manifest,
# 2 on any other error (an expired session, the network): never read as "incomplete".
has_manifest() {
  local err
  if err="$(aws s3api head-object --bucket "$1" --key "backups/$2/manifest.json" 2>&1 >/dev/null)"; then
    return 0
  fi
  [[ "$err" == *"Not Found"* || "$err" == *404* || "$err" == *NoSuchKey* ]] && return 1
  echo "$err" >&2
  return 2
}

# backup_ids <bucket>: every backup id (complete or not), newest first. Fails on an AWS error.
backup_ids() {
  local prefixes
  prefixes="$(aws s3api list-objects-v2 --bucket "$1" --prefix backups/ --delimiter / \
    --query 'CommonPrefixes[].Prefix' --output text)" || return 1
  echo "$prefixes" | tr '\t' '\n' | sed -n 's|^backups/\(.*\)/$|\1|p' | sort -r
}

# s3_totals <bucket> <prefix>: "<objects> <bytes>", 0 0 for none. Fails on an AWS error (aws s3 ls
# exits 1 on an empty prefix, so it can't tell "empty" from "failed").
s3_totals() {
  aws s3api list-objects-v2 --bucket "$1" --prefix "$2" \
    --query '[length(Contents || `[]`), sum(Contents[].Size || `[]`)]' --output text | tr '\t' ' '
}

# host_run <timeout seconds> <script>: runs a script on the data host through SSM Run Command and
# prints its stdout. Its stderr is copied to ours; a command that doesn't succeed returns 1. A
# fresh data host may not have registered with SSM yet, so sending is retried for 5 minutes.
host_run() {
  local timeout="$1" script="$2" instance params id="" status="" _
  instance="$(data_host_id)" || return 1
  if [[ -z "$instance" ]]; then
    echo "the data host isn't running" >&2
    return 1
  fi
  params="$(python3 -c 'import json, sys; print(json.dumps({"commands": [sys.argv[1]], "executionTimeout": [sys.argv[2]]}))' "$script" "$timeout")"
  local err errors=0 errfile
  errfile="$(mktemp)"
  for _ in $(seq 30); do
    if id="$(aws ssm send-command --instance-ids "$instance" --document-name AWS-RunShellScript \
      --parameters "$params" --query Command.CommandId --output text 2>"$errfile")"; then
      break
    fi
    id=""
    err="$(cat "$errfile")"
    # Only an instance SSM doesn't know yet is worth waiting for; anything else is final.
    if [[ "$err" != *InvalidInstanceId* ]]; then
      rm -f "$errfile"
      echo "$err" >&2
      return 1
    fi
    sleep 10
  done
  rm -f "$errfile"
  if [[ -z "$id" ]]; then
    echo "SSM couldn't reach the data host ($instance) for 5 minutes" >&2
    return 1
  fi
  while :; do
    # Right after send-command the invocation may not exist yet; a minute of errors is the end.
    if status="$(aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" \
      --query Status --output text 2>/dev/null)"; then
      errors=0
    else
      status=Pending
      errors=$((errors + 1))
      if ((errors > 12)); then
        echo "can't read SSM command $id's status (an expired session?)" >&2
        return 1
      fi
    fi
    case "$status" in Pending | InProgress | Delayed) sleep 5 ;; *) break ;; esac
  done
  aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" \
    --query StandardErrorContent --output text >&2 || true
  aws ssm get-command-invocation --command-id "$id" --instance-id "$instance" \
    --query StandardOutputContent --output text
  if [[ "$status" != Success ]]; then
    echo "the command on the data host ended $status (SSM command $id)" >&2
    return 1
  fi
}

# backup_telemetry <direction> <result> <backup id> <summary file> [message] [drawings seconds]
# [drawings bytes] [total seconds]: CloudWatch metrics (DoodleArena/Backups) and log lines
# (/doodle-arena/backups), which outlive the stack (docs/observability.md, "Backups"). A failure
# here only warns: telemetry must not decide whether data is kept.
backup_telemetry() {
  local direction="$1" result="$2" id="$3" summary="$4" message="${5:-}" stream file
  file="$(mktemp)"
  if python3 "$ROOT/scripts/aws/backups.py" metrics "$direction" "$result" "$summary" "${6:-}" "${7:-}" "${8:-}" > "$file" &&
    aws cloudwatch put-metric-data --namespace DoodleArena/Backups --metric-data "file://$file"; then :; else
    echo "warning: the $direction metrics didn't reach CloudWatch" >&2
  fi
  stream="$direction-${id:-none}-$(date -u +%Y%m%dT%H%M%SZ)"
  if python3 "$ROOT/scripts/aws/backups.py" log-events "$direction" "$result" "${id:-none}" \
    "$(git -C "$ROOT" rev-parse --short=12 HEAD)" "$summary" "$message" > "$file" &&
    aws logs create-log-stream --log-group-name /doodle-arena/backups --log-stream-name "$stream" &&
    aws logs put-log-events --log-group-name /doodle-arena/backups --log-stream-name "$stream" \
      --log-events "file://$file" >/dev/null; then :; else
    echo "warning: the $direction log lines didn't reach CloudWatch Logs" >&2
  fi
  rm -f "$file"
}

# wait_stable <service>...: aws ecs wait services-stable for any number of services (it takes at
# most 10 per call).
wait_stable() {
  local all=("$@") i
  for ((i = 0; i < ${#all[@]}; i += 10)); do
    aws ecs wait services-stable --cluster "$cluster" --services "${all[@]:i:10}"
  done
}
