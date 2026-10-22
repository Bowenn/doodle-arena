-- Task 6.6: ledger_entries partitioned by month on created_at (design doc, section 8). Old months
-- stop changing, so the reconciliation job keeps their per-account totals and scans only the
-- open months (V9), and a month can be moved or archived as one table.
--
-- The table is rebuilt: Postgres can't partition an existing table in place. Everything the
-- ledger promises is recreated on the new one: the same columns and checks, the foreign keys,
-- the indexes, the deferred "sums to zero" trigger and the append-only triggers. The rows are
-- copied before the triggers exist: they were checked when they were written, and checking
-- them again would only take time. The identity goes on from the old one, so entry ids keep
-- growing.

ALTER TABLE ledger_entries RENAME TO ledger_entries_unpartitioned;

CREATE TABLE ledger_entries (
    entry_id   BIGINT GENERATED ALWAYS AS IDENTITY,
    tx_id      UUID NOT NULL REFERENCES ledger_transactions (tx_id),
    account_id BIGINT NOT NULL REFERENCES accounts (account_id),
    -- Signed: positive credits the account, negative debits it.
    amount     BIGINT NOT NULL CHECK (amount <> 0),
    -- The partition key: when the posting's transaction started (now()).
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A partitioned table's unique keys must include the partition key.
    PRIMARY KEY (entry_id, created_at)
) PARTITION BY RANGE (created_at);

-- Anything outside the monthly partitions lands here; it should stay empty (ledger_ensure_partitions
-- creates months ahead, and market.ledger.default_partition_rows watches it).
CREATE TABLE ledger_entries_default PARTITION OF ledger_entries DEFAULT;

-- Creates the monthly partitions from the month of from_month up to months_ahead months after
-- the current one, each with the truncate guard (a statement trigger on the parent doesn't
-- fire for a TRUNCATE of one partition; row triggers are cloned to partitions on their own).
-- Idempotent; the marketplace runs it daily (LedgerPartitions).
CREATE FUNCTION ledger_ensure_partitions(from_month DATE, months_ahead INT) RETURNS INT
LANGUAGE plpgsql AS $$
DECLARE
    m       DATE := date_trunc('month', from_month)::date;
    last    DATE := (date_trunc('month', now()) + make_interval(months => months_ahead))::date;
    name    TEXT;
    created INT := 0;
BEGIN
    WHILE m <= last LOOP
        name := 'ledger_entries_' || to_char(m, 'YYYY_MM');
        IF to_regclass(name) IS NULL THEN
            EXECUTE format('CREATE TABLE %I PARTITION OF ledger_entries FOR VALUES FROM (%L) TO (%L)',
                name, m, (m + interval '1 month')::date);
            EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change()',
                name || '_no_truncate', name);
            created := created + 1;
        END IF;
        m := (m + interval '1 month')::date;
    END LOOP;
    RETURN created;
END;
$$;

SELECT ledger_ensure_partitions(
    COALESCE((SELECT min(created_at) FROM ledger_entries_unpartitioned), now())::date, 3);

INSERT INTO ledger_entries (entry_id, tx_id, account_id, amount, created_at) OVERRIDING SYSTEM VALUE
SELECT entry_id, tx_id, account_id, amount, created_at FROM ledger_entries_unpartitioned ORDER BY entry_id;

SELECT setval(pg_get_serial_sequence('ledger_entries', 'entry_id'),
    COALESCE((SELECT max(entry_id) FROM ledger_entries), 0) + 1, false);

DROP TABLE ledger_entries_unpartitioned;

-- A wallet's history, newest first, and the per-account sums of the reconciliation job (on
-- every partition).
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, entry_id);
CREATE INDEX ledger_entries_tx_idx ON ledger_entries (tx_id);

-- The balance check reads a transaction's entries by tx_id AND created_at. A posting writes its
-- transaction row and its entries in one database transaction, so they share created_at
-- (now() is the database transaction's start): the condition finds the same rows as before,
-- and it lets Postgres read one partition instead of probing every month's index. It also makes
-- a rule of what the incremental reconciliation relies on: no ledger transaction spans two
-- months, so every month's entries sum to zero on their own.
CREATE OR REPLACE FUNCTION ledger_check_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    total   NUMERIC;
    entries INT;
BEGIN
    SELECT COALESCE(SUM(amount), 0), COUNT(*) INTO total, entries
    FROM ledger_entries WHERE tx_id = NEW.tx_id AND created_at = NEW.created_at;
    IF total <> 0 OR entries < 2 THEN
        RAISE EXCEPTION 'ledger transaction % is unbalanced: % entries summing to % at %',
            NEW.tx_id, entries, total, NEW.created_at
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();
CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_refuse_change();
CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();
CREATE TRIGGER ledger_entries_default_no_truncate
    BEFORE TRUNCATE ON ledger_entries_default
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();

COMMENT ON TABLE ledger_entries IS 'Double-entry ledger lines, partitioned by month on created_at (task 6.6). Append-only; each transaction''s entries sum to zero.';
