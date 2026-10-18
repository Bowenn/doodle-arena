-- Task 4.8 (the web app's economy pages).

-- "My drawings": a user's drawings, most recently acquired first.
CREATE INDEX ownership_owner_idx ON ownership (owner_id, updated_at DESC, drawing_id DESC);

-- The ownership history shows each sale's price from its listing; a sale closes exactly one listing.
CREATE UNIQUE INDEX listings_sale_idx ON listings (sale_id) WHERE sale_id IS NOT NULL;
