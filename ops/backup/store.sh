#!/usr/bin/env bash
# Upload the encrypted archive and its manifest to the private S3-compatible bucket, then confirm the
# stored object's size. Credentials come from the environment only (AWS_ACCESS_KEY_ID,
# AWS_SECRET_ACCESS_KEY, S3_ENDPOINT, S3_BUCKET, AWS_DEFAULT_REGION); nothing is echoed.
# Usage: store.sh <outdir> <archive-name>
set -euo pipefail
out="${1:?outdir}"; name="${2:?archive name}"
: "${S3_BUCKET:?S3_BUCKET is not configured}"; : "${S3_ENDPOINT:?S3_ENDPOINT is not configured}"
: "${AWS_ACCESS_KEY_ID:?}"; : "${AWS_SECRET_ACCESS_KEY:?}"
prefix="s3://${S3_BUCKET}/champion-db"
aws s3 cp "$out/${name}.tar.gpg" "$prefix/${name}.tar.gpg" --endpoint-url "$S3_ENDPOINT" --only-show-errors
aws s3 cp "$out/${name}.manifest.txt" "$prefix/${name}.manifest.txt" --endpoint-url "$S3_ENDPOINT" --only-show-errors
stored=$(aws s3api head-object --bucket "$S3_BUCKET" --key "champion-db/${name}.tar.gpg" --endpoint-url "$S3_ENDPOINT" --query ContentLength --output text)
local_size=$(stat -c %s "$out/${name}.tar.gpg")
if [ "$stored" != "$local_size" ]; then
  echo "stored object size $stored differs from the archive ($local_size)" >&2
  exit 1
fi
echo "stored: champion-db/${name}.tar.gpg ($stored bytes) and its manifest"
