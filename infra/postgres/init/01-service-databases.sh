#!/usr/bin/env bash
# Runs once, on the first start of an empty Postgres volume.
# Creates one database and one owner per service (database per service, design doc section 5).
set -euo pipefail

create_db() {
  local name="$1" password="$2"
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
    -v name="$name" -v password="$password" <<'SQL'
CREATE ROLE :"name" LOGIN PASSWORD :'password';
CREATE DATABASE :"name" OWNER :"name";
REVOKE ALL ON DATABASE :"name" FROM PUBLIC;
SQL
}

create_db auth "$AUTH_DB_PASSWORD"
create_db assets "$ASSETS_DB_PASSWORD"
create_db marketplace "$MARKETPLACE_DB_PASSWORD"
create_db notifier "$NOTIFIER_DB_PASSWORD"
