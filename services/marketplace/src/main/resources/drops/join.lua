-- Joins a drop's waiting room (ADR 0022). The score decides the order: a random number in [0, 1)
-- before the open (the lottery), 1 + the time in ms after it. A user keeps their first place.
-- KEYS[1] queue. ARGV[1] user, ARGV[2] score, ARGV[3] seconds to keep the key.
-- Returns the user's rank (0-based) in the queue.
redis.call('ZADD', KEYS[1], 'NX', ARGV[2], ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[3])
return redis.call('ZRANK', KEYS[1], ARGV[1])
