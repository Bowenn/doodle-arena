#!/usr/bin/env bash
# End-to-end telemetry check (M1: "the trace and logs of one request are visible together in
# Grafana"). Sends one request through the gateway with a known trace id, then asks Grafana's
# data sources for it:
#   Tempo       the trace exists, with spans from the gateway and auth
#   Loki        auth logged a line carrying that trace_id
#   Prometheus  every service exports metrics (target_info), and HTTP server metrics exist
# Run it after `make up`. Export is batched, so it polls for up to TIMEOUT seconds.
set -uo pipefail

GATEWAY="${GATEWAY:-http://localhost:8080}"
GRAFANA="${GRAFANA:-http://localhost:3000}"
TIMEOUT="${TIMEOUT:-60}"
SERVICES=(lobby game-server notifier auth ai assets marketplace)

# A refresh without a cookie: a 401 that auth logs ("refresh rejected", missing_cookie).
# Retried on 429, because right after `make smoke` the gateway's auth rate limit is still
# draining; each attempt gets a fresh trace id, so the checked trace holds only the real request.
for _ in $(seq 10); do
  trace_id="$(openssl rand -hex 16)"
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST -H "Origin: $GATEWAY" \
    -H "traceparent: 00-${trace_id}-$(openssl rand -hex 8)-01" "$GATEWAY/api/auth/refresh")"
  [[ "$code" != 429 ]] && break
  sleep 1
done
echo "trace_id $trace_id: POST /api/auth/refresh -> $code"

proxy() { echo "$GRAFANA/api/datasources/proxy/uid/$1"; }

trace_services() {
  curl -s --max-time 5 "$(proxy tempo)/api/traces/$trace_id" | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
names = set()
for batch in data.get("batches", []):
    for attr in batch["resource"]["attributes"]:
        if attr["key"] == "service.name":
            names.add(attr["value"].get("stringValue", ""))
print(" ".join(sorted(names)))'
}

# Loki's raw answer to a LogQL query over the last 15 minutes, followed by "HTTP <code>".
loki_raw() {
  curl -s --max-time 5 -w '\nHTTP %{http_code}' -G "$(proxy loki)/loki/api/v1/query_range" \
    --data-urlencode "query=$1" --data-urlencode "since=15m"
}

# Counts the log lines Loki returns for a LogQL query.
loki_count() {
  loki_raw "$1" | sed '$d' | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except ValueError:
    print(0); sys.exit(0)
print(sum(len(s["values"]) for s in data.get("data", {}).get("result", [])))'
}

auth_log_lines() { loki_count "{service_name=\"auth\"} | trace_id=\"$trace_id\""; }

prom_label_values() {
  curl -s --max-time 5 -G "$(proxy prometheus)/api/v1/query" --data-urlencode "query=$1" | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
print(" ".join(sorted(r["metric"].get("service_name", "") for r in data["data"]["result"])))'
}

deadline=$((SECONDS + TIMEOUT))
trace_ok=0; logs_ok=0; metrics_ok=0
while ((SECONDS < deadline)); do
  if ((trace_ok == 0)); then
    services="$(trace_services)"
    [[ " $services " == *" gateway "* && " $services " == *" auth "* ]] && trace_ok=1
  fi
  if ((logs_ok == 0)); then
    [[ "$(auth_log_lines)" -gt 0 ]] && logs_ok=1
  fi
  if ((metrics_ok == 0)); then
    exporting=" $(prom_label_values 'group by (service_name) (target_info)') "
    missing=()
    for s in "${SERVICES[@]}"; do [[ "$exporting" == *" $s "* ]] || missing+=("$s"); done
    http=" $(prom_label_values 'group by (service_name) (http_server_request_duration_seconds_count)') "
    [[ ${#missing[@]} -eq 0 && "$http" == *" auth "* ]] && metrics_ok=1
  fi
  ((trace_ok && logs_ok && metrics_ok)) && break
  sleep 3
done

failed=0
report() {
  if [[ "$1" == 1 ]]; then printf '  ok    %s\n' "$2"; else printf '  FAIL  %s\n' "$3"; failed=1; fi
}
report "$trace_ok" "Tempo has the trace with spans from: ${services:-}" \
  "Tempo: no trace $trace_id with gateway and auth spans (got: ${services:-none})"
report "$logs_ok" "Loki has auth log lines with trace_id $trace_id" \
  "Loki: no auth log line with trace_id $trace_id"
report "$metrics_ok" "Prometheus: all ${#SERVICES[@]} services export metrics; HTTP metrics present" \
  "Prometheus: missing target_info for: ${missing[*]:-none}; HTTP metrics from: ${http:-none}"

if ((logs_ok == 0)); then
  # Diagnostics: is Loki answering, and does it have any auth lines at all?
  echo "  Loki's answer: $(loki_raw "{service_name=\"auth\"} | trace_id=\"$trace_id\"" | tail -c 300 | tr '\n' ' ')"
  echo "  auth lines in Loki (any trace): $(loki_count '{service_name="auth"}')"
fi

if ((failed)); then
  echo "Telemetry check failed. In Grafana: $GRAFANA/explore (Tempo, search for the trace id)" >&2
  exit 1
fi
echo "Telemetry check passed: open $GRAFANA/explore and search Tempo for $trace_id"
