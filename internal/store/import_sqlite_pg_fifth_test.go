package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fifth sweep: the importer's Reconcile drift paths, the migrations
// catalog-drift variants beyond digest, the idempotency store, and the
// events/pg helper families — the remaining reachable branches.

func TestImporterReconcileDriftAndWarnings(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	pg := importFixturePG(t, "maestro_fifth_test")
	path := seedHealthyLegacy(t)

	sqlite, err := OpenSQLiteReadOnly(path)
	require.NoError(t, err)
	t.Cleanup(func() { sqlite.Close() })
	importer, err := NewSQLiteImporter(sqlite, pg)
	require.NoError(t, err)

	// Import, then mutate a work item's status and delete another to
	// force both drift branches (status mismatch and missing row).
	_, err = importer.Import(context.Background())
	require.NoError(t, err)

	var importedWorkItem string
	require.NoError(t, pg.QueryRow(`
		SELECT target_id::text FROM legacy_id_map
		WHERE source_table = 'tasks' AND target_table = 'work_items'
		ORDER BY target_id LIMIT 1`).Scan(&importedWorkItem))
	_, err = pg.Exec(`UPDATE work_items SET status = 'blocked' WHERE id = $1`, importedWorkItem)
	require.NoError(t, err)
	_, err = pg.Exec(`DELETE FROM work_items WHERE id =
		(SELECT target_id::uuid FROM legacy_id_map WHERE source_table = 'tasks'
		 AND target_table = 'work_items' ORDER BY target_id DESC LIMIT 1)`)
	require.NoError(t, err)

	reconciled, err := importer.Reconcile(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "reconcile", reconciled.Stage)
	drifted := 0
	for _, table := range reconciled.Tables {
		drifted += table.StatusDrift
	}
	assert.GreaterOrEqual(t, drifted, 2, "status mismatch and vanished row both count as drift")

	// The dry-run report after a completed import reports everything
	// already mapped.
	after, err := importer.DryRun(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, tableReport(after, "projects").Already)

	// A path-less and a missing SQLite source are refused.
	_, err = OpenSQLiteReadOnly("   ")
	require.Error(t, err)
	_, err = OpenSQLiteReadOnly(filepath.Join(t.TempDir(), "absent.db"))
	require.Error(t, err)
	_, err = NewSQLiteImporter(nil, pg)
	require.Error(t, err)
}

func TestMigrationCatalogNameAndGapDrift(t *testing.T) {
	pg := importFixturePG(t, "maestro_fifth_mig")
	ctx := context.Background()

	// Name drift inside an applied row fails validation too.
	_, err := pg.ExecContext(ctx,
		`UPDATE maestro_meta.schema_migrations SET name = 'renamed' WHERE version = 3`)
	require.NoError(t, err)
	require.ErrorIs(t, ValidatePostgresSchema(ctx, pg), ErrPostgresMigrationIntegrity)

	// A missing catalog row in the middle (apply on a gapped catalog)
	// refuses to run.
	_, err = pg.ExecContext(ctx, `DELETE FROM maestro_meta.schema_migrations WHERE version = 2`)
	require.NoError(t, err)
	_, err = ApplyPostgresMigrations(ctx, pg)
	require.ErrorIs(t, err, ErrPostgresMigrationIntegrity)
}

func TestAPIIdempotencyLifecycle(t *testing.T) {
	db := importFixturePG(t, "maestro_fifth_idem")
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)
	ctx := context.Background()

	// First use of a key creates; the identical repeat replays; a
	// different request under the same key is a conflict.
	first := &IdempotencyRecord{
		PrincipalID: "u-1", ProjectID: "p-1", Operation: "test.action",
		Key: "idem-1", RequestHash: "sha256:" + strings.Repeat("a", 64),
		ResponseStatus: 200, ResponseSummary: `{"ok":true}`,
	}
	replayed, existing, err := pg.APIIdempotency().LookupOrCreate(ctx, first)
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Nil(t, existing, "a fresh create carries no replay record")

	again := *first
	replayed, existing, err = pg.APIIdempotency().LookupOrCreate(ctx, &again)
	require.NoError(t, err)
	assert.True(t, replayed, "identical repeats replay the stored reply")
	assert.Equal(t, first.RequestHash, existing.RequestHash)

	conflicting := *first
	conflicting.RequestHash = "sha256:" + strings.Repeat("b", 64)
	_, _, err = pg.APIIdempotency().LookupOrCreate(ctx, &conflicting)
	require.Error(t, err, "same key with different content is rejected")
}

