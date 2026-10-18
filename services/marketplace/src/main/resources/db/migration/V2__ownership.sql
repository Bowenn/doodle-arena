-- Drawing ownership and provenance (design doc, section 8). A drawing is minted once, when
-- assets approves it (DrawingApproved), and changes hands only through a sale (task 4.3).

-- The current owner. The primary key is where a redelivered DrawingApproved stops: the second
-- INSERT ... ON CONFLICT DO NOTHING finds the row and writes nothing.
CREATE TABLE ownership (
    drawing_id UUID PRIMARY KEY,
    owner_id   TEXT NOT NULL,
    -- 1 at mint, +1 per sale: always the number of ownership events (the reconciliation job checks).
    version    BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1),
    minted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Everyone who drew it, in drawing order. Royalties are split between them (task 4.6).
CREATE TABLE drawing_authors (
    drawing_id   UUID NOT NULL REFERENCES ownership (drawing_id),
    user_id      TEXT NOT NULL,
    author_order SMALLINT NOT NULL CHECK (author_order >= 0),
    PRIMARY KEY (drawing_id, author_order),
    UNIQUE (drawing_id, user_id)
);

-- Append-only provenance: who owned the drawing, from when, and why.
CREATE TABLE ownership_events (
    -- An identity, not a UUID, so a drawing's history has a strict order.
    event_id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    drawing_id      UUID NOT NULL REFERENCES ownership (drawing_id),
    kind            TEXT NOT NULL CHECK (kind IN ('MINT', 'SALE')),
    from_owner      TEXT,
    to_owner        TEXT NOT NULL,
    sale_id         UUID,
    -- The Kafka event (DrawingApproved's event_id) or request that caused it, to follow it back.
    source_event_id TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A mint comes from nobody and isn't a sale; a sale has a seller and a sale id.
    CONSTRAINT ownership_events_kind_shape CHECK (
        (kind = 'MINT' AND from_owner IS NULL AND sale_id IS NULL)
        OR (kind = 'SALE' AND from_owner IS NOT NULL AND sale_id IS NOT NULL))
);

-- At most one mint per drawing, whatever the code does.
CREATE UNIQUE INDEX ownership_events_one_mint ON ownership_events (drawing_id) WHERE kind = 'MINT';
CREATE INDEX ownership_events_drawing_idx ON ownership_events (drawing_id, event_id);

-- Like the ledger, provenance is never rewritten (V1's ledger_refuse_change).
CREATE TRIGGER ownership_events_append_only
    BEFORE UPDATE OR DELETE ON ownership_events
    FOR EACH ROW EXECUTE FUNCTION ledger_refuse_change();
CREATE TRIGGER ownership_events_no_truncate
    BEFORE TRUNCATE ON ownership_events
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();
