#!/usr/bin/env bash
# Backs up the stack's data before aws-down destroys it (ADR 0016). scripts/aws/down.sh runs it
# first and destroys nothing unless it succeeds.
#   DOMAIN=example.com scripts/aws/backup.sh
# 1. Stops the writers: the gateway (nothing new comes in), the web app, the lobby, auth's web
#    process and the game servers (they drain, task 5.5).
# 2. Waits until the consumers, the Celery worker and the outbox relays have nothing left
#    (infra/backup/idle.sh), at most DRAIN_TIMEOUT seconds (default 300). After that it warns and
#    goes on: the databases are consistent, only events still in Kafka are at risk.
# 3. Stops every other service, so nothing writes while the databases are dumped.
# 4. Dumps the four Postgres databases and assets' MongoDB on the data host, which uploads each
#    file to the backup bucket with its SHA-256 (infra/backup/dump.sh). Each upload's checksum,
#    as S3 computed it, is compared with the host's, and so is its size.
# 5. Copies the drawings bucket into the backup and compares object counts and bytes.
# 6. Writes manifest.json last: a backup without one is incomplete and never restored.
# 7. Keeps the newest 10 complete backups.
# Any failure exits 1 with the stack still up (and still costing money), services at 0.
source "$(dirname "$0")/common.sh"
require_aws
stack_vars
init_stack

start=$SECONDS
cluster=doodle-arena
bucket="$(backup_bucket)"
id="$(date -u +%Y%m%dT%H%M%SZ)"
work="$(mktemp -d)"
summary="$work/summary.json"
echo '{"artifacts": []}' > "$summary"
trap 'rm -rf "$work"' EXIT

fail() {
  echo "BACKUP FAILED: $1" >&2
  backup_telemetry backup error "$id" "$summary" "$1"
  echo "Nothing was destroyed: the stack is still up, its services at 0, and it still costs money." >&2
  echo "Fix the cause and run make aws-down again, or start the services again with make aws-up." >&2
  echo "To end the session without keeping its data: SKIP_BACKUP=1 DOMAIN=$DOMAIN make aws-down" >&2
  exit 1
}

