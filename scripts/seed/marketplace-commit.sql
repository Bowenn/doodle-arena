-- The end of the marketplace's seeded year (make seed). The reconciliation job closes months in
-- order from the ledger's oldest entry, and scans only the months after the last closed one
-- (LedgerMonths): it assumes nothing posts into the past, and only this seed does. So the
-- closed months from the first seeded one up to the last closed one, if the job has closed any,
-- are closed again here, in the same transaction as the seed's rows: both become visible at
-- once, reconciliation never sees one without the other, and the closed months stay one run
-- from the oldest entry with no gap. Their totals are derived data, sums of ledger entries.
-- Later months stay open; the job closes them on its next run, from complete partitions.

-- The deferred balance checks run now, not while the lock below is held.
SET CONSTRAINTS ALL IMMEDIATE;
-- Waits for a month the job is closing or checking right now, and holds off the next one until
-- the commit. A close that waited for it sums its month (the one after the last closed) after
-- the commit, seed included.
LOCK TABLE ledger_months_closed IN EXCLUSIVE MODE;
CREATE TEMP TABLE reclosed AS
SELECT CAST(m AS DATE) AS month
FROM generate_series(date_trunc('month', pg_temp.ts(0, 0)),
                     CAST((SELECT max(month) FROM ledger_months_closed) AS TIMESTAMPTZ), interval '1 month') m;
DELETE FROM ledger_month_totals WHERE month IN (SELECT month FROM reclosed);
DELETE FROM ledger_months_closed WHERE month IN (SELECT month FROM reclosed);
INSERT INTO ledger_months_closed (month, entries)
SELECT r.month, count(e.entry_id)
FROM reclosed r
LEFT JOIN ledger_entries e ON e.created_at >= r.month AND e.created_at < r.month + interval '1 month'
GROUP BY r.month;
INSERT INTO ledger_month_totals (month, account_id, total)
SELECT r.month, e.account_id, SUM(e.amount)
FROM reclosed r
JOIN ledger_entries e ON e.created_at >= r.month AND e.created_at < r.month + interval '1 month'
GROUP BY r.month, e.account_id;
DROP TABLE reclosed;
COMMIT;
