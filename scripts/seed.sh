#!/usr/bin/env bash
# make seed (task 6.6): about 5 million rows in the local stack's Postgres, for query tuning
# (make explain) and the read replica. Each database is seeded as its own service's role, through
# the same triggers and constraints the services meet, in twelve monthly transactions: the ledger
# balances, balances match their entries, ownership matches its provenance, so the reconciliation
# job reads 0 throughout. Users are seed-u-<n>, drawings come from rounds seed-m-<n>.
#
# It runs once: the ledger is append-only (a trigger refuses DELETE), so seeded money can't be
# taken out again. To start over, remove the Postgres volume (make down, then
# docker volume rm doodle-arena_postgres-data), which removes everything else too.
set -euo pipefail

cd "$(dirname "$0")/.."
if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi
env_file="${ENV_FILE:-.env}"
# SEED_CONTAINER seeds another Postgres container (a trial run), SEED_SCALE divides every count.
if [[ -n "${SEED_CONTAINER:-}" ]]; then
  pg=(docker exec -i)
  target="$SEED_CONTAINER"
else
  pg=(docker compose -f infra/docker-compose.yml --env-file "$env_file" exec -T)
  target=postgres
fi
options="-c seed.base_ms=\${base_ms:-0} -c seed.scale=${SEED_SCALE:-1}"

psql_as() { # role db file... : psql over the container's local socket, as the service's role
  local role="$1" db="$2"
  shift 2
  cat "$@" | "${pg[@]}" -e PGOPTIONS="$options" "$target" \
    psql -q -X -v ON_ERROR_STOP=1 -v month="${month:-0}" -U "$role" -d "$db" -f -
}

sql() { # role db statement
  "${pg[@]}" "$target" psql -At -X -U "$1" -d "$2" -c "$3"
}

already=$(sql marketplace marketplace "SELECT count(*) FROM accounts WHERE owner_id = 'seed-u-0'")
if [[ "$already" != "0" ]]; then
  echo "make seed: this database is seeded already (the ledger is append-only, so seeding twice"
  echo "would double it). To start over: make down; docker volume rm doodle-arena_postgres-data"
  exit 0
fi

# The seeded year ends a day ago, so every row is in the past.
base_ms=$(( ($(date +%s) - 367 * 86400) * 1000 ))
options="-c seed.base_ms=$base_ms -c seed.scale=${SEED_SCALE:-1}"
start=$SECONDS
for month in $(seq 0 11); do
  psql_as marketplace marketplace scripts/seed/common.sql scripts/seed/marketplace-month.sql
  echo "  marketplace: month $month seeded ($((SECONDS - start)) s)"
done
month=0 psql_as marketplace marketplace scripts/seed/common.sql scripts/seed/marketplace-outbox.sql
echo "  marketplace: outbox seeded ($((SECONDS - start)) s)"
psql_as assets assets scripts/seed/common.sql scripts/seed/assets.sql
echo "  assets: drawings seeded ($((SECONDS - start)) s)"

# Fresh statistics and visibility maps, so the plans make explain shows are the steady state.
# (A service role can't vacuum the shared catalogs; those warnings say nothing.)
for db in marketplace assets; do
  sql "$db" "$db" "VACUUM (ANALYZE)" 2>&1 | { grep -v "permission denied to vacuum" || true; }
done
sql marketplace marketplace "
  SELECT 'ledger_entries ' || count(*) FROM ledger_entries UNION ALL
  SELECT 'ledger_transactions ' || count(*) FROM ledger_transactions UNION ALL
  SELECT 'ownership_events ' || count(*) FROM ownership_events UNION ALL
  SELECT 'listings ' || count(*) FROM listings UNION ALL
  SELECT 'outbox ' || count(*) FROM outbox UNION ALL
  SELECT 'imbalance ' || COALESCE(SUM(amount), 0) FROM ledger_entries"
echo "make seed: done in $((SECONDS - start)) s"
