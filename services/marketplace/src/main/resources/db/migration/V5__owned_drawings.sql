-- "My drawings" in the web app (task 4.8): a user's drawings, most recently acquired first.
CREATE INDEX ownership_owner_idx ON ownership (owner_id, updated_at DESC, drawing_id DESC);
