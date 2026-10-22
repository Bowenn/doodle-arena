#!/usr/bin/env bash
# make analytics-check (task 6.5, ADR 0018): checks the top-guesses job end to end on
# `PROFILES=analytics make up`, as dev-tools. A second room gets a guess every 2 seconds
# throughout: its own TopGuesses shows that the job reads (a job without committed offsets starts
# at the end of the topic, so guesses sent before its reader starts are never counted), and its
# guesses move the watermark past the first room's windows (event time only advances with new
# guesses). Then the room under test gets six guesses (dog three times, cat twice, sun once). It
# passes when live.top-guesses.v1 has a TopGuesses for that room ranking dog 3, cat 2, sun 1.
# CI's stack job runs it.
set -euo pipefail

cd "$(dirname "$0")/.."
if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi
# A worktree has no .env of its own: point ENV_FILE at the main checkout's.
env_file="${ENV_FILE:-.env}"
if [[ ! -f "$env_file" ]]; then
  echo "no $env_file: run make up first, or set ENV_FILE" >&2
  exit 2
fi
KAFKA_DEV_TOOLS_PASSWORD="$(sed -n 's/^KAFKA_DEV_TOOLS_PASSWORD=//p' "$env_file")"
if [[ -z "$KAFKA_DEV_TOOLS_PASSWORD" ]]; then
  echo "no KAFKA_DEV_TOOLS_PASSWORD in $env_file: run make up first" >&2
  exit 2
fi
export KAFKA_DEV_TOOLS_PASSWORD

# The job must be running: Flink's REST API lists it on the JobManager.
state="$(curl -fsS http://localhost:8081/jobs/overview 2>/dev/null |
  python3 -c 'import json, sys; print(" ".join(j["state"] for j in json.load(sys.stdin)["jobs"] if j["name"] == "analytics"))' ||
  true)"
if [[ "$state" != *RUNNING* ]]; then
  echo "the analytics job isn't running (state: ${state:-no JobManager on localhost:8081})." >&2
  echo "Start it with PROFILES=analytics make up; make logs s=analytics shows why it stopped." >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
(cd tools/eventctl && go build -o "$work/eventctl" .)
eventctl="$work/eventctl"

room="check-$(date +%s)-$RANDOM"
tick="$room-tick"

# find_top <room> [want]: exits 0 when the room has a TopGuesses (ranked as want, if given), 3
# when it is ranked otherwise, and 1 when there's none yet.
find_top() {
  "$eventctl" decode -topic live.top-guesses.v1 -n 200 > "$work/out" 2>&1 || return 1
  python3 - "$1" "$work/out" "${2:-}" <<'PY'
import json, re, sys

room, path, want = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(path).read()
# decode prints "--- <topic> ... key=<room> ..." and then the event as JSON.
headers = re.findall(r"^--- .*$", text, flags=re.M)
bodies = re.split(r"^--- .*$", text, flags=re.M)[1:]
for header, body in zip(headers, bodies):
    if f" key={room} " not in header + " ":
        continue
    top = json.loads(body)["topGuesses"]
    got = [(g["text"], int(g["count"])) for g in top["guesses"]]
    print(f"TopGuesses for {room}, window ending {top['windowEnd']}: {got}")
    sys.exit(0 if not want or str(got) == want else 3)
sys.exit(1)
PY
}

want="[('dog', 3), ('cat', 2), ('sun', 1)]"
reading=0
for _ in $(seq 1 60); do
  "$eventctl" guesses -room "$tick" -round 1 tick >/dev/null
  sleep 2
  if [[ $reading -eq 0 ]]; then
    if find_top "$tick" >/dev/null; then
      reading=1
      "$eventctl" guesses -room "$room" -round 1 dog cat dog sun cat dog
    fi
    continue
  fi
  status=0
  find_top "$room" "$want" || status=$?
  if [[ $status -eq 0 ]]; then
    echo "analytics-check: OK"
    exit 0
  elif [[ $status -eq 3 ]]; then
    echo "analytics-check: FAIL, want $want" >&2
    exit 1
  fi
done
if [[ $reading -eq 0 ]]; then
  echo "analytics-check: FAIL, the job never answered $tick's guesses (make logs s=analytics)" >&2
else
  echo "analytics-check: FAIL, no TopGuesses for $room within 2 minutes (make logs s=analytics)" >&2
fi
exit 1
