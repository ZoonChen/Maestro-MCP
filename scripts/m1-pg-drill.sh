#!/usr/bin/env bash
# M1 P3 exit-gate drill (plans/stages/m1-control-plane.md P3 / DISCIPLINE-PHASES P3).
#
# Proves on the local Compose PostgreSQL:
#   1. forward migration (maestro migrate up)
#   2. SQLite import dry-run with quarantine report
#   3. transactional import + idempotent re-import (imported=0 on rerun)
#   4. reconcile (coverage + status projection + DATA-INV-002)
#   5. rollback drill (migrate revert) + clean re-migrate
#
# Usage: scripts/m1-pg-drill.sh [path-to-maestro-binary]
# Requires: docker compose, sqlite3, a running Docker daemon.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${1:-$ROOT/bin/maestro}"
if [[ ! -x "$BIN" ]]; then
  echo "maestro binary not found at $BIN (build with: make build)" >&2
  exit 1
fi
command -v sqlite3 >/dev/null || { echo "sqlite3 CLI is required" >&2; exit 1; }

export MAESTRO_DB_DRIVER=postgres
# Pick a free loopback port: host defaults collide with other local stacks.
DRILL_PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
export MAESTRO_POSTGRES_PORT="$DRILL_PORT"
export MAESTRO_DATABASE_DSN="postgres://maestro:maestro-local-dev@127.0.0.1:${DRILL_PORT}/maestro?sslmode=disable"

WORK="$(mktemp -d /tmp/maestro-pg-drill.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT
SAMPLE_DB="$WORK/sample.db"
step() { printf '\n=== %s ===\n' "$1"; }

step "0. start drill postgres (loopback publish on 127.0.0.1:${MAESTRO_POSTGRES_PORT})"
docker compose -f "$ROOT/docker-compose.yaml" --profile m1 up -d maestro-postgres
DRILL_PG="$(docker compose -f "$ROOT/docker-compose.yaml" ps -q maestro-postgres)"
[[ -n "$DRILL_PG" ]] || { echo "maestro-postgres container not found" >&2; exit 1; }
for _ in $(seq 1 60); do
  if docker exec "$DRILL_PG" pg_isready -U maestro -d maestro >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

step "1. forward migration"
"$BIN" migrate up

step "1b. doctor on postgres"
"$BIN" doctor

step "2. seed sample SQLite (v5 schema + valid rows + one quarantined row)"
MAESTRO_DB_DRIVER= MAESTRO_DATABASE_DSN= "$BIN" migrate up --db "$SAMPLE_DB" >/dev/null
sqlite3 "$SAMPLE_DB" <<'SQL'
-- The import source simulates a pre-v2 legacy database: the M0 insert
-- guards would reject the legacy/invalid rows the importer must handle.
DROP TRIGGER IF EXISTS trg_tasks_valid_status_insert;
INSERT INTO agent_sessions (id, project_id, role, client_type, capacity, status, last_heartbeat, created_at, version, external_id)
VALUES ('sess-1', 'proj-1', 'backend', 'cli', 1, 'offline', '2026-08-28 09:00:00', '2026-08-28 09:00:00', 0, 'sess-1');
INSERT INTO agent_workers (id, session_id, project_id, current_task_id, status, tasks_completed, version, last_active)
VALUES ('worker-1', 'sess-1', 'proj-1', NULL, 'idle', 0, 0, '2026-08-28 09:00:00');
INSERT INTO projects (id, name, workspace_path, description, status, config, created_at, updated_at)
VALUES ('proj-1', 'Drill Project', '/tmp/drill', 'sample', 'active', '{}',
        '2026-08-28 09:00:00', '2026-08-28 09:00:00');
INSERT INTO features (id, project_id, title, description, reference_urls, status, created_at, updated_at)
VALUES ('feat-1', 'proj-1', 'Sample feature', '', '[]', 'planning',
        '2026-08-28 09:01:00', '2026-08-28 09:01:00');
INSERT INTO tasks (id, project_id, feature_id, title, description, role, status,
                   allowed_directories, forbidden_patterns, required_apis, dependencies,
                   priority, version, lease_epoch, created_at, updated_at)
