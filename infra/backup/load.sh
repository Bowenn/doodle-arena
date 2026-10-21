#!/usr/bin/env bash
# Restores a backup that dump.sh made (ADR 0016) into a fresh data host's empty stores, before
# the migrations run and before any service starts.
#   load.sh <plan>: each line of the plan is "<name> <sha256 hex> <url>", for the artifacts
#   pg-auth, pg-assets, pg-marketplace, pg-notifier and mongo-assets. The URLs are presigned
#   GETs (the data host's role can't read the backup bucket) or, locally, file:// URLs.
#
# It refuses to load into a store that has data. If every Postgres database already has tables
# (aws-up rerun on a stack whose stores were restored or are in use), it changes nothing and
# reports "skipped". A database that is half restored can't happen: each pg_restore is one
# transaction. The plan lists mongo-assets first (scripts/aws/restore.sh), so a restore that
# fails partway always leaves a store with data next to an empty database, which a rerun refuses
# instead of skipping.
# Prints the JSON summary (each store's rows after the restore) between BACKUP-SUMMARY-BEGIN
# and -END; any failure exits non-zero.
source "$(dirname "$0")/lib.sh"

plan="${1:?usage: load.sh <plan>}"
dir="${BACKUP_DIR:-/opt/doodle/restore-work}"
start_work "$dir" remove

wait_ready

full=0
for db in $DATABASES; do
  [[ "$(user_tables "$db")" == 0 ]] || full=$((full + 1))
done
if ((full == 4)); then
  echo "every database already has tables: not restoring" >&2
  print_summary "$dir" skipped
  exit 0
fi
if ((full > 0)); then
  echo "$full of 4 databases already have tables: refusing to restore over them" >&2
  exit 1
fi
if [[ "$(mongo_collections)" != 0 ]]; then
  echo "MongoDB's assets database already has collections: refusing to restore over it" >&2
  exit 1
fi

# The plan is read on fd 3: docker exec -i inside the loop would swallow the rest of stdin.
while read -r name sha256 url <&3; do
  [[ -z "$name" ]] && continue
  start="$(now)"
  file="$dir/$name"
  curl -fsS --retry 3 -o "$file" "$url"
  info="$(file_info "$file")"
  if [[ "${info#* }" != "$sha256" ]]; then
    echo "$name: its SHA-256 doesn't match the manifest's" >&2
    exit 1
  fi
  case "$name" in
    pg-*)
      db="${name#pg-}"
      # --no-owner and --role: the objects belong to this session's role of the same name,
      # whatever password it has now. --no-privileges: the grants are the init script's.
      docker exec -i "$PG" pg_restore -U postgres -d "$db" --no-owner --no-privileges --role="$db" \
        --single-transaction --exit-on-error < "$file"
      pg_counts "$db" > "$dir/$name.counts"
      ;;
    mongo-assets)
      mongo_root mongorestore --archive --gzip --nsInclude 'assets.*' < "$file"
      mongo_counts > "$dir/$name.counts"
      ;;
    *)
      echo "unknown artifact: $name" >&2
      exit 1
      ;;
  esac
  echo "$info $(python3 -c 'import sys, time; print(f"{time.time() - float(sys.argv[1]):.3f}")' "$start") $name" > "$dir/$name.info"
  echo "    $name restored: ${info%% *} bytes" >&2
done 3< "$plan"

print_summary "$dir" restored
