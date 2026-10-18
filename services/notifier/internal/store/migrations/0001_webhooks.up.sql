-- Webhook endpoints and deliveries (design doc, section 4.7; ADR 0014).

CREATE TABLE endpoints (
    id                UUID PRIMARY KEY,
    -- The user whose API key registered it; it receives only that user's events.
    owner_id          TEXT NOT NULL,
    api_key_id        UUID NOT NULL,
    url               TEXT NOT NULL CHECK (char_length(url) BETWEEN 1 AND 2048),
    event_types       TEXT[] NOT NULL CHECK (cardinality(event_types) > 0),
    -- The signing secret, AES-256-GCM encrypted with NOTIFIER_SECRET_KEY; the id is the
    -- associated data, so a ciphertext copied onto another endpoint doesn't decrypt.
    secret_ciphertext BYTEA NOT NULL,
    secret_nonce      BYTEA NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ
);

CREATE INDEX endpoints_owner_idx ON endpoints (owner_id) WHERE deleted_at IS NULL;

-- One row per (endpoint, event): the work queue and the delivery log in one table.
CREATE TABLE deliveries (
    id                UUID PRIMARY KEY,
    endpoint_id       UUID NOT NULL REFERENCES endpoints (id),
    -- The source event's meta.event_id; a redelivered Kafka record finds the existing row.
    source_event_id   TEXT NOT NULL,
    event_type        TEXT NOT NULL,
    -- The JSON body sent, exactly; every attempt sends the same bytes.
    payload           TEXT NOT NULL,
    -- The trace context the delivery was queued under, so every attempt joins the event's trace.
    traceparent       TEXT,
    -- CANCELLED: its endpoint was deleted before it was delivered.
    status            TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'SUCCEEDED', 'DEAD', 'CANCELLED')),
    attempts          INT NOT NULL DEFAULT 0,
    -- When a worker may take it next. A worker that claims it pushes this forward by a lease,
    -- so the row comes back if the worker dies mid-attempt.
    next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_status_code  INT,
    last_error        TEXT,
    first_attempt_at  TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    -- Set once Kafka acknowledged its DeliveryDead on notifier.dlq.v1.
    dead_published_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (endpoint_id, source_event_id)
);

CREATE INDEX deliveries_due_idx ON deliveries (next_attempt_at) WHERE status = 'PENDING';
CREATE INDEX deliveries_endpoint_idx ON deliveries (endpoint_id, created_at DESC, id DESC);
CREATE INDEX deliveries_dead_unpublished_idx ON deliveries (finished_at)
    WHERE status = 'DEAD' AND dead_published_at IS NULL;

CREATE TABLE delivery_attempts (
    delivery_id UUID NOT NULL REFERENCES deliveries (id),
    attempt_no  INT NOT NULL CHECK (attempt_no >= 1),
    -- 0 when there was no response (timeout, refused connection, blocked address).
    status_code INT NOT NULL,
    error       TEXT,
    duration_ms INT NOT NULL,
    at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (delivery_id, attempt_no)
);
