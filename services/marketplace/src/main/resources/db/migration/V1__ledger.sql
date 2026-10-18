-- The coin ledger (design doc, section 8): double-entry, integer coins, append-only.
-- The service checks every rule below before it writes; Postgres checks them again, so a bug or
-- a hand-written statement can't break them. The reconciliation job is the third line.

CREATE TABLE accounts (
    account_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_type TEXT NOT NULL CHECK (owner_type IN ('user', 'system')),
    -- A user id from auth (a UUID; drills use ids like u_bob), or a system account's name.
    owner_id   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_type, owner_id),
    -- Lets balances reference (account_id, owner_type), so its copy of owner_type can't drift.
    UNIQUE (account_id, owner_type)
);

-- One row per account, locked by every posting that touches the account.
CREATE TABLE balances (
    account_id BIGINT PRIMARY KEY,
    owner_type TEXT NOT NULL,
    balance    BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (account_id, owner_type) REFERENCES accounts (account_id, owner_type),
    -- Users can't overdraw; system accounts (stripe_clearing, rewards_issuance) go negative by
    -- design: they are where coins come from.
    CONSTRAINT balances_user_not_negative CHECK (owner_type = 'system' OR balance >= 0)
);

CREATE TABLE ledger_transactions (
    tx_id           UUID PRIMARY KEY DEFAULT uuidv7(),
    type            TEXT NOT NULL CHECK (type IN ('reward', 'stripe_purchase', 'sale')),
    -- reward:<match_id>:<user_id>, stripe:<session_id>, ... A replay finds the existing row.
    idempotency_key TEXT NOT NULL UNIQUE,
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ledger_entries (
    entry_id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tx_id      UUID NOT NULL REFERENCES ledger_transactions (tx_id),
    account_id BIGINT NOT NULL REFERENCES accounts (account_id),
    -- Signed: positive credits the account, negative debits it.
    amount     BIGINT NOT NULL CHECK (amount <> 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A wallet's history, newest first, and the per-account sums of the reconciliation job.
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, entry_id);
CREATE INDEX ledger_entries_tx_idx ON ledger_entries (tx_id);

-- Every transaction has at least two entries and they sum to zero. Checked at COMMIT (a deferred
-- constraint trigger), because the entries are inserted one by one and only the finished set
-- has to balance. It fires for the transaction row too, so a transaction without entries fails.
CREATE FUNCTION ledger_check_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    total   NUMERIC;
    entries INT;
BEGIN
    SELECT COALESCE(SUM(amount), 0), COUNT(*) INTO total, entries
    FROM ledger_entries WHERE tx_id = NEW.tx_id;
    IF total <> 0 OR entries < 2 THEN
        RAISE EXCEPTION 'ledger transaction % is unbalanced: % entries summing to %',
            NEW.tx_id, entries, total
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();

CREATE CONSTRAINT TRIGGER ledger_transactions_balanced
    AFTER INSERT ON ledger_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();

-- The ledger is the audit trail: rows are never changed or removed. A mistake is fixed with a
-- new, reversing transaction. A trigger rather than REVOKE, because the service's user owns
-- these tables and an owner can grant itself the rights back.
CREATE FUNCTION ledger_refuse_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only (% refused)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER ledger_transactions_append_only
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_refuse_change();
CREATE TRIGGER ledger_transactions_no_truncate
    BEFORE TRUNCATE ON ledger_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();
CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_refuse_change();
CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();

-- The system accounts coins come from: Stripe coin packs and match rewards.
INSERT INTO accounts (owner_type, owner_id) VALUES
    ('system', 'stripe_clearing'),
    ('system', 'rewards_issuance');
INSERT INTO balances (account_id, owner_type)
SELECT account_id, owner_type FROM accounts;
