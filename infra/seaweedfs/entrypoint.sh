#!/bin/sh
# Starts SeaweedFS (master, volume, filer and S3 gateway in one process) with one S3 identity
# per service, scoped to its buckets (task 5.5), as the IAM task roles are in AWS
# (infra/terraform/stack/iam.tf). Keys come from the environment, so no credentials are committed.
#   game-server  writes round-logs (stroke logs)
#   assets       reads round-logs (the claim check, design doc D13), reads and writes drawings
#   analytics    reads and writes analytics-checkpoints (Flink's checkpoints, ADR 0018)
#   dev-tools    the host tools (S3_ACCESS_KEY in .env): reads everything, writes round-logs
#                for scripts/fake-events.sh and pipeline-check
# The buckets are created by seaweedfs-buckets with `weed shell`, which needs no identity.
set -eu

: "${S3_GAME_SERVER_SECRET_KEY:?}" "${S3_ASSETS_SECRET_KEY:?}" "${S3_ANALYTICS_SECRET_KEY:?}" "${S3_ACCESS_KEY:?}" "${S3_SECRET_KEY:?}"
cat > /tmp/s3.json <<EOF
{
  "identities": [
    {
      "name": "game-server",
      "credentials": [{ "accessKey": "game-server", "secretKey": "${S3_GAME_SERVER_SECRET_KEY}" }],
      "actions": ["Write:round-logs"]
    },
    {
      "name": "assets",
      "credentials": [{ "accessKey": "assets", "secretKey": "${S3_ASSETS_SECRET_KEY}" }],
      "actions": ["Read:round-logs", "Read:drawings", "Write:drawings"]
    },
    {
      "name": "analytics",
      "credentials": [{ "accessKey": "analytics", "secretKey": "${S3_ANALYTICS_SECRET_KEY}" }],
      "actions": ["Read:analytics-checkpoints", "Write:analytics-checkpoints", "List:analytics-checkpoints"]
    },
    {
      "name": "dev-tools",
      "credentials": [{ "accessKey": "${S3_ACCESS_KEY}", "secretKey": "${S3_SECRET_KEY}" }],
      "actions": ["Read", "List", "Write:round-logs"]
    }
  ]
}
EOF

# Each bucket is a collection whose first write grows up to 7 volumes of its own, and
# -volume.max=0 fits as many volumes as the free disk allows. With the 30 GB default the second
# bucket gets none on a laptop or CI runner with little free disk (its writes fail with
# InternalError); 1 GB volumes need about 8 GB free for both.
exec weed server -dir=/data -ip=seaweedfs -ip.bind=0.0.0.0 -volume.max=0 -master.volumeSizeLimitMB=1024 \
  -s3 -s3.port=8333 -s3.config=/tmp/s3.json
