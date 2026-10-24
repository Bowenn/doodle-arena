#!/usr/bin/env bash
# Task 6.4 (ADR 0025): stroke batch intervals and JSON against binary (protobuf) stroke frames,
# on the local stack (make up; run it on the dev box). make experiment-strokes runs it.
#
# 1. The matrix: BATCHES (16 33 50 100 ms) x WIRES (json pb), each a fixed load of ROOMS rooms
#    (40: 160 players, 80 drawers at POINTS_PER_SECOND each) held for HOLD seconds (240),
#    REPEAT times (2). Every cell records k6's stroke fan-out and ink delay, bytes per second
#    each way, the game servers' CPU, Redis commands per second (each stroke batch is one
#    commit) and the servers' p95 input and send lag.
# 2. The ceiling, for CEILING_BATCH (50) and each wire: rooms from 20 up by 20, HOLD_CEILING
#    seconds each (120), until p95 input or send lag passes 250 ms (SlowBroadcasts' line).
#
# Each game server runs limited to one CPU (infra/experiments/cpu-limit.yml) for the runs and is
# put back afterwards. k6 runs in a container on the stack's network. Results go to OUT
# (a Markdown file, printed at the end). SKIP_MATRIX=1 or SKIP_CEILING=1 skip a part.
set -euo pipefail

cd "$(dirname "$0")/../.."
if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi
ENV_FILE="${ENV_FILE:-.env}"
BATCHES="${BATCHES:-16 33 50 100}"
WIRES="${WIRES:-json pb}"
ROOMS="${ROOMS:-40}"
HOLD="${HOLD:-240}"
REPEAT="${REPEAT:-2}"
POINTS_PER_SECOND="${POINTS_PER_SECOND:-160}"
CEILING_BATCH="${CEILING_BATCH:-50}"
HOLD_CEILING="${HOLD_CEILING:-120}"
OUT="${OUT:-/tmp/strokes-$(date +%Y%m%d-%H%M).md}"
RESULTS="$(mktemp -d)"
# k6's container runs as its own user and writes its summary here.
chmod 0777 "$RESULTS"

compose=(docker compose -f infra/docker-compose.yml --env-file "$ENV_FILE")
limited=(docker compose -f infra/docker-compose.yml -f infra/experiments/cpu-limit.yml --env-file "$ENV_FILE")

env_value() { grep "^$1=" "$ENV_FILE" | cut -d= -f2-; }

# Prometheus inside otel-lgtm: one instant query at time $2 (epoch seconds), the first value.
prom() {
  docker exec doodle-arena-otel-lgtm-1 curl -s "localhost:9090/api/v1/query" \
    --data-urlencode "query=$1" --data-urlencode "time=$2" |
    python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; print(round(float(r[0]["value"][1]),1) if r and r[0]["value"][1] not in ("NaN","+Inf") else "")'
}

redis_commands() {
  REDISCLI_AUTH="$(env_value REDIS_ADMIN_PASSWORD)" "${compose[@]}" exec -T -e REDISCLI_AUTH redis \
    redis-cli --user admin --no-auth-warning info stats | tr -d '\r' | awk -F: '$1=="total_commands_processed"{print $2}'
}

# CPU in cores, sampled every 5 s until the file $1 appears: the game servers', then k6's (a
# saturated k6 reads its sockets late and would make the servers look slow).
sample_cpu() {
  local stop="$1" out="$2"
  local servers k6
  while [[ ! -e "$stop" ]]; do
    # Separate calls: docker stats fails as a whole if one container isn't there (yet).
    servers="$(docker stats --no-stream --format '{{.CPUPerc}}' doodle-arena-game-server-1-1 doodle-arena-game-server-2-1 |
      tr -d % | awk '{s += $1} END {print s / 100}')"
    k6="$(docker stats --no-stream --format '{{.CPUPerc}}' strokes-k6 2>/dev/null | tr -d % | awk '{print $1 / 100}')"
    echo "$servers ${k6:-0}" >>"$out"
    sleep 5
  done
}

# Recreated game servers register with the lobby by heartbeat: until both have, creating a room
# answers 503 NO_GAME_SERVER. Waits until the lobby places a room (an empty private one).
wait_for_lobby() {
  local token
  token="$(uv run -q --project services/auth python scripts/drills/mint_tokens.py ExpProbe |
    python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["token"])')"
  for _ in $(seq 1 60); do
    if [[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $token" \
      -H 'Content-Type: application/json' -d '{"public": false}' http://localhost:8080/api/lobby/rooms)" == 201 ]]; then
      sleep 5 # both servers, not just the first to report
      return 0
    fi
    sleep 2
  done
  echo "The lobby can't place a room after 2 minutes." >&2
  exit 1
}

