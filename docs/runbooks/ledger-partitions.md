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
- **Queries can't skip those rows:** partition pruning can't leave out the default partition, so every scan of a month, reconciliation's included, reads all of it.

The marketplace creates partitions three months ahead, at startup and daily at 03:15 UTC (`LedgerMonths.ensurePartitions`, `ledger_ensure_partitions`). So this means that job hasn't run for months, or postings have a `created_at` outside them: a hand-written statement, a bad clock, or `make seed` from before the fix of 2026-10-23, which backdated a year of postings without creating their partitions.

That old seed usually also leaves reconciliation reporting "invariants broken" (every seeded account in `mismatched_accounts`) and "closed ledger month changed", so LedgerImbalance fires too. The job closes a month a day after it ends, assuming nothing posts into the past. It closed the seeded months while the seed was still writing into them, so their stored totals miss most of the seeded rows. Fix 2 below rebuilds them.

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

3. Is a clock wrong? Look at the rows' `created_at` against `now()`. Are they seeded? `make seed`'s ledger transactions carry `metadata` `{"seed": true}`.

4. Are closed months stale too? Compare each closed month's entry count when it closed with now, and how many of its entries are seeded:

   ```sql
   SELECT c.month, c.closed_at, c.entries AS entries_at_close,
          count(e.entry_id) AS entries_now,
          count(e.entry_id) FILTER (WHERE t.metadata->>'seed' = 'true') AS seeded_now
   FROM ledger_months_closed c
   LEFT JOIN ledger_entries e ON e.created_at >= c.month AND e.created_at < c.month + interval '1 month'
   LEFT JOIN ledger_transactions t ON t.tx_id = e.tx_id
   GROUP BY c.month, c.closed_at, c.entries ORDER BY c.month;
   ```

   The old seed explains a month when `entries_now` is greater than `entries_at_close` by no more than `seeded_now`, and `closed_at` falls while the seed ran. The triggers refuse updates and deletes, so entries can only have been added.

## Fix 1: move the rows into monthly partitions

One transaction: detach the default partition, create every month from its oldest row, and copy its rows back through the parent, which routes each into its month (or into the new default, if it is still outside every partition). The ledger never misses a row, and the balance trigger checks every copied transaction again. Detaching locks `ledger_entries`, so postings wait until the commit: about a minute for 1.7 million rows.

```sql
BEGIN;
ALTER TABLE ledger_entries DETACH PARTITION ledger_entries_default;
ALTER TABLE ledger_entries_default RENAME TO ledger_entries_default_old;
SELECT ledger_ensure_partitions(CAST((SELECT min(created_at) FROM ledger_entries_default_old) AS DATE), 3);
CREATE TABLE ledger_entries_default PARTITION OF ledger_entries DEFAULT;
CREATE TRIGGER ledger_entries_default_no_truncate BEFORE TRUNCATE ON ledger_entries_default
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();
-- Copy, don't move: the old default's rows can't be deleted (the append-only trigger), and it
-- is dropped once they're copied.
INSERT INTO ledger_entries (entry_id, tx_id, account_id, amount, created_at) OVERRIDING SYSTEM VALUE
    SELECT entry_id, tx_id, account_id, amount, created_at FROM ledger_entries_default_old;
DROP TABLE ledger_entries_default_old;
-- The balance checks of the copied rows run here; COMMIT would run them too.
SET CONSTRAINTS ALL IMMEDIATE;
SELECT (SELECT count(*) FROM ledger_entries_default) AS still_in_default,
       (SELECT COALESCE(SUM(amount), 0) FROM ledger_entries) AS ledger_sum;
COMMIT;
```

`still_in_default` should be 0 (unless rows lie beyond three months ahead: a clock to fix, see Afterwards) and `ledger_sum` must be 0. Rehearsed on the GCP dev box on 2026-10-23 with the 1,693,750 rows of an old `make seed`.

## Fix 2: rebuild stale closed months

Only when Check 4 shows that rows were added to closed months and the seed explains every one of them. A closed month that changed for any other reason is LedgerImbalance's case (`docs/runbooks/ledger-imbalance.md`): something wrote behind the triggers' back. Rebuilding its totals would hide that, so stop there.

The totals are derived data, sums of entries. Rebuild every closed month from the first stale one up to the last closed one, so the closed months stay one unbroken run from the ledger's oldest entry (reconciliation only scans the months after the last closed one). `make seed` ends with the same statements (`scripts/seed/marketplace-commit.sql`). Put the first stale month from Check 4 in place of `2025-10-01`:

```sql
BEGIN;
-- Waits for a month the job is closing or checking, and holds off the next one until the commit.
LOCK TABLE ledger_months_closed IN EXCLUSIVE MODE;
CREATE TEMP TABLE reclosed ON COMMIT DROP AS
SELECT CAST(m AS DATE) AS month
FROM generate_series(TIMESTAMPTZ '2025-10-01',
                     CAST((SELECT max(month) FROM ledger_months_closed) AS TIMESTAMPTZ), interval '1 month') m;
DELETE FROM ledger_month_totals WHERE month IN (SELECT month FROM reclosed);
DELETE FROM ledger_months_closed WHERE month IN (SELECT month FROM reclosed);
INSERT INTO ledger_months_closed (month, entries)
SELECT r.month, count(e.entry_id) FROM reclosed r
LEFT JOIN ledger_entries e ON e.created_at >= r.month AND e.created_at < r.month + interval '1 month'
GROUP BY r.month;
INSERT INTO ledger_month_totals (month, account_id, total)
SELECT r.month, e.account_id, SUM(e.amount) FROM reclosed r
JOIN ledger_entries e ON e.created_at >= r.month AND e.created_at < r.month + interval '1 month'
GROUP BY r.month, e.account_id;
COMMIT;
```

## Then

Make sure the partitions run ahead again: restart the marketplace, or run `SELECT ledger_ensure_partitions(CAST(now() AS DATE), 3);`. Watch the next reconciliation run, every minute: it must log "ledger reconciled" with imbalance and mismatched accounts at 0, and no "closed ledger month changed". The re-check goes through one closed month per run, so the rebuilt months are all checked again within as many minutes as there are closed months.

## Afterwards

- Find out why the partition job didn't run, and record it.
- If a bad `created_at` caused it, fix where it came from: postings take `now()`, never a client's time.
- `make seed` creates the partitions of its backdated year first, and commits the year in one transaction with the closed months it writes into, so it no longer causes this.
