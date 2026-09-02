package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A second importer fixture shape: every quarantine branch the happy
// fixture never trips (unmappable statuses, invalid JSON, dangling
// references, unparseable timestamps, done-without-merge-commit) plus
// the drift counters. Quarantined rows are the human checklist — the
// importer must record each reason and still import the healthy rest.

func TestSQLiteImporterQuarantineSweep(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_quarantine_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_quarantine_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_quarantine_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	pg, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_quarantine_test")
	require.NoError(t, err)
	t.Cleanup(func() { pg.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), pg)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "quarantine.db")
	database, err := NewSQLiteDB(path)
	require.NoError(t, err)
	require.NoError(t, database.Init(context.Background()))
	db := database.DB()
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.Exec(query, args...)
		require.NoError(t, err)
	}

	// One healthy project; everything else dangles off it or is broken.
	exec(`INSERT INTO projects (id, name, workspace_path, status, config, created_at, updated_at)
		VALUES ('p-ok', 'ok', '/tmp/ok', 'active', '{}', '2026-08-28 09:00:00', '2026-08-28 09:00:00')`)
	exec(`INSERT INTO projects (id, name, workspace_path, status, config, created_at, updated_at)
		VALUES ('p-weird', 'weird', '/tmp/w', 'paused', '{}', '2026-08-28 09:00:00', '2026-08-28 09:00:00')`)
	exec(`INSERT INTO projects (id, name, workspace_path, status, config, created_at, updated_at)
		VALUES ('p-badjson', 'badjson', '/tmp/j', 'active', '{not json', '2026-08-28 09:00:00', '2026-08-28 09:00:00')`)
	exec(`INSERT INTO projects (id, name, workspace_path, status, config, created_at, updated_at)
		VALUES ('p-badtime', 'badtime', '/tmp/t', 'active', '{}', 'not-a-time', '2026-08-28 09:00:00')`)

	exec(`INSERT INTO features (id, project_id, title, description, reference_urls, status, created_at, updated_at)
		VALUES ('f-ok', 'p-ok', 'fine', '', '[]', 'active', '2026-08-28 09:01:00', '2026-08-28 09:01:00')`)
	exec(`INSERT INTO features (id, project_id, title, description, reference_urls, status, created_at, updated_at)
		VALUES ('f-orphan', 'p-weird', 'orphan', '', '[]', 'active', '2026-08-28 09:01:00', '2026-08-28 09:01:00')`)
	exec(`INSERT INTO features (id, project_id, title, description, reference_urls, status, created_at, updated_at)
		VALUES ('f-status', 'p-ok', 'status', '', '[]', 'frozen', '2026-08-28 09:01:00', '2026-08-28 09:01:00')`)
	exec(`INSERT INTO features (id, project_id, title, description, reference_urls, status, created_at, updated_at)
		VALUES ('f-json', 'p-ok', 'json', '', '[oops', 'active', '2026-08-28 09:01:00', '2026-08-28 09:01:00')`)

	mkTask := func(id, feature, status, priority, createdAt string, mergeCommit *string) {
		t.Helper()
		mc := ""
		if mergeCommit != nil {
			mc = *mergeCommit
		}
		exec(`INSERT INTO tasks (id, project_id, feature_id, title, description, role, status, allowed_directories, priority, merge_commit, version, lease_epoch, created_at, updated_at)
			VALUES (?, 'p-ok', ?, ?, '', 'backend', ?, '[]', ?, ?, 1, 0, ?, ?)`, id, feature, "t-"+id, status, priority, mc, createdAt, createdAt)
	}
	doneCommit := "abc123def456"
	// Both done rows predate the guard trigger (legacy databases; the
	// importer's quarantine exists exactly for them).
	exec(`DROP TRIGGER trg_tasks_m0_no_done_insert`)
	mkTask("t-done-ok", "f-ok", "done", "normal", "2026-08-28 09:02:00", &doneCommit)
	mkTask("t-done-nomerge", "f-ok", "done", "normal", "2026-08-28 09:02:00", nil)
	exec(`DROP TRIGGER trg_tasks_valid_status_insert`)
	mkTask("t-weird", "f-ok", "weird_state", "normal", "2026-08-28 09:02:00", nil)
	exec(`DROP TRIGGER trg_tasks_project_scope_insert`)
	mkTask("t-orphanfeat", "f-orphan", "executing", "normal", "2026-08-28 09:02:00", nil)
	mkTask("t-badtime", "f-ok", "executing", "normal", "not-a-time", nil)

	exec(`DROP TRIGGER trg_worktrees_valid_status_insert`)
	exec(`DROP TRIGGER trg_worktrees_project_scope_insert`)
	exec(`INSERT INTO worktrees (id, task_id, project_id, worktree_path, branch_name, base_commit, status, created_at)
		VALUES (1, 't-done-ok', 'p-ok', '/tmp/w1', 'maestro/x', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'floating', '2026-08-28 09:04:00')`)
	require.NoError(t, database.Close())

	sqlite, err := OpenSQLiteReadOnly(path)
	require.NoError(t, err)
	defer sqlite.Close()
	importer, err := NewSQLiteImporter(sqlite, pg)
	require.NoError(t, err)
	plan, err := importer.DryRun(context.Background())
	require.NoError(t, err)

	reasons := map[string]string{}
	for _, row := range plan.Quarantine {
		reasons[row.SourceTable+"/"+row.SourceID] = row.Reason
	}
	for _, want := range []string{
		"projects/p-weird", "projects/p-badjson", "projects/p-badtime",
		"features/f-orphan", "features/f-status", "features/f-json",
		"tasks/t-weird", "worktrees/1", "tasks/t-orphanfeat", "tasks/t-badtime",
		"worktrees/1",
	} {
		assert.Contains(t, reasons, want, "expected quarantine for %s", want)
	}
	assert.NotContains(t, reasons, "tasks/t-done-ok", "done with a merge commit imports")
	assert.NotContains(t, reasons, "projects/p-ok", "the healthy project imports")

	// The import writes exactly the healthy set; reconcile then proves
	// the checksum discipline over the imported rows.
	result, err := importer.Import(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, tableReport(result, "projects").Planned)

	again, err := importer.Import(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, tableReport(again, "projects").Already, "second import maps through legacy_id_map")

	reconciled, err := importer.Reconcile(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "reconcile", reconciled.Stage)
	for _, table := range reconciled.Tables {
		assert.Zero(t, table.StatusDrift, "no drift in %s", table.SourceTable)
	}
}
