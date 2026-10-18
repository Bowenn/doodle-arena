-- Stripe coin packs (design doc, sections 7.7 and 8).

-- Every Checkout Session we created: what was sold to whom. The webhook credits the coins
-- recorded here, never an amount taken from the event.
CREATE TABLE stripe_checkouts (
    session_id   TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    pack_id      TEXT NOT NULL,
    coins        BIGINT NOT NULL CHECK (coins > 0),
    amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
    currency     TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every verified webhook event, once: a redelivery finds its row and changes nothing.
CREATE TABLE stripe_events (
    stripe_event_id TEXT PRIMARY KEY,
    type            TEXT NOT NULL,
    session_id      TEXT,
    -- processed, ignored or unknown_session (duplicates never get a second row).
    result          TEXT NOT NULL,
    processed_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
