-- Task 6.6 (make explain on the seeded data): the reconciliation job groups every ownership event
-- by drawing and takes each drawing's newest new owner (ARRAY_AGG(to_owner ORDER BY event_id
-- DESC)). With the columns it reads in the index, in that order, it reads the index alone and
-- sorts nothing: 1.35 s to 1.11 s for 607k events. It replaces ownership_events_drawing_idx,
-- whose leading columns it has, so a drawing's history (WHERE drawing_id = ? ORDER BY event_id)
-- still uses it and every insert still maintains one index, not two.
CREATE INDEX ownership_events_drawing_cover ON ownership_events (drawing_id, event_id DESC) INCLUDE (kind, to_owner);
DROP INDEX ownership_events_drawing_idx;