VALUES
  ('task-1', 'proj-1', 'feat-1', 'Queued task', '', 'backend', 'queued',
   '[]', '[]', '[]', '[]', 'normal', 0, 0,
   '2026-08-28 09:02:00', '2026-08-28 09:02:00'),
  ('task-2', 'proj-1', 'feat-1', 'Legacy pending task', '', 'frontend', 'pending',
   '[]', '[]', '[]', '[]', 'low', 0, 0,
   '2026-08-28 09:03:00', '2026-08-28 09:03:00'),
  ('task-bad', 'proj-1', 'feat-1', 'Invalid state', '', 'backend', 'weird_state',
   '[]', '[]', '[]', '[]', 'normal', 0, 0,
   '2026-08-28 09:04:00', '2026-08-28 09:04:00');
INSERT INTO task_leases (id, project_id, task_id, session_id, worker_id, epoch, status, version, expires_at, created_at, updated_at)
VALUES ('lease-1', 'proj-1', 'task-1', 'sess-1', 'worker-1', 1, 'completed', 1,
        '2026-08-28 10:00:00', '2026-08-28 09:05:00', '2026-08-28 09:30:00');
INSERT INTO worktrees (id, task_id, project_id, session_id, worktree_path, branch_name, base_commit, status, generation, version, created_at, updated_at)
VALUES (1, 'task-1', 'proj-1', 'sess-1', '/tmp/drill/wt-1', 'maestro/drill/task-1', 'abc123', 'abandoned', 1, 0,
        '2026-08-28 09:06:00', '2026-08-28 09:40:00');
INSERT INTO validation_runs (id, task_id, project_id, attempt, base_commit, changed_files, test_command,
                             test_exit_code, test_output, coverage, boundary_ok, test_ok, coverage_ok,
                             summary, result, error_code, duration_ms, log_path, created_at)
VALUES (1, 'task-1', 'proj-1', 1, 'abc123', '["main.go"]', 'go test ./...',
        0, 'ok', 0.75, 1, 1, 1, '{}', 'passed', NULL, 1200, NULL, '2026-08-28 09:07:00');
SQL

step "3. pg-import --dry-run"
"$BIN" pg-import --sqlite "$SAMPLE_DB" --dry-run --report "$WORK/dry-run.json" | tail -1
grep -q '"stage": "dry-run"' "$WORK/dry-run.json"
grep -q '"source_id": "task-bad"' "$WORK/dry-run.json" && echo "quarantine captured task-bad ✓"

step "4. pg-import (transactional)"
"$BIN" pg-import --sqlite "$SAMPLE_DB" --report "$WORK/import.json" >/dev/null
grep -q '"stage": "import"' "$WORK/import.json"

step "4b. idempotent re-import (planned must be 0)"
"$BIN" pg-import --sqlite "$SAMPLE_DB" --dry-run --report "$WORK/rerun.json" >/dev/null
if python3 -c '
import json,sys
report=json.load(open(sys.argv[1]))
tasks=[t for t in report["tables"] if t["source_table"]=="tasks"][0]
sys.exit(0 if tasks["planned"]==0 and tasks["already_imported"]==2 else 1)' "$WORK/rerun.json"; then
  echo "re-import is a no-op ✓"
else
  echo "FAIL: re-import planned new rows" >&2
  exit 1
fi

step "5. reconcile"
"$BIN" pg-import --sqlite "$SAMPLE_DB" --reconcile --report "$WORK/reconcile.json" >/dev/null
python3 -c '
import json,sys
report=json.load(open(sys.argv[1]))
assert report["stage"]=="reconcile"
drift=[t for t in report["tables"] if t["source_table"]=="tasks"][0].get("status_drift",0)
assert drift==0, f"status drift: {drift}"
assert not [w for w in report.get("warnings",[]) if "violation" in w], report["warnings"]
print("reconcile clean: coverage + status + invariants ✓")' "$WORK/reconcile.json"

step "6. rollback drill (revert -> empty -> re-migrate)"
"$BIN" migrate revert --steps 1
"$BIN" migrate up
"$BIN" doctor >/dev/null && echo "schema restored from empty ✓"

step "7. re-import after re-migrate still quarantines the bad row and imports the rest"
"$BIN" pg-import --sqlite "$SAMPLE_DB" --report "$WORK/reimport.json" >/dev/null
python3 -c '
import json,sys
report=json.load(open(sys.argv[1]))
tasks=[t for t in report["tables"] if t["source_table"]=="tasks"][0]
assert tasks["planned"]==2 and tasks["quarantined"]==1, tasks
print("re-import after revert: 2 imported, 1 quarantined ✓")' "$WORK/reimport.json"

step "8. cleanup drill data"
docker exec "$DRILL_PG" psql -U maestro -d maestro -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;' >/dev/null
"$BIN" migrate up >/dev/null && echo "drill database reset ✓"

printf '\nALL DRILL STEPS PASSED\n'
