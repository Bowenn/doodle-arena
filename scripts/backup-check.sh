#!/usr/bin/env bash
# make backup-check: the AWS backup's dump and restore (ADR 0016, infra/backup/), end to end
# against the local stack. It dumps the running make up stores, restores into throwaway
# Postgres and MongoDB containers (the data host's images, with Postgres' init script and new
# passwords, as a new AWS session has), then checks that every table's rows and every
# collection's documents match and that the backup's schema isn't newer than this checkout.
# Read-only for the local stack. Takes a few minutes with load-test replays in MongoDB.
# The mongodump reads the stack's MongoDB hard: don't run it during a load test.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD"
if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi

compose_image() { sed -n "s|^ *image: \($1:.*\)$|\1|p" infra/aws/data-host/compose.yml | head -1; }
pg_image="$(compose_image postgres)"
mongo_image="$(compose_image mongo)"
work="$(mktemp -d)"
pg="backup-check-postgres-$$"
mongo="backup-check-mongo-$$"
cleanup() {
  docker rm -f "$pg" "$mongo" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

secret() { python3 -c 'import secrets; print(secrets.token_hex(20))'; }

echo "==> dump the local stack"
BACKUP_DIR="$work/dump" PG_CONTAINER=doodle-arena-postgres-1 MONGO_CONTAINER=doodle-arena-mongo-1 \
  infra/backup/dump.sh > "$work/dump.out"
python3 scripts/aws/backups.py summary "$work/dump.out" > "$work/summary.json"
python3 scripts/aws/backups.py manifest "$work/summary.json" local "$(git rev-parse --short=12 HEAD)" 0 0 > "$work/manifest.json"

echo "==> the schema check against this checkout"
python3 scripts/aws/backups.py check-schema "$work/manifest.json" "$ROOT" HEAD

echo "==> fresh stores: $pg_image and $mongo_image"
# Memory-capped: they share Docker's VM with the local stack, and the first run of this check,
# restoring 2.5 GB of load-test replays, got the stack's own MongoDB OOM-killed.
docker run -d --name "$pg" --memory 512m -e POSTGRES_PASSWORD="$(secret)" \
  -e AUTH_DB_PASSWORD="$(secret)" -e ASSETS_DB_PASSWORD="$(secret)" \
  -e MARKETPLACE_DB_PASSWORD="$(secret)" -e NOTIFIER_DB_PASSWORD="$(secret)" \
  -v "$ROOT/infra/postgres/init:/docker-entrypoint-initdb.d:ro" "$pg_image" >/dev/null
docker run -d --name "$mongo" --memory 1g -e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD="$(secret)" \
  "$mongo_image" --wiredTigerCacheSizeGB 0.25 >/dev/null

echo "==> restore"
while read -r name file _ sha256 _; do
  echo "$name $sha256 file://$work/dump/$file"
done < <(python3 scripts/aws/backups.py artifacts "$work/manifest.json") > "$work/plan.tsv"
start=$SECONDS
BACKUP_DIR="$work/restore" PG_CONTAINER="$pg" MONGO_CONTAINER="$mongo" \
  infra/backup/load.sh "$work/plan.tsv" > "$work/load.out"
python3 scripts/aws/backups.py summary "$work/load.out" > "$work/restored.json"
seconds=$((SECONDS - start))

echo "==> compare"
python3 scripts/aws/backups.py compare-counts "$work/manifest.json" "$work/restored.json"
python3 - "$work/manifest.json" "$work/restored.json" "$seconds" <<'PY'
import json, sys
manifest, restored = (json.load(open(p)) for p in sys.argv[1:3])
for a in manifest["artifacts"]:
    rows = sum(manifest["counts"].get(a["name"], {}).values())
    print(f'    {a["name"]}: {a["bytes"]} bytes, {rows} rows or documents, dumped in {a["seconds"]:.1f} s')
print(f"    restored in {sys.argv[3]} s; every table and collection matches")
PY

echo "==> a second load"
BACKUP_DIR="$work/again" PG_CONTAINER="$pg" MONGO_CONTAINER="$mongo" infra/backup/load.sh "$work/plan.tsv" > "$work/again.out" 2>/dev/null
python3 scripts/aws/backups.py summary "$work/again.out" | grep -q '"result": "skipped"'
echo "    skipped: every database already has tables"
# One database empty next to three with data: a restore that stopped partway. Refused.
docker exec "$pg" psql -U postgres -d notifier -Atqc "DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION notifier" >/dev/null 2>&1
if BACKUP_DIR="$work/again" PG_CONTAINER="$pg" MONGO_CONTAINER="$mongo" infra/backup/load.sh "$work/plan.tsv" >/dev/null 2>&1; then
  echo "load.sh restored next to databases that hold data" >&2
  exit 1
fi
echo "    refused: one database empty, three with data"
echo "backup-check passed"
