#!/usr/bin/env bash
# Starts a session in AWS (ADR 0003): creates the stack with every service at 0, runs the
# database migrations as one-off tasks, then starts the services and waits until they are stable.
# On a stack that is already running it redeploys instead, one game server at a time.
#   DOMAIN=example.com [ALLOWED_CIDRS=a.b.c.d/32,...] [IMAGE_TAG=<sha>] scripts/aws/up.sh
# It costs money from here until scripts/aws/down.sh (about $0.30-0.80 an hour, measured in
# docs/performance.md).
source "$(dirname "$0")/common.sh"
require_aws
stack_vars
init_stack

start=$SECONDS

# On a stack that is already running (a new IMAGE_TAG mid-session, or a rerun after a failure),
# step 1 must not park the services: that would be an outage. A redeploy instead holds
# game-server-2 at its current image while game-server-1 is replaced, so each game server
# drains into the other one instead of both stopping together (task 5.5's drain).
running_tag=""
if [[ "$(aws ecs describe-services --cluster doodle-arena --services game-server-2 \
    --query 'services[?status==`ACTIVE`].runningCount | [0]' --output text 2>/dev/null)" =~ ^[1-9] ]]; then
  definition="$(aws ecs describe-services --cluster doodle-arena --services game-server-2 \
    --query 'services[0].taskDefinition' --output text)"
  running_tag="$(aws ecs describe-task-definition --task-definition "$definition" \
    --query 'taskDefinition.containerDefinitions[0].image' --output text)"
  running_tag="${running_tag##*:}"
fi

if [[ -n "$running_tag" ]]; then
  echo "==> 1/3 the stack is running (game-server-2 on $running_tag): the migrations' task definitions for $IMAGE_TAG"
  terraform -chdir="$TF/stack" apply "${TF_STACK_VARS[@]}" -var running=true -target=aws_ecs_task_definition.migrate
else
  echo "==> 1/3 the stack, no service running (open to: $ALLOWED_CIDRS, images: $IMAGE_TAG)"
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
echo "==> 2/3 migrations"
migrate auth-migrate
migrate assets-migrate

echo "==> 3/3 the services"
if [[ -n "$running_tag" && "$running_tag" != "$IMAGE_TAG" ]]; then
  echo "    everything but game-server-2 first; game-server-1 drains into game-server-2"
  terraform -chdir="$TF/stack" apply "${TF_STACK_VARS[@]}" -var running=true \
    -var "game_server_2_image_tag=$running_tag" -auto-approve
  aws ecs wait services-stable --cluster "$cluster" --services game-server-1
  echo "    then game-server-2, which drains into the new game-server-1"
fi
terraform -chdir="$TF/stack" apply "${TF_STACK_VARS[@]}" -var running=true -auto-approve
# services-stable takes at most 10 services per call.
services=()
while IFS= read -r s; do services+=("$s"); done < <(terraform -chdir="$TF/stack" output -json services | python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)))')
aws ecs wait services-stable --cluster "$cluster" --services "${services[@]:0:10}"
aws ecs wait services-stable --cluster "$cluster" --services "${services[@]:10}"
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
echo "Up in $((SECONDS - start)) s: $url"
echo "Check it: DOMAIN=$DOMAIN make aws-check. Tear it down after the session: DOMAIN=$DOMAIN make aws-down"
