-- Reserves one edition for an admitted buyer, atomically (ADR 0022). Postgres stays the truth:
-- this only keeps the burst away from it, so at most N purchases ever reach it.
-- KEYS[1] queue, KEYS[2] buyers, KEYS[3] free list.
-- ARGV[1] user, ARGV[2] now (ms), ARGV[3] places admitted so far (from the time since the open),
-- ARGV[4] seconds to keep the keys.
-- Returns {outcome, value}: RESERVED with the edition (also for a reservation already held),
-- ALREADY_BOUGHT with the edition, NOT_JOINED, NOT_ADMITTED with the place still ahead, SOLD_OUT.
local held = redis.call('HGET', KEYS[2], ARGV[1])
if held then
  local kind, edition = string.match(held, '^(%a):(%d+)')
  if kind == 'p' then
    return {'ALREADY_BOUGHT', edition}
  end
  return {'RESERVED', edition}
end
local rank = redis.call('ZRANK', KEYS[1], ARGV[1])
if not rank then
  return {'NOT_JOINED', '0'}
end
local admitted = tonumber(ARGV[3])
if rank >= admitted then
  return {'NOT_ADMITTED', tostring(rank - admitted + 1)}
end
local edition = redis.call('LPOP', KEYS[3])
if not edition then
  return {'SOLD_OUT', '0'}
end
redis.call('HSET', KEYS[2], ARGV[1], 'r:' .. edition .. ':' .. ARGV[2])
redis.call('EXPIRE', KEYS[2], ARGV[4])
return {'RESERVED', edition}
