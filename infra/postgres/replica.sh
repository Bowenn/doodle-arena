#!/usr/bin/env bash
# The read replica (ADR 0020): the same image as the primary, as a hot standby. On its first
# start (an empty volume) it clones the primary with pg_basebackup through the replica_1 slot and
# writes standby.signal and primary_conninfo (-R); after that it only streams. The clone carries
# the primary's roles and databases, so each service reads with its own role. To start over
# (the slot lost its WAL after a long stop), remove the replica's volume.
set -euo pipefail
: "${POSTGRES_REPLICATION_PASSWORD:?}"
data="${PGDATA:?}"
if [[ ! -s "$data/PG_VERSION" ]]; then
  mkdir -p "$data"
  chown -R postgres:postgres "$(dirname "$data")"
  until gosu postgres pg_isready -h postgres -q; do sleep 1; done
  echo "cloning the primary into $data"
  PGPASSWORD="$POSTGRES_REPLICATION_PASSWORD" gosu postgres pg_basebackup -h postgres -U replicator \
    -D "$data" -X stream -S replica_1 -R --checkpoint=fast
  chmod 0700 "$data"
fi
# hot_standby_feedback: the primary's vacuum waits for rows the replica's queries still read, so
# a gallery page is never cancelled mid-read (its queries take milliseconds).
exec gosu postgres postgres -D "$data" -c hot_standby=on -c hot_standby_feedback=on
