#!/usr/bin/env bash
# The local checks against AWS: make smoke through the load balancer, then, through the Grafana
# tunnel (scripts/aws/grafana.sh, in another terminal), make telemetry-check and make
# alerts-check (with Grafana's admin password from SSM, never printed).
#   DOMAIN=example.com scripts/aws/check.sh
source "$(dirname "$0")/common.sh"
require_aws
: "${DOMAIN:?set DOMAIN to the Route 53 domain}"
export GATEWAY="https://$DOMAIN" GRAFANA="${GRAFANA:-http://localhost:3300}"
if ! curl -fsS -o /dev/null "$GRAFANA/api/health"; then
  echo "Grafana isn't reachable at $GRAFANA: start the tunnel first (make aws-grafana)." >&2
  exit 1
fi
# Both relays run in AWS since task 6.9 (ADR 0017).
SMOKE_RELAYS="relay-1 relay-2" "$ROOT/scripts/smoke.sh"
"$ROOT/scripts/telemetry-check.sh"
GRAFANA_USER=admin GRAFANA_PASSWORD="$(aws ssm get-parameter --with-decryption \
  --name /doodle-arena/stack/grafana-admin-password --query Parameter.Value --output text)" \
  python3 "$ROOT/scripts/alerts-check.py"
