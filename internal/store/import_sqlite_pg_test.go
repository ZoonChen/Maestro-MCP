package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSQLiteTimestampFormats(t *testing.T) {
	parsed, err := parseSQLiteTimestamp("2026-08-28 09:00:00")
	require.NoError(t, err)
	assert.Equal(t, 2026, parsed.UTC().Year())

	parsed, err = parseSQLiteTimestamp("2026-08-28T09:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, 2026, parsed.UTC().Year())

	_, err = parseSQLiteTimestamp("")
	assert.Error(t, err)

	_, err = parseSQLiteTimestamp("not-a-timestamp")
	assert.Error(t, err)
}

func TestValidJSONOrDefaults(t *testing.T) {
	value, err := validJSONOr("", "[]")
	require.NoError(t, err)
	assert.Equal(t, "[]", value)

	value, err = validJSONOr(`["a"]`, "[]")
	require.NoError(t, err)
	assert.Equal(t, `["a"]`, value)

	_, err = validJSONOr("{not json", "{}")
	assert.Error(t, err)
}

func TestSlugifyProjectKeyDeterministicAndUnique(t *testing.T) {
	used := map[string]struct{}{}
	first := slugifyProjectKey("Drill Project", "proj-1", used)
	second := slugifyProjectKey("Drill Project", "proj-2", used)
	assert.Regexp(t, `^[a-z][a-z0-9-]{2,31}$`, first)
	assert.Regexp(t, `^[a-z][a-z0-9-]{2,31}$`, second)
	assert.NotEqual(t, first, second, "same-name projects must get distinct keys")

	fallback := slugifyProjectKey("!!", "weird id", map[string]struct{}{})
	assert.Regexp(t, `^[a-z][a-z0-9-]{2,31}$`, fallback)
}

// seedImportFixture builds a frozen SQLite database with the current M0
// schema: two valid tasks, one legacy-status task, one invalid-state task
// that must be quarantined, a lease, a worktree and a validation run.
func seedImportFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.db")
	database, err := NewSQLiteDB(path)
	require.NoError(t, err)
	require.NoError(t, database.Init(context.Background()))
	db := database.DB()

	exec := func(query string, args ...any) {
		_, err := db.Exec(query, args...)
		require.NoError(t, err)
	}
	// The fixture simulates a pre-v2 legacy source: the M0 insert guard
	// would reject the legacy/invalid rows the importer must handle.
	exec(`DROP TRIGGER IF EXISTS trg_tasks_valid_status_insert`)
	exec(`INSERT INTO projects (id, name, workspace_path, description, status, config, created_at, updated_at)
	      VALUES ('proj-1', 'Drill Project', '/tmp/drill', 'sample', 'active', '{}', '2026-08-28 09:00:00', '2026-08-28 09:00:00')`)
	exec(`INSERT INTO agent_sessions (id, project_id, role, client_type, capacity, status, last_heartbeat, created_at, version, external_id)
	      VALUES ('sess-1', 'proj-1', 'backend', 'cli', 1, 'offline', '2026-08-28 09:00:00', '2026-08-28 09:00:00', 0, 'sess-1')`)
	exec(`INSERT INTO features (id, project_id, title, description, reference_urls, status, created_at, updated_at)
	      VALUES ('feat-1', 'proj-1', 'Sample feature', '', '[]', 'planning', '2026-08-28 09:01:00', '2026-08-28 09:01:00')`)
	exec(`INSERT INTO tasks (id, project_id, feature_id, title, description, role, status, allowed_directories, priority, version, lease_epoch, created_at, updated_at)
	      VALUES ('task-1', 'proj-1', 'feat-1', 'Queued task', '', 'backend', 'queued', '[]', 'normal', 0, 0, '2026-08-28 09:02:00', '2026-08-28 09:02:00')`)
	exec(`INSERT INTO tasks (id, project_id, feature_id, title, description, role, status, allowed_directories, priority, version, lease_epoch, created_at, updated_at)
	      VALUES ('task-2', 'proj-1', 'feat-1', 'Legacy pending task', '', 'frontend', 'pending', '[]', 'low', 0, 0, '2026-08-28 09:03:00', '2026-08-28 09:03:00')`)
	exec(`INSERT INTO tasks (id, project_id, feature_id, title, description, role, status, allowed_directories, priority, version, lease_epoch, created_at, updated_at)
	      VALUES ('task-bad', 'proj-1', 'feat-1', 'Invalid state', '', 'backend', 'weird_state', '[]', 'normal', 0, 0, '2026-08-28 09:04:00', '2026-08-28 09:04:00')`)
	exec(`INSERT INTO agent_workers (id, session_id, project_id, current_task_id, status, tasks_completed, version, last_active)
	      VALUES ('worker-1', 'sess-1', 'proj-1', NULL, 'idle', 0, 0, '2026-08-28 09:00:00')`)
	exec(`INSERT INTO task_leases (id, project_id, task_id, session_id, worker_id, epoch, status, version, expires_at, created_at, updated_at)
	      VALUES ('lease-1', 'proj-1', 'task-1', 'sess-1', 'worker-1', 1, 'completed', 1, '2026-08-28 10:00:00', '2026-08-28 09:05:00', '2026-08-28 09:30:00')`)
	exec(`INSERT INTO worktrees (id, task_id, project_id, session_id, worktree_path, branch_name, base_commit, status, generation, version, created_at, updated_at)
	      VALUES (1, 'task-1', 'proj-1', 'sess-1', '/tmp/drill/wt-1', 'maestro/drill/task-1', 'abc123', 'abandoned', 1, 0, '2026-08-28 09:06:00', '2026-08-28 09:40:00')`)
	exec(`INSERT INTO validation_runs (id, task_id, project_id, attempt, base_commit, changed_files, test_command, test_exit_code, test_output, coverage, boundary_ok, test_ok, coverage_ok, summary, result, error_code, duration_ms, log_path, created_at)
	      VALUES (1, 'task-1', 'proj-1', 1, 'abc123', '["main.go"]', 'go test ./...', 0, 'ok', 0.75, 1, 1, 1, '{}', 'passed', NULL, 1200, NULL, '2026-08-28 09:07:00')`)
	require.NoError(t, database.Close())
	return path
}

