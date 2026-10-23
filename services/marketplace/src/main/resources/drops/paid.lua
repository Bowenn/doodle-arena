-- Marks a reservation paid once its Postgres transaction committed (ADR 0022), so the buyer
-- can't take a second edition.
-- KEYS[1] buyers. ARGV[1] user, ARGV[2] edition, ARGV[3] seconds to keep the key.
redis.call('HSET', KEYS[1], ARGV[1], 'p:' .. ARGV[2])
redis.call('EXPIRE', KEYS[1], ARGV[3])
return 1
