#!/usr/bin/env bash
# PostgreSQL logical backup for the Maestro Control Plane (M1-DEP-001).
#
# Takes a pg_dump -Fc (custom format, compressed) snapshot into a dated
# file, then prunes snapshots beyond the retention count. Restores are
# performed by scripts/pg-restore.sh against the SAME schema state; the
# restore runbook asserts integrity by running `maestro migrate up` and
# requiring applied=0 (catalog digests match the embedded migrations).
#
# Usage: scripts/pg-backup.sh [backup-dir]
# Env:   MAESTRO_BACKUP_DIR  target directory (default: ./backups)
#        PGHOST/PGPORT/PGUSER/PGDATABASE or MAESTRO_TEST_POSTGRES_DSN-derived defaults
#        MAESTRO_BACKUP_RETENTION  snapshots to keep (default 7)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKUP_DIR="${1:-${MAESTRO_BACKUP_DIR:-$ROOT/backups}}"
RETENTION="${MAESTRO_BACKUP_RETENTION:-7}"

PGHOST="${PGHOST:-127.0.0.1}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-maestro}"
PGDATABASE="${PGDATABASE:-maestro}"
export PGHOST PGPORT PGUSER PGDATABASE

# Host client tools are preferred; on machines without them the compose
# postgres container provides identical binaries (local-first topology).
if command -v pg_dump >/dev/null; then
  PG_DUMP() { pg_dump "$@"; }
  PG_RESTORE_LIST() { pg_restore --list "$1"; }
else
  ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  PG_CID="$(cd "$ROOT_DIR" && docker compose ps -q maestro-postgres 2>/dev/null || true)"
  if [ -z "$PG_CID" ]; then
    echo "backup: neither host pg_dump nor a running maestro-postgres container" >&2
    exit 1
  fi
  PG_DUMP() { docker exec -e PGPASSWORD="${PGPASSWORD:-}" "$PG_CID" pg_dump "$@"; }
  PG_RESTORE_LIST() { docker exec "$PG_CID" pg_restore --list "$1"; }
fi

mkdir -p "$BACKUP_DIR"
chmod 0700 "$BACKUP_DIR"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
TARGET="$BACKUP_DIR/maestro-$STAMP.dump"

echo "backup: $PGDATABASE@$PGHOST:$PGPORT -> $TARGET"
# Container-side dumps stream to stdout; host-side dumps write directly.
if command -v pg_dump >/dev/null; then
  pg_dump --format=custom --file="$TARGET" --no-owner --no-privileges
else
  docker exec -e PGPASSWORD="${PGPASSWORD:-}" "$PG_CID" \
    pg_dump -U "$PGUSER" -d "$PGDATABASE" --format=custom --no-owner --no-privileges > "$TARGET"
fi
test -s "$TARGET" || { echo "backup: dump file is empty" >&2; exit 1; }

# Integrity gate: a dump that cannot be listed is not a backup. Listing
# happens wherever the binaries live; container dumps copy back first.
if command -v pg_restore >/dev/null; then
  pg_restore --list "$TARGET" >/dev/null
else
  docker cp "$TARGET" "$PG_CID:/tmp/backup-verify.dump" >/dev/null
  docker exec "$PG_CID" pg_restore --list /tmp/backup-verify.dump >/dev/null
  docker exec "$PG_CID" rm -f /tmp/backup-verify.dump
fi

echo "backup: $(du -h "$TARGET" | cut -f1) written; pruning to $RETENTION snapshots"
ls -1t "$BACKUP_DIR"/maestro-*.dump 2>/dev/null | tail -n +$((RETENTION + 1)) | while read -r stale; do
  echo "backup: pruning $stale"
  rm -f "$stale"
done

echo "backup: ok"
