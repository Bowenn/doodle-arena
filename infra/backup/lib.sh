#!/usr/bin/env bash
# Shared by dump.sh and load.sh (ADR 0016). Sourced, not run. They run on the AWS data host
# through SSM Run Command (scripts/aws/backup.sh, restore.sh) and locally against make up
# (scripts/backup-check.sh), so they stay bash 3.2 compatible and reach the stores only through
# docker exec: every password stays inside its container.
set -euo pipefail

PG="${PG_CONTAINER:-doodle-data-postgres-1}"
MONGO="${MONGO_CONTAINER:-doodle-data-mongo-1}"
# One database per service (design doc section 5); each is owned by the role of the same name.
DATABASES="auth assets marketplace notifier"

# A fresh work directory, mode 700. On any exit that isn't 0, the summary of what finished is
# printed with result "failed", so the caller knows which store the run stopped on. With
# "remove", the directory goes when the script exits.
start_work() { # start_work <dir> [remove]
  work="$1" remove_work="${2:-}"
  rm -rf "$work"
  mkdir -p "$work"
  chmod 700 "$work"
  trap end_work EXIT
}

end_work() {
  local status=$?
  if ((status != 0)); then
    print_summary "$work" failed || true
  fi
  if [[ -n "$remove_work" ]]; then
    rm -rf "$work"
  fi
  exit "$status"
}

now() { python3 -c 'import time; print(f"{time.time():.3f}")'; }

# bytes and SHA-256 (hex) of a file: python3, because macOS has no sha256sum.
file_info() {
  python3 - "$1" <<'PY'
import hashlib, os, sys
h = hashlib.sha256()
with open(sys.argv[1], "rb") as f:
    for block in iter(lambda: f.read(1 << 20), b""):
        h.update(block)
print(os.path.getsize(sys.argv[1]), h.hexdigest())
PY
}

psql_at() { docker exec -i "$PG" psql -U postgres -d "$1" -XAtq -v ON_ERROR_STOP=1 "${@:2}"; }

# mongosh, mongodump and mongorestore as root. The password is expanded inside the container.
# GOMEMLIMIT keeps the Go tools' heap small: with replays of about 360 KB each, mongorestore
# grew past 600 MB and was OOM-killed in a 1 GB container (make backup-check).
mongo_root() {
  local tool="$1"
  shift
  docker exec -i -e GOMEMLIMIT=256MiB "$MONGO" sh -c 'tool="$1"; shift; exec "$tool" --quiet --username root --password "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin "$@"' sh "$tool" "$@"
}

# Tables other than Postgres' own: none means the database was never migrated or restored.
user_tables() {
  psql_at "$1" -c "SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')"
}

mongo_collections() {
  mongo_root mongosh --eval 'print(db.getSiblingDB("assets").getCollectionNames().length)'
}

# Exact rows per table ("schema.table|rows"), for the check that a restore brought back what the
# dump held. count(*) on every table is fine at today's size; at millions of rows it would want
# a sample instead.
pg_counts() {
  psql_at "$1" <<'SQL'
SELECT format('SELECT %L || ''|'' || count(*) FROM %I.%I', schemaname || '.' || tablename, schemaname, tablename)
FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1
\gexec
SQL
}

mongo_counts() {
  mongo_root mongosh --eval 'const a = db.getSiblingDB("assets"); a.getCollectionNames().sort().forEach(c => print(c + "|" + a[c].countDocuments({})))'
}

# The migrations a database has applied, one per line, for the check that refuses to restore a
# backup into code that is older than its schema (scripts/aws/backups.py check-schema).
pg_migrations() {
  local db="$1" table query
  case "$db" in
    auth) table=alembic_version query="SELECT version_num FROM alembic_version" ;;
    assets) table=django_migrations query="SELECT app || '.' || name FROM django_migrations ORDER BY id" ;;
    marketplace) table=flyway_schema_history query="SELECT version FROM flyway_schema_history WHERE success AND version IS NOT NULL ORDER BY installed_rank" ;;
    notifier) table=schema_migrations query="SELECT version || CASE WHEN dirty THEN ' dirty' ELSE '' END FROM schema_migrations" ;;
  esac
  if [[ "$(psql_at "$db" -c "SELECT to_regclass('$table') IS NOT NULL")" == t ]]; then
    psql_at "$db" -c "$query"
  fi
}

# Waits until both stores answer and Postgres' init script has created every service's role (a
# fresh data host is still starting when aws-up gets here).
wait_ready() {
  local deadline=$((SECONDS + ${READY_TIMEOUT:-600})) db missing
  while :; do
    missing=""
    if ! docker exec "$PG" pg_isready -U postgres -q 2>/dev/null; then
      missing="postgres"
    else
      for db in $DATABASES; do
        [[ "$(psql_at postgres -c "SELECT 1 FROM pg_database WHERE datname = '$db'" 2>/dev/null)" == 1 ]] || missing="$missing database:$db"
      done
    fi
    [[ "$(mongo_root mongosh --eval 'print(db.adminCommand("ping").ok)' 2>/dev/null)" == 1 ]] || missing="$missing mongo"
    [[ -z "$missing" ]] && return 0
    if ((SECONDS > deadline)); then
      echo "not ready after ${READY_TIMEOUT:-600} s:$missing" >&2
      return 1
    fi
    sleep 5
  done
}

# The JSON summary scripts/aws/*.sh read from the command's output, between two marker lines.
# Its input is the work directory: <name>.info ("bytes sha256 seconds file") per artifact, plus
# <store>.counts and <db>.migrations.
print_summary() {
  echo "BACKUP-SUMMARY-BEGIN"
  python3 - "$1" "${2:-}" <<'PY'
import json, pathlib, sys
work, result = pathlib.Path(sys.argv[1]), sys.argv[2]
out = {"artifacts": [], "counts": {}, "migrations": {}}
for info in sorted(work.glob("*.info")):
    size, sha256, seconds, file = info.read_text().split()
    out["artifacts"].append({"name": info.stem, "file": file, "bytes": int(size), "sha256": sha256, "seconds": float(seconds)})
for counts in sorted(work.glob("*.counts")):
    rows = (line.rsplit("|", 1) for line in counts.read_text().splitlines() if line)
    out["counts"][counts.stem] = {name: int(n) for name, n in rows}
for migrations in sorted(work.glob("*.migrations")):
    out["migrations"][migrations.stem] = [line for line in migrations.read_text().splitlines() if line]
if result:
    out["result"] = result
print(json.dumps(out, sort_keys=True))
PY
  echo "BACKUP-SUMMARY-END"
}
