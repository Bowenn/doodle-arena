#!/usr/bin/env bash
# Start the local stack. Creates .env from .env.example on the first run.
set -euo pipefail

cd "$(dirname "$0")/.."

if [[ ! -f .env ]]; then
  echo "Creating .env with random local passwords"
  while IFS= read -r line; do
    if [[ "$line" == *=change-me ]]; then
      echo "${line%change-me}$(openssl rand -hex 16)"
    else
      echo "$line"
    fi
  done < .env.example > .env
  chmod 600 .env
fi

# --renew-anon-volumes refreshes web's node_modules volume after a dependency change.
# Data lives in named volumes, which it never touches.
docker compose -f infra/docker-compose.yml --env-file .env up -d --build --wait --renew-anon-volumes "$@"

cat <<'EOF'

Stack is up.
  Gateway: http://localhost:8080   (health: ./scripts/smoke.sh)
  Grafana: http://localhost:3000
EOF
