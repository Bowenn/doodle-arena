#!/bin/sh
# Starts SeaweedFS (master, volume, filer and S3 gateway in one process) with one S3 identity
# whose keys come from the environment, so no credentials are committed.
set -eu

cat > /tmp/s3.json <<EOF
{
  "identities": [
    {
      "name": "doodle-local",
      "credentials": [{ "accessKey": "${S3_ACCESS_KEY}", "secretKey": "${S3_SECRET_KEY}" }],
      "actions": ["Admin", "Read", "List", "Tagging", "Write"]
    }
  ]
}
EOF

exec weed server -dir=/data -ip=seaweedfs -ip.bind=0.0.0.0 -volume.max=0 \
  -s3 -s3.port=8333 -s3.config=/tmp/s3.json
