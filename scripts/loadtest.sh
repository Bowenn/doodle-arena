#!/usr/bin/env bash
# The k6 load tests against the local stack or AWS:
#   scripts/loadtest.sh                 rooms (task 5.3, loadtest/rooms.js): the full ramp against
#                                       http://localhost:8080 (make loadtest)
#   scripts/loadtest.sh smoke           2 rooms for a minute (make loadtest-smoke; CI)
#   scripts/loadtest.sh live            one live room and 5,000 viewers (task 6.1c,
#                                       loadtest/live.js; make loadtest-live, about 15 minutes)
#   scripts/loadtest.sh live-smoke      the live room and 50 viewers for half a minute (CI)
#   scripts/loadtest.sh tips            200 seeded users tipping one round, 300 tips a second for
#                                       a minute, then its settlement (task 6.3, loadtest/tips.js;
#                                       make loadtest-tips; needs make seed; local only)
#   GATEWAY=https://icanfly.click scripts/loadtest.sh
# It mints four access tokens per room (and, for live, one per viewer VU) with auth's own
# TokenIssuer (mint_tokens.py): the local
# development key, or, for an https GATEWAY, the AWS stack's key read from SSM. The tokens go to
# k6 in the environment, never into a file. k6's metrics go to the collector over OTLP when
# OTLP_ENDPOINT answers (localhost:4318 locally; localhost:14318, which make aws-grafana forwards,
# for AWS).
set -euo pipefail

cd "$(dirname "$0")/.."
if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi
profile="${1:-full}"
export GATEWAY="${GATEWAY:-http://localhost:8080}"
# Locally the stack's collector; for AWS, make aws-grafana forwards the data host's to 14318.
if [[ "$GATEWAY" == https://* ]]; then default_otlp=localhost:14318; else default_otlp=localhost:4318; fi
OTLP_ENDPOINT="${OTLP_ENDPOINT:-$default_otlp}"
script=loadtest/rooms.js viewer_tokens=0

if [[ "$profile" == tips ]]; then
  if [[ "$GATEWAY" != http://localhost:8080 ]]; then
    echo "The tips load test runs locally: k6 calls the marketplace on the stack's network." >&2
    exit 2
  fi
  tippers="${TIPPERS:-200}"
  # The seeded users with the most coins (make seed), read as the marketplace's own role: the
  # richest is the drawer, the next TIPPERS tip.
  ids="$(docker compose -f infra/docker-compose.yml --env-file "${ENV_FILE:-.env}" exec -T postgres \
    psql -X -U marketplace -d marketplace -tAc "SELECT a.owner_id FROM accounts a JOIN balances b USING (account_id)
      WHERE a.owner_type = 'user' AND a.owner_id LIKE 'seed-u-%' ORDER BY b.balance DESC LIMIT $((tippers + 1))")"
  if [[ -z "$ids" ]]; then
    echo "No seeded users: run make seed first." >&2
    exit 1
  fi
  drawer="$(echo "$ids" | head -1)"
  MATCH_ID="m_tips_load_$(date +%s)"
  export MATCH_ID
  env_args=(-e MATCH_ID)
  # 100 tokens per variable (TOKENS_0, TOKENS_1, ...): Linux refuses one environment string over
  # 128 KB, about 160 tokens. They go to k6 in the environment, never into a file.
  chunk=0
  while read -r batch; do
    export "TOKENS_$chunk=$(uv run -q --project services/auth python scripts/drills/mint_tokens.py --ids $batch)"
    env_args+=(-e "TOKENS_$chunk")
    chunk=$((chunk + 1))
  done < <(echo "$ids" | tail -n +2 | xargs -n 100)
  echo "==> k6 tips: $tippers tippers, ${RATE:-300} tips a second for ${DURATION:-60} s, round 1 of $MATCH_ID"
  for v in RATE DURATION; do
    if [[ -n "${!v:-}" ]]; then env_args+=(-e "$v"); fi
  done
  status=0
  docker run --rm --network doodle-arena_default -v "$PWD/loadtest:/loadtest:ro" "${env_args[@]}" \
    grafana/k6:2.3.0 run /loadtest/tips.js || status=$?
  # 99 is k6's "thresholds crossed": the tips it took still need their settlement.
  if [[ "$status" != 0 && "$status" != 99 ]]; then
    exit "$status"
  fi
  echo "==> the round finishes: its RoundFinished pays $drawer"
  uv run -q --project services/auth --with confluent-kafka --with boto3 --with 'protobuf>=7.36.2,<8' \
    python scripts/drills/tips.py settle "$MATCH_ID" "$drawer"
  exit "$status"
fi
case "$profile" in
  smoke) rooms=2 ;;
  full) rooms=80 ;;
  live | live-smoke)
    rooms=1 script=loadtest/live.js
    viewer_tokens="${VIEWER_VUS:-50}"
    ;;
  *) echo "usage: $0 [full|smoke|live|live-smoke|tips]" >&2; exit 2 ;;
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
if [[ "$script" == loadtest/live.js ]]; then
  # Not for the live test: k6 makes a sample of every message it receives (about 100,000 a
  # second at 2,500 viewers), and exporting them took k6 from 2.5 to 5 cores. A slower k6 reads
  # its sockets late, the relays' writes back up behind it, and the relays' p99 went from 49 ms
  # to 630 ms (task 6.1c). The relays' own metrics are the measurement; k6 keeps its summary.
  echo "k6's metrics stay in its summary for the live test (the relays' are in Grafana)." >&2
