#!/usr/bin/env bash
# Dumps every service's durable data (ADR 0016): the auth, assets, marketplace and notifier
# Postgres databases (pg_dump's custom format) and assets' MongoDB database (a gzipped archive).
# Each dump is read back before it counts, and the summary also records each database's rows and
# applied migrations.
#
# With BACKUP_BUCKET, each file goes to s3://$BACKUP_BUCKET/$BACKUP_PREFIX/ with its SHA-256,
# which S3 checks on arrival (a mismatch fails the upload), and the files are deleted afterwards.
# The data host's role may only put objects there (infra/terraform/stack/iam.tf).
#   On the data host, from scripts/aws/backup.sh: BACKUP_BUCKET=... BACKUP_PREFIX=backups/<id> bash backup/dump.sh
#   Locally, from scripts/backup-check.sh: BACKUP_DIR=<dir> PG_CONTAINER=... MONGO_CONTAINER=... infra/backup/dump.sh
# Prints the JSON summary between BACKUP-SUMMARY-BEGIN and -END; any failure exits non-zero.
source "$(dirname "$0")/lib.sh"

dir="${BACKUP_DIR:-/opt/doodle/backup-work}"
bucket="${BACKUP_BUCKET:-}"
prefix="${BACKUP_PREFIX:-}"
if [[ -n "$bucket" && -z "$prefix" ]]; then
  echo "BACKUP_PREFIX is required with BACKUP_BUCKET" >&2
  exit 2
fi

# On the host the files are only a step on the way to S3; locally they are the result.
start_work "$dir" ${bucket:+remove}

upload() { # upload <file> <sha256 hex>
  local b64
  b64="$(python3 -c 'import base64, sys; print(base64.b64encode(bytes.fromhex(sys.argv[1])).decode())' "$2")"
  # put-object, not s3 cp: one request with the whole file's checksum (a multipart upload's
  # checksum would be of its parts). Its limit, 5 GB per file, is far above today's dumps.
  aws s3api put-object --bucket "$bucket" --key "$prefix/$(basename "$1")" --body "$1" \
    --checksum-sha256 "$b64" --output text --query ChecksumSHA256 >/dev/null
}

finish() { # finish <name> <file> <start>: records bytes, SHA-256 and seconds, and uploads
  local name="$1" file="$2" start="$3" info
  info="$(file_info "$file")"
  [[ -n "$bucket" ]] && upload "$file" "${info#* }"
  echo "$info $(python3 -c 'import sys, time; print(f"{time.time() - float(sys.argv[1]):.3f}")' "$start") $(basename "$file")" > "$dir/$name.info"
  echo "    $name: ${info%% *} bytes" >&2
}

wait_ready

for db in $DATABASES; do
  start="$(now)"
  # Read the counts and migrations first: nothing writes while aws-down backs up (every service
  # is at 0), so they describe the same data as the dump.
  pg_counts "$db" > "$dir/pg-$db.counts"
  pg_migrations "$db" > "$dir/$db.migrations"
  docker exec "$PG" pg_dump -U postgres -d "$db" --format=custom > "$dir/pg-$db.dump"
  # A dump pg_restore can't list is truncated or corrupt.
  docker exec -i "$PG" pg_restore --list < "$dir/pg-$db.dump" > /dev/null
  finish "pg-$db" "$dir/pg-$db.dump" "$start"
done

start="$(now)"
mongo_counts > "$dir/mongo-assets.counts"
mongo_root mongodump --db assets --archive --gzip > "$dir/mongo-assets.archive"
# The archive is one gzip stream: gzip -t checks all of it, the dry run checks it parses.
gzip -t < "$dir/mongo-assets.archive"
mongo_root mongorestore --archive --gzip --dryRun < "$dir/mongo-assets.archive"
finish mongo-assets "$dir/mongo-assets.archive" "$start"

print_summary "$dir"
