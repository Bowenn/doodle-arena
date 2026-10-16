#!/usr/bin/env bash
# Checks that the gateway and every service answer /healthz through the gateway
# (M1 exit criterion). Run it after `make up`.
set -uo pipefail

GATEWAY="${GATEWAY:-http://localhost:8080}"
TARGETS=(gateway auth lobby game-server-1 game-server-2 assets ai marketplace notifier web)

failed=0
for t in "${TARGETS[@]}"; do
  if [[ "$t" == gateway ]]; then url="$GATEWAY/healthz"; else url="$GATEWAY/healthz/$t"; fi
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url")"
  if [[ "$code" == 200 ]]; then
    printf '  ok    %-14s %s\n' "$t" "$url"
  else
    printf '  FAIL  %-14s %s (HTTP %s)\n' "$t" "$url" "$code"
    failed=1
  fi
done

if [[ $failed -ne 0 ]]; then
  echo "Some health checks failed. Inspect with: make logs s=<service>" >&2
  exit 1
fi
echo "All health checks passed through the gateway."
