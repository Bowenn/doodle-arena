#!/usr/bin/env bash
# The k6 load tests against the local stack or AWS:
#   scripts/loadtest.sh                 rooms (task 5.3, loadtest/rooms.js): the full ramp against
#                                       http://localhost:8080 (make loadtest)
#   scripts/loadtest.sh smoke           2 rooms for a minute (make loadtest-smoke; CI)
#   scripts/loadtest.sh live            one live room and 5,000 viewers (task 6.1c,
#                                       loadtest/live.js; make loadtest-live, about 15 minutes)
#   scripts/loadtest.sh live-smoke      the live room and 50 viewers for half a minute (CI)
#   GATEWAY=https://icanfly.click scripts/loadtest.sh
# It mints four access tokens per room (and, for live, one per viewer VU) with auth's own
# TokenIssuer (mint_tokens.py): the local
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
script=loadtest/rooms.js viewer_tokens=0
case "$profile" in
  smoke) rooms=2 ;;
  full) rooms=80 ;;
  live | live-smoke)
    rooms=1 script=loadtest/live.js
    viewer_tokens="${VIEWER_VUS:-50}"
    ;;
  *) echo "usage: $0 [full|smoke|live|live-smoke]" >&2; exit 2 ;;
esac

if [[ "$GATEWAY" == https://* && -z "${DRILL_JWT_PEM:-}" ]]; then
  DRILL_JWT_PEM="$(aws ssm get-parameter --profile "${AWS_PROFILE:-doodle-arena}" --region us-west-2 \
    --with-decryption --name /doodle-arena/stack/jwt-private-key --query Parameter.Value --output text)"
  export DRILL_JWT_PEM
fi
names=()
for i in $(seq 1 $((rooms * 4))); do names+=("$(printf 'Load%04d' "$i")"); done
for i in $(seq 1 "$viewer_tokens"); do names+=("$(printf 'Viewer%03d' "$i")"); done
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
if [[ "$script" == loadtest/live.js ]]; then
  echo "==> k6 $profile: one live room, ${VIEWERS:-its default number of} viewers, against $GATEWAY"
  # Thousands of sockets: k6's own file limit, as high as this shell allows.
  ulimit -n "$(ulimit -Hn)" 2>/dev/null || ulimit -n 65536 2>/dev/null || true
else
  echo "==> k6 $profile: $rooms rooms at most, against $GATEWAY"
fi
# PIN_HOST and PIN_IP, when set, reach k6 too (loadtest/rooms.js). An empty array is
# "unbound" under set -u in macOS's bash 3.2, hence the ${out[@]+...} form.
exec k6 run ${out[@]+"${out[@]}"} "$script"
