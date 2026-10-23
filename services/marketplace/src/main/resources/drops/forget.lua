-- Drops a user's reservation without giving its edition back: Postgres already holds that
-- edition for someone else (Redis was wrong), so the number must not be sold again.
-- KEYS[1] buyers. ARGV[1] user, ARGV[2] edition.
local held = redis.call('HGET', KEYS[1], ARGV[1])
if held and string.match(held, '^r:(%d+):') == ARGV[2] then
  redis.call('HDEL', KEYS[1], ARGV[1])
  return 1
end
return 0
