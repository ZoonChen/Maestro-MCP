package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// PostgreSQL migration runner for the M1 control plane. The discipline
// mirrors the SQLite schema catalog: every applied migration is recorded in
// schema_migrations with the sha256 digest of its up+down content, and any
// drift between the catalog and the embedded files fails closed instead of
// being silently repaired. Applications run under a session-level advisory
// lock so concurrent migrators (or a racing control-plane start) serialize
// instead of racing DDL.
//
//go:embed migrations/postgresql/*.sql
var postgresMigrationFS embed.FS

// ErrPostgresMigrationIntegrity identifies a schema_migrations catalog that
// does not match the embedded migration set. A live runtime must never
// attempt to repair this condition; restore from a verified backup instead.
var ErrPostgresMigrationIntegrity = errors.New("postgres schema migration integrity check failed")

// PostgresMigration is one numbered, digest-fixed schema step.
type PostgresMigration struct {
	Version int64
	Name    string
	UpSQL   string
	DownSQL string
	Digest  string
}

// ParsePostgresMigrations loads the embedded .sql files and pairs up/down
// halves. Versions must be unique, contiguous is NOT required (gaps allowed),
// and every up file must have exactly one down counterpart.
func ParsePostgresMigrations() ([]PostgresMigration, error) {
	entries, err := fs.Glob(postgresMigrationFS, "migrations/postgresql/*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob postgres migrations: %w", err)
	}
	ups := map[int64]string{}
	downs := map[int64]string{}
	names := map[int64]string{}
	for _, entry := range entries {
		base := path.Base(entry)
		data, readErr := postgresMigrationFS.ReadFile(entry)
		if readErr != nil {
			return nil, fmt.Errorf("read migration %s: %w", base, readErr)
		}
		parts := strings.SplitN(base, "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("migration file %s must be named NNNN_name.(up|down).sql", base)
		}
		version, convErr := strconv.ParseInt(parts[0], 10, 64)
		if convErr != nil || version < 1 {
			return nil, fmt.Errorf("migration file %s must start with a positive number", base)
		}
		rest := parts[1]
		switch {
		case strings.HasSuffix(rest, ".up.sql"):
			ups[version] = string(data)
			names[version] = strings.TrimSuffix(rest, ".up.sql")
		case strings.HasSuffix(rest, ".down.sql"):
			downs[version] = string(data)
		default:
			return nil, fmt.Errorf("migration file %s must end in .up.sql or .down.sql", base)
		}
	}

	migrations := make([]PostgresMigration, 0, len(ups))
	for version, name := range names {
		down, ok := downs[version]
		if !ok {
			return nil, fmt.Errorf("migration %d (%s) has no .down.sql counterpart", version, name)
		}
		digest := sha256.Sum256([]byte(ups[version] + "\x00" + down))
		migrations = append(migrations, PostgresMigration{
			Version: version,
			Name:    name,
			UpSQL:   ups[version],
			DownSQL: down,
			Digest:  fmt.Sprintf("sha256:%s", fmt.Sprintf("%x", digest)),
		})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	for index, migration := range migrations {
		if index > 0 && migrations[index-1].Version == migration.Version {
			return nil, fmt.Errorf("duplicate migration version %d", migration.Version)
		}
	}
	if len(migrations) == 0 {
		return nil, errors.New("no postgres migrations embedded")
	}
	return migrations, nil
}

// CurrentPostgresSchemaVersion returns the highest embedded migration
// version (the target schema version).
func CurrentPostgresSchemaVersion() (int64, error) {
	migrations, err := ParsePostgresMigrations()
	if err != nil {
		return 0, err
	}
	return migrations[len(migrations)-1].Version, nil
}

const postgresAdvisoryLockQuery = `SELECT pg_advisory_lock(hashtext(current_database() || ':maestro_schema_migration'))`

