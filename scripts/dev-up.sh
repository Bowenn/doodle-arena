#!/usr/bin/env bash
# Start the local stack. Creates .env from .env.example on the first run, adds variables that
# were added to .env.example since, and creates the local JWT and join-ticket signing keys.
set -euo pipefail

cd "$(dirname "$0")/.."

# Prints an .env.example line, with "change-me" replaced by a random value.
fill() {
  if [[ "$1" == *=change-me ]]; then
    echo "${1%change-me}$(openssl rand -hex 16)"
  else
    echo "$1"
  fi
}

if [[ ! -f .env ]]; then
  echo "Creating .env with random local passwords"
  while IFS= read -r line; do fill "$line"; done < .env.example > .env
  chmod 600 .env
else
  # Never overwrite existing values; only append variables .env doesn't have yet.
  while IFS= read -r line; do
    [[ "$line" =~ ^([A-Z0-9_]+)= ]] || continue
    if ! grep -q "^${BASH_REMATCH[1]}=" .env; then
      echo "Adding ${BASH_REMATCH[1]} to .env"
      fill "$line" >> .env
    fi
  done < .env.example
fi

# A .env copied by hand from .env.example still holds placeholders; give them random values.
# Real values (anything other than "change-me") are left alone.
if grep -q '=change-me$' .env; then
  echo "Replacing change-me placeholders in .env with random values"
  tmp="$(mktemp)"
  while IFS= read -r line; do fill "$line"; done < .env > "$tmp"
  cat "$tmp" > .env
  rm -f "$tmp"
  chmod 600 .env
fi

./scripts/gen-jwt-key.sh
./scripts/gen-ticket-key.sh

# --renew-anon-volumes refreshes web's node_modules volume after a dependency change.
# Data lives in named volumes, which it never touches. --remove-orphans removes the containers
# of services that were renamed or removed (kafka-topics became kafka-init).
# Opt-in parts of the stack (compose profiles), to keep the Docker VM's memory for the rest:
#   PROFILES=live make up             also the relays (live rooms, ADR 0017)
#   PROFILES=live,analytics make up   and Flink (ADR 0018)
# COMPOSE_PROFILES in .env does the same for every make up.
if [[ -n "${PROFILES:-}" ]]; then
  export COMPOSE_PROFILES="$PROFILES"
fi
docker compose -f infra/docker-compose.yml --env-file .env up -d --build --wait --renew-anon-volumes --remove-orphans "$@"

# A deploy marker on every dashboard (task 6.9): the commit, with -dirty for local changes.
version="$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
git diff --quiet HEAD 2>/dev/null || version="$version-dirty"
for _ in $(seq 10); do
  curl -s -o /dev/null --max-time 2 http://localhost:3000/api/health && break
  sleep 3
done
./scripts/deploy-marker.sh "$version" local || true

cat <<'EOF'

Stack is up.
  Gateway: http://localhost:8080   (health: ./scripts/smoke.sh)
  Grafana: http://localhost:3000
EOF
