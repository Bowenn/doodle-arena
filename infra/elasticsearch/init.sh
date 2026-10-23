#!/bin/bash
# One-shot job (ADR 0023): the users assets searches with. Elasticsearch's own superuser, elastic,
# is for this job and humans only. Two users, so only the process that writes can write:
#   assets          the web process: reads drawings* (the alias drawings and its versions)
#   assets-indexer  the indexer and the rebuild: writes drawings*, creates and replaces versions
#                   of it, moves the alias
# Neither can read or change anything else in the cluster. Idempotent: PUT replaces the roles
# and the users' passwords on every run.
set -euo pipefail

: "${ELASTIC_PASSWORD:?}" "${ELASTICSEARCH_ASSETS_PASSWORD:?}" "${ELASTICSEARCH_INDEXER_PASSWORD:?}"
ES="${ELASTICSEARCH_URL:-http://elasticsearch:9200}"
auth=(-u "elastic:$ELASTIC_PASSWORD")

until curl -fsS "${auth[@]}" "$ES/_cluster/health?wait_for_status=yellow&timeout=5s" >/dev/null 2>&1; do
  echo "waiting for Elasticsearch"
  sleep 2
done

put() { curl -fsS "${auth[@]}" -X PUT "$ES/$1" -H 'Content-Type: application/json' --data-binary @- >/dev/null; }

put _security/role/assets_search_read <<'JSON'
{"cluster": [], "indices": [{"names": ["drawings*"], "privileges": ["read", "view_index_metadata"]}]}
JSON
put _security/role/assets_search_write <<'JSON'
{"cluster": [], "indices": [{"names": ["drawings*"], "privileges": ["read", "write", "create_index", "manage", "view_index_metadata"]}]}
JSON
# The passwords go in the body on stdin, never on the command line.
printf '{"password": "%s", "roles": ["assets_search_read"], "full_name": "assets (search, read)"}' \
  "$ELASTICSEARCH_ASSETS_PASSWORD" | put _security/user/assets
printf '{"password": "%s", "roles": ["assets_search_write"], "full_name": "assets indexer (search, write)"}' \
  "$ELASTICSEARCH_INDEXER_PASSWORD" | put _security/user/assets-indexer
echo "users assets (read) and assets-indexer (write) ready"
