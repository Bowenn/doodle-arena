-- The assets service's seed (make seed, task 6.6), as the assets role: every seeded drawing,
-- approved by the AI, with the same id and time the marketplace minted it with. They show an
-- existing approved drawing's image (the gallery needs one); their replays don't exist.
\set ON_ERROR_STOP on
BEGIN;
CREATE TEMP TABLE picture ON COMMIT DROP AS
SELECT image_key, thumb_key, sha256 FROM drawings_drawing
WHERE status = 'approved' AND image_key <> '' AND match_id NOT LIKE 'seed-%'
ORDER BY created_at LIMIT 1;
INSERT INTO drawings_drawing (id, match_id, round_no, room_id, word, author_ids, initial_owner_id,
    stroke_log_key, stroke_count, canvas_width, canvas_height, ended_at, source_event_id, replay_id,
    sha256, image_key, thumb_key, status, moderation_attempts, decided_by, decided_at, decision_reason,
    created_at, updated_at)
SELECT pg_temp.drawing_id(d), 'seed-m-' || d, 1, 'seed-r-' || (d % 5000),
       (ARRAY['apple', 'lighthouse', 'violin', 'cactus', 'rocket', 'giraffe', 'umbrella', 'castle'])[1 + d % 8],
       ARRAY[pg_temp.author(d)], pg_temp.author(d), 'seed/' || d || '.binpb.gz', 40, 800, 600,
       t, 'seed-round-' || d, '', COALESCE(p.sha256, ''), COALESCE(p.image_key, ''), COALESCE(p.thumb_key, ''),
       'approved', 1, 'ai', t, 'seed', t, t
FROM (SELECT d, pg_temp.ts(pg_temp.mint_month(d), d) AS t FROM generate_series(0, pg_temp.n_drawings() - 1) d) x
LEFT JOIN picture p ON true;
COMMIT;
