-- Answers the refusals a purchase would get, without changing anything (ADR 0022), so most of a
-- burst is turned away by Redis before it claims an idempotency key in Postgres. Sold out comes
-- before admission: once every edition is taken, everyone in the queue hears it at once instead
-- of waiting for their turn to be told (the flash sale's load test found that).
-- KEYS[1] queue, KEYS[2] buyers, KEYS[3] free list. ARGV[1] user, ARGV[2] places admitted so far.
-- Returns {outcome, value}: HELD (the user has a reservation or bought: go on, Postgres decides),
-- SOLD_OUT, NOT_JOINED, NOT_ADMITTED with the place still ahead, or GO.
if redis.call('HEXISTS', KEYS[2], ARGV[1]) == 1 then
  return {'HELD', '0'}
end
if redis.call('LLEN', KEYS[3]) == 0 then
  return {'SOLD_OUT', '0'}
end
local rank = redis.call('ZRANK', KEYS[1], ARGV[1])
if not rank then
  return {'NOT_JOINED', '0'}
end
local admitted = tonumber(ARGV[2])
if rank >= admitted then
  return {'NOT_ADMITTED', tostring(rank - admitted + 1)}
end
return {'GO', '0'}
