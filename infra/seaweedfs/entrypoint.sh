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

# Each bucket is a collection whose first write grows up to 7 volumes of its own, and
# -volume.max=0 fits as many volumes as the free disk allows. With the 30 GB default the second
# bucket gets none on a laptop or CI runner with little free disk (its writes fail with
# InternalError); 1 GB volumes need about 8 GB free for both.
exec weed server -dir=/data -ip=seaweedfs -ip.bind=0.0.0.0 -volume.max=0 -master.volumeSizeLimitMB=1024 \
  -s3 -s3.port=8333 -s3.config=/tmp/s3.json
