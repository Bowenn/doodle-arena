#!/usr/bin/env bash
# Starts a session in AWS (ADR 0003): creates the stack with every service at 0, restores the
# latest backup into its empty stores (ADR 0016, scripts/aws/restore.sh), runs the database
# migrations as one-off tasks, then starts the services and waits until they are stable.
# On a stack that is already running it redeploys instead: the other services, then one game
# server at a time.
#   DOMAIN=example.com [ALLOWED_CIDRS=a.b.c.d/32,...] [IMAGE_TAG=<sha>] [BACKUP_ID=<id> | RESTORE=none] scripts/aws/up.sh
# It costs money from here until scripts/aws/down.sh (about $0.30-0.80 an hour, measured in
# docs/performance.md).
source "$(dirname "$0")/common.sh"
require_aws
stack_vars
init_stack

start=$SECONDS

# On a stack that is already running (a new IMAGE_TAG mid-session, or a rerun after a failure),
# step 1 must not park the services: that would be an outage. A redeploy instead replaces
# everything but the game servers first, then game-server-1 (which drains into game-server-2),
# then game-server-2 (which drains into the new game-server-1): two game servers stopping
# together couldn't take each other's rooms, and a gateway or lobby restarting during a drain
# makes every player reconnect at once (measured in AWS, PR #62).
running_tag_of() {
  [[ "$(aws ecs describe-services --cluster doodle-arena --services "$1" \
    --query 'services[?status==`ACTIVE`].runningCount | [0]' --output text 2>/dev/null)" =~ ^[1-9] ]] || return 0
  task_image_tag "$1"
}
held() { # held <server>=<tag>...: the -var that keeps those game servers at their tag
  python3 -c 'import json, sys; print("held_game_servers=" + json.dumps(dict(a.split("=", 1) for a in sys.argv[1:])))' "$@"
}
running_tag="$(running_tag_of game-server-2)"
running_tag_1="$(running_tag_of game-server-1)"

if [[ -n "$running_tag" ]]; then
  echo "==> 1/4 the stack is running (game-server-2 on $running_tag): the migrations' task definitions for $IMAGE_TAG"
  terraform -chdir="$TF/stack" apply "${TF_STACK_VARS[@]}" -var running=true -target=aws_ecs_task_definition.migrate
else
  echo "==> 1/4 the stack, no service running (open to: $ALLOWED_CIDRS, images: $IMAGE_TAG)"
  terraform -chdir="$TF/stack" apply "${TF_STACK_VARS[@]}" -var running=false
fi

cluster="$(tf_output cluster)"
subnets="$(terraform -chdir="$TF/stack" output -json private_subnets | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin)))')"
sgs="$(terraform -chdir="$TF/stack" output -json migrate_security_groups)"

# The data host may still be starting its containers: a failed migration is retried.
migrate() {
  local name="$1" sg arn code
  sg="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])[sys.argv[2]])' "$sgs" "$name")"
  for attempt in 1 2 3 4 5 6 7 8 9 10; do
    # run-task answers "None" when ECS can't place the task, and any call can hit a network
    # error: both are a failed attempt, not the end of the script.
    arn="$(aws ecs run-task --cluster "$cluster" --task-definition "doodle-$name" --launch-type FARGATE \
      --network-configuration "awsvpcConfiguration={subnets=[$subnets],securityGroups=[$sg],assignPublicIp=DISABLED}" \
      --query 'tasks[0].taskArn' --output text)" || arn=""
    code=""
    if [[ -n "$arn" && "$arn" != None ]] && aws ecs wait tasks-stopped --cluster "$cluster" --tasks "$arn"; then
      code="$(aws ecs describe-tasks --cluster "$cluster" --tasks "$arn" \
        --query 'tasks[0].containers[0].exitCode' --output text)" || code=""
    fi
    if [[ "$code" == 0 ]]; then
      echo "    $name done"
      return
    fi
    echo "    $name: ${code:+exited $code}${code:-not run} (attempt $attempt; the data host may still be starting), retrying in 20 s"
    sleep 20
  done
  echo "$name failed; its logs are in CloudWatch Logs, /doodle-arena/ecs, stream prefix $name" >&2
  exit 1
}
# Only into a stack whose services are stopped: a running stack has its data already.
if [[ -n "$running_tag" ]]; then
  echo "==> 2/4 restore: skipped, the stack is running"
else
  echo "==> 2/4 restore the latest backup"
  restore_start=$SECONDS
  "$(dirname "$0")/restore.sh"
  echo "    (restore step: $((SECONDS - restore_start)) s, including waiting for the data host)"
fi

echo "==> 3/4 migrations"
migrate auth-migrate
migrate assets-migrate

echo "==> 4/4 the services"
if [[ -n "$running_tag" && -n "$running_tag_1" && ( "$running_tag" != "$IMAGE_TAG" || "$running_tag_1" != "$IMAGE_TAG" ) ]]; then
  echo "    everything but the game servers first"
  terraform -chdir="$TF/stack" apply "${TF_STACK_VARS[@]}" -var running=true \
    -var "$(held game-server-1="$running_tag_1" game-server-2="$running_tag")" -auto-approve
  others=()
  while IFS= read -r s; do [[ "$s" == game-server-* ]] || others+=("$s"); done < <(terraform -chdir="$TF/stack" output -json services | python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)))')
  wait_stable "${others[@]}"
  echo "    then game-server-1, which drains into game-server-2"
  terraform -chdir="$TF/stack" apply "${TF_STACK_VARS[@]}" -var running=true \
    -var "$(held game-server-2="$running_tag")" -auto-approve
  aws ecs wait services-stable --cluster "$cluster" --services game-server-1
  echo "    then game-server-2, which drains into the new game-server-1"
fi
terraform -chdir="$TF/stack" apply "${TF_STACK_VARS[@]}" -var running=true -auto-approve
services=()
while IFS= read -r s; do services+=("$s"); done < <(terraform -chdir="$TF/stack" output -json services | python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)))')
wait_stable "${services[@]}"
# ECS calls a service stable while a container is still in its health check's start period (the
# marketplace's JVM takes about a minute): wait until every service answers through the gateway.
url="$(tf_output url)"
for target in healthz healthz/auth healthz/lobby healthz/game-server-1 healthz/game-server-2 healthz/assets \
  healthz/ai healthz/marketplace healthz/notifier healthz/web; do
  until [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url/$target")" == 200 ]]; do
    if ((SECONDS - start > 1800)); then
      echo "$url/$target still isn't healthy; see CloudWatch Logs, /doodle-arena/ecs" >&2
      exit 1
    fi
    sleep 5
  done
done
# A deploy marker on every dashboard (task 6.9), posted on the data host, where Grafana listens.
host_run 60 "cd /opt/doodle && GRAFANA_PASSWORD=\$(grep '^GRAFANA_ADMIN_PASSWORD=' .env | cut -d= -f2-) && \
  curl -s -o /dev/null -w '%{http_code}' -u \"admin:\$GRAFANA_PASSWORD\" -X POST -H 'Content-Type: application/json' \
  -d '{\"tags\": [\"deploy\", \"aws\"], \"text\": \"Deployed $IMAGE_TAG (aws)\"}' http://127.0.0.1:3000/api/annotations" >/dev/null ||
  echo "Deploy marker not posted (the stack is up)" >&2
echo "Up in $((SECONDS - start)) s: $url"
echo "Check it: DOMAIN=$DOMAIN make aws-check. Tear it down after the session: DOMAIN=$DOMAIN make aws-down"
