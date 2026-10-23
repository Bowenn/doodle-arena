-- A per-user token bucket (ADR 0022): rate tokens a second, at most burst.
-- KEYS[1] the user's bucket. ARGV[1] now (ms), ARGV[2] rate, ARGV[3] burst.
-- Returns 0 when the request may go ahead, else the ms until a token is free.
local now = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local state = redis.call('HMGET', KEYS[1], 't', 'ts')
local tokens = tonumber(state[1]) or burst
local ts = tonumber(state[2]) or now
tokens = math.min(burst, tokens + (math.max(0, now - ts) / 1000) * rate)
if tokens < 1 then
  return math.ceil((1 - tokens) * 1000 / rate)
end
redis.call('HSET', KEYS[1], 't', tostring(tokens - 1), 'ts', tostring(now))
redis.call('PEXPIRE', KEYS[1], math.ceil(burst / rate * 1000) + 1000)
return 0
