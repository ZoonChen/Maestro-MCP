#!/usr/bin/env bash
# PostgreSQL restore for the Maestro Control Plane (M1-DEP-001).
#
# Restores a pg-backup.sh snapshot. DESTRUCTIVE: drops and recreates the
# public schema of the target database before restoring. After the
# restore it runs `maestro migrate up` and REQUIRES applied=0 — the
# restored migration catalog must match the embedded digests exactly;
# any drift fails the restore (V1 drill: integrity assertion).
#
# Usage: scripts/pg-restore.sh <dump-file> [maestro-binary]
# Env:   PGHOST/PGPORT/PGUSER/PGDATABASE (as pg-backup.sh)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DUMP="${1:?usage: pg-restore.sh <dump-file> [maestro-binary]}"
BIN="${2:-$ROOT/bin/maestro}"

test -f "$DUMP" || { echo "restore: dump file not found: $DUMP" >&2; exit 1; }
test -x "$BIN" || { echo "restore: maestro binary not found at $BIN (make build)" >&2; exit 1; }
command -v pg_restore >/dev/null || command -v docker >/dev/null || {
  echo "restore: pg_restore (host) or docker (compose container) is required" >&2
  exit 1
}

PGHOST="${PGHOST:-127.0.0.1}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-maestro}"
PGDATABASE="${PGDATABASE:-maestro}"
export PGHOST PGPORT PGUSER PGDATABASE

echo "restore: DESTROYING schema of $PGDATABASE@$PGHOST:$PGPORT and restoring $DUMP"
if command -v psql >/dev/null && command -v pg_restore >/dev/null; then
  psql --no-psqlrc -v ON_ERROR_STOP=1 <<SQL
DROP SCHEMA public CASCADE;
DROP SCHEMA IF EXISTS maestro_meta CASCADE;
CREATE SCHEMA public;
SQL
  pg_restore --dbname="$PGDATABASE" --no-owner --no-privileges "$DUMP"
else
  # Compose topology: the container holds both the binaries and the data.
  PG_CID="$(cd "$ROOT" && docker compose ps -q maestro-postgres 2>/dev/null || true)"
  [ -n "$PG_CID" ] || { echo "restore: no host psql and no maestro-postgres container" >&2; exit 1; }
  docker exec -e PGPASSWORD="${PGPASSWORD:-}" "$PG_CID" psql -U "$PGUSER" -d "$PGDATABASE" \
    -v ON_ERROR_STOP=1 -c 'DROP SCHEMA public CASCADE; DROP SCHEMA IF EXISTS maestro_meta CASCADE; CREATE SCHEMA public;'
  docker cp "$DUMP" "$PG_CID:/tmp/restore.dump" >/dev/null
  docker exec -e PGPASSWORD="${PGPASSWORD:-}" "$PG_CID" \
    pg_restore -U "$PGUSER" -d "$PGDATABASE" --no-owner --no-privileges /tmp/restore.dump
  docker exec "$PG_CID" rm -f /tmp/restore.dump
fi

echo "restore: verifying migration catalog integrity (applied must be 0)"
MIGRATE_OUTPUT="$(MAESTRO_DB_DRIVER=postgres "$BIN" migrate up 2>&1)"
echo "$MIGRATE_OUTPUT"
if ! echo "$MIGRATE_OUTPUT" | grep -q 'applied=0'; then
  echo "restore: INTEGRITY FAILURE — restored catalog drifted from embedded migrations" >&2
  exit 1
fi

echo "restore: ok"
