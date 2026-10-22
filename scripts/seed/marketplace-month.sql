-- One month of the marketplace's seed (make seed, task 6.6), as the marketplace role, in ONE
-- transaction: that month's rewards, mints, sales and new listings, and the balances they move.
-- Every rule the service and Postgres enforce holds at the commit: each ledger transaction has
-- two or more entries summing to zero (the deferred trigger checks), balances equal their
-- entries, no user goes below zero, and every drawing's ownership, provenance and listings agree,
-- so the reconciliation job reads 0 between months too. Expects psql variable :month (0..11).
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL seed.month = :'month';

-- Every seed user's account in month 0, with a balance of 0 (no entries yet).
INSERT INTO accounts (owner_type, owner_id)
SELECT 'user', pg_temp.user_id(u) FROM generate_series(0, pg_temp.n_users() - 1) u
WHERE current_setting('seed.month')::int = 0;
INSERT INTO balances (account_id, owner_type)
SELECT a.account_id, 'user' FROM accounts a
WHERE current_setting('seed.month')::int = 0 AND a.owner_type = 'user' AND a.owner_id LIKE 'seed-u-%'
  AND NOT EXISTS (SELECT 1 FROM balances b WHERE b.account_id = a.account_id);

CREATE TEMP TABLE acct ON COMMIT DROP AS
SELECT owner_id, account_id FROM accounts WHERE owner_type = 'user' AND owner_id LIKE 'seed-u-%';
CREATE UNIQUE INDEX ON acct (owner_id);
CREATE TEMP TABLE batch_entries (tx_id uuid, account_id bigint, amount bigint, created_at timestamptz) ON COMMIT DROP;

-- Rewards: the welcome rewards in month 0, the rest spread over the year.
CREATE TEMP TABLE rewards ON COMMIT DROP AS
SELECT r, pg_temp.user_id(r) AS user_id,
       CASE WHEN r < pg_temp.n_users() THEN 100 ELSE 30 END AS amount,
       pg_temp.ts(current_setting('seed.month')::int, r) AS t
FROM generate_series(0, pg_temp.n_rewards() - 1) r
WHERE (CASE WHEN r < pg_temp.n_users() THEN 0 ELSE r % 12 END) = current_setting('seed.month')::int;
ALTER TABLE rewards ADD COLUMN tx_id uuid;
UPDATE rewards SET tx_id = pg_temp.seed_uuid(t, 'reward', r);
INSERT INTO ledger_transactions (tx_id, type, idempotency_key, metadata, created_at)
SELECT tx_id, 'reward', 'reward:seed-m-' || r, '{"seed": true}', t FROM rewards;
INSERT INTO batch_entries
SELECT tx_id, (SELECT account_id FROM accounts WHERE owner_type = 'system' AND owner_id = 'rewards_issuance'), -amount, t FROM rewards
UNION ALL
SELECT r.tx_id, a.account_id, r.amount, r.t FROM rewards r JOIN acct a ON a.owner_id = r.user_id;

-- Mints: drawings whose mint month is this one, each with its author as first owner.
CREATE TEMP TABLE mints ON COMMIT DROP AS
SELECT d, pg_temp.drawing_id(d) AS drawing_id, pg_temp.author(d) AS author,
       pg_temp.ts(pg_temp.mint_month(d), d) AS t
FROM generate_series(0, pg_temp.n_drawings() - 1) d
WHERE pg_temp.mint_month(d) = current_setting('seed.month')::int;
INSERT INTO ownership (drawing_id, owner_id, version, minted_at, updated_at)
SELECT drawing_id, author, 1, t, t FROM mints;
INSERT INTO drawing_authors (drawing_id, user_id, author_order)
SELECT drawing_id, author, 0 FROM mints;
INSERT INTO ownership_events (drawing_id, kind, to_owner, source_event_id, created_at)
SELECT drawing_id, 'MINT', author, 'seed-approved-' || d, t FROM mints ORDER BY t;

