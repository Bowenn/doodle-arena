-- Before the marketplace's seeded year (make seed): its ledger partitions, then the one
-- transaction the twelve months run in (marketplace-month.sql, marketplace-commit.sql).
\set ON_ERROR_STOP on

-- The seed backdates its postings up to a year, and the marketplace only keeps partitions from
-- the current month ahead (LedgerMonths.ensurePartitions). Without these, every seeded entry
-- lands in ledger_entries_default, and its month can't get a partition any more. Created in
-- their own transaction: a new partition locks all of ledger_entries until the commit.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM ledger_entries_default
               WHERE created_at >= date_trunc('month', pg_temp.ts(0, 0))) THEN
        RAISE EXCEPTION 'ledger_entries_default holds rows in the seeded months: repair it first (docs/runbooks/ledger-partitions.md)';
    END IF;
END;
$$;
SELECT ledger_ensure_partitions(CAST(pg_temp.ts(0, 0) AS DATE), 3) AS "partitions created";

BEGIN;
