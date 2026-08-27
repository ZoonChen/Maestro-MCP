package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchemaManifestRejectsForgedCurrentVersionWithoutRepair(t *testing.T) {
	database, err := NewSQLiteDB(filepath.Join(t.TempDir(), "forged-current.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	ctx := context.Background()
	_, err = database.DB().ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", CurrentSchemaVersion()))
	require.NoError(t, err)

	for _, validate := range []struct {
		name string
		call func() error
	}{
		{name: "runtime", call: func() error { return database.EnsureRuntimeSchema(ctx) }},
		{name: "doctor", call: func() error { return database.ValidateSchema(ctx) }},
		{name: "explicit migration", call: func() error { return database.Init(ctx) }},
	} {
		t.Run(validate.name, func(t *testing.T) {
			err := validate.call()
			require.ErrorIs(t, err, ErrSchemaIntegrity)
			var integrityErr *SchemaIntegrityError
			require.True(t, errors.As(err, &integrityErr))
			require.Equal(t, "manifest", integrityErr.Check)
		})
	}

	var version, objects int
	require.NoError(t, database.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version))
	require.Equal(t, CurrentSchemaVersion(), version)
	require.NoError(t, database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL`).Scan(&objects))
	require.Zero(t, objects, "integrity checks must not repair a forged current database")
}

func TestSchemaManifestRejectsMissingAndReplacedObjects(t *testing.T) {
	tests := []struct {
		name    string
		corrupt string
		assert  func(*testing.T, *SQLiteDB)
	}{
		{
			name:    "required table",
			corrupt: `DROP TABLE runtime_state`,
			assert: func(t *testing.T, database *SQLiteDB) {
				assertSchemaObjectCount(t, database, "table", "runtime_state", 0)
			},
		},
		{
			name:    "upgraded column",
			corrupt: `ALTER TABLE tasks DROP COLUMN lease_expires_at`,
			assert: func(t *testing.T, database *SQLiteDB) {
				var columns int
				require.NoError(t, database.DB().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks')
					WHERE name = 'lease_expires_at'`).Scan(&columns))
				require.Zero(t, columns)
			},
		},
		{
			name:    "append-only trigger",
			corrupt: `DROP TRIGGER trg_validation_runs_append_only_update`,
			assert: func(t *testing.T, database *SQLiteDB) {
				assertSchemaObjectCount(t, database, "trigger", "trg_validation_runs_append_only_update", 0)
			},
		},
		{
			name: "same-name no-op trigger",
			corrupt: `DROP TRIGGER trg_tasks_m0_no_done_insert;
				CREATE TRIGGER trg_tasks_m0_no_done_insert BEFORE INSERT ON tasks BEGIN SELECT 1; END;`,
			assert: func(t *testing.T, database *SQLiteDB) {
				var definition string
				require.NoError(t, database.DB().QueryRow(`SELECT sql FROM sqlite_schema
					WHERE type = 'trigger' AND name = 'trg_tasks_m0_no_done_insert'`).Scan(&definition))
				require.Contains(t, definition, "SELECT 1")
			},
		},
		{
			name: "same-name trigger with case-altered status literal",
			corrupt: `DROP TRIGGER trg_tasks_m0_no_done_insert;
				CREATE TRIGGER trg_tasks_m0_no_done_insert
				BEFORE INSERT ON tasks
				WHEN NEW.status = 'DONE'
				BEGIN
					SELECT RAISE(ABORT, 'M0_DONE_REQUIRES_VERIFIED_MERGE_FACT');
				END;`,
			assert: func(t *testing.T, database *SQLiteDB) {
				var definition string
				require.NoError(t, database.DB().QueryRow(`SELECT sql FROM sqlite_schema
					WHERE type = 'trigger' AND name = 'trg_tasks_m0_no_done_insert'`).Scan(&definition))
				require.Contains(t, definition, "'DONE'")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, err := NewSQLiteDB(filepath.Join(t.TempDir(), "damaged.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			ctx := context.Background()
			require.NoError(t, database.Init(ctx))
			_, err = database.DB().ExecContext(ctx, `INSERT INTO projects(id,name,workspace_path)
				VALUES('manifest-marker','preserve-me','/manifest-marker')`)
			require.NoError(t, err)
			_, err = database.DB().ExecContext(ctx, test.corrupt)
			require.NoError(t, err)

			require.ErrorIs(t, database.EnsureRuntimeSchema(ctx), ErrSchemaIntegrity)
			require.ErrorIs(t, database.ValidateSchema(ctx), ErrSchemaIntegrity)
			require.ErrorIs(t, database.Init(ctx), ErrSchemaIntegrity)
			test.assert(t, database)

			var marker string
			require.NoError(t, database.DB().QueryRowContext(ctx,
				`SELECT name FROM projects WHERE id = 'manifest-marker'`).Scan(&marker))
			require.Equal(t, "preserve-me", marker, "failed integrity checks must not change application data")
		})
	}
}

func TestSchemaManifestNormalizationPreservesSemanticCaseAndLiteralWhitespace(t *testing.T) {
	definition := " CREATE  TRIGGER x BEFORE INSERT ON t WHEN NEW.status = 'DoNe  status' BEGIN SELECT \"MiXeD\"; END; "
	normalized := normalizeSchemaDefinition(definition)
	require.Equal(t,
		`CREATE TRIGGER x BEFORE INSERT ON t WHEN NEW.status = 'DoNe  status' BEGIN SELECT "MiXeD"; END;`,
		normalized,
	)
}

func assertSchemaObjectCount(t *testing.T, database *SQLiteDB, objectType, name string, expected int) {
	t.Helper()
	var count int
	require.NoError(t, database.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE type = ? AND name = ?`, objectType, name).Scan(&count))
	require.Equal(t, expected, count)
}
