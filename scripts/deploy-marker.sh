#!/usr/bin/env bash
# deploy-marker.sh <version> <environment>: a Grafana annotation tagged "deploy" and the
# environment (task 6.9), drawn on every dashboard as a vertical line, so a change in a graph can
# be matched to the release that caused it. make up posts one locally; aws-up posts one in AWS
# from the data host (scripts/aws/up.sh), where Grafana listens on localhost only.
#   GRAFANA                      default http://localhost:3000 (local Grafana allows anonymous edits)
#   GRAFANA_USER, GRAFANA_PASSWORD   an admin, where anonymous visitors only view
# A marker that can't be posted is reported, never fatal: the deploy itself worked.
set -uo pipefail

version="${1:?usage: $0 <version> <environment>}"
environment="${2:?usage: $0 <version> <environment>}"
GRAFANA="${GRAFANA:-http://localhost:3000}"
auth=()
if [[ -n "${GRAFANA_USER:-}" ]]; then auth=(-u "$GRAFANA_USER:${GRAFANA_PASSWORD:-}"); fi

body="$(python3 -c 'import json, sys, time; print(json.dumps({"time": int(time.time() * 1000), "tags": ["deploy", sys.argv[2]], "text": f"Deployed {sys.argv[1]} ({sys.argv[2]})"}))' "$version" "$environment")"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 ${auth[@]+"${auth[@]}"} -X POST \
  -H 'Content-Type: application/json' -d "$body" "$GRAFANA/api/annotations")"
if [[ "$code" == 200 ]]; then
  echo "Deploy marker: $version ($environment)"
else
  echo "Deploy marker not posted (Grafana answered $code)" >&2
fi
