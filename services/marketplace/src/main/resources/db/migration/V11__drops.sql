-- Limited drops (task 6.8, ADR 0022): N numbered editions of an approved drawing, sold at a set
-- time and price. Redis gates the burst; these tables are the truth, and their constraints make
-- overselling impossible whatever Redis says.

CREATE TABLE drops (
    drop_id          UUID PRIMARY KEY DEFAULT uuidv7(),
    -- A minted (approved) drawing; its unique original keeps its owner.
    drawing_id       UUID NOT NULL REFERENCES ownership (drawing_id),
    editions         INT NOT NULL CHECK (editions BETWEEN 1 AND 1000),
    price            BIGINT NOT NULL CHECK (price BETWEEN 1 AND 100000),
    opens_at         TIMESTAMPTZ NOT NULL,
    closes_at        TIMESTAMPTZ NOT NULL,
    -- Waiting-room places admitted per second after the open.
    admit_per_second INT NOT NULL CHECK (admit_per_second BETWEEN 1 AND 10000),
    created_by       TEXT NOT NULL,
    -- How many settlements paid this drop's authors: the next one's key is drop-settle:...:<n+1>.
    settlements      INT NOT NULL DEFAULT 0,
    -- Set once the drop sold out or closed and every edition's coins reached the authors.
    settled_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (closes_at > opens_at),
    -- Lets drop_editions reference (drop_id, editions, price), so an edition's copies can't drift.
    UNIQUE (drop_id, editions, price)
);

CREATE INDEX drops_opens_idx ON drops (opens_at);
-- The sweeper's work: drops not yet settled.
CREATE INDEX drops_unsettled_idx ON drops (closes_at) WHERE settled_at IS NULL;

CREATE TABLE drop_editions (
    drop_id      UUID NOT NULL,
    edition_no   INT NOT NULL,
    -- Copies of the drop's supply and price, tied to it by the foreign key below: the CHECK on
    -- edition_no can then compare with the drop's supply in this row.
    editions     INT NOT NULL,
    price        BIGINT NOT NULL,
    buyer_id     TEXT NOT NULL,
    -- Which drop_escrow account holds its coins until the authors are paid.
    shard        SMALLINT NOT NULL CHECK (shard BETWEEN 0 AND 15),
    status       TEXT NOT NULL CHECK (status IN ('PAID', 'SETTLED')),
    -- The purchase's posting (buyer to escrow), and the one that paid the authors.
    tx_id        UUID NOT NULL REFERENCES ledger_transactions (tx_id),
    settle_tx_id UUID REFERENCES ledger_transactions (tx_id),
    purchased_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at   TIMESTAMPTZ,
    -- Never two buyers for one edition, never two editions for one buyer, never past the supply.
    PRIMARY KEY (drop_id, edition_no),
    UNIQUE (drop_id, buyer_id),
    CHECK (edition_no BETWEEN 1 AND editions),
    FOREIGN KEY (drop_id, editions, price) REFERENCES drops (drop_id, editions, price),
    CONSTRAINT drop_editions_status_shape CHECK (
        (status = 'PAID' AND settle_tx_id IS NULL AND settled_at IS NULL)
        OR (status = 'SETTLED' AND settle_tx_id IS NOT NULL AND settled_at IS NOT NULL))
);

-- A buyer's editions, newest first; a drop's editions still in escrow.
CREATE INDEX drop_editions_buyer_idx ON drop_editions (buyer_id, purchased_at DESC);
CREATE INDEX drop_editions_paid_idx ON drop_editions (drop_id) WHERE status = 'PAID';

-- The escrow shards: system accounts, checked by the reconciliation job against the editions
-- they hold (ADR 0022), like tips_escrow (V10).
INSERT INTO accounts (owner_type, owner_id)
SELECT 'system', 'drop_escrow:' || shard FROM generate_series(0, 15) AS shard;
INSERT INTO balances (account_id, owner_type)
SELECT account_id, owner_type FROM accounts WHERE owner_type = 'system' AND owner_id LIKE 'drop\_escrow:%';

ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_type_check;
ALTER TABLE ledger_transactions ADD CONSTRAINT ledger_transactions_type_check
    CHECK (type IN ('reward', 'stripe_purchase', 'sale', 'tip', 'tip_settlement', 'tip_refund',
                    'drop_purchase', 'drop_settlement'));
COMMENT ON COLUMN ledger_transactions.idempotency_key IS
    'reward:<match_id>, sale:<listing_id>, stripe:<session_id>, tip:<tip_id>, tip-settle:<match_id>:<round_no>:<n>, tip-refund:<tip_id>, drop:<drop_id>:<buyer_id>, drop-settle:<drop_id>:<n>. A replay finds the existing row.';