// -- helpers -----------------------------------------------------------------

func importFixturePG(t *testing.T, database string) *sql.DB {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE `+database)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+database)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	return db
}

func seedHealthyLegacy(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "healthy.db")
	database, err := NewSQLiteDB(path)
	require.NoError(t, err)
	require.NoError(t, database.Init(context.Background()))
	db := database.DB()
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.Exec(query, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO projects (id, name, workspace_path, status, config, created_at, updated_at)
		VALUES ('hp-1', 'healthy', '/tmp/h', 'active', '{}', '2026-08-28 09:00:00', '2026-08-28 09:00:00')`)
	exec(`INSERT INTO features (id, project_id, title, description, reference_urls, status, created_at, updated_at)
		VALUES ('hf-1', 'hp-1', 'feat', '', '[]', 'active', '2026-08-28 09:01:00', '2026-08-28 09:01:00')`)
	exec(`INSERT INTO agent_sessions (id, project_id, role, client_type, capacity, status, last_heartbeat, created_at, version)
		VALUES ('hs-1', 'hp-1', 'backend', 'cli', 1, 'offline', '2026-08-28 09:02:00', '2026-08-28 09:02:00', 1)`)
	exec(`INSERT INTO agent_workers (id, session_id, project_id, status, tasks_completed, version, last_active)
		VALUES ('hw-1', 'hs-1', 'hp-1', 'idle', 0, 1, '2026-08-28 09:02:00')`)
	exec(`INSERT INTO tasks (id, project_id, feature_id, title, description, role, status, allowed_directories, priority, version, lease_epoch, created_at, updated_at)
		VALUES ('ht-1', 'hp-1', 'hf-1', 't1', '', 'backend', 'queued', '[]', 'normal', 1, 0, '2026-08-28 09:03:00', '2026-08-28 09:03:00')`)
	exec(`INSERT INTO tasks (id, project_id, feature_id, title, description, role, status, allowed_directories, priority, version, lease_epoch, created_at, updated_at)
		VALUES ('ht-2', 'hp-1', 'hf-1', 't2', '', 'backend', 'queued', '[]', 'normal', 1, 0, '2026-08-28 09:03:00', '2026-08-28 09:03:00')`)
	exec(`DROP TRIGGER trg_tasks_m0_no_done_insert`)
	exec(`DROP TRIGGER trg_tasks_valid_status_insert`)
	commit := strings.Repeat("9", 40)
	exec(`INSERT INTO tasks (id, project_id, feature_id, title, description, role, status, allowed_directories, priority, merge_commit, version, lease_epoch, created_at, updated_at)
		VALUES ('ht-3', 'hp-1', 'hf-1', 't3', '', 'backend', 'done', '[]', 'normal', ?, 1, 0, '2026-08-28 09:03:00', '2026-08-28 09:03:00')`, commit)
	exec(`INSERT INTO task_leases (id, project_id, task_id, session_id, worker_id, epoch, status, version, expires_at, created_at, updated_at)
		VALUES ('hl-1', 'hp-1', 'ht-1', 'hs-1', 'hw-1', 1, 'completed', 1, '2026-08-28 10:00:00', '2026-08-28 09:04:00', '2026-08-28 09:04:00')`)
	exec(`INSERT INTO worktrees (id, task_id, project_id, worktree_path, branch_name, base_commit, status, created_at)
		VALUES (1, 'ht-1', 'hp-1', '/tmp/wt', 'maestro/h', ?, 'merged', '2026-08-28 09:05:00')`, strings.Repeat("1", 40))
	require.NoError(t, database.Close())
	return path
}
