-- Listings, purchases' idempotency keys and the transactional outbox (design doc, section 8).

-- A drawing for sale. Only its owner lists it, at most one listing is active per drawing, and a
-- listing closes once: SOLD by a purchase or CANCELLED by its seller.
CREATE TABLE listings (
    listing_id UUID PRIMARY KEY DEFAULT uuidv7(),
    drawing_id UUID NOT NULL REFERENCES ownership (drawing_id),
    seller_id  TEXT NOT NULL,
    -- Integer coins. The cap keeps every sum far from BIGINT overflow.
    price      BIGINT NOT NULL CHECK (price BETWEEN 1 AND 1000000),
    status     TEXT NOT NULL CHECK (status IN ('ACTIVE', 'SOLD', 'CANCELLED')),
    buyer_id   TEXT,
    -- The ledger transaction that paid for it.
    sale_id    UUID REFERENCES ledger_transactions (tx_id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at  TIMESTAMPTZ,
    CONSTRAINT listings_status_shape CHECK (
        (status = 'ACTIVE' AND buyer_id IS NULL AND sale_id IS NULL AND closed_at IS NULL)
        OR (status = 'SOLD' AND buyer_id IS NOT NULL AND sale_id IS NOT NULL AND closed_at IS NOT NULL)
        OR (status = 'CANCELLED' AND buyer_id IS NULL AND sale_id IS NULL AND closed_at IS NOT NULL))
);

CREATE UNIQUE INDEX listings_one_active ON listings (drawing_id) WHERE status = 'ACTIVE';
-- The market page: active listings, newest first (UUIDv7 ids sort by time).
CREATE INDEX listings_active_idx ON listings (listing_id DESC) WHERE status = 'ACTIVE';
CREATE INDEX listings_seller_idx ON listings (seller_id, listing_id DESC) WHERE status = 'ACTIVE';

-- Idempotency-Key handling (ADR 0012). A request claims its key in one transaction and completes
-- it in the transaction that does the work; the claim is a lease, so a crashed request's key can
-- be taken over once it expires, and claim_token fences the completion.
CREATE TABLE idempotency_keys (
    user_id       TEXT NOT NULL,
    key           TEXT NOT NULL,
    operation     TEXT NOT NULL,
    -- SHA-256 of method, path and body: the same key with another request is refused.
    request_hash  TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('IN_PROGRESS', 'DONE')),
    claim_token   UUID NOT NULL,
    locked_until  TIMESTAMPTZ NOT NULL,
    response_code INT,
    -- The exact JSON text sent, so a replay is byte for byte the first answer (JSONB would reformat it).
    response_body TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at  TIMESTAMPTZ,
    PRIMARY KEY (user_id, key),
    CONSTRAINT idempotency_keys_done_shape CHECK (
        (status = 'IN_PROGRESS' AND response_code IS NULL)
        OR (status = 'DONE' AND response_code IS NOT NULL AND completed_at IS NOT NULL))
);

-- Kept 24 hours, then deleted by the cleanup job.
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

-- Events written in the same transaction as the change they describe; the relay publishes them.
CREATE TABLE outbox (
    -- Also the event's meta.event_id, so a resend after a crash is a duplicate downstream.
    event_id     UUID PRIMARY KEY,
    topic        TEXT NOT NULL,
    key          TEXT NOT NULL,
    payload      BYTEA NOT NULL,
    -- The W3C trace context of the transaction that wrote it, sent as the Kafka traceparent header.
    traceparent  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);

CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