func TestSQLiteImporterStagesOnPostgres(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	pg := testPostgresDB(t)

	// Isolation: fresh schema for this test.
	_, err := pg.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;`)
	require.NoError(t, err)
	_, err = ApplyPostgresMigrations(ctx, pg)
	require.NoError(t, err)

	fixture := seedImportFixture(t)
	sqliteDB, err := OpenSQLiteReadOnly(fixture)
	require.NoError(t, err)
	defer sqliteDB.Close()

	importer, err := NewSQLiteImporter(sqliteDB, pg)
	require.NoError(t, err)

	// Dry-run quarantines the invalid task and plans the rest.
	dryRun, err := importer.DryRun(ctx)
	require.NoError(t, err)
	require.Equal(t, "dry-run", dryRun.Stage)
	assert.True(t, quarantineContains(dryRun, "tasks", "task-bad"), "invalid state must be quarantined")
	tasksDry := tableReport(dryRun, "tasks")
	assert.Equal(t, 3, tasksDry.SourceCount)
	assert.Equal(t, 2, tasksDry.Planned)
	assert.Equal(t, 1, tasksDry.Quarantined)

	// Import applies the plan in one transaction.
	imported, err := importer.Import(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, tableReport(imported, "tasks").Planned)

	// Legacy status normalization: pending -> queued.
	var normalized int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT count(*) FROM work_items wi JOIN legacy_id_map m
		 ON m.source_table='tasks' AND m.target_table='work_items' AND m.target_id::uuid = wi.id
		 WHERE wi.status = 'queued'`).Scan(&normalized))
	assert.Equal(t, 2, normalized, "both importable tasks land as queued")

	// Re-run import: no new rows, everything already mapped.
	again, err := importer.Import(ctx)
	require.NoError(t, err)
	tasksAgain := tableReport(again, "tasks")
	assert.Equal(t, 0, tasksAgain.Planned)
	assert.Equal(t, 2, tasksAgain.Already)

	// Reconcile: coverage + status projection clean.
	reconciled, err := importer.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, tableReport(reconciled, "tasks").StatusDrift)
	assert.NotEmpty(t, reconciled.ManualChecklist, "ownerless projects must surface a manual checklist")

	// The importer must never write to the frozen source.
	var sourceTasks int
	require.NoError(t, sqliteDB.QueryRow(`SELECT count(*) FROM tasks`).Scan(&sourceTasks))
	assert.Equal(t, 3, sourceTasks)
}

func TestSQLiteReadOnlyRejectsMissingPath(t *testing.T) {
	_, err := OpenSQLiteReadOnly(" /tmp/does-not-exist-maestro.db")
	require.Error(t, err)
}

func TestImportReportEncodesAsJSON(t *testing.T) {
	report := &ImportReport{Stage: "dry-run", GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"stage":"dry-run"`)
}

func tableReport(report *ImportReport, source string) ImportTableReport {
	for _, table := range report.Tables {
		if table.SourceTable == source {
			return table
		}
	}
	return ImportTableReport{}
}

func quarantineContains(report *ImportReport, table, id string) bool {
	for _, entry := range report.Quarantine {
		if entry.SourceTable == table && entry.SourceID == id {
			return true
		}
	}
	return false
}
