#!/usr/bin/env bash
# Publishes a fake RoundFinished and MatchFinished (and uploads the round's stroke log), so
# Track B can build consumers before the game server exists. Needs `make up` first.
#   ./scripts/fake-events.sh                    a new match each run
#   ./scripts/fake-events.sh -match-id <id>     replay a match (idempotency tests)
#   ./scripts/fake-events.sh -dry-run           print the events only
# Read them back with: (cd tools/eventctl && go run . decode -topic game.rounds.v1 -n 1)
set -euo pipefail

cd "$(dirname "$0")/.."
if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi

cd tools/eventctl
exec go run . publish "$@"
