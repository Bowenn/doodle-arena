#!/bin/bash
# One-shot job (ADR 0023): the role and user assets searches with. Elasticsearch's own superuser,
# elastic, is for this job and humans only; assets gets a role limited to the indices drawings*
# (the alias drawings and its versions), so it can't read or change anything else in the
# cluster. Idempotent: PUT replaces the role and the user's password on every run.
set -euo pipefail

: "${ELASTIC_PASSWORD:?}" "${ELASTICSEARCH_ASSETS_PASSWORD:?}"
ES="${ELASTICSEARCH_URL:-http://elasticsearch:9200}"
auth=(-u "elastic:$ELASTIC_PASSWORD")

until curl -fsS "${auth[@]}" "$ES/_cluster/health?wait_for_status=yellow&timeout=5s" >/dev/null 2>&1; do
  echo "waiting for Elasticsearch"
  sleep 2
done

curl -fsS "${auth[@]}" -X PUT "$ES/_security/role/assets_search" -H 'Content-Type: application/json' -d '{
  "cluster": [],
  "indices": [{"names": ["drawings*"], "privileges": ["read", "write", "create_index", "manage", "view_index_metadata"]}]
}' >/dev/null
# The password goes in the body on stdin, never on the command line.
printf '{"password": "%s", "roles": ["assets_search"], "full_name": "assets (search)"}' "$ELASTICSEARCH_ASSETS_PASSWORD" |
  curl -fsS "${auth[@]}" -X PUT "$ES/_security/user/assets" -H 'Content-Type: application/json' --data-binary @- >/dev/null
echo "role assets_search and user assets ready"
