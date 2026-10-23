-- Gives a reserved edition back (ADR 0022): the purchase failed (no coins) or leaked. Only the
-- user's own, still unpaid reservation of that edition is released.
-- KEYS[1] buyers, KEYS[2] free list. ARGV[1] user, ARGV[2] edition.
-- Returns 1 when released, 0 otherwise.
local held = redis.call('HGET', KEYS[1], ARGV[1])
if not held or string.match(held, '^r:(%d+):') ~= ARGV[2] then
  return 0
end
redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('LPUSH', KEYS[2], ARGV[2])
return 1
