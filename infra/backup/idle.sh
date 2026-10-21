#!/usr/bin/env bash
# Is the event pipeline idle? scripts/aws/backup.sh asks the data host before it dumps (ADR
# 0016): once the gateway and the game servers are stopped, the consumers, the Celery worker
# and the outbox relays finish what is in flight. Reads otel-lgtm's Prometheus on the host and
# prints one "<signal> <value>" line per signal, with "none" for a signal that has no sample.
# Always exits 0: the caller decides.
set -euo pipefail

PROMETHEUS="${PROMETHEUS_URL:-http://127.0.0.1:9090}"

query() { # query <signal> <PromQL>
  local value
  value="$(curl -fsS --max-time 5 "$PROMETHEUS/api/v1/query" --data-urlencode "query=$2" 2>/dev/null |
    python3 -c 'import json, sys
r = json.load(sys.stdin)["data"]["result"]
print(r[0]["value"][1] if r else "none")' 2>/dev/null)" || value="error"
  echo "$1 ${value:-error}"
}

# docs/observability.md has each metric. Events in Kafka not yet consumed, by any group:
query kafka_lag 'sum(kafka_consumer_group_lag_sum)'
# The oldest event each outbox hasn't published yet (0 when drained):
query auth_outbox 'max(auth_outbox_lag_seconds)'
query assets_outbox 'max(assets_outbox_lag_seconds)'
query market_outbox 'max(market_outbox_lag_seconds)'
# Celery jobs waiting (rendering and moderating drawings):
query assets_jobs 'max(assets_jobs_oldest_age_seconds)'