echo "==> game servers limited to one CPU each"
"${limited[@]}" up -d --wait game-server-1 game-server-2 >/dev/null
wait_for_lobby
restore() {
  echo "==> game servers back without the CPU limit"
  "${compose[@]}" up -d --wait game-server-1 game-server-2 >/dev/null || true
  rm -rf "$RESULTS"
}
trap restore EXIT

# Fresh tokens for every run (they live 90 minutes; the experiment takes longer), 100 per
# variable: one environment string may not exceed 128 KB.
mint() {
  local rooms="$1" names=() i chunk=0
  token_args=()
  for i in $(seq 1 $((rooms * 4))); do names+=("$(printf 'Exp%04d' "$i")"); done
  for ((i = 0; i < ${#names[@]}; i += 100)); do
    export "TOKENS_$chunk=$(uv run -q --project services/auth python scripts/drills/mint_tokens.py "${names[@]:i:100}")"
    token_args+=(-e "TOKENS_$chunk")
    chunk=$((chunk + 1))
  done
}

# run <label> <batch_ms> <wire> <rooms> <hold>: one fixed load; appends a JSON line to cells.
run() {
  local label="$1" batch="$2" wire="$3" rooms="$4" hold="$5"
  local summary="$RESULTS/$label.json" stop="$RESULTS/$label.stop" cpu="$RESULTS/$label.cpu"
  echo "==> $label: $rooms rooms, batches every $batch ms, $wire, $hold s"
  mint "$rooms"
  local before_cmds start end status=0
  before_cmds="$(redis_commands)"
  start="$(date +%s)"
  sample_cpu "$stop" "$cpu" &
  local sampler=$!
  docker run --rm --name strokes-k6 --network doodle-arena_default -v "$PWD/loadtest:/loadtest:ro" -v "$RESULTS:/results" \
    "${token_args[@]}" -e PROFILE=fixed -e GATEWAY=http://gateway -e ROOMS="$rooms" -e HOLD="$hold" \
    -e BATCH_MS="$batch" -e WIRE="$wire" -e POINTS_PER_SECOND="$POINTS_PER_SECOND" \
    grafana/k6:2.3.0 run --quiet --summary-export "/results/$label.json" /loadtest/rooms.js >"$RESULTS/$label.log" 2>&1 || status=$?
  end="$(date +%s)"
  touch "$stop"
  wait "$sampler" || true
  if [[ ! -s "$summary" || ( "$status" != 0 && "$status" != 99 ) ]]; then
    echo "k6 failed ($status):" >&2
    tail -20 "$RESULTS/$label.log" >&2
    exit "$status"
  fi
  local after_cmds window
  after_cmds="$(redis_commands)"
  # The hold, after every room started: the servers' lags over it.
  window=$((hold > 30 ? hold - 10 : hold))
  local input send batches
  # The stroke batches the servers took in, by their own count: the work actually done (a
  # client that falls behind sends fewer).
  batches="$(prom "sum(rate(game_ws_frames_total{direction=\"in\", type=\"stroke\"}[${window}s]))" "$((end - 5))")"
  input="$(prom "histogram_quantile(0.95, sum by (le) (rate(game_room_input_lag_milliseconds_bucket[${window}s])))" "$((end - 5))")"
  send="$(prom "histogram_quantile(0.95, sum by (le) (rate(game_ws_send_lag_milliseconds_bucket[${window}s])))" "$((end - 5))")"
  python3 - "$summary" "$cpu" "$label" "$batch" "$wire" "$rooms" "$((end - start))" "$((after_cmds - before_cmds))" "$input" "$send" "$batches" >>"$RESULTS/cells.jsonl" <<'PY'
import json, statistics, sys
summary, cpu, label, batch, wire, rooms, seconds, cmds, input_lag, send_lag, batches = sys.argv[1:]
m = json.load(open(summary))["metrics"]
def trend(name, stat):
    return round(m.get(name, {}).get(stat, 0), 1)
def count(name):
    return m.get(name, {}).get("count", 0)
samples = [line.split() for line in open(cpu) if line.strip()]
cores = [float(s[0]) for s in samples]
k6 = [float(s[1]) for s in samples if len(s) > 1]
seconds = int(seconds)
print(json.dumps({
    "label": label, "batch_ms": int(batch), "wire": wire, "rooms": int(rooms),
    "fanout_p50": trend("stroke_fanout", "p(50)"), "fanout_p95": trend("stroke_fanout", "p(95)"), "fanout_p99": trend("stroke_fanout", "p(99)"),
    "ink_p50": trend("ink_delay", "p(50)"), "ink_p95": trend("ink_delay", "p(95)"), "ink_p99": trend("ink_delay", "p(99)"),
    "stroke_kb_s_in": round(count("ws_bytes_sent{kind:stroke}") / seconds / 1024, 1),
    "stroke_kb_s_out": round(count("ws_bytes_received{kind:stroke}") / seconds / 1024, 1),
    "other_kb_s_out": round(count("ws_bytes_received{kind:other}") / seconds / 1024, 1),
    "cpu_cores": round(statistics.median(cores), 2) if cores else None,
    "batches_in_s": round(float(batches)) if batches else None,
    # Game-server CPU per 1,000 stroke batches a second: the cost of the format, whatever k6 sent.
    "cpu_per_1k_batches_s": round(statistics.median(cores) / float(batches) * 1000, 3) if cores and batches and float(batches) > 0 else None,
    "k6_cores": round(statistics.median(k6), 2) if k6 else None,
    "redis_cmds_s": round(int(cmds) / seconds),
    "input_lag_p95": float(input_lag) if input_lag else None, "send_lag_p95": float(send_lag) if send_lag else None,
    "room_errors": count("room_errors"),
}))
PY
  tail -1 "$RESULTS/cells.jsonl"
}

if [[ -z "${SKIP_MATRIX:-}" ]]; then
  for rep in $(seq 1 "$REPEAT"); do
    for batch in $BATCHES; do
      for wire in $WIRES; do
        run "m-$batch-$wire-$rep" "$batch" "$wire" "$ROOMS" "$HOLD"
      done
    done
  done
fi
if [[ -z "${SKIP_CEILING:-}" ]]; then
  for wire in $WIRES; do
    for rooms in 20 40 60 80 100 120; do
      run "c-$CEILING_BATCH-$wire-$rooms" "$CEILING_BATCH" "$wire" "$rooms" "$HOLD_CEILING"
      over="$(tail -1 "$RESULTS/cells.jsonl" | python3 -c 'import json,sys; c=json.load(sys.stdin); print(int(max(c["input_lag_p95"] or 0, c["send_lag_p95"] or 0) > 250))')"
      if [[ "$over" == 1 ]]; then break; fi
    done
  done
fi

python3 - "$RESULTS/cells.jsonl" >"$OUT" <<'PY'
import json, statistics, sys
cells = [json.loads(l) for l in open(sys.argv[1])]
matrix = [c for c in cells if c["label"].startswith("m-")]
ceiling = [c for c in cells if c["label"].startswith("c-")]
cols = ["fanout_p50", "fanout_p95", "fanout_p99", "ink_p50", "ink_p95", "ink_p99", "stroke_kb_s_in", "stroke_kb_s_out",
        "other_kb_s_out", "batches_in_s", "cpu_cores", "cpu_per_1k_batches_s", "k6_cores", "redis_cmds_s", "input_lag_p95", "send_lag_p95", "room_errors"]
def med(values):
    values = [v for v in values if v is not None]
    return round(statistics.median(values), 2) if values else ""
if matrix:
    print("## Matrix (medians of the repeats)\n")
    print("| batch ms | wire | " + " | ".join(cols) + " |")
    print("|---|---|" + "---|" * len(cols))
    keys = sorted({(c["batch_ms"], c["wire"]) for c in matrix})
    for b, w in keys:
        group = [c for c in matrix if c["batch_ms"] == b and c["wire"] == w]
        print(f"| {b} | {w} | " + " | ".join(str(med([c[k] for c in group])) for k in cols) + " |")
if ceiling:
    print("\n## Ceiling\n")
    print("| wire | rooms | fanout_p95 | batches_in_s | cpu_cores | k6_cores | input_lag_p95 | send_lag_p95 | room_errors |")
    print("|---|---|---|---|---|---|---|---|---|")
    for c in ceiling:
        print(f"| {c['wire']} | {c['rooms']} | {c['fanout_p95']} | {c.get('batches_in_s')} | {c['cpu_cores']} | {c.get('k6_cores')} | {c['input_lag_p95']} | {c['send_lag_p95']} | {c['room_errors']} |")
print("\n<details><summary>Every run</summary>\n")
for c in cells:
    print("    " + json.dumps(c))
print("\n</details>")
PY
cp "$RESULTS/cells.jsonl" "${OUT%.md}.jsonl"
echo "==> results in $OUT"
cat "$OUT"
