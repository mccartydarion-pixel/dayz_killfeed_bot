#!/usr/bin/env bash
# Consistent, read-only PostgreSQL 18 backup (docs/PRODUCTION_BACKUP.md).
#
# One REPEATABLE READ, READ ONLY transaction exports a snapshot; pg_dump (custom format) dumps exactly
# that snapshot, and the inventory (schema, migrations, per-table counts and content hashes) is taken
# inside the same transaction - so the restored copy can be compared with the source exactly, even
# while the bot keeps writing. Every session also runs with default_transaction_read_only=on.
#
# Usage: SRC_URL=<connection string> dump.sh <workdir>
# The connection string is read from the environment only; it is never echoed or written to disk.
set -euo pipefail
: "${SRC_URL:?SRC_URL is not set}"
work="$(cd "${1:?workdir}" && pwd)"
image="${PG_IMAGE:-postgres:18}"
here="$(cd "$(dirname "$0")" && pwd)"
cp "$here/inventory.sql" "$work/inventory.sql"
export SRC_URL
export PGOPTIONS="-c default_transaction_read_only=on -c TimeZone=UTC -c statement_timeout=0 -c idle_in_transaction_session_timeout=0"
export PGSSLMODE="${PGSSLMODE:-require}"
export PGCONNECT_TIMEOUT=30

pg() { docker run --rm -i --network host -e SRC_URL -e PGOPTIONS -e PGSSLMODE -e PGCONNECT_TIMEOUT -v "$work:/work" "$image" "$@"; }

# Client and server versions (no credentials).
pg pg_dump --version | tee "$work/pg_dump_version.txt"
pg sh -c 'psql "$SRC_URL" -X -At -c "SHOW server_version"' > "$work/server_version.txt"
echo "server version: $(cat "$work/server_version.txt")"

# The snapshot-holding session.
coproc SESSION { pg sh -c 'exec psql "$SRC_URL" -X -q -At -v ON_ERROR_STOP=1'; }
printf 'BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;\nSELECT pg_export_snapshot();\n' >&"${SESSION[1]}"
if ! read -r -t 120 snapshot <&"${SESSION[0]}" || [[ ! "$snapshot" =~ ^[0-9A-F-]+$ ]]; then
  echo "could not export a snapshot" >&2
  exit 1
fi
echo "snapshot exported"

# Inventory inside the snapshot transaction.
printf '\\o /work/source_inventory.raw\n\\i /work/inventory.sql\n\\o\nSELECT %s;\n' "'inventory-done'" >&"${SESSION[1]}"
if ! read -r -t 1800 marker <&"${SESSION[0]}" || [[ "$marker" != "inventory-done" ]]; then
  echo "inventory failed" >&2
  exit 1
fi
LC_ALL=C sort "$work/source_inventory.raw" > "$work/source_inventory.txt"
rm -f "$work/source_inventory.raw"

# The dump of the same snapshot (custom format, compressed).
start=$(date +%s)
pg sh -c 'exec pg_dump "$SRC_URL" --format=custom --compress=6 --no-password --snapshot="$0" --file=/work/backup.dump' "$snapshot"
echo "pg_dump finished in $(( $(date +%s) - start ))s"

# Release the snapshot.
printf 'COMMIT;\n\\q\n' >&"${SESSION[1]}"
wait "$SESSION_PID" || true

test -s "$work/backup.dump"
echo "tables in inventory: $(grep -c '^table|' "$work/source_inventory.txt")"
echo "migrations in inventory: $(grep -c '^migration|' "$work/source_inventory.txt"), last $(grep '^migration|' "$work/source_inventory.txt" | tail -1 | cut -d'|' -f2)"
