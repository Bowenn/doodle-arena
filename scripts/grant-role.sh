#!/usr/bin/env bash
# Gives a user of the local stack a role in auth, for example moderator for the Django admin:
#
#   scripts/grant-role.sh <user_id> moderator
#
# The user id is "id" in GET /api/auth/me. The role reaches the user's next access token, so
# sign out and in again (or wait up to 15 minutes for the next refresh). This is a local
# operator tool: it runs psql inside the stack's Postgres container, never from a service.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <user_id> <player|moderator|admin>" >&2
  exit 2
fi
user_id="$1" role="$2"
case "$role" in
  player | moderator | admin) ;;
  *) echo "unknown role: $role (player, moderator or admin)" >&2; exit 2 ;;
esac

if [[ -d "$HOME/.docker/bin" ]]; then
  export PATH="$HOME/.docker/bin:$PATH"
fi

# psql variables (:'name') quote the values, so neither argument can change the statement.
docker exec -i doodle-arena-postgres-1 \
  psql -v ON_ERROR_STOP=1 --quiet --tuples-only -U postgres -d auth \
  -v user_id="$user_id" -v role="$role" <<'SQL'
\set QUIET on
WITH changed AS (
  UPDATE users
  SET roles = CASE WHEN :'role' = ANY (roles) THEN roles ELSE array_append(roles, :'role') END
  WHERE id::text = :'user_id'
  RETURNING roles
)
SELECT CASE WHEN count(*) = 0 THEN 'no such user' ELSE 'roles: ' || array_to_string(max(roles), ', ') END
FROM changed;
SQL
