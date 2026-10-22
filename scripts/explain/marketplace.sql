-- make explain (task 6.6): the marketplace's queries as the service sends them, with
-- parameters taken from the seeded data. Each "### name" block is run and timed by
-- scripts/explain.sh. Queries with optional filters ("CAST(:x AS ...) IS NULL OR ...") are also
-- shown as generic plans: after five executions the JDBC driver prepares them on the server,
-- and Postgres may then plan once for any parameters.
\set ON_ERROR_STOP on
SELECT 'seed-u-42' AS usr \gset
SELECT drawing_id AS drawing FROM ownership WHERE version = 3 ORDER BY drawing_id LIMIT 1 \gset
SELECT seller_id AS seller FROM listings WHERE status = 'ACTIVE' ORDER BY listing_id LIMIT 1 \gset

\echo '### wallet_balance'
EXPLAIN (ANALYZE, BUFFERS) SELECT b.balance, b.updated_at FROM accounts a JOIN balances b USING (account_id, owner_type)
WHERE a.owner_type = 'user' AND a.owner_id = :'usr';

\echo '### wallet_transactions'
EXPLAIN (ANALYZE, BUFFERS) SELECT e.entry_id, e.tx_id, t.type, e.amount, t.created_at
FROM accounts a JOIN ledger_entries e ON e.account_id = a.account_id JOIN ledger_transactions t ON t.tx_id = e.tx_id
WHERE a.owner_type = 'user' AND a.owner_id = :'usr' ORDER BY e.entry_id DESC LIMIT 21;

\echo '### listings_browse'
EXPLAIN (ANALYZE, BUFFERS) SELECT listing_id, drawing_id, seller_id, price, status, buyer_id, sale_id, created_at, closed_at
FROM listings WHERE status = 'ACTIVE' ORDER BY listing_id DESC LIMIT 21;

-- One seller's active listings, as ListingService.active sends it since task 6.6 (only the
-- filters given are in the SQL), as a custom and as a generic plan. Before task 6.6 the
-- statement held every filter as "CAST($n AS ...) IS NULL OR column = $n", and its generic plan
-- read all 50,000 active listings (48 ms).
PREPARE listings_by_seller(text, int) AS
SELECT listing_id, drawing_id, seller_id, price, status, buyer_id, sale_id, created_at, closed_at FROM listings
WHERE status = 'ACTIVE' AND seller_id = $1 ORDER BY listing_id DESC LIMIT $2;
SET plan_cache_mode = force_custom_plan;
\echo '### listings_by_seller'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE listings_by_seller(:'seller', 21);
SET plan_cache_mode = force_generic_plan;
\echo '### listings_by_seller_generic'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE listings_by_seller(:'seller', 21);
RESET plan_cache_mode;

\echo '### ownership_history'
EXPLAIN (ANALYZE, BUFFERS) SELECT e.kind, e.from_owner, e.to_owner, e.sale_id, l.price, e.created_at
FROM ownership_events e LEFT JOIN listings l ON l.sale_id = e.sale_id WHERE e.drawing_id = :'drawing' ORDER BY e.event_id;

\echo '### owned_drawings'
EXPLAIN (ANALYZE, BUFFERS) SELECT o.drawing_id, o.version, o.minted_at, o.updated_at, l.listing_id, l.price, l.created_at AS listed_at
FROM ownership o LEFT JOIN listings l ON l.drawing_id = o.drawing_id AND l.status = 'ACTIVE'
WHERE o.owner_id = :'usr' ORDER BY o.updated_at DESC, o.drawing_id DESC LIMIT 21;

\echo '### reconcile_imbalance'
EXPLAIN (ANALYZE, BUFFERS) SELECT COALESCE(SUM(amount), 0) FROM ledger_entries;

\echo '### reconcile_accounts'
EXPLAIN (ANALYZE, BUFFERS) SELECT a.account_id FROM accounts a LEFT JOIN balances b USING (account_id)
LEFT JOIN (SELECT account_id, SUM(amount) AS total FROM ledger_entries GROUP BY account_id) e USING (account_id)
WHERE b.account_id IS NULL OR b.balance <> COALESCE(e.total, 0) OR (a.owner_type = 'user' AND b.balance < 0)
ORDER BY a.account_id;

\echo '### reconcile_ownership'
EXPLAIN (ANALYZE, BUFFERS) WITH ev AS (
  SELECT drawing_id, COUNT(*) AS events, COUNT(*) FILTER (WHERE kind = 'MINT') AS mints,
         (ARRAY_AGG(to_owner ORDER BY event_id DESC))[1] AS last_owner
  FROM ownership_events GROUP BY drawing_id)
SELECT o.drawing_id FROM ownership o LEFT JOIN ev USING (drawing_id)
WHERE ev.drawing_id IS NULL OR ev.mints <> 1 OR ev.events <> o.version OR ev.last_owner <> o.owner_id
   OR NOT EXISTS (SELECT 1 FROM drawing_authors a WHERE a.drawing_id = o.drawing_id)
UNION SELECT ev.drawing_id FROM ev WHERE NOT EXISTS (SELECT 1 FROM ownership o WHERE o.drawing_id = ev.drawing_id)
UNION SELECT a.drawing_id FROM drawing_authors a WHERE NOT EXISTS (SELECT 1 FROM ownership o WHERE o.drawing_id = a.drawing_id)
ORDER BY 1;

\echo '### outbox_relay_pick'
EXPLAIN (ANALYZE, BUFFERS) SELECT event_id, topic, key, payload, traceparent FROM outbox
WHERE published_at IS NULL ORDER BY created_at LIMIT 100 FOR UPDATE SKIP LOCKED;

\echo '### outbox_retention'
BEGIN;
EXPLAIN (ANALYZE, BUFFERS) DELETE FROM outbox WHERE published_at < now() - make_interval(secs => 604800);
ROLLBACK;
