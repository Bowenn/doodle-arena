#!/usr/bin/env bash
# Publishes a fake RoundFinished and MatchFinished (and uploads the round's stroke log), so
# Track B can build consumers before the game server exists. Needs `make up` first.
#   ./scripts/fake-events.sh                    a new match each run
#   ./scripts/fake-events.sh -match-id <id>     replay a match (idempotency tests)
#   ./scripts/fake-events.sh -dry-run           print the events only
# Read them back with: (cd tools/eventctl && go run . decode -topic game.rounds.v1 -n 1)
set -euo pipefail

cd "$(dirname "$0")/.."
# The dev-tools Kafka user's password (ADR 0013) and the S3 keys come from .env; a worktree has
# none of its own, so set ENV_FILE to the main checkout's.
env_file="${ENV_FILE:-.env}"
if [[ -f "$env_file" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$env_file"
  set +a
fi

cd tools/eventctl
exec go run . publish "$@"