# scale_to_zero <service>...: sets each to 0 and waits until its tasks have stopped. A service
# that doesn't exist (a rerun after a destroy that failed halfway) is skipped.
scale_to_zero() {
  local s err present=()
  for s in "$@"; do
    if err="$(aws ecs update-service --cluster "$cluster" --service "$s" --desired-count 0 2>&1 >/dev/null)"; then
      present+=("$s")
    elif [[ "$err" != *ServiceNotFoundException* && "$err" != *ServiceNotActiveException* && "$err" != *ClusterNotFoundException* ]]; then
      echo "$err" >&2
      return 1
    fi
  done
  ((${#present[@]})) || return 0
  # services-stable takes at most 10 services per call.
  aws ecs wait services-stable --cluster "$cluster" --services "${present[@]:0:10}" || return 1
  if ((${#present[@]} > 10)); then
    aws ecs wait services-stable --cluster "$cluster" --services "${present[@]:10}" || return 1
  fi
}

# The code that wrote this data, for the manifest (the schema check reads the migrations).
deployed="$(task_image_tag auth)" || deployed=unknown
echo "==> backup $id (data from images $deployed) to s3://$bucket/backups/$id/"
echo "    1/6 stopping the writers: gateway, web, lobby, auth, game servers (they drain)"
scale_to_zero gateway web lobby auth game-server-1 game-server-2 || fail "the writers didn't stop"

echo "    2/6 waiting until the pipeline is idle (at most ${DRAIN_TIMEOUT:-300} s)"
idle_in_a_row=0
deadline=$((SECONDS + ${DRAIN_TIMEOUT:-300}))
while :; do
  signals="$(host_run 60 'bash /opt/doodle/backup/idle.sh' 2>/dev/null)" || signals="data_host error"
  # Busy: any signal that is neither 0 nor "none" (no sample: that part never reported).
  busy="$(echo "$signals" | awk 'NF == 2 && $2 != "none" && ($2 == "error" || $2 + 0 != 0) { printf "%s=%s ", $1, $2 }')"
  if [[ -z "$busy" ]]; then
    idle_in_a_row=$((idle_in_a_row + 1))
    # Twice, 30 s apart: the broker's lag is measured every 30 s (docs/observability.md).
    ((idle_in_a_row == 2)) && {
      echo "    idle"
      break
    }
  else
    idle_in_a_row=0
  fi
  if ((SECONDS > deadline)); then
    echo "    warning: still busy after ${DRAIN_TIMEOUT:-300} s ($busy): backing up anyway." >&2
    echo "    Outbox entries are in the databases and will be published after the restore; events" >&2
    echo "    already in Kafka but not yet consumed are lost (ADR 0016)." >&2
    break
  fi
  sleep 30
done

echo "    3/6 stopping every other service"
services=()
while IFS= read -r s; do services+=("$s"); done < <(terraform -chdir="$TF/stack" output -json services | python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)))')
scale_to_zero "${services[@]}" || fail "the services didn't stop"

echo "    4/6 dumping the databases on the data host"
dump_start=$SECONDS
output="$work/dump.out"
dumped=0
# The host's CLI has no default region: its boot script passes --region to every call.
host_run 3600 "AWS_DEFAULT_REGION=$AWS_REGION BACKUP_BUCKET=$bucket BACKUP_PREFIX=backups/$id bash /opt/doodle/backup/dump.sh" > "$output" || dumped=1
# Even a failed dump prints what it finished, so the telemetry names the store it stopped on.
if python3 "$ROOT/scripts/aws/backups.py" summary "$output" > "$summary.new" 2>/dev/null; then
  mv "$summary.new" "$summary"
fi
((dumped == 0)) || fail "the dump on the data host failed"
while read -r name file bytes _ sha256_b64; do
  head="$(aws s3api head-object --bucket "$bucket" --key "backups/$id/$file" --checksum-mode ENABLED \
    --query '[ContentLength, ChecksumSHA256]' --output text)" || fail "$file isn't in the backup bucket"
  [[ "$head" == "$bytes	$sha256_b64" ]] || fail "$file in S3 ($head) isn't what the host dumped ($bytes $sha256_b64)"
  echo "    $name: $bytes bytes, checksum verified"
done < <(python3 "$ROOT/scripts/aws/backups.py" artifacts "$summary")
echo "    dumped in $((SECONDS - dump_start)) s"

echo "    5/6 copying the drawings"
drawings_start=$SECONDS
aws s3 sync "s3://$(drawings_bucket)/" "s3://$bucket/backups/$id/drawings/" --only-show-errors || fail "copying the drawings failed"
source_totals="$(s3_totals "$(drawings_bucket)" "")" || fail "counting the drawings failed"
copied="$(s3_totals "$bucket" "backups/$id/drawings/")" || fail "counting the copied drawings failed"
[[ "$copied" == "$source_totals" ]] || fail "the drawings copy ($copied) doesn't match the bucket ($source_totals)"
read -r drawings drawings_bytes <<< "$source_totals"
drawings_seconds=$((SECONDS - drawings_start))
echo "    $drawings drawings, $drawings_bytes bytes, in $drawings_seconds s"

echo "    6/6 the manifest"
python3 "$ROOT/scripts/aws/backups.py" manifest "$summary" "$id" "$deployed" "$drawings" "$drawings_bytes" > "$work/manifest.json" ||
  fail "the backup is incomplete"
aws s3 cp "$work/manifest.json" "s3://$bucket/backups/$id/manifest.json" --only-show-errors ||
  fail "writing the manifest failed"

# Keep the newest 10 complete backups. Incomplete ones (no manifest, from a run that failed) go.
# A backup whose manifest can't be looked up (an error, not a 404) is left alone.
complete=()
ids="$(backup_ids "$bucket")" || ids=""
for b in $ids; do
  status=0
  has_manifest "$bucket" "$b" || status=$?
  if ((status == 0)); then
    complete+=("$b")
  elif ((status == 1)); then
    aws s3 rm "s3://$bucket/backups/$b/" --recursive --only-show-errors || true
  fi
done
# backup_ids lists newest first.
for old in ${complete[@]+"${complete[@]:10}"}; do
  echo "    removing the old backup $old"
  aws s3 rm "s3://$bucket/backups/$old/" --recursive --only-show-errors || true
done

backup_telemetry backup ok "$id" "$summary" "backup $id complete" "$drawings_seconds" "$drawings_bytes" "$((SECONDS - start))"
echo "Backup $id complete in $((SECONDS - start)) s: s3://$bucket/backups/$id/"
