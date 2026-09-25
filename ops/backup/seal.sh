#!/usr/bin/env bash
# Bundle, encrypt (GPG AES-256), prove the archive opens, and write the public manifest (versions, sizes,
# SHA-256 values and the verification summary - no data). Removes the plaintext dump afterwards.
# Usage: BACKUP_PASS=... seal.sh <workdir> <outdir> <archive-name>
set -euo pipefail
work="${1:?workdir}"; out="${2:?outdir}"; name="${3:?archive name}"
: "${BACKUP_PASS:?}"
mkdir -p "$out"
tar -C "$work" -cf bundle.tar backup.dump source_inventory.txt target_inventory.txt toc.txt verify_summary.txt pg_dump_version.txt server_version.txt
printf '%s' "$BACKUP_PASS" | gpg --batch --yes --pinentry-mode loopback --passphrase-fd 0 --symmetric --cipher-algo AES256 \
  --s2k-digest-algo SHA512 --s2k-count 65011712 -o "$out/${name}.tar.gpg" bundle.tar
printf '%s' "$BACKUP_PASS" | gpg --batch --quiet --pinentry-mode loopback --passphrase-fd 0 --decrypt -o check.tar "$out/${name}.tar.gpg"
cmp bundle.tar check.tar
{
  echo "archive=${name}"
  echo "created_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "run=${GITHUB_SERVER_URL:-local}/${GITHUB_REPOSITORY:-}/actions/runs/${GITHUB_RUN_ID:-0}"
  echo "commit=${GITHUB_SHA:-unknown}"
  echo "server_version=$(cat "$work/server_version.txt")"
  echo "pg_dump=$(cat "$work/pg_dump_version.txt")"
  echo "dump_bytes=$(stat -c %s "$work/backup.dump")"
  echo "dump_sha256=$(sha256sum "$work/backup.dump" | cut -d' ' -f1)"
  echo "encrypted_bytes=$(stat -c %s "$out/${name}.tar.gpg")"
  echo "encrypted_sha256=$(sha256sum "$out/${name}.tar.gpg" | cut -d' ' -f1)"
  echo "encryption=gpg symmetric AES256, S2K SHA512 x65011712"
  cat "$work/verify_summary.txt"
} > "$out/${name}.manifest.txt"
rm -f bundle.tar check.tar "$work/backup.dump"
echo "sealed: ${name}.tar.gpg"