elif curl -s -o /dev/null --max-time 2 "http://$OTLP_ENDPOINT/v1/metrics"; then
  out=(--out opentelemetry)
  export K6_OTEL_SERVICE_NAME=k6 K6_OTEL_METRIC_PREFIX=k6_ K6_OTEL_EXPORTER_PROTOCOL=http/protobuf \
    K6_OTEL_HTTP_EXPORTER_ENDPOINT="$OTLP_ENDPOINT" K6_OTEL_HTTP_EXPORTER_INSECURE=true \
    K6_OTEL_FLUSH_INTERVAL=5s
else
  echo "No collector at $OTLP_ENDPOINT: k6's metrics stay in its summary only." >&2
fi
if [[ "$script" == loadtest/live.js && "$GATEWAY" == http://localhost:8080 ]] &&
  ! docker ps --format '{{.Names}}' 2>/dev/null | grep -qx doodle-arena-relay-1-1; then
  echo "No relays are running: start them with PROFILES=live make up" >&2
  exit 1
fi
if [[ "$script" == loadtest/live.js ]]; then
  echo "==> k6 $profile: one live room, ${VIEWERS:-its default number of} viewers, against $GATEWAY"
  # Thousands of sockets: k6's own file limit, as high as this shell allows.
  ulimit -n "$(ulimit -Hn)" 2>/dev/null || ulimit -n 65536 2>/dev/null || true
else
  echo "==> k6 $profile: $rooms rooms at most, against $GATEWAY"
fi
# Locally, the live test's k6 runs inside the stack's network (task 6.1c). From the host, every
# viewer's messages also cross Docker Desktop's proxy between macOS and its VM, which saturated
# at about 2,000 viewers (80,000 messages a second): the players' own stroke fan-out, which
# never touches a relay, went from 4 ms to seconds. Inside the network k6 talks to the gateway
# directly, as browsers talk to the load balancer in AWS. K6_IN_DOCKER=0 runs it on the host.
if [[ "$script" == loadtest/live.js && "$GATEWAY" == http://localhost:8080 && "${K6_IN_DOCKER:-1}" == 1 ]]; then
  env_args=(-e TOKENS -e PROFILE -e GATEWAY=http://gateway)
  for v in VIEWERS VIEWER_VUS RATE HOLD; do
    if [[ -n "${!v:-}" ]]; then env_args+=(-e "$v"); fi
  done
  if ((${#out[@]})); then
    env_args+=(-e K6_OTEL_SERVICE_NAME -e K6_OTEL_METRIC_PREFIX -e K6_OTEL_EXPORTER_PROTOCOL
      -e K6_OTEL_HTTP_EXPORTER_ENDPOINT=otel-lgtm:4318 -e K6_OTEL_HTTP_EXPORTER_INSECURE -e K6_OTEL_FLUSH_INTERVAL)
  fi
  echo "    k6 runs in a container on the stack's network (K6_IN_DOCKER=0: on the host)"
  exec docker run --rm --network doodle-arena_default -v "$PWD/loadtest:/loadtest:ro" \
    --ulimit nofile=65535:65535 "${env_args[@]}" grafana/k6:2.3.0 run ${out[@]+"${out[@]}"} /loadtest/live.js
fi
# PIN_HOST and PIN_IP, when set, reach k6 too (loadtest/rooms.js). An empty array is
# "unbound" under set -u in macOS's bash 3.2, hence the ${out[@]+...} form.
exec k6 run ${out[@]+"${out[@]}"} "$script"
