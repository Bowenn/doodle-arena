#!/usr/bin/env bash
# The k6 load test (task 5.3, loadtest/rooms.js) against the local stack or AWS:
#   scripts/loadtest.sh                 full ramp against http://localhost:8080 (make loadtest)
#   scripts/loadtest.sh smoke           2 rooms for a minute (make loadtest-smoke; CI)
#   GATEWAY=https://icanfly.click scripts/loadtest.sh
# It mints four access tokens per room with auth's own TokenIssuer (mint_tokens.py): the local
# development key, or, for an https GATEWAY, the AWS stack's key read from SSM. The tokens go to
# k6 in the environment, never into a file. k6's metrics go to the collector over OTLP when
# OTLP_ENDPOINT answers (localhost:4318 locally; localhost:14318, which make aws-grafana forwards,
# for AWS).
set -euo pipefail

cd "$(dirname "$0")/.."
profile="${1:-full}"
export GATEWAY="${GATEWAY:-http://localhost:8080}"
# Locally the stack's collector; for AWS, make aws-grafana forwards the data host's to 14318.
if [[ "$GATEWAY" == https://* ]]; then default_otlp=localhost:14318; else default_otlp=localhost:4318; fi
OTLP_ENDPOINT="${OTLP_ENDPOINT:-$default_otlp}"
case "$profile" in
  smoke) rooms=2 ;;
  full) rooms=80 ;;
  *) echo "usage: $0 [full|smoke]" >&2; exit 2 ;;
esac

if [[ "$GATEWAY" == https://* && -z "${DRILL_JWT_PEM:-}" ]]; then
  DRILL_JWT_PEM="$(aws ssm get-parameter --profile "${AWS_PROFILE:-doodle-arena}" --region us-west-2 \
    --with-decryption --name /doodle-arena/stack/jwt-private-key --query Parameter.Value --output text)"
  export DRILL_JWT_PEM
fi
names=()
for i in $(seq 1 $((rooms * 4))); do names+=("$(printf 'Load%04d' "$i")"); done
TOKENS="$(uv run -q --project services/auth python scripts/drills/mint_tokens.py "${names[@]}")"
export TOKENS PROFILE="$profile"

out=()
if curl -s -o /dev/null --max-time 2 "http://$OTLP_ENDPOINT/v1/metrics"; then
  out=(--out opentelemetry)
  export K6_OTEL_SERVICE_NAME=k6 K6_OTEL_METRIC_PREFIX=k6_ K6_OTEL_EXPORTER_PROTOCOL=http/protobuf \
    K6_OTEL_HTTP_EXPORTER_ENDPOINT="$OTLP_ENDPOINT" K6_OTEL_HTTP_EXPORTER_INSECURE=true \
    K6_OTEL_FLUSH_INTERVAL=5s
else
  echo "No collector at $OTLP_ENDPOINT: k6's metrics stay in its summary only." >&2
fi
echo "==> k6 $profile: $rooms rooms at most, against $GATEWAY"
exec k6 run "${out[@]}" loadtest/rooms.js
