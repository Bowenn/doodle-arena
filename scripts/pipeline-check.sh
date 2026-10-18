#!/usr/bin/env bash
# Checks the drawing pipeline end to end on `make up`: one RoundFinished in, an approved drawing,
# its image and its DrawingApproved out (scripts/drills/pipeline_check.py, run inside the assets
# container). Needs the AI stub to approve (its default; see scripts/ai-mode.sh).
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
docker compose -f infra/docker-compose.yml --env-file "$env_file" exec -T assets python - < scripts/drills/pipeline_check.py
