#!/usr/bin/env bash
# Ends a session (ADR 0003): backs up the data (ADR 0016, scripts/aws/backup.sh), then destroys
# the whole stack. The bootstrap root (state, images, secrets, backups) and the budget stay.
#   DOMAIN=example.com [SKIP_BACKUP=1] scripts/aws/down.sh
# A backup that fails stops the teardown before anything is deleted. SKIP_BACKUP=1 ends the
# session without keeping its data; with no data host running there is nothing to back up.
# A destroy that fails halfway (a lost network, an AWS API hiccup) is retried, and afterwards
# the script looks for anything still billing: a stack half torn down keeps costing money.
source "$(dirname "$0")/common.sh"
require_aws
stack_vars
init_stack

if [[ -n "${SKIP_BACKUP:-}" ]]; then
  echo "SKIP_BACKUP is set: this session's data will not be kept."
else
  # Only a data host that is certainly gone skips the backup; a lookup that fails stops here.
  if ! state="$(data_host_state)"; then
    echo "Couldn't look up the data host: nothing destroyed. Retry, or SKIP_BACKUP=1 to discard the data." >&2
    exit 1
  fi
  case "$state" in
    "") echo "No data host exists, so there is nothing to back up (a rerun after a destroy that failed halfway)." ;;
    running) "$(dirname "$0")/backup.sh" || exit 1 ;;
    *)
      echo "The data host is $state, so it can't be backed up: nothing destroyed. Start it" >&2
      echo "(aws ec2 start-instances) and rerun, or SKIP_BACKUP=1 to discard the data." >&2
      exit 1
      ;;
  esac
fi

# The first destroy shows the plan and asks; a retry destroys the same stack, so it doesn't.
approve=()
for attempt in 1 2 3; do
  # ${approve[@]+...}: an empty array is "unbound" under set -u in macOS's bash 3.2.
  if terraform -chdir="$TF/stack" destroy "${TF_STACK_VARS[@]}" ${approve[@]+"${approve[@]}"}; then
    break
  fi
  if ((attempt == 3)); then
    echo "terraform destroy failed 3 times" >&2
    break
  fi
  echo "terraform destroy failed (attempt $attempt): retrying in 30 s" >&2
  approve=(-auto-approve)
  sleep 30
done

# What costs money while it exists. Every stack resource carries the provider's default tag
# Root=stack (versions.tf); the cluster and the load balancer are found by name.
left=()
add() { [[ -n "$2" && "$2" != None ]] && left+=("$1: $2"); return 0; }
add "NAT gateways" "$(aws ec2 describe-nat-gateways --filter Name=tag:Root,Values=stack \
  Name=state,Values=pending,available,deleting --query 'NatGateways[].NatGatewayId' --output text)"
add "Elastic IPs" "$(aws ec2 describe-addresses --filters Name=tag:Root,Values=stack \
  --query 'Addresses[].PublicIp' --output text)"
add "EC2 instances" "$(aws ec2 describe-instances --filters Name=tag:Root,Values=stack \
  Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' --output text)"
add "load balancers" "$(aws elbv2 describe-load-balancers --names doodle-arena \
  --query 'LoadBalancers[].LoadBalancerName' --output text 2>/dev/null || true)"
add "ECS cluster" "$(aws ecs describe-clusters --clusters doodle-arena \
  --query 'clusters[?status==`ACTIVE`].clusterName' --output text)"
if ((${#left[@]})); then
  echo "STACK STILL BILLING: these still exist after the destroy:" >&2
  printf '  %s\n' "${left[@]}" >&2
  echo "Run DOMAIN=$DOMAIN make aws-down again; if it keeps failing, delete them in the console." >&2
  exit 1
fi
echo "Stack destroyed: no NAT gateway, Elastic IP, instance, load balancer or ECS cluster is left."
