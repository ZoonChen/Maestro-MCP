package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePostgresMigrationsShape(t *testing.T) {
	migrations, err := ParsePostgresMigrations()
	require.NoError(t, err)
	require.NotEmpty(t, migrations)

	seenVersions := map[int64]bool{}
	for index, migration := range migrations {
		assert.False(t, seenVersions[migration.Version], "duplicate version %d", migration.Version)
		seenVersions[migration.Version] = true
		if index > 0 {
			assert.Greater(t, migration.Version, migrations[index-1].Version, "migrations must sort by version")
		}
		assert.NotEmpty(t, migration.Name)
		assert.True(t, strings.HasPrefix(migration.Digest, "sha256:"), "digest must be a sha256 hex string")
		assert.Len(t, strings.TrimPrefix(migration.Digest, "sha256:"), 64)
		assert.NotEmpty(t, migration.UpSQL)
		assert.NotEmpty(t, migration.DownSQL)
	}
}

func TestCurrentPostgresSchemaVersionMatchesFiles(t *testing.T) {
	migrations, err := ParsePostgresMigrations()
	require.NoError(t, err)
	target, err := CurrentPostgresSchemaVersion()
	require.NoError(t, err)
	assert.Equal(t, migrations[len(migrations)-1].Version, target)
}

// testPostgresDB gates PostgreSQL-backed tests on the dedicated service DSN
// wired by .github/workflows/m1-runtime.yml; without it the suite runs the
// SQLite baseline only.
func testPostgresDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	db, err := OpenPostgres(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPostgresMigrateApplyValidateRevert(t *testing.T) {
	db := testPostgresDB(t)
	ctx := context.Background()

	// Isolate: start from a clean schema regardless of prior runs.
	_, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;`)
	require.NoError(t, err)

	migrations, err := ParsePostgresMigrations()
	require.NoError(t, err)
	applied, err := ApplyPostgresMigrations(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, len(migrations), applied, "a fresh schema applies every embedded migration")

	require.NoError(t, ValidatePostgresSchema(ctx, db))

	// Re-apply is a no-op.
	applied, err = ApplyPostgresMigrations(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, 0, applied)

	// Digest tampering in the catalog fails closed.
	_, err = db.ExecContext(ctx, `UPDATE maestro_meta.schema_migrations SET digest = 'sha256:deadbeef'`)
	require.NoError(t, err)
	err = ValidatePostgresSchema(ctx, db)
	require.ErrorIs(t, err, ErrPostgresMigrationIntegrity)
	_, err = ApplyPostgresMigrations(ctx, db)
	require.ErrorIs(t, err, ErrPostgresMigrationIntegrity)

	// Restore every real digest, then revert everything and verify the
	// schema is gone (the envelope trigger fix rides along with the baseline).
	for _, migration := range migrations {
		_, err = db.ExecContext(ctx,
			`UPDATE maestro_meta.schema_migrations SET digest = $1 WHERE version = $2`,
			migration.Digest, migration.Version)
		require.NoError(t, err)
	}

	reverted, err := RevertPostgresMigrations(ctx, db, len(migrations))
	require.NoError(t, err)
	assert.Equal(t, len(migrations), reverted)

	var tables int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&tables))
	assert.Equal(t, 0, tables, "revert must leave no public objects behind")

	// Forward again for potential follow-up tests in this package.
	_, err = ApplyPostgresMigrations(ctx, db)
	require.NoError(t, err)
}
