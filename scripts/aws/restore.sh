#!/usr/bin/env bash
# Restores the latest backup into a fresh stack's empty stores (ADR 0016). scripts/aws/up.sh runs
# it after creating the stack with every service at 0, before the migrations: the dumps carry
# each service's migration history, so the migrations then apply only what is newer.
#   DOMAIN=example.com [BACKUP_ID=<id>] [RESTORE=none] scripts/aws/restore.sh
# BACKUP_ID picks an older backup (aws s3 ls s3://doodle-arena-backups-<account>/backups/);
# RESTORE=none starts with empty stores. A backup whose schema is newer than the code being
# deployed (IMAGE_TAG) is refused: deploy newer code, or pick an older backup.
# 1. Reads the manifest and checks its schema against the code at IMAGE_TAG.
# 2. On the data host, waits for the stores, then loads every dump through presigned URLs (the
#    host's role can't read the bucket; 15 minutes), MongoDB first (infra/backup/load.sh). Stores that
#    already hold data are left alone: a rerun of aws-up on a restored stack skips this.
# 3. Compares every table's rows and every collection's documents with the manifest's.
# 4. Copies the drawings back and checks their count.
source "$(dirname "$0")/common.sh"
require_aws
stack_vars
init_stack

if [[ "${RESTORE:-}" == none ]]; then
  echo "    RESTORE=none: starting with empty stores"
  exit 0
fi

start=$SECONDS
bucket="$(backup_bucket)"
work="$(mktemp -d)"
summary="$work/summary.json"
echo '{"artifacts": []}' > "$summary"
trap 'rm -rf "$work"' EXIT
id="${BACKUP_ID:-}"

fail() {
  echo "RESTORE FAILED: $1" >&2
  backup_telemetry restore error "$id" "$summary" "$1"
  echo "The stack is up with every service at 0. To start without this backup: RESTORE=none make aws-up" >&2
  echo "(after a load that stopped partway, recreate the data host first: docs/runbooks/aws-backup.md)." >&2
  exit 1
}

if [[ -z "$id" ]]; then
  # Ids are UTC timestamps, so the newest complete backup is the last one with a manifest.
  for candidate in $(aws s3api list-objects-v2 --bucket "$bucket" --prefix backups/ --delimiter / \
    --query 'CommonPrefixes[].Prefix' --output text | tr '\t' '\n' | sed -n 's|^backups/\(.*\)/$|\1|p' | sort -r); do
    if aws s3api head-object --bucket "$bucket" --key "backups/$candidate/manifest.json" >/dev/null 2>&1; then
      id="$candidate"
      break
    fi
  done
  if [[ -z "$id" ]]; then
    echo "    no backup in s3://$bucket/backups/ yet: starting with empty stores"
    exit 0
  fi
fi
manifest="$work/manifest.json"
aws s3 cp "s3://$bucket/backups/$id/manifest.json" "$manifest" --only-show-errors || fail "backup $id has no manifest"
echo "    backup $id (made by images $(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["image_tag"])' "$manifest"))"

git -C "$ROOT" cat-file -e "$IMAGE_TAG^{commit}" 2>/dev/null || fail "IMAGE_TAG $IMAGE_TAG isn't a commit in this checkout"
python3 "$ROOT/scripts/aws/backups.py" check-schema "$manifest" "$ROOT" "$IMAGE_TAG" ||
  fail "backup $id has migrations the code at $IMAGE_TAG doesn't (above): it is from a newer schema"

echo "    waiting for the data host's stores"
host_run 1200 "for i in \$(seq 120); do [ -f /opt/doodle/backup/load.sh ] && break; sleep 5; done
bash /opt/doodle/backup/load.sh --ready" >/dev/null || fail "the data host's stores didn't come up"

# The plan load.sh reads: one artifact per line, MongoDB first, each with a presigned GET. The
# URLs are part of the SSM command, so they stay in its history: signed only now that the host
# is ready, for 15 minutes, enough for the downloads to start.
plan=""
while read -r name file _ sha256 _; do
  url="$(aws s3 presign "s3://$bucket/backups/$id/$file" --expires-in 900)" || fail "presigning $file failed"
  plan="$plan$name $sha256 $url"$'\n'
done < <(python3 "$ROOT/scripts/aws/backups.py" artifacts "$manifest")

echo "    loading the dumps on the data host"
output="$work/load.out"
loaded=0
host_run 3600 "cd /opt/doodle
umask 077
cat > restore-plan.tsv <<'PLAN'
$plan
PLAN
status=0
bash backup/load.sh restore-plan.tsv || status=\$?
rm -f restore-plan.tsv
exit \$status" > "$output" || loaded=1
if python3 "$ROOT/scripts/aws/backups.py" summary "$output" > "$summary.new" 2>/dev/null; then
  mv "$summary.new" "$summary"
fi
((loaded == 0)) || fail "loading backup $id on the data host failed"

result="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("result", ""))' "$summary")"
if [[ "$result" == skipped ]]; then
  echo "    the stores already hold data (a rerun on a restored stack): not loaded again"
else
  python3 "$ROOT/scripts/aws/backups.py" compare-counts "$manifest" "$summary" ||
    fail "the restored stores don't hold what backup $id does (above)"
  echo "    every table's rows and every collection's documents match the backup"
fi

# Additive: a rerun copies only what is missing, and drawing keys never repeat.
drawings_start=$SECONDS
read -r drawings drawings_bytes < <(python3 -c 'import json,sys; d=json.load(open(sys.argv[1]))["drawings"]; print(d["objects"], d["bytes"])' "$manifest")
aws s3 sync "s3://$bucket/backups/$id/drawings/" "s3://$(drawings_bucket)/" --only-show-errors || fail "copying the drawings back failed"
copied="$(aws s3 ls "s3://$(drawings_bucket)/" --recursive --summarize | awk '/Total Objects:/ { print $3 + 0 }')"
((${copied:-0} >= drawings)) || fail "the drawings bucket has $copied objects, the backup $drawings"
drawings_seconds=$((SECONDS - drawings_start))
echo "    $drawings drawings copied back in $drawings_seconds s"

backup_telemetry restore ok "$id" "$summary" "restore of $id ${result:-restored}" "$drawings_seconds" "$drawings_bytes" "$((SECONDS - start))"
echo "    restored backup $id in $((SECONDS - start)) s"
