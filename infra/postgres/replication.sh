#!/usr/bin/env bash
# One-shot job (postgres-replication, ADR 0020): the replica's streaming user and its replication
# slot on the primary. Idempotent, and unlike the init scripts it also runs on an existing volume.
# The slot makes the primary keep the WAL the replica hasn't received yet, up to
# max_slot_wal_keep_size (1 GB), so a stopped replica can catch up but can't fill the disk.
set -euo pipefail
: "${POSTGRES_PASSWORD:?}" "${POSTGRES_REPLICATION_PASSWORD:?}"
export PGPASSWORD="$POSTGRES_PASSWORD"
psql -v ON_ERROR_STOP=1 -h "${PGHOST:-postgres}" -U postgres -d postgres \
  -v password="$POSTGRES_REPLICATION_PASSWORD" <<'SQL'
SELECT NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'replicator') AS create_role \gset
\if :create_role
  CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD :'password';
\else
  ALTER ROLE replicator WITH REPLICATION LOGIN PASSWORD :'password';
\endif
SELECT pg_create_physical_replication_slot('replica_1')
WHERE NOT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = 'replica_1');
SQL
echo "replicator and slot replica_1 ready"
