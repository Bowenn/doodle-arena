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
#   scripts/loadtest.sh drops           a flash sale: 1,000 seeded buyers for 100 editions of one
#                                       drop, then Postgres checked for overselling (task 6.8b,
#                                       loadtest/drops.js; make loadtest-drops; needs make seed)
#   scripts/loadtest.sh gallery         the gallery's pages, Market lookups and drawing pages at
#                                       RATE a second while a drawing is approved every 2 s
#                                       (task 6.7b, loadtest/gallery.js; make loadtest-gallery;
#                                       CACHE=off recreates assets without its cache, ADR 0024;
#                                       needs make seed; local only)
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
if [[ "$profile" == drops ]]; then
  if [[ "$GATEWAY" != http://localhost:8080 ]]; then
    echo "The drops load test runs locally: k6 calls the marketplace on the stack's network." >&2
    exit 2
  fi
  buyers="${BUYERS:-1000}" editions="${EDITIONS:-100}" price="${PRICE:-10}"
  psql_market() {
    docker compose -f infra/docker-compose.yml --env-file "${ENV_FILE:-.env}" exec -T postgres \
      psql -X -U marketplace -d marketplace -v ON_ERROR_STOP=1 -tAc "$1"
  }
  # A minted drawing with authors, and seeded buyers who can pay and didn't draw it (make seed).
  drawing="$(psql_market "SELECT o.drawing_id FROM ownership o WHERE EXISTS (SELECT 1 FROM drawing_authors a
    WHERE a.drawing_id = o.drawing_id) ORDER BY o.minted_at DESC LIMIT 1")"
  ids="$(psql_market "SELECT a.owner_id FROM accounts a JOIN balances b USING (account_id)
    WHERE a.owner_type = 'user' AND a.owner_id LIKE 'seed-u-%' AND b.balance >= $price
      AND a.owner_id NOT IN (SELECT user_id FROM drawing_authors WHERE drawing_id = '$drawing')
    ORDER BY b.balance DESC LIMIT $buyers")"
  if [[ -z "$drawing" || -z "$ids" ]]; then
    echo "No seeded data: run make seed first." >&2
    exit 1
  fi
  # An admin creates the drop through the gateway, opening 35 s from now (30 s is the minimum).
  read -r DROP_ID OPENS_AT < <(uv run -q --project services/auth python - "$drawing" "$editions" "$price" <<'PY'
import json, os, sys, urllib.request, uuid
from datetime import UTC, datetime, timedelta
sys.path.insert(0, "services/auth")
from app.tokens import SigningKey, TokenIssuer
gw = os.environ["GATEWAY"]
key = SigningKey.load(os.environ.get("DRILL_JWT_PEM", ""), os.environ.get("DRILL_JWT_KEY") or "secrets/jwt-private.pem")
admin = TokenIssuer(key, gw + "/api/auth", timedelta(minutes=10)).issue("u_load_admin", ["player", "admin"], datetime.now(UTC), name="load")[0]
opens = datetime.now(UTC) + timedelta(seconds=35)
body = {"drawing_id": sys.argv[1], "editions": int(sys.argv[2]), "price": int(sys.argv[3]),
        "opens_at": opens.isoformat().replace("+00:00", "Z"), "duration_minutes": 10}
req = urllib.request.Request(gw + "/api/market/drops", method="POST", data=json.dumps(body).encode())
for k, v in {"Authorization": "Bearer " + admin, "Content-Type": "application/json", "Idempotency-Key": str(uuid.uuid4())}.items():
    req.add_header(k, v)
with urllib.request.urlopen(req, timeout=10) as resp:
    print(json.load(resp)["drop_id"], int(opens.timestamp() * 1000))
PY
)
  export DROP_ID OPENS_AT EDITIONS="$editions"
  env_args=(-e DROP_ID -e OPENS_AT -e EDITIONS)
  # 100 tokens per variable, as for tips (one environment string may not exceed 128 KB).
  chunk=0
  while read -r batch; do
    # shellcheck disable=SC2086 # one id per word
    export "TOKENS_$chunk=$(uv run -q --project services/auth python scripts/drills/mint_tokens.py --ids $batch)"
    env_args+=(-e "TOKENS_$chunk")
    chunk=$((chunk + 1))
  done < <(echo "$ids" | xargs -n 100)
  echo "==> k6 drops: $(echo "$ids" | wc -l | tr -d ' ') buyers for $editions editions at $price coins, drop $DROP_ID"
  status=0
  docker run --rm --network doodle-arena_default -v "$PWD/loadtest:/loadtest:ro" --ulimit nofile=65535:65535 \
    "${env_args[@]}" grafana/k6:2.3.0 run /loadtest/drops.js || status=$?
  echo "==> Postgres, the truth: editions, buyers charged, the authors paid, the ledger"
  for _ in $(seq 1 30); do
    [[ "$(psql_market "SELECT settled_at IS NOT NULL FROM drops WHERE drop_id = '$DROP_ID'")" == t ]] && break
    sleep 2
  done
  sold="$(psql_market "SELECT COUNT(*) || ' ' || COUNT(DISTINCT buyer_id) FROM drop_editions WHERE drop_id = '$DROP_ID'")"
  charged="$(psql_market "SELECT COUNT(*) FROM ledger_transactions WHERE type = 'drop_purchase'
    AND metadata->>'drop_id' = '$DROP_ID'")"
  authors="$(psql_market "SELECT COALESCE(SUM(e.amount), 0) FROM ledger_entries e JOIN ledger_transactions t USING (tx_id)
    WHERE t.type = 'drop_settlement' AND t.metadata->>'drop_id' = '$DROP_ID' AND e.amount > 0")"
  imbalance="$(psql_market "SELECT COALESCE(SUM(amount), 0) FROM ledger_entries")"
  echo "    editions sold and buyers: $sold (of $editions); purchases charged: $charged; paid to the authors: $authors coins; ledger sum: $imbalance"
  if [[ "$sold" != "$editions $editions" || "$charged" != "$editions" || "$authors" != "$((editions * price))" || "$imbalance" != 0 ]]; then
    echo "FAIL: oversold, undersold or unbalanced" >&2
    exit 1
  fi
  echo "no overselling: exactly $editions editions to $editions buyers, $((editions * price)) coins to the authors, the ledger at 0"
  exit "$status"
fi
if [[ "$profile" == gallery ]]; then
  if [[ "$GATEWAY" != http://localhost:8080 ]]; then
    echo "The gallery load test runs locally: k6 calls assets on the stack's network." >&2
    exit 2
  fi
  compose=(docker compose -f infra/docker-compose.yml --env-file "${ENV_FILE:-.env}")
  psql_assets() {
    "${compose[@]}" exec -T postgres psql -X -U assets -d assets -v ON_ERROR_STOP=1 -tAc "$1"
  }
  # Postgres's own count of rows read for assets, primary and replica (pg_stat_database).
  rows_read() {
    local sql="SELECT tup_returned + tup_fetched FROM pg_stat_database WHERE datname = 'assets'"
    echo $(( $("${compose[@]}" exec -T postgres psql -X -U assets -d assets -tAc "$sql") \
      + $("${compose[@]}" exec -T postgres-replica psql -X -U assets -d assets -tAc "$sql") ))
  }
  cache="${CACHE:-on}"
  if [[ "$cache" != on && "$cache" != off ]]; then
    echo "CACHE is on or off" >&2
    exit 2
  fi
  # A random sample of the seeded drawings (make seed): the Market's and the drawing pages' ids.
  ids="$(psql_assets "SELECT string_agg(id::text, ',') FROM (SELECT id FROM drawings_drawing
    WHERE status = 'approved' ORDER BY random() LIMIT 2000) s")"
  if [[ -z "$ids" ]]; then
    echo "No approved drawings: run make seed first." >&2
    exit 1
  fi
  echo "==> assets with ASSETS_CACHE=$cache"
  ASSETS_CACHE="$cache" "${compose[@]}" up -d --wait assets >/dev/null
  TOKENS="$(uv run -q --project services/auth python scripts/drills/mint_tokens.py $(seq -f 'gallery%g' 1 20))"
  IDS="$ids" CACHE="$cache"
  export TOKENS IDS CACHE
  env_args=(-e TOKENS -e IDS -e CACHE)
  for v in RATE DURATION; do
    if [[ -n "${!v:-}" ]]; then env_args+=(-e "$v"); fi
  done
  duration="${DURATION:-120}"
  echo "==> k6 gallery: ${RATE:-200} requests a second for $duration s, cache $cache, an approval every 2 s"
  uv run -q --project services/auth --with confluent-kafka --with boto3 --with 'protobuf>=7.36.2,<8' \
    python scripts/drills/approvals.py "$duration" 2 &
  approvals=$!
  before="$(rows_read)"
  status=0
  docker run --rm --network doodle-arena_default -v "$PWD/loadtest:/loadtest:ro" "${env_args[@]}" \
    grafana/k6:2.3.0 run /loadtest/gallery.js || status=$?
  after="$(rows_read)"
  wait "$approvals" || true
  echo "==> Postgres rows read for assets during the run (primary and replica): $((after - before))"
  if [[ "$cache" == off ]]; then
    echo "==> assets back with its cache"
    ASSETS_CACHE=on "${compose[@]}" up -d --wait assets >/dev/null
  fi
  exit "$status"
fi
case "$profile" in
  smoke) rooms=2 ;;
  full) rooms=80 ;;
  live | live-smoke)
    rooms=1 script=loadtest/live.js
    viewer_tokens="${VIEWER_VUS:-50}"
    ;;
  *) echo "usage: $0 [full|smoke|live|live-smoke|tips|drops]" >&2; exit 2 ;;
