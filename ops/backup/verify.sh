#!/usr/bin/env bash
# Verify a custom-format backup by restoring it into a DISPOSABLE PostgreSQL 18 database and comparing
# it with the inventory taken from the source snapshot (docs/PRODUCTION_BACKUP.md).
#
# Usage: TARGET_URL=postgres://...@127.0.0.1:5432/restore_check verify.sh <workdir>
# The target must be a local, disposable database: this script refuses any other host, so it can never
# restore over production.
set -euo pipefail
: "${TARGET_URL:?TARGET_URL is not set}"
work="$(cd "${1:?workdir}" && pwd)"
image="${PG_IMAGE:-postgres:18}"
case "$TARGET_URL" in
  postgres://*@127.0.0.1:*|postgres://*@localhost:*|postgresql://*@127.0.0.1:*|postgresql://*@localhost:*) ;;
  *) echo "refusing to restore: the target is not a local disposable database" >&2; exit 2 ;;
esac
export TARGET_URL
export PGOPTIONS="-c TimeZone=UTC"
pg() { docker run --rm -i --network host -e TARGET_URL -e PGOPTIONS -v "$work:/work" "$image" "$@"; }

# 1. The archive is readable and complete (table of contents).
pg pg_restore --list /work/backup.dump > "$work/toc.txt"
entries=$(grep -vc '^;' "$work/toc.txt" || true)
echo "archive TOC entries: $entries"
test "$entries" -gt 0

# 2. The target is empty before the restore.
existing=$(pg psql "$TARGET_URL" -X -At -c "SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'")
if [ "$existing" != "0" ]; then
  echo "refusing to restore: the disposable target is not empty" >&2
  exit 2
fi

# 3. Restore (single transaction, stop on the first error).
pg pg_restore --dbname="$TARGET_URL" --no-owner --no-privileges --exit-on-error --single-transaction /work/backup.dump
echo "restore: OK"

# 4. Same inventory on the restored copy, compared line by line with the source snapshot.
pg psql "$TARGET_URL" -X -q -v ON_ERROR_STOP=1 -f /work/inventory.sql -o /work/target_inventory.raw
LC_ALL=C sort "$work/target_inventory.raw" > "$work/target_inventory.txt"
rm -f "$work/target_inventory.raw"
if diff -q "$work/source_inventory.txt" "$work/target_inventory.txt" > /dev/null; then
  result=PASS
else
  result=FAIL
  diff "$work/source_inventory.txt" "$work/target_inventory.txt" | cut -d'|' -f1-2 | sort -u | head -40 > "$work/inventory_diff_keys.txt" || true
fi
{
  echo "result=$result"
  echo "toc_entries=$entries"
  for k in migration object trigger function constraint index extension sequence table; do
    echo "${k}s=$(grep -c "^$k|" "$work/source_inventory.txt" || true)"
  done
  echo "last_migration=$(grep '^migration|' "$work/source_inventory.txt" | tail -1 | cut -d'|' -f2)"
} > "$work/verify_summary.txt"
cat "$work/verify_summary.txt"
[ "$result" = PASS ]
