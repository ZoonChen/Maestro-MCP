package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/stretchr/testify/require"
)

func TestEnsureRuntimeSchemaBootstrapsOnlyEmptyDatabase(t *testing.T) {
	t.Run("empty database is bootstrapped", func(t *testing.T) {
		database, err := NewSQLiteDB(filepath.Join(t.TempDir(), "empty.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, database.Close()) })

		require.NoError(t, database.EnsureRuntimeSchema(context.Background()))
		var version int
		require.NoError(t, database.DB().QueryRow("PRAGMA user_version").Scan(&version))
		require.Equal(t, CurrentSchemaVersion(), version)
	})

	t.Run("unversioned existing schema is rejected without mutation", func(t *testing.T) {
		database, err := NewSQLiteDB(filepath.Join(t.TempDir(), "unversioned.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, database.Close()) })
		_, err = database.DB().Exec(`CREATE TABLE legacy_data (id TEXT PRIMARY KEY)`)
		require.NoError(t, err)

		err = database.EnsureRuntimeSchema(context.Background())
		require.ErrorIs(t, err, ErrSchemaVersionIncompatible)
		var versionErr *SchemaVersionError
		require.True(t, errors.As(err, &versionErr))
		require.Equal(t, 0, versionErr.Actual)
		require.Equal(t, CurrentSchemaVersion(), versionErr.Expected)
		require.False(t, versionErr.Empty)

		var version int
		require.NoError(t, database.DB().QueryRow("PRAGMA user_version").Scan(&version))
		require.Zero(t, version)
		var maestroTables int
		require.NoError(t, database.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_schema
			WHERE type = 'table' AND name = 'projects'`).Scan(&maestroTables))
		require.Zero(t, maestroTables, "runtime check must not apply bootstrap DDL to an existing schema")
	})
}

func TestEnsureRuntimeSchemaRequiresExplicitUpgrade(t *testing.T) {
	database, err := NewSQLiteDB(filepath.Join(t.TempDir(), "schema-v4.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	ctx := context.Background()
	for _, migration := range migrations {
		if migration.version > 4 {
			break
		}
		_, err := database.DB().ExecContext(ctx, migration.sql)
		require.NoError(t, err, "apply legacy migration v%d", migration.version)
	}
	_, err = database.DB().ExecContext(ctx, `PRAGMA user_version = 4`)
	require.NoError(t, err)

	err = database.EnsureRuntimeSchema(ctx)
	require.ErrorIs(t, err, ErrSchemaVersionIncompatible)
	var versionErr *SchemaVersionError
	require.True(t, errors.As(err, &versionErr))
	require.Equal(t, 4, versionErr.Actual)
	require.Equal(t, CurrentSchemaVersion(), versionErr.Expected)
	require.False(t, versionErr.Empty)

	var version, v5Triggers int
	require.NoError(t, database.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version))
	require.Equal(t, 4, version)
	require.NoError(t, database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'trigger' AND name = 'trg_tasks_m0_no_done_insert'`).Scan(&v5Triggers))
	require.Zero(t, v5Triggers, "live runtime must not apply pending DDL")

	require.NoError(t, database.Init(ctx), "explicit migration path must remain able to upgrade")
	require.NoError(t, database.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version))
	require.Equal(t, CurrentSchemaVersion(), version)
	require.NoError(t, database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'trigger' AND name = 'trg_tasks_m0_no_done_insert'`).Scan(&v5Triggers))
	require.Equal(t, 1, v5Triggers)
}

func TestEnsureRuntimeSchemaRejectsNewerDatabase(t *testing.T) {
	database, err := NewSQLiteDB(filepath.Join(t.TempDir(), "newer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	ctx := context.Background()
	require.NoError(t, database.Init(ctx))
	newerVersion := CurrentSchemaVersion() + 1
	_, err = database.DB().ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", newerVersion))
	require.NoError(t, err)

	err = database.EnsureRuntimeSchema(ctx)
	require.ErrorIs(t, err, ErrSchemaVersionIncompatible)
	var versionErr *SchemaVersionError
	require.True(t, errors.As(err, &versionErr))
	require.Equal(t, newerVersion, versionErr.Actual)
	require.Equal(t, CurrentSchemaVersion(), versionErr.Expected)

	var version int
	require.NoError(t, database.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version))
	require.Equal(t, newerVersion, version)
}

func TestSQLiteInitConcurrentIsIdempotent(t *testing.T) {
	t.Parallel()

	const migratorCount = 24
	databasePath := filepath.Join(t.TempDir(), "concurrent-migrate.db")
	start := make(chan struct{})
	errors := make(chan error, migratorCount)

	var workers sync.WaitGroup
	workers.Add(migratorCount)
	for worker := range migratorCount {
		go func() {
			defer workers.Done()
			<-start

			database, err := NewSQLiteDB(databasePath)
			if err != nil {
				errors <- fmt.Errorf("migrator %d open: %w", worker, err)
				return
			}
			defer database.Close()

			if err := database.Init(context.Background()); err != nil {
				errors <- fmt.Errorf("migrator %d init: %w", worker, err)
				return
			}
			// Each caller may safely retry after an uncertain command result.
			if err := database.Init(context.Background()); err != nil {
				errors <- fmt.Errorf("migrator %d retry: %w", worker, err)
			}
		}()
	}

	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}

	database, err := NewSQLiteDB(databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.NoError(t, database.Init(context.Background()))

	var version int
	require.NoError(t, database.DB().QueryRow("PRAGMA user_version").Scan(&version))
	require.Equal(t, CurrentSchemaVersion(), version)
	var journalMode string
	require.NoError(t, database.DB().QueryRow("PRAGMA journal_mode").Scan(&journalMode))
	require.Equal(t, "wal", journalMode)

	var integrity string
	require.NoError(t, database.DB().QueryRow("PRAGMA integrity_check").Scan(&integrity))
	require.Equal(t, "ok", integrity)

	rows, err := database.DB().Query("PRAGMA foreign_key_check")
	require.NoError(t, err)
	defer rows.Close()
	require.False(t, rows.Next(), "concurrent migration left a foreign-key violation")
	require.NoError(t, rows.Err())
}

func TestSchemaV5PreservesDoneHistoryAndBlocksNewDoneTransitions(t *testing.T) {
	database, err := NewSQLiteDB(filepath.Join(t.TempDir(), "schema-v4.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	ctx := context.Background()
	for _, migration := range migrations {
		if migration.version > 4 {
			break
		}
		_, err := database.DB().ExecContext(ctx, migration.sql)
		require.NoError(t, err, "apply legacy migration v%d", migration.version)
	}
	_, err = database.DB().ExecContext(ctx, `PRAGMA user_version = 4`)
	require.NoError(t, err)

	seedProjectAndFeature(t, database.DB())
	tasks := NewSQLiteTaskStore(database.DB())
	historical := newTestTask("T-v4-done-history", testProjectID, testFeatureID)
	historical.Status = model.TaskStatusDone
	mustCreateTask(t, tasks, historical)
	candidate := newTestTask("T-v5-no-local-done", testProjectID, testFeatureID)
	candidate.Status = model.TaskStatusReadyForHumanMerge
	mustCreateTask(t, tasks, candidate)

	require.NoError(t, database.Init(ctx))

	var historicalStatus string
	require.NoError(t, database.DB().QueryRowContext(ctx,
		`SELECT status FROM tasks WHERE project_id = ? AND id = ?`,
		testProjectID, historical.ID,
	).Scan(&historicalStatus))
	require.Equal(t, model.TaskStatusDone, historicalStatus)

	_, err = database.DB().ExecContext(ctx, `UPDATE tasks
		SET status = 'done', version = version + 1,
		    merged_fact_id = 'unverified-local-fact', merge_commit = ?
		WHERE project_id = ? AND id = ?`,
		strings.Repeat("a", 40), testProjectID, candidate.ID,
	)
	require.ErrorContains(t, err, "ILLEGAL_TASK_TRANSITION")

	injected := newTestTask("T-v5-no-done-insert", testProjectID, testFeatureID)
	injected.Status = model.TaskStatusDone
	require.ErrorContains(t, tasks.Create(ctx, testProjectID, injected), "M0_DONE_REQUIRES_VERIFIED_MERGE_FACT")

	var version int
	require.NoError(t, database.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version))
	require.Equal(t, CurrentSchemaVersion(), version)
}