esac

# The players' tokens, 100 per variable (TOKENS_0, TOKENS_1, ...): Linux refuses one environment
# string over 128 KB, about 160 tokens. Tokens already given in TOKENS_0... are used as they are:
# from the dev box (task 6.9), the Mac mints them with the AWS stack's key and pipes them over, so
# the signing key itself never leaves the Mac.
token_vars=()
if [[ -z "${TOKENS_0:-}" ]]; then
  if [[ "$GATEWAY" == https://* && -z "${DRILL_JWT_PEM:-}" ]]; then
    DRILL_JWT_PEM="$(aws ssm get-parameter --profile "${AWS_PROFILE:-doodle-arena}" --region us-west-2 \
      --with-decryption --name /doodle-arena/stack/jwt-private-key --query Parameter.Value --output text)"
    export DRILL_JWT_PEM
  fi
  names=()
  for i in $(seq 1 $((rooms * 4))); do names+=("$(printf 'Load%04d' "$i")"); done
  for i in $(seq 1 "$viewer_tokens"); do names+=("$(printf 'Viewer%03d' "$i")"); done
  chunk=0
  for ((i = 0; i < ${#names[@]}; i += 100)); do
    export "TOKENS_$chunk=$(uv run -q --project services/auth python scripts/drills/mint_tokens.py "${names[@]:i:100}")"
    chunk=$((chunk + 1))
  done
fi
for ((chunk = 0; ; chunk++)); do
  v="TOKENS_$chunk"
  [[ -n "${!v:-}" ]] || break
  token_vars+=(-e "$v")
done
export PROFILE="$profile"

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
  env_args=("${token_vars[@]}" -e PROFILE -e GATEWAY=http://gateway)
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
