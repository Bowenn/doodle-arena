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
  S3_GAME_SERVER_SECRET_KEY S3_ASSETS_SECRET_KEY)
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
docker compose -f infra/docker-compose.yml --env-file "$env_file" run --rm --no-deps -T \
  "${flags[@]}" assets-consumer python - < scripts/drills/data_acl_check.py