// reserveMigrationConnection takes the advisory lock and pins search_path to
// public so unqualified DDL in migration files always lands in the public
// schema regardless of the login user's $user resolution. Callers defer
// releaseMigrationConnection on the same connection.
func reserveMigrationConnection(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, postgresAdvisoryLockQuery); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `SET search_path TO public`); err != nil {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtext(current_database() || ':maestro_schema_migration'))`)
		return fmt.Errorf("pin migration search_path: %w", err)
	}
	return nil
}

func releaseMigrationConnection(ctx context.Context, conn *sql.Conn) {
	_, _ = conn.ExecContext(context.WithoutCancel(ctx),
		`SELECT pg_advisory_unlock(hashtext(current_database() || ':maestro_schema_migration'))`)
}

type appliedMigration struct {
	version int64
	name    string
	digest  string
}

// ApplyPostgresMigrations brings the schema to the newest embedded version.
// It bootstraps schema_migrations, verifies the catalog is a digest-matching
// prefix of the embedded set, and applies the remainder — one transaction
// per migration on the lock-holding connection.
func ApplyPostgresMigrations(ctx context.Context, db *sql.DB) (int, error) {
	migrations, err := ParsePostgresMigrations()
	if err != nil {
		return 0, err
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("reserve migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, postgresAdvisoryLockQuery); err != nil {
		return 0, fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtext(current_database() || ':maestro_schema_migration'))`)
	}()

	// The catalog lives in its own schema so a migration's down script may
	// legitimately drop and recreate the public schema without destroying
	// the record of what was applied. The schema is deliberately not named
	// after the application: a catalog schema matching the login user would
	// shadow `public` through search_path's $user resolution.
	if _, err := conn.ExecContext(ctx, `
		CREATE SCHEMA IF NOT EXISTS maestro_meta;
		CREATE TABLE IF NOT EXISTS maestro_meta.schema_migrations (
			version    bigint PRIMARY KEY,
			name       text NOT NULL,
			digest     text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return 0, fmt.Errorf("bootstrap schema_migrations: %w", err)
	}

	applied, err := loadAppliedMigrations(ctx, conn)
	if err != nil {
		return 0, err
	}
	if err := verifyCatalogPrefix(migrations, applied); err != nil {
		return 0, err
	}

	pending := len(migrations) - len(applied)
	for _, migration := range migrations[len(applied):] {
		tx, txErr := conn.BeginTx(ctx, nil)
		if txErr != nil {
			return 0, fmt.Errorf("begin migration %d: %w", migration.Version, txErr)
		}
		if _, execErr := tx.ExecContext(ctx, migration.UpSQL); execErr != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("apply migration %d (%s): %w", migration.Version, migration.Name, execErr)
		}
		if _, execErr := tx.ExecContext(ctx,
			`INSERT INTO maestro_meta.schema_migrations (version, name, digest) VALUES ($1, $2, $3)`,
			migration.Version, migration.Name, migration.Digest); execErr != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("record migration %d: %w", migration.Version, execErr)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return 0, fmt.Errorf("commit migration %d: %w", migration.Version, commitErr)
		}
	}
	return pending, nil
}

// RevertPostgresMigrations rolls back up to steps migrations (newest first)
// by executing the recorded .down.sql. Rollback is a pre-cutover drill
// affordance; after cutover, recovery is PITR/forward-fix only (ADR-002).
func RevertPostgresMigrations(ctx context.Context, db *sql.DB, steps int) (int, error) {
	if steps < 1 {
		return 0, errors.New("revert steps must be positive")
	}
	migrations, err := ParsePostgresMigrations()
	if err != nil {
		return 0, err
	}
	byVersion := map[int64]PostgresMigration{}
	for _, migration := range migrations {
		byVersion[migration.Version] = migration
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("reserve migration connection: %w", err)
	}
	defer conn.Close()

	if err := reserveMigrationConnection(ctx, conn); err != nil {
		return 0, err
	}
	defer releaseMigrationConnection(ctx, conn)

	applied, err := loadAppliedMigrations(ctx, conn)
	if err != nil {
		return 0, err
	}
	if err := verifyCatalogPrefix(migrations, applied); err != nil {
		return 0, err
	}

	reverted := 0
	for index := len(applied) - 1; index >= 0 && reverted < steps; index-- {
		row := applied[index]
		migration, ok := byVersion[row.version]
		if !ok {
			return reverted, fmt.Errorf("applied migration %d is not embedded: %w", row.version, ErrPostgresMigrationIntegrity)
		}
		tx, txErr := conn.BeginTx(ctx, nil)
		if txErr != nil {
			return reverted, fmt.Errorf("begin revert %d: %w", row.version, txErr)
		}
		if _, execErr := tx.ExecContext(ctx, migration.DownSQL); execErr != nil {
			_ = tx.Rollback()
			return reverted, fmt.Errorf("revert migration %d (%s): %w", row.version, migration.Name, execErr)
		}
		if _, execErr := tx.ExecContext(ctx,
			`DELETE FROM maestro_meta.schema_migrations WHERE version = $1`, row.version); execErr != nil {
			_ = tx.Rollback()
			return reverted, fmt.Errorf("unrecord migration %d: %w", row.version, execErr)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return reverted, fmt.Errorf("commit revert %d: %w", row.version, commitErr)
		}
		reverted++
	}
	return reverted, nil
}

// ValidatePostgresSchema verifies the applied catalog exactly matches the
// embedded migration set (digest included) without applying anything.
func ValidatePostgresSchema(ctx context.Context, db *sql.DB) error {
	migrations, err := ParsePostgresMigrations()
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve schema validation connection: %w", err)
	}
	defer conn.Close()

	applied, err := loadAppliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	if len(applied) != len(migrations) {
		return fmt.Errorf("%w: applied=%d expected=%d", ErrPostgresMigrationIntegrity, len(applied), len(migrations))
	}
	return verifyCatalogPrefix(migrations, applied)
}

func loadAppliedMigrations(ctx context.Context, conn *sql.Conn) ([]appliedMigration, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version, name, digest FROM maestro_meta.schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	var applied []appliedMigration
	for rows.Next() {
		var row appliedMigration
		if scanErr := rows.Scan(&row.version, &row.name, &row.digest); scanErr != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", scanErr)
		}
		applied = append(applied, row)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", rowsErr)
	}
	return applied, nil
}

// verifyCatalogPrefix enforces two invariants: the applied catalog is a
// prefix of the embedded set (no holes, no unknown versions) and every
// applied digest matches the embedded file byte-for-byte.
func verifyCatalogPrefix(migrations []PostgresMigration, applied []appliedMigration) error {
	if len(applied) > len(migrations) {
		return fmt.Errorf("%w: catalog has %d rows but only %d migrations are embedded",
			ErrPostgresMigrationIntegrity, len(applied), len(migrations))
	}
	for index, row := range applied {
		expected := migrations[index]
		if row.version != expected.Version {
			return fmt.Errorf("%w: catalog row %d has version %d, expected %d (holes are not allowed)",
				ErrPostgresMigrationIntegrity, index, row.version, expected.Version)
		}
		if row.name != expected.Name {
			return fmt.Errorf("%w: migration %d name drift %q != %q",
				ErrPostgresMigrationIntegrity, row.version, row.name, expected.Name)
		}
		if row.digest != expected.Digest {
			return fmt.Errorf("%w: migration %d digest drift (applied content differs from embedded file)",
				ErrPostgresMigrationIntegrity, row.version)
		}
	}
	return nil
}
