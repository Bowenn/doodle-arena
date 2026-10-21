# ADR 0016: Keeping AWS data across sessions

- Status: Accepted (2026-10-20)
- Date: 2026-10-20
- Deciders: Bowen (covering Will's track until he joins)

## Context

ADR 0003 creates the whole AWS stack for a session and destroys it afterwards, data included: "No AWS data survives a teardown." That kept the cost at about $0.30–0.80 per stack-hour and nothing at all between sessions. Since task 5.5 the stack is open to everyone, and people now make drawings, buy coins and trade during a session. All of it was lost at the first teardown after a public session (2026-10-20), and Bowen wants drawings to stay.

A drawing can't be kept on its own. Its record and image live in assets, but its owner, its sales and the coins that paid for them are in the marketplace, and the accounts are in auth. Keeping drawings consistently means keeping every service's durable data:

- Postgres: the `auth`, `assets`, `marketplace` and `notifier` databases (one per service, design doc section 5).
- MongoDB: assets' `assets` database.
- S3: the `drawings` bucket.

Kafka and Redis don't need to survive. Every event that matters is already in a database: the outboxes hold whatever wasn't published yet, and consumers are idempotent. Redis holds rooms, tickets, Celery's queue and the weekly leaderboard, all short-lived by design. The `round-logs` bucket is a claim check with a 7-day expiry. The stack is still torn down after each session, because running it around the clock would cost $220–580 a month.

## Options

### Option A: A data disk and buckets that live in the bootstrap root

Move the drawings bucket and a separate EBS data volume into `infra/terraform/bootstrap/`, which a teardown never touches. The data host attaches that volume at boot and keeps Postgres and MongoDB on it.

- Pros: nothing to copy, so start and teardown stay as fast as today. Nothing is lost, up to the last write before the teardown.
- Cons: the volume keeps the database users' passwords and Kafka's SCRAM credentials from the session that created them, while the stack generates new random ones every session. So either those passwords move to the bootstrap root too, or every start resets them in each store. An EBS volume belongs to one availability zone, which ties the data host to it. A disk carried from session to session is also the opposite of the stateless stack ADR 0003 chose. About $1.60 a month for 20 GB.

### Option B: Snapshot the data disk at teardown, restore it at start

`aws-down` snapshots the data host's volume and keeps the drawings bucket. `aws-up` creates the next volume from the latest snapshot.

- Pros: the whole disk comes back, Kafka included. Snapshots are incremental and cost a few cents a month.
- Cons: the same password problem as Option A. Teardown and start each take a few minutes longer. A crash-consistent snapshot of a running Postgres works, but only because Postgres replays its WAL at start, which is harder to explain than a dump.

### Option C: Back up logically at teardown, restore at start

Before it destroys anything, `aws-down` writes `pg_dump` of each service's database, a `mongodump` of assets' database and a copy of the drawings bucket to a backup bucket in the bootstrap root. `aws-up` restores the latest backup into the new, empty stores before the migrations and services start.

- Pros: the stack stays create-everything, destroy-everything, with no state carried in disks or volumes. New passwords don't matter, because a dump restored with `--no-owner` becomes owned by whichever user restores it. Every session is a restore test, so the backups are known to work. The recovery point is the last teardown, and the restore time can be measured. The same dumps would work if the data stores moved to RDS or a managed MongoDB.
- Cons: more script: one dump and one restore per database, in the right order. A teardown that fails before the backup finishes must not delete the data. Larger data makes start and teardown slower (minutes at today's size).

### Option D: Keep the stack running

- Pros: no new code.
- Cons: $220–580 a month against a $35 budget.

## Decision

Option C, chosen by Bowen. It keeps ADR 0003's stateless, per-session stack and adds a backup that's restored every session. The backup is therefore exercised all the time, so it can't silently rot, and it gives the strongest story for interviews: a stated recovery point (the last teardown), a measured restore time, and a restore that does the same thing on every start. Options A and B carry state between sessions in a form that has to be kept compatible with credentials the stack regenerates. Option D costs too much.

## Consequences

- **The backup comes before any deletion, and its failure stops the teardown.** `aws-down` first stops the services that write: game servers drain (task 5.5) and outbox relays flush. Then it dumps, uploads and checks every artifact (size and checksum), and only then runs `terraform destroy`. A failed dump, a failed upload or a lost network aborts the teardown with the stack still running and still costing money, which is better than losing the data. The teardown on 2026-10-20 that lost its network halfway shows why the order matters.
- **Recovery point: the last completed teardown.** If a session dies without a clean `aws-down` (the data host crashes, or someone runs `terraform destroy` by hand), that session's data is lost. An optional periodic dump during long sessions would narrow this; it is out of scope for the first version.
- **The restore runs before the migrations.** The dumps include each service's migration history, so the migration step then applies only migrations newer than the backup. A dump from a newer schema than the code being deployed must be refused, not loaded.
- **Some secrets must outlive a session:**
  - The notifier encrypts webhook endpoint secrets with `notifier-secret-key` (`internal/secretbox`), which the stack generates per session today, so a restored endpoint couldn't be read. That key moves to the bootstrap root's SSM parameters.
  - The JWT and ticket signing keys still rotate every session, so everyone signs in again and old refresh tokens fail. The accounts themselves are kept.
  - The Django secret key also still rotates, which only signs out the Django admin.
- **Events in flight at teardown.** An outbox entry not yet published is in the database and is published after the restore. An event that was published but not yet consumed is lost with Kafka. For example, a `RoundFinished` the assets consumer hadn't read yet would never become a drawing. Draining and flushing before the dump makes that window small. The relays' and consumers' existing metrics show whether anything was left behind.
- **The backup bucket** lives in the bootstrap root: private, encrypted, versioned, with a lifecycle rule that keeps a few weeks of backups. Its cost is a few cents a month at today's size. Only the data host's role and the humans' SSO role can write it.
- **Telemetry:** backup and restore each report duration, size and result per database. They also leave log lines with the backup's id, never its contents.
- **What to watch:** start and teardown times as the data grows, and a backup whose restore fails.
- ADR 0003 stays Accepted. Its "No AWS data survives a teardown" consequence is amended by this ADR.
