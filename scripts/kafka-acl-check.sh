#!/usr/bin/env bash
# make kafka-acl-check (ADR 0013): checks on `make up` that the broker refuses what each service
# must not do: no credential, a wrong password, writing another service's topic, joining another
# service's consumer group, creating a topic, changing a topic's config. The cases are in
# scripts/drills/kafka_acl_check.sh, run in a one-off kafka-init container, which is on the Kafka
# network and holds every user's password. CI's stack job runs it.
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
docker compose -f infra/docker-compose.yml --env-file "$env_file" run --rm --no-deps -T \
  --entrypoint /bin/bash kafka-init -s < scripts/drills/kafka_acl_check.sh
