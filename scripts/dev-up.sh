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
# Data lives in named volumes, which it never touches.
docker compose -f infra/docker-compose.yml --env-file .env up -d --build --wait --renew-anon-volumes "$@"

cat <<'EOF'

Stack is up.
  Gateway: http://localhost:8080   (health: ./scripts/smoke.sh)
  Grafana: http://localhost:3000
EOF
