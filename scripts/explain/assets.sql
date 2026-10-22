-- make explain (task 6.6): the assets service's queries as Django sends them (drawings/api.py),
-- with parameters taken from the seeded data. See marketplace.sql.
\set ON_ERROR_STOP on
SELECT 'seed-u-42' AS usr \gset
SELECT id AS drawing, match_id AS match FROM drawings_drawing WHERE match_id LIKE 'seed-m-%' ORDER BY id DESC LIMIT 1 OFFSET 1000 \gset

\echo '### gallery'
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM drawings_drawing WHERE status = 'approved' ORDER BY id DESC LIMIT 26;

\echo '### gallery_by_author'
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM drawings_drawing
WHERE (status = 'approved' OR author_ids @> ARRAY[:'usr']::varchar(64)[]) AND author_ids @> ARRAY[:'usr']::varchar(64)[]
ORDER BY id DESC LIMIT 26;

\echo '### gallery_by_match'
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM drawings_drawing
WHERE (status = 'approved' OR author_ids @> ARRAY[:'usr']::varchar(64)[]) AND match_id = :'match' ORDER BY id DESC LIMIT 26;

\echo '### drawing'
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM drawings_drawing
WHERE (status = 'approved' OR author_ids @> ARRAY[:'usr']::varchar(64)[]) AND id = :'drawing' LIMIT 1;

\echo '### review_queue'
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM drawings_drawing WHERE status = 'review' ORDER BY created_at LIMIT 100;

\echo '### outbox_unpublished'
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM drawings_outboxevent WHERE published_at IS NULL ORDER BY id LIMIT 100;
