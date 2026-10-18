#!/usr/bin/env bash
# Shows or changes the AI moderation stub's behaviour on the local stack, for drills:
#
#   scripts/ai-mode.sh                        show the current mode
#   scripts/ai-mode.sh fail                   every call fails with 503
#   scripts/ai-mode.sh approve 5000           approve, after 5 s (longer than assets' 3 s timeout)
#   scripts/ai-mode.sh approve 0 0.5          approve, but half the calls fail
#
# Modes: approve, reject, review (unsure: a human decides), fail. The change lasts until the ai
# container restarts (it then starts from MODERATION_* in infra/docker-compose.yml). It goes
# through the internal port 9091 inside the container, which is never published or routed.
set -euo pipefail

if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi

container=doodle-arena-ai-1
if [[ $# -eq 0 ]]; then
  method=GET body=""
else
  case "$1" in
    approve | reject | review | fail) ;;
    *) echo "usage: $0 [approve|reject|review|fail] [delay_ms] [failure_rate]" >&2; exit 2 ;;
  esac
  method=PUT body="{\"mode\": \"$1\", \"delay_ms\": ${2:-0}, \"failure_rate\": ${3:-0}}"
fi

docker exec -i -e METHOD="$method" -e BODY="$body" "$container" python - <<'PY'
import os, sys, urllib.error, urllib.request
body = os.environ["BODY"].encode() or None
req = urllib.request.Request(
    "http://127.0.0.1:9091/internal/moderation-mode", data=body, method=os.environ["METHOD"],
    headers={"Content-Type": "application/json"},
)
try:
    with urllib.request.urlopen(req, timeout=5) as resp:
        print(resp.read().decode())
except urllib.error.HTTPError as e:
    sys.exit(f"HTTP {e.code}: {e.read().decode()}")
PY
