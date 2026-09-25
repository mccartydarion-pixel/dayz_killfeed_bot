#!/usr/bin/env bash
# Connectivity check - NOT a backup. Proves, without printing any value, that:
#   * the private bucket accepts a write, returns the same bytes on read, and allows the probe's removal;
#   * (when SRC_URL is set) the production database accepts a TLS, read-only connection (SELECT 1 only).
# Usage: connectivity.sh
set -euo pipefail
: "${S3_BUCKET:?}"; : "${S3_ENDPOINT:?}"; : "${AWS_ACCESS_KEY_ID:?}"; : "${AWS_SECRET_ACCESS_KEY:?}"
aws s3api head-bucket --bucket "$S3_BUCKET" --endpoint-url "$S3_ENDPOINT"
echo "bucket: reachable"
probe="$(mktemp)"; back="$(mktemp)"
head -c 4096 /dev/urandom > "$probe"
key="champion-db/_connectivity/probe-${GITHUB_RUN_ID:-local}-$(date +%s).bin"
aws s3 cp "$probe" "s3://${S3_BUCKET}/${key}" --endpoint-url "$S3_ENDPOINT" --only-show-errors
aws s3 cp "s3://${S3_BUCKET}/${key}" "$back" --endpoint-url "$S3_ENDPOINT" --only-show-errors
cmp "$probe" "$back"
aws s3 rm "s3://${S3_BUCKET}/${key}" --endpoint-url "$S3_ENDPOINT" --only-show-errors
rm -f "$probe" "$back"
echo "bucket: write, read-back (identical) and cleanup: OK"
if [ -n "${SRC_URL:-}" ]; then
  export SRC_URL PGOPTIONS="-c default_transaction_read_only=on" PGSSLMODE="${PGSSLMODE:-require}" PGCONNECT_TIMEOUT=30
  res=$(docker run --rm --network host -e SRC_URL -e PGOPTIONS -e PGSSLMODE -e PGCONNECT_TIMEOUT "${PG_IMAGE:-postgres:18}" \
    sh -c 'psql "$SRC_URL" -X -At -c "SELECT current_setting('"'"'server_version_num'"'"')::int / 10000, (SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid())"')
  echo "database: reachable read-only (major version and TLS: ${res})"
fi
