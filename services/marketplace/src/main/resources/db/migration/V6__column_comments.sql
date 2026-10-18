-- Corrections to comments in applied migrations, which can't be edited (Flyway checksums them).
-- COMMENT ON keeps the explanation with the schema, where psql's \d+ shows it.

-- V1 said reward:<match_id>:<user_id>; rewards are one transaction per match (D37).
COMMENT ON COLUMN ledger_transactions.idempotency_key IS
    'reward:<match_id>, sale:<listing_id>, stripe:<session_id>. A replay finds the existing row.';

-- V4 left out duplicate: a second Stripe event about a session already credited gets its own row.
COMMENT ON COLUMN stripe_events.result IS
    'processed, duplicate (another event for a session already credited), ignored or unknown_session. The same event id never gets a second row.';
