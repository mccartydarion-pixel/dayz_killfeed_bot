#!/usr/bin/env bash
# Recovery rehearsal from the DESTINATION (not from the job's local copy): download the encrypted archive
# from the private bucket, check its SHA-256 against the manifest, decrypt it with the passphrase, check
# the dump's SHA-256, and restore that downloaded dump into a second, empty, disposable PostgreSQL 18,
# comparing it with the inventory stored inside the archive.
# Usage: TARGET_URL=<local disposable db> BACKUP_PASS=... retrieve_verify.sh <archive-name> <manifest-file>
set -euo pipefail
name="${1:?archive name}"; manifest="${2:?manifest}"
: "${S3_BUCKET:?}"; : "${S3_ENDPOINT:?}"; : "${BACKUP_PASS:?}"; : "${TARGET_URL:?}"
here="$(cd "$(dirname "$0")" && pwd)"
dir="$(mktemp -d)"
aws s3 cp "s3://${S3_BUCKET}/champion-db/${name}.tar.gpg" "$dir/${name}.tar.gpg" --endpoint-url "$S3_ENDPOINT" --only-show-errors
want_enc=$(grep '^encrypted_sha256=' "$manifest" | cut -d= -f2)
got_enc=$(sha256sum "$dir/${name}.tar.gpg" | cut -d' ' -f1)
[ "$got_enc" = "$want_enc" ] || { echo "retrieved archive SHA-256 mismatch" >&2; exit 1; }
echo "retrieved from the bucket: SHA-256 matches the manifest"
printf '%s' "$BACKUP_PASS" | gpg --batch --quiet --pinentry-mode loopback --passphrase-fd 0 --decrypt -o "$dir/bundle.tar" "$dir/${name}.tar.gpg"
mkdir -p "$dir/work"
tar -C "$dir/work" -xf "$dir/bundle.tar"
want_dump=$(grep '^dump_sha256=' "$manifest" | cut -d= -f2)
got_dump=$(sha256sum "$dir/work/backup.dump" | cut -d' ' -f1)
[ "$got_dump" = "$want_dump" ] || { echo "decrypted dump SHA-256 mismatch" >&2; exit 1; }
echo "decrypted: dump SHA-256 matches the manifest"
cp "$here/inventory.sql" "$dir/work/inventory.sql"
rm -f "$dir/work/target_inventory.txt" "$dir/work/verify_summary.txt"
bash "$here/verify.sh" "$dir/work"
echo "complete restore of the retrieved copy: PASS"
rm -rf "$dir"
