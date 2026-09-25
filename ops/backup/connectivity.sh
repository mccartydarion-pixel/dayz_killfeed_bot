#!/usr/bin/env bash
# Connectivity check - NOT a backup. Proves, without printing any value, that:
#   * the environment's encryption passphrase really encrypts and decrypts: a random probe is sealed
#     exactly like a backup (gpg AES256, S2K SHA512 x65011712) BEFORE upload, so only ciphertext ever
#     reaches the bucket;
#   * the private bucket accepts that write, returns the same ciphertext bytes (SHA-256 compared),
#     the retrieved copy decrypts back to the original probe, and the probe can be removed;
#   * (when SRC_URL is set) the production database accepts a TLS, read-only connection (SELECT only).
# Usage: BACKUP_PASS=... connectivity.sh
set -euo pipefail
: "${S3_BUCKET:?}"; : "${S3_ENDPOINT:?}"; : "${AWS_ACCESS_KEY_ID:?}"; : "${AWS_SECRET_ACCESS_KEY:?}"
: "${BACKUP_PASS:?BACKUP_PASS (the encryption passphrase) is required: the probe is encrypted before upload}"
aws s3api head-bucket --bucket "$S3_BUCKET" --endpoint-url "$S3_ENDPOINT"
echo "bucket: reachable"
dir="$(mktemp -d)"
key="champion-db/_connectivity/probe-${GITHUB_RUN_ID:-local}-$(date +%s).bin.gpg"
cleanup() {
  # Best effort: never leave a probe behind, even when a later check fails.
  aws s3 rm "s3://${S3_BUCKET}/${key}" --endpoint-url "$S3_ENDPOINT" --only-show-errors >/dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT
head -c 4096 /dev/urandom > "$dir/probe.bin"
printf '%s' "$BACKUP_PASS" | gpg --batch --yes --quiet --pinentry-mode loopback --passphrase-fd 0 --symmetric --cipher-algo AES256 \
  --s2k-digest-algo SHA512 --s2k-count 65011712 -o "$dir/probe.bin.gpg" "$dir/probe.bin"
# The uploaded object must be an OpenPGP symmetric message, never the plaintext probe.
if cmp -s "$dir/probe.bin" "$dir/probe.bin.gpg"; then echo "probe was not encrypted" >&2; exit 1; fi
gpg --batch --list-packets --pinentry-mode loopback --passphrase-fd 3 3< <(printf '%s' "$BACKUP_PASS") "$dir/probe.bin.gpg" 2>/dev/null \
  | grep -q 'symkey enc packet: version [0-9]*, cipher 9' || { echo "probe is not AES256 symmetric-encrypted" >&2; exit 1; }
echo "probe: encrypted before upload (AES256, $(stat -c %s "$dir/probe.bin.gpg") bytes)"
aws s3 cp "$dir/probe.bin.gpg" "s3://${S3_BUCKET}/${key}" --endpoint-url "$S3_ENDPOINT" --only-show-errors
aws s3 cp "s3://${S3_BUCKET}/${key}" "$dir/back.bin.gpg" --endpoint-url "$S3_ENDPOINT" --only-show-errors
[ "$(sha256sum < "$dir/probe.bin.gpg")" = "$(sha256sum < "$dir/back.bin.gpg")" ] || { echo "retrieved ciphertext differs" >&2; exit 1; }
printf '%s' "$BACKUP_PASS" | gpg --batch --quiet --pinentry-mode loopback --passphrase-fd 0 --decrypt -o "$dir/back.bin" "$dir/back.bin.gpg"
cmp -s "$dir/probe.bin" "$dir/back.bin" || { echo "retrieved probe does not decrypt to the original" >&2; exit 1; }
aws s3 rm "s3://${S3_BUCKET}/${key}" --endpoint-url "$S3_ENDPOINT" --only-show-errors
if aws s3api head-object --bucket "$S3_BUCKET" --key "$key" --endpoint-url "$S3_ENDPOINT" >/dev/null 2>&1; then
  echo "probe still present after removal" >&2; exit 1
fi
echo "bucket: encrypted write, identical read-back, decrypt with the environment passphrase, and cleanup: OK"
if [ -n "${SRC_URL:-}" ]; then
  export SRC_URL PGOPTIONS="-c default_transaction_read_only=on" PGSSLMODE="${PGSSLMODE:-require}" PGCONNECT_TIMEOUT=30
  res=$(docker run --rm --network host -e SRC_URL -e PGOPTIONS -e PGSSLMODE -e PGCONNECT_TIMEOUT "${PG_IMAGE:-postgres:18}" \
    sh -c 'psql "$SRC_URL" -X -At -c "SELECT current_setting('"'"'server_version_num'"'"')::int / 10000, (SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid())"')
  echo "database: reachable read-only (major version and TLS: ${res})"
fi
