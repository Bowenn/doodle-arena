#!/usr/bin/env bash
# Checks the drawing pipeline end to end on `make up`: one RoundFinished in, an approved drawing,
# its image and its DrawingApproved out (scripts/drills/pipeline_check.py, in a one-off container
# of assets-consumer's: the assets image, on the Kafka network, with assets' database and S3).
# It publishes the RoundFinished as dev-tools, the one user besides the game server allowed to
# write game.rounds.v1 (ADR 0013). Needs the AI stub to approve (its default; scripts/ai-mode.sh).
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
KAFKA_PASSWORD="$(sed -n 's/^KAFKA_DEV_TOOLS_PASSWORD=//p' "$env_file")"
if [[ -z "$KAFKA_PASSWORD" ]]; then
  echo "no KAFKA_DEV_TOOLS_PASSWORD in $env_file: run make up first" >&2
  exit 2
fi
export KAFKA_PASSWORD
docker compose -f infra/docker-compose.yml --env-file "$env_file" run --rm --no-deps -T \
  -e KAFKA_USERNAME=dev-tools -e KAFKA_PASSWORD assets-consumer python - < scripts/drills/pipeline_check.py
