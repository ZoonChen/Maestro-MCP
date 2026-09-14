#!/usr/bin/env bash
# pg-backup — resident pilot PostgreSQL dump (brief P3-6, restore floor
# after the three live-wipe incidents ART-incident-002/003).
#
# Dumps the resident control-plane database (5434,
# maestro-resident-postgres) into $PILOT_BACKUPS_DIR as
# maestro-<YYYYmmdd-HHMMSS>.dump (pg_dump -Fc custom format) and prunes
# dumps older than RETENTION_HOURS (default 72). Manual .sql snapshots
# taken by sessions are NOT touched — pruning only matches this tool's
# own naming pattern.
#
# pg_dump runs inside the resident container (postgres:16-alpine ships
# it; the host does not), streaming to the host-side file. Override with
# PILOT_PG_DSN/host args only if the resident DB is not containerized.
#
# Usage:
#   ./pg-backup.sh                 # dump + prune
#   ./pg-backup.sh --list          # show backups and retention window
#
# Env:
#   PILOT_PG_CONTAINER   default maestro-resident-postgres
#   PILOT_PG_USER        default maestro
#   PILOT_PG_DATABASE    default maestro
#   PILOT_BACKUPS_DIR    default ~/Works/yuandong/projects/maestro-p5a-bases/pilot-backups
#   RETENTION_HOURS      default 72
#
# Cron (every 4h, installed by brief-P3):
#   0 */4 * * * <abs-path>/pg-backup.sh >> <backups-dir>/pg-backup.log 2>&1
set -euo pipefail

PG_CONTAINER="${PILOT_PG_CONTAINER:-maestro-resident-postgres}"
PG_USER="${PILOT_PG_USER:-maestro}"
PG_DATABASE="${PILOT_PG_DATABASE:-maestro}"
BACKUPS_DIR="${PILOT_BACKUPS_DIR:-$HOME/Works/yuandong/projects/maestro-p5a-bases/pilot-backups}"
RETENTION_HOURS="${RETENTION_HOURS:-72}"

if [ "${1:-}" = "--list" ]; then
  ls -lt "$BACKUPS_DIR"/maestro-*.dump 2>/dev/null || echo "no dumps yet in $BACKUPS_DIR"
  echo "retention: ${RETENTION_HOURS}h (only maestro-*.dump files this tool wrote)"
  exit 0
fi

mkdir -p "$BACKUPS_DIR"

# Serialize concurrent runs (cron overlap protection). macOS has no
# flock(1); mkdir is atomic, and a lock dir older than an hour is stale
# (a dump of this size takes seconds) and gets reclaimed.
LOCK_DIR="$BACKUPS_DIR/.pg-backup.lockdir"
if mkdir "$LOCK_DIR" 2>/dev/null; then
  trap 'rmdir "$LOCK_DIR" 2>/dev/null || true' EXIT
else
  if [ -n "$(find "$LOCK_DIR" -maxdepth 0 -mmin +60 2>/dev/null)" ]; then
    rmdir "$LOCK_DIR" 2>/dev/null || true
    mkdir "$LOCK_DIR" || { echo "pg-backup: lock held; skipping" >&2; exit 0; }
    trap 'rmdir "$LOCK_DIR" 2>/dev/null || true' EXIT
  else
    echo "pg-backup: another run holds the lock; skipping" >&2
    exit 0
  fi
fi

docker inspect "$PG_CONTAINER" >/dev/null 2>&1 || {
  echo "pg-backup: container $PG_CONTAINER not running (resident stack down?)" >&2
  exit 66
}

STAMP="$(date +%Y%m%d-%H%M%S)"
OUT="$BACKUPS_DIR/maestro-${STAMP}.dump"

# Custom-format dump from the container's own pg_dump 16 to a host file.
docker exec "$PG_CONTAINER" pg_dump -U "$PG_USER" -d "$PG_DATABASE" \
  --format=custom --no-owner --no-privileges > "$OUT"

# A valid custom dump is at least a few KB; guard against a truncated
# zero-byte file being counted as today's restore point.
MIN_BYTES=4096
SIZE="$(stat -f %z "$OUT" 2>/dev/null || stat -c %s "$OUT")"
if [ "$SIZE" -lt "$MIN_BYTES" ]; then
  echo "pg-backup: dump suspiciously small (${SIZE}B < ${MIN_BYTES}B) — keeping it as maestro-${STAMP}.dump.suspect and failing" >&2
  mv "$OUT" "$OUT.suspect"
  exit 1
fi
echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) dumped $OUT (${SIZE}B)"

# Prune this tool's own dumps older than the retention window. -mmin
# (minutes) keeps it exact across BSD/Linux find without -mtime day
# rounding; the pattern only ever matches files this script names.
find "$BACKUPS_DIR" -maxdepth 1 -name 'maestro-*.dump' -type f \
  -mmin "+$((RETENTION_HOURS * 60))" -delete
echo "pruned dumps older than ${RETENTION_HOURS}h (maestro-*.dump only)"
