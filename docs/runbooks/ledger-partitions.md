# Runbook: LedgerPartitionMissing

- Severity: warning
- Dashboard: Doodle Arena: marketplace (`da-marketplace`), "Ledger entries outside the monthly partitions"

## What it means

`ledger_entries` is partitioned by month (task 6.6, migration V8). Rows whose month has no partition land in `ledger_entries_default`, and some are there now.

Money is safe:

- Those postings are complete, balanced and counted by the reconciliation job like any other.
- The ledger's triggers apply to the default partition as well.

What breaks:

- **That month's partition can't be created any more** while its rows sit in the default partition. Postgres refuses ("updated partition constraint for default partition would be violated").
- **Reconciliation keeps rescanning those rows:** a month can't be closed into stored totals while it has no partition of its own.

The marketplace creates partitions three months ahead, at startup and daily at 03:15 UTC (`LedgerMonths.ensurePartitions`, `ledger_ensure_partitions`). So this means that job hasn't run for months, or a posting has a `created_at` far outside them (a hand-written statement, a bad clock).

## Check

1. Which months? As the marketplace role:

   ```bash
   docker compose -f infra/docker-compose.yml --env-file .env exec postgres psql -U marketplace -d marketplace -c "SELECT date_trunc('month', created_at) AS month, count(*) FROM ledger_entries_default GROUP BY 1 ORDER BY 1"
   ```

2. Did the partition job run? `make logs s=marketplace` and look for "ledger partitions created" or its errors. List the partitions:

   ```sql
   SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
   WHERE i.inhparent = 'ledger_entries'::regclass ORDER BY 1;
   ```

3. Is a clock wrong? Look at the rows' `created_at` against `now()`.

## Fix

Move the rows to their own partition, in one transaction, so the ledger never misses a row. The steps were rehearsed on a copy of the schema (task 6.6): the rows moved, the sum stayed 0, and the new default refuses a `DELETE` and a `TRUNCATE` like the old one. Take the month from Check 1, then:

```sql
BEGIN;
ALTER TABLE ledger_entries DETACH PARTITION ledger_entries_default;
CREATE TABLE ledger_entries_2027_05 PARTITION OF ledger_entries FOR VALUES FROM ('2027-05-01') TO ('2027-06-01');
CREATE TRIGGER ledger_entries_2027_05_no_truncate BEFORE TRUNCATE ON ledger_entries_2027_05
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();
-- Copy, don't move: the default's rows can't be deleted (the append-only trigger), and the
-- detached default is dropped below.
INSERT INTO ledger_entries_2027_05 (entry_id, tx_id, account_id, amount, created_at) OVERRIDING SYSTEM VALUE
    SELECT entry_id, tx_id, account_id, amount, created_at FROM ledger_entries_default
    WHERE created_at >= '2027-05-01' AND created_at < '2027-06-01';
-- Every other row goes back through the parent (into the new default below).
CREATE TABLE ledger_entries_default_new PARTITION OF ledger_entries DEFAULT;
INSERT INTO ledger_entries (entry_id, tx_id, account_id, amount, created_at) OVERRIDING SYSTEM VALUE
    SELECT entry_id, tx_id, account_id, amount, created_at FROM ledger_entries_default
    WHERE NOT (created_at >= '2027-05-01' AND created_at < '2027-06-01');
DROP TABLE ledger_entries_default;
ALTER TABLE ledger_entries_default_new RENAME TO ledger_entries_default;
CREATE TRIGGER ledger_entries_default_no_truncate BEFORE TRUNCATE ON ledger_entries_default
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();
COMMIT;
```

Then make sure the partitions run ahead again: restart the marketplace, or run `SELECT ledger_ensure_partitions(CAST(now() AS DATE), 3);`. Watch the next reconciliation run: imbalance and mismatched accounts must stay 0.

## Afterwards

- Find out why the partition job didn't run, and record it.
- If a bad `created_at` caused it, fix where it came from: postings take `now()`, never a client's time.
