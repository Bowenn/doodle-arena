#!/bin/sh
# Redis with one ACL user per service (task 5.5), so a key prefix belongs to its service by
# rule, not by convention (design doc, section 5):
#   lobby        lobby:*
#   game-server  room:* and ticket:* (the join tickets it redeems)
#   assets       assets:*, Celery's broker (kombu's global_keyprefix puts every key and
#                pub/sub channel under assets:); Celery's two PSUBSCRIBE patterns are listed
#                literally, because Redis compares a subscribed pattern with the ACL as text
#   admin        everything: the healthcheck and humans (redis-cli --user admin)
# The default user is off, so a client without a password can't run a command. No user has
# @admin or @dangerous commands (FLUSHALL, KEYS, CONFIG, ...). The file holds SHA-256 hashes,
# never the passwords themselves.
set -eu

hash() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }

: "${REDIS_ADMIN_PASSWORD:?}" "${REDIS_LOBBY_PASSWORD:?}" "${REDIS_GAME_SERVER_PASSWORD:?}" "${REDIS_ASSETS_PASSWORD:?}"
umask 077
cat > /tmp/users.acl <<EOF
user default off
user admin on #$(hash "$REDIS_ADMIN_PASSWORD") ~* &* +@all
user lobby on #$(hash "$REDIS_LOBBY_PASSWORD") ~lobby:* resetchannels +@all -@admin -@dangerous
user game-server on #$(hash "$REDIS_GAME_SERVER_PASSWORD") ~room:* ~ticket:* resetchannels +@all -@admin -@dangerous
user assets on #$(hash "$REDIS_ASSETS_PASSWORD") ~assets:* resetchannels &assets:* &assets:/1.celery.pidbox &assets:/1.celeryev/worker.* +@all -@admin -@dangerous
EOF
chown redis:redis /tmp/users.acl

# The image's own entrypoint drops to the redis user before starting the server.
exec docker-entrypoint.sh redis-server --appendonly yes --aclfile /tmp/users.acl
