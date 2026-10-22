#!/bin/sh
# One-shot job: creates the buckets from design doc section 5. Safe to run again.
# round-logs objects expire after 7 days (a SeaweedFS TTL on the bucket's path).
set -eu

MASTER=seaweedfs:9333

until echo "cluster.check" | weed shell -master="$MASTER" >/dev/null 2>&1; do
  echo "waiting for seaweedfs master"
  sleep 1
done

for bucket in round-logs drawings analytics-checkpoints; do
  if echo "s3.bucket.list" | weed shell -master="$MASTER" 2>/dev/null | grep -qw "$bucket"; then
    echo "bucket $bucket exists"
  else
    echo "s3.bucket.create -name $bucket" | weed shell -master="$MASTER"
  fi
done

printf 'fs.configure -locationPrefix=/buckets/round-logs/ -ttl=7d -apply\n' | weed shell -master="$MASTER"
echo "buckets ready"
