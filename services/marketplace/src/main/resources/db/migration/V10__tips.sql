-- Tips (task 6.3, ADR 0021): a tip moves the tipper's coins into one of 16 escrow accounts at
-- once; the round's RoundFinished then pays them to its drawers in one posting per batch, so
-- thousands of tips never queue on the drawer's balance row.

-- The escrow shards: system accounts, so no CHECK keeps them at or above zero. The reconciliation
-- job checks instead that each holds exactly its pending tips.
INSERT INTO accounts (owner_type, owner_id)
SELECT 'system', 'tips_escrow:' || shard FROM generate_series(0, 15) AS shard;
INSERT INTO balances (account_id, owner_type)
SELECT account_id, owner_type FROM accounts WHERE owner_type = 'system' AND owner_id LIKE 'tips\_escrow:%';

ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_type_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_type_check
    CHECK (type IN ('reward', 'stripe_purchase', 'sale', 'tip', 'tip_settlement', 'tip_refund'));
COMMENT ON COLUMN ledger_transactions.idempotency_key IS
    'reward:<match_id>, sale:<listing_id>, stripe:<session_id>, tip:<tip_id>, tip-settle:<match_id>:<round_no>:<n>, tip-refund:<tip_id>. A replay finds the existing row.';

-- A round the game server finished (RoundFinished): who drew it, so its tips can be paid. Only
-- settlers lock a row here, never tippers.
CREATE TABLE tip_rounds (
    match_id    TEXT NOT NULL,
    round_no    INT NOT NULL CHECK (round_no >= 1),
    room_id     TEXT NOT NULL,
    -- In drawing order; the first gets a split's remainder.
    drawer_ids  TEXT[] NOT NULL CHECK (cardinality(drawer_ids) BETWEEN 1 AND 2),
    -- How many settlements paid this round's tips: the next one's key is tip-settle:...:<n+1>.
    settlements INT NOT NULL DEFAULT 0,
    -- When the marketplace learned the round ended (its own clock): tips stop 30 s later.
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (match_id, round_no)
);

CREATE TABLE tips (
    tip_id        UUID PRIMARY KEY DEFAULT uuidv7(),
    tipper_id     TEXT NOT NULL,
    -- The round the tipper watched, as the client named it; RoundFinished decides who drew it.
    room_id       TEXT NOT NULL,
    match_id      TEXT NOT NULL,
    round_no      INT NOT NULL CHECK (round_no >= 1),
    amount        BIGINT NOT NULL CHECK (amount BETWEEN 1 AND 1000),
    -- Which tips_escrow account holds it.
    shard         SMALLINT NOT NULL CHECK (shard BETWEEN 0 AND 15),
    status        TEXT NOT NULL CHECK (status IN ('PENDING', 'SETTLED', 'REFUNDED')),
    -- The tip's own posting (tipper to escrow), and the one that closed it (escrow to the
    -- drawers, or back to the tipper).
    tx_id         UUID NOT NULL REFERENCES ledger_transactions (tx_id),
    closing_tx_id UUID REFERENCES ledger_transactions (tx_id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at     TIMESTAMPTZ,
    CONSTRAINT tips_status_shape CHECK (
        (status = 'PENDING' AND closing_tx_id IS NULL AND closed_at IS NULL)
        OR (status <> 'PENDING' AND closing_tx_id IS NOT NULL AND closed_at IS NOT NULL))
);

-- A round's pending tips (settlement), and the oldest pending ones (refunds, the TipsStuck alert).
CREATE INDEX tips_pending_round_idx ON tips (match_id, round_no) WHERE status = 'PENDING';
CREATE INDEX tips_pending_age_idx ON tips (created_at) WHERE status = 'PENDING';
