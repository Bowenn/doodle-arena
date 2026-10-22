# ADR 0020: A read replica, with read-your-writes by WAL position

- Status: Accepted (2026-10-22)
- Date: 2026-10-22
- Deciders: Bowen (covering Will's track until he joins)

## Context

Task 6.6 adds a Postgres read replica for read-heavy endpoints (design doc, section 10: "eventual, with read-your-writes for a user's own actions"). Every service's database lives on one Postgres server, so one physical replica serves them all.

A replica lags. A user who buys a drawing and then opens "My drawings" must see it, even if the replica hasn't replayed the purchase yet. The design doc said only "reads after a user's own write go to the primary for a short window"; the mechanism was open.

## Options

How a read knows whether the replica has the user's writes:

- **A. A WAL-position token.** Every successful write request answers with a cookie holding the primary's WAL position after its commit (`pg_current_wal_lsn()`). A read that carries the cookie goes to the replica only if the replica has replayed past that position (`pg_last_wal_replay_lsn()`); otherwise it goes to the primary.
  - Pros: exact. It doesn't guess a window, and it reads the primary only while the replica is actually behind. All databases share one WAL, so one cookie works for every service.
  - Cons: one extra query per write (the position) and per read with a cookie (the replica's position).
- **B. A time window.** A cookie with the time of the user's last write; reads within 5 seconds go to the primary.
  - Pros: simple.
  - Cons: a guess. A replica more than 5 seconds behind breaks the guarantee, and every read in the window hits the primary even when the replica has caught up.
- **C. Personal reads on the primary.** Anything about the caller ("my drawings", "my transactions") always reads the primary; only public browsing uses the replica.
  - Pros: no tokens.
  - Cons: the reads that matter most to a user never leave the primary, so the replica offloads less.

## Decision

Bowen chose **A**, with the replica in both the local stack and the AWS data host's compose file (measured in AWS in task 6.9):

- **Replication:**
  - The replica is the same official Postgres image as a hot standby (`postgres-replica`). It is cloned with `pg_basebackup` from the primary on its first start and then streams WAL.
  - It streams as `replicator`, a role with `REPLICATION` and nothing else, through a physical replication slot (`replica_1`), so the primary keeps the WAL the replica still needs. The primary caps what a slot may hold (`max_slot_wal_keep_size` = 1 GB), so a dead replica can't fill its disk.
  - A one-shot job (`postgres-replication`) creates the role and the slot idempotently, because the init scripts only run on an empty volume. The primary's `pg_hba.conf` is mounted (`hba_file`) with one line for `replicator`.
- **The token:**
  - After every successful write request (not `GET`), the marketplace sets the cookie `da_rw_lsn` (path `/api`, HttpOnly, SameSite=Lax, Secure in AWS, 1 hour) to `pg_current_wal_lsn()` read after the commit, which is at or past the commit record.
  - A routed read compares that position with the replica's `pg_last_wal_replay_lsn()`. No cookie means the replica.
- **What reads the replica:**
  - Marketplace: the listings browse and a listing, a drawing's ownership history, "my drawings", and wallet transactions.
  - Assets: the gallery and a drawing's metadata.
  - Never a read inside a write transaction, never the wallet balance, never anything that decides money. Writes and their reads stay on the primary.
- **Failure:** a replica that can't be reached, or that is behind the token, means the primary. A read never fails because of the replica.

## Consequences

- **Reads move off the primary**, which keeps its connections and buffers for writes, locks and reconciliation. In this deployment both run on one host, so the gain is in contention, not in hardware (task 6.9 measures it).
- **Writes not made by the user stay eventual.** A drawing minted by the Kafka consumer, rewards, a webhook's coins: the user has no token for those, and they show up once the replica replays them, usually within milliseconds. That matches the consistency model.
- **The replica is a second copy of every database**, about 200 MB of memory, with the same data-ownership rules: each service connects with its own role, which exists on the replica because roles replicate.
- **A slot pins WAL.** If the replica is down for long, the primary keeps up to 1 GB of WAL, then drops the slot's claim. The replica must then be recloned: remove its volume.
- **What to watch:** replica lag (seconds behind, from the replica), reads by target and reason, and the ReplicaLagHigh alert.
