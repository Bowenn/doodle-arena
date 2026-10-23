-- Rebuilds a drop's free list after Redis lost it (a restart without its data, a flush): every
-- edition Postgres hasn't sold goes back (ADR 0022). The sweeper calls it for open drops whose
-- stocked marker is gone. Paid buyers are relearned on their next attempt (Postgres refuses a
-- second edition), and reservations in flight are lost, so their purchases fail and retry.
-- KEYS[1] free list, KEYS[2] stocked marker. ARGV[1] editions, ARGV[2] seconds to keep the keys,
-- ARGV[3..] the edition numbers Postgres has sold. Returns how many editions it put back.
if redis.call('EXISTS', KEYS[2]) == 1 then
  return -1
end
local sold = {}
for i = 3, #ARGV do
  sold[ARGV[i]] = true
end
redis.call('DEL', KEYS[1])
local batch, count = {}, 0
local editions = tonumber(ARGV[1])
for n = 1, editions do
  if not sold[tostring(n)] then
    batch[#batch + 1] = n
    count = count + 1
  end
  if #batch == 500 or (n == editions and #batch > 0) then
    redis.call('RPUSH', KEYS[1], unpack(batch))
    batch = {}
  end
end
redis.call('SET', KEYS[2], editions, 'EX', ARGV[2])
if count > 0 then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return count
