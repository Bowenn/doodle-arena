-- Helpers for make seed (task 6.6), loaded at the start of every seed session. Both databases
-- compute the same drawing ids and times from the same numbers, so the marketplace's ownership
-- matches the assets service's drawings without either reading the other's database.
--
-- seed.base_ms (a session setting, from scripts/seed.sh) is the start of the seeded year, and
-- seed.scale (default 1) divides every count, for a quick trial run.

-- The scale (about 5 million rows, design doc section 8 and task 6.6's plan).
CREATE FUNCTION pg_temp.n_users() RETURNS bigint LANGUAGE sql STABLE AS
    $$ SELECT 100000 / COALESCE(NULLIF(current_setting('seed.scale', true), ''), '1')::bigint $$;
CREATE FUNCTION pg_temp.n_drawings() RETURNS bigint LANGUAGE sql STABLE AS
    $$ SELECT 300000 / COALESCE(NULLIF(current_setting('seed.scale', true), ''), '1')::bigint $$;
-- Drawings 0 .. n_sold-1 are sold twice; the next n_listed are listed and unsold.
CREATE FUNCTION pg_temp.n_sold() RETURNS bigint LANGUAGE sql STABLE AS
    $$ SELECT 150000 / COALESCE(NULLIF(current_setting('seed.scale', true), ''), '1')::bigint $$;
CREATE FUNCTION pg_temp.n_listed() RETURNS bigint LANGUAGE sql STABLE AS
    $$ SELECT 50000 / COALESCE(NULLIF(current_setting('seed.scale', true), ''), '1')::bigint $$;
-- Rewards 0 .. n_users-1 are everyone's 100-coin welcome in month 0, then 30 coins each.
CREATE FUNCTION pg_temp.n_rewards() RETURNS bigint LANGUAGE sql STABLE AS
    $$ SELECT 500000 / COALESCE(NULLIF(current_setting('seed.scale', true), ''), '1')::bigint $$;
CREATE FUNCTION pg_temp.n_outbox() RETURNS bigint LANGUAGE sql STABLE AS
    $$ SELECT 500000 / COALESCE(NULLIF(current_setting('seed.scale', true), ''), '1')::bigint $$;

-- A moment in month m (0..11) of the seeded year: n spreads rows over the month's 30 days.
CREATE FUNCTION pg_temp.ts(m bigint, n bigint) RETURNS timestamptz LANGUAGE sql STABLE AS $$
    SELECT to_timestamp((current_setting('seed.base_ms')::bigint + m * 2629800000 + (n % 2592000) * 1000) / 1000.0)
$$;

-- A UUIDv7 for the time t, with the rest of its bits from md5(kind || n): the same inputs give
-- the same id in every database, and ids sort by time like the services' own.
CREATE FUNCTION pg_temp.seed_uuid(t timestamptz, kind text, n bigint) RETURNS uuid LANGUAGE sql IMMUTABLE AS $$
    SELECT (substr(ms, 1, 8) || '-' || substr(ms, 9, 4) || '-7' || substr(h, 1, 3) || '-'
        || substr('89ab', (get_byte(decode(h, 'hex'), 0) % 4) + 1, 1) || substr(h, 4, 3) || '-'
        || substr(h, 7, 12))::uuid
    FROM (SELECT lpad(to_hex(floor(extract(epoch FROM t) * 1000)::bigint), 12, '0') AS ms,
                 md5(kind || ':' || n) AS h) x
$$;

CREATE FUNCTION pg_temp.user_id(u bigint) RETURNS text LANGUAGE sql STABLE AS $$
    SELECT 'seed-u-' || (u % pg_temp.n_users())
$$;

-- A drawing's mint month: sold drawings in the first half, so both sales fit in the year.
CREATE FUNCTION pg_temp.mint_month(d bigint) RETURNS bigint LANGUAGE sql STABLE AS $$
    SELECT CASE WHEN d < pg_temp.n_sold() THEN d % 6 ELSE d % 12 END
$$;

CREATE FUNCTION pg_temp.drawing_id(d bigint) RETURNS uuid LANGUAGE sql STABLE AS $$
    SELECT pg_temp.seed_uuid(pg_temp.ts(pg_temp.mint_month(d), d), 'drawing', d)
$$;

-- The author (and initial owner) of drawing d.
CREATE FUNCTION pg_temp.author(d bigint) RETURNS text LANGUAGE sql STABLE AS $$
    SELECT pg_temp.user_id(d)
$$;