-- New listings: unsold drawings are listed a day after their mint and stay active.
INSERT INTO listings (listing_id, drawing_id, seller_id, price, status, created_at)
SELECT pg_temp.seed_uuid(t + interval '1 day', 'listing', d), drawing_id, author, 5 + d % 96, 'ACTIVE', t + interval '1 day'
FROM mints WHERE d >= pg_temp.n_sold() AND d < pg_temp.n_sold() + pg_temp.n_listed();

-- Sales: sold drawings change hands 3 and 6 months after their mint. Sale k of drawing d goes
-- from its owner to buyer(d, k); the price is 5..20 coins, and the author gets a 10% royalty
-- when they are neither the seller nor the buyer (design doc, section 8).
CREATE TEMP TABLE sales ON COMMIT DROP AS
SELECT d, k, pg_temp.drawing_id(d) AS drawing_id, pg_temp.author(d) AS author,
       pg_temp.ts(pg_temp.mint_month(d) + 3 * k, d) AS t, 5 + (d * k) % 16 AS price
FROM generate_series(0, pg_temp.n_sold() - 1) d, generate_series(1, 2) k
WHERE pg_temp.mint_month(d) + 3 * k = current_setting('seed.month')::int;
ALTER TABLE sales ADD COLUMN seller text, ADD COLUMN buyer text, ADD COLUMN listing_id uuid,
    ADD COLUMN tx_id uuid, ADD COLUMN royalty bigint;
-- The owner before sale k is the buyer of sale k-1 (or the author).
UPDATE sales s SET seller = o.owner_id FROM ownership o WHERE o.drawing_id = s.drawing_id;
UPDATE sales SET buyer = CASE WHEN pg_temp.user_id(d * (6 * k + 1) + k) = seller
                              THEN pg_temp.user_id(d * (6 * k + 1) + k + 1)
                              ELSE pg_temp.user_id(d * (6 * k + 1) + k) END;
UPDATE sales SET listing_id = pg_temp.seed_uuid(t - interval '1 hour', 'listing-sold', d * 2 + k),
                 tx_id = pg_temp.seed_uuid(t, 'sale', d * 2 + k),
                 royalty = CASE WHEN author <> seller AND author <> buyer THEN price * 10 / 100 ELSE 0 END;
INSERT INTO ledger_transactions (tx_id, type, idempotency_key, metadata, created_at)
SELECT tx_id, 'sale', 'sale:' || listing_id, '{"seed": true}', t FROM sales;
INSERT INTO batch_entries
SELECT s.tx_id, a.account_id, -s.price, s.t FROM sales s JOIN acct a ON a.owner_id = s.buyer
UNION ALL
SELECT s.tx_id, a.account_id, s.price - s.royalty, s.t FROM sales s JOIN acct a ON a.owner_id = s.seller
UNION ALL
SELECT s.tx_id, a.account_id, s.royalty, s.t FROM sales s JOIN acct a ON a.owner_id = s.author WHERE s.royalty > 0;
INSERT INTO listings (listing_id, drawing_id, seller_id, price, status, buyer_id, sale_id, created_at, closed_at)
SELECT listing_id, drawing_id, seller, price, 'SOLD', buyer, tx_id, t - interval '1 hour', t FROM sales;
UPDATE ownership o SET owner_id = s.buyer, version = o.version + 1, updated_at = s.t
FROM sales s WHERE s.drawing_id = o.drawing_id;
INSERT INTO ownership_events (drawing_id, kind, from_owner, to_owner, sale_id, source_event_id, created_at)
SELECT drawing_id, 'SALE', seller, buyer, tx_id, listing_id::text, t FROM sales ORDER BY t;

-- The entries, in time order, and the balances they move, in the same transaction.
INSERT INTO ledger_entries (tx_id, account_id, amount, created_at)
SELECT tx_id, account_id, amount, created_at FROM batch_entries ORDER BY created_at, tx_id;
UPDATE balances b SET balance = b.balance + e.delta, updated_at = now()
FROM (SELECT account_id, SUM(amount) AS delta FROM batch_entries GROUP BY account_id) e
WHERE b.account_id = e.account_id;

COMMIT;
