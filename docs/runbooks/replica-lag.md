# Runbook: ReplicaLagHigh

- Severity: warning
- Dashboard: Doodle Arena: marketplace (`da-marketplace`), "Replica lag" and "Reads by target"

## What it means

The Postgres read replica (ADR 0020) is more than 30 seconds behind the primary, or it can't be reached, so reads fell back to the primary, for 5 minutes.

Nothing is wrong for users:

- A user's own writes are always read from wherever has them: the WAL-position cookie sends a read to the primary while the replica is behind.
- A read never fails because of the replica.

What suffers:

- **The primary carries the reads** the replica should take, next to its writes and locks.
- **Other users' changes show up late** on browsing pages: listings, ownership history, the gallery.
- **The replication slot pins WAL** on the primary (up to 1 GB, `max_slot_wal_keep_size`).

## Check

1. Is the replica up? Locally `make ps` (`postgres-replica`); in AWS, the data host's containers (`sudo docker ps` through Session Manager).
2. Is it streaming? On the primary:

   ```bash
   docker compose -f infra/docker-compose.yml --env-file .env exec postgres psql -U postgres -c "SELECT application_name, state, pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) AS behind_bytes FROM pg_stat_replication"
   docker compose -f infra/docker-compose.yml --env-file .env exec postgres psql -U postgres -c "SELECT slot_name, active, wal_status FROM pg_replication_slots"
   ```

   - No row in `pg_stat_replication` means the replica isn't connected.
   - `wal_status = lost` means the slot gave up its WAL: the replica can never catch up and must be recloned (Fix, last step).
3. Is replay paused or stuck? On the replica: `SELECT pg_is_in_recovery(), pg_is_wal_replay_paused(), now() - pg_last_xact_replay_timestamp()`. Also read its log: `make logs s=postgres-replica`. A long query conflicting with replay shows as "canceling statement due to conflict with recovery". `hot_standby_feedback` is on, so that should be rare.
4. Is the primary writing a lot? A big migration or `make seed` produces WAL faster than the replica can replay it for a while. That catches up on its own.

## Fix

- **Replica stopped:** start it (`make up` locally). It reconnects through its slot and catches up.
- **Replay paused** (someone ran `pg_wal_replay_pause()`): `SELECT pg_wal_replay_resume()` on the replica.
- **A burst of writes:** wait. The lag falls once replay catches up, and reads use the primary meanwhile.
- **Slot lost, or the replica can't start from its data:** reclone it. Stop it, remove its volume, and start it again. It clones the primary with `pg_basebackup` on start.

  ```bash
  docker compose -f infra/docker-compose.yml --env-file .env stop postgres-replica
  docker compose -f infra/docker-compose.yml --env-file .env rm -f postgres-replica
  docker volume rm doodle-arena_postgres-replica-data
  make up
  ```

  If the slot itself is gone or broken, drop it on the primary (`SELECT pg_drop_replication_slot('replica_1')`) first. The `postgres-replication` job creates it again.

## Afterwards

- Record how far behind it got and why. A replica that falls behind under normal load needs more resources than the primary has to spare: on the data host they share one machine (ADR 0003).
- If reads fell back for a long time, check the primary's latency on the same dashboard (HTTP latency p95).
