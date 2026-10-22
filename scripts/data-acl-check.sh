#!/usr/bin/env bash
# make data-acl-check (task 5.5): checks on `make up` that Redis and SeaweedFS refuse each
# service another service's keys and buckets (scripts/drills/data_acl_check.py, in a one-off
# assets container, which has redis and boto3 and is on the stores' network). CI's stack job
# runs it, next to make kafka-acl-check.
set -euo pipefail

cd "$(dirname "$0")/.."
if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi
# A worktree has no .env of its own: point ENV_FILE at the main checkout's.
env_file="${ENV_FILE:-.env}"
if [[ ! -f "$env_file" ]]; then
  echo "no $env_file: run make up first, or set ENV_FILE" >&2
  exit 2
fi
vars=(REDIS_LOBBY_PASSWORD REDIS_GAME_SERVER_PASSWORD REDIS_ASSETS_PASSWORD
  S3_GAME_SERVER_SECRET_KEY S3_ASSETS_SECRET_KEY S3_ANALYTICS_SECRET_KEY)
flags=()
for var in "${vars[@]}"; do
  value="$(sed -n "s/^${var}=//p" "$env_file")"
  if [[ -z "$value" ]]; then
    echo "no $var in $env_file: run make up first" >&2
    exit 2
  fi
  export "${var}=${value}"
  flags+=(-e "$var")
done
status=0
docker compose -f infra/docker-compose.yml --env-file "$env_file" run --rm --no-deps -T \
  "${flags[@]}" assets-consumer python - < scripts/drills/data_acl_check.py || status=$?

# SeaweedFS's filer, master and volume ports take no credentials, so the S3 identities above
# hold only if nothing else can reach them: a service off the seaweedfs network (here the AI
# service) must not even resolve the host. The control, from assets, must reach the filer.
probe='import socket, sys
try:
    socket.create_connection(("seaweedfs", 8888), timeout=3).close()
except OSError as e:
    print(type(e).__name__); sys.exit(1)
print("reached")'
echo "SeaweedFS network (its own ports take no credentials):"
if docker compose -f infra/docker-compose.yml --env-file "$env_file" run --rm --no-deps -T \
    assets-consumer python -c "$probe" >/dev/null 2>&1; then
  echo "  ok    assets reaches the filer (control)"
else
  echo "  FAIL  assets can't reach the filer: the probe is broken"; status=1
fi
if out="$(docker compose -f infra/docker-compose.yml --env-file "$env_file" run --rm --no-deps -T \
    ai python -c "$probe" 2>&1)"; then
  echo "  FAIL  the AI service reaches SeaweedFS's filer"; status=1
elif grep -q gaierror <<<"$out"; then
  echo "  ok    the AI service can't resolve seaweedfs"
else
  echo "  FAIL  the AI service's probe failed for another reason: ${out##*$'\n'}"; status=1
fi
exit "$status"
