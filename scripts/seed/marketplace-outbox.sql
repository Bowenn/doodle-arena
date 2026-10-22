-- The marketplace outbox as a busy week leaves it (make seed, task 6.6): published events of
-- the last 7 days, which the relay's retention keeps (app.outbox.retention) and scans to delete.
\set ON_ERROR_STOP on
INSERT INTO outbox (event_id, topic, key, payload, created_at, published_at)
SELECT pg_temp.seed_uuid(t, 'outbox', n), CASE WHEN n % 3 = 0 THEN 'market.sales.v1' ELSE 'market.wallet.v1' END,
       pg_temp.user_id(n), '\x00'::bytea, t, t + interval '1 second'
FROM (SELECT n, now() - interval '7 days' + (n % 600000) * interval '1 second' AS t
      FROM generate_series(0, pg_temp.n_outbox() - 1) n) x;
