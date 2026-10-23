-- Stocks a new drop (ADR 0022): edition numbers 1..N in the free list, and the stocked marker.
-- KEYS[1] free list, KEYS[2] stocked marker. ARGV[1] editions, ARGV[2] seconds to keep the keys.
-- Returns 1 when it stocked the drop, 0 when it was already stocked (a retried create).
if redis.call('EXISTS', KEYS[2]) == 1 then
  return 0
end
local editions = tonumber(ARGV[1])
local batch = {}
for n = 1, editions do
  batch[#batch + 1] = n
  if #batch == 500 or n == editions then
    redis.call('RPUSH', KEYS[1], unpack(batch))
    batch = {}
  end
end
redis.call('SET', KEYS[2], editions, 'EX', ARGV[2])
redis.call('EXPIRE', KEYS[1], ARGV[2])
return 1
