package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrSchemaIntegrity identifies a database whose declared current version does
// not match the immutable M0 schema catalog or whose SQLite integrity checks
// fail. A live runtime must never attempt to repair this condition.
var ErrSchemaIntegrity = errors.New("database schema integrity check failed")

// SchemaIntegrityError names the failed integrity gate without exposing SQL or
// application data. It unwraps to ErrSchemaIntegrity for stable CLI mapping.
type SchemaIntegrityError struct {
	Check  string
	Detail string
}

func (e *SchemaIntegrityError) Error() string {
	return fmt.Sprintf("database schema integrity check failed: %s: %s", e.Check, e.Detail)
}

func (e *SchemaIntegrityError) Unwrap() error { return ErrSchemaIntegrity }

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type schemaObject struct {
	objectType string
	tableName  string
	digest     [sha256.Size]byte
}

var (
	expectedSchemaOnce     sync.Once
	expectedSchemaCatalog  map[string]schemaObject
	expectedSchemaBuildErr error
)

// ValidateSchema verifies that an existing database is exactly the current M0
// catalog. It never creates or alters a durable schema.
func (s *SQLiteDB) ValidateSchema(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve schema validation connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin schema validation transaction: %w", err)
	}
	inTransaction := true
	defer func() {
		if inTransaction {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version != currentSchemaVersion {
		empty, inspectErr := isEmptySchema(ctx, conn)
		if inspectErr != nil {
			return inspectErr
		}
		return &SchemaVersionError{Actual: version, Expected: currentSchemaVersion, Empty: empty}
	}
	if err := validateCurrentSchema(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit schema validation: %w", err)
	}
	inTransaction = false
	return nil
}

func validateCurrentSchema(ctx context.Context, queryer schemaQueryer) error {
	var foreignKeys int
	if err := queryer.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return schemaIntegrityFailure("foreign_keys", "could not read enforcement state")
	}
	if foreignKeys != 1 {
		return schemaIntegrityFailure("foreign_keys", "enforcement is disabled")
	}

	quickRows, err := queryer.QueryContext(ctx, "PRAGMA quick_check(1)")
	if err != nil {
		return schemaIntegrityFailure("quick_check", "check could not be executed")
	}
	quickOK := false
	for quickRows.Next() {
		var result string
		if err := quickRows.Scan(&result); err != nil {
			_ = quickRows.Close()
			return schemaIntegrityFailure("quick_check", "result could not be read")
		}
		if result == "ok" && !quickOK {
			quickOK = true
			continue
		}
		_ = quickRows.Close()
		return schemaIntegrityFailure("quick_check", "database pages are inconsistent")
	}
	if err := quickRows.Err(); err != nil {
		_ = quickRows.Close()
		return schemaIntegrityFailure("quick_check", "check did not complete")
	}
	if err := quickRows.Close(); err != nil {
		return schemaIntegrityFailure("quick_check", "result could not be closed")
	}
	if !quickOK {
		return schemaIntegrityFailure("quick_check", "check returned no successful result")
	}

	foreignKeyRows, err := queryer.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return schemaIntegrityFailure("foreign_key_check", "check could not be executed")
	}
	if foreignKeyRows.Next() {
		_ = foreignKeyRows.Close()
		return schemaIntegrityFailure("foreign_key_check", "referential integrity violation detected")
	}
	if err := foreignKeyRows.Err(); err != nil {
		_ = foreignKeyRows.Close()
		return schemaIntegrityFailure("foreign_key_check", "check did not complete")
	}
	if err := foreignKeyRows.Close(); err != nil {
		return schemaIntegrityFailure("foreign_key_check", "result could not be closed")
	}

	expected, err := expectedCurrentSchemaCatalog()
	if err != nil {
		return schemaIntegrityFailure("manifest", "embedded catalog could not be constructed")
	}
	actual, err := readSchemaCatalog(ctx, queryer)
	if err != nil {
		return schemaIntegrityFailure("manifest", "database catalog could not be read")
	}
	for name, expectedObject := range expected {
		actualObject, ok := actual[name]
		if !ok {
			return schemaIntegrityFailure("manifest", fmt.Sprintf("required object %q is missing", name))
		}
		if actualObject != expectedObject {
			return schemaIntegrityFailure("manifest", fmt.Sprintf("required object %q definition does not match", name))
		}
	}
	for name := range actual {
		if _, ok := expected[name]; !ok {
			return schemaIntegrityFailure("manifest", fmt.Sprintf("unexpected object %q is present", name))
		}
	}
	return nil
}

func schemaIntegrityFailure(check, detail string) error {
	return &SchemaIntegrityError{Check: check, Detail: detail}
}

func expectedCurrentSchemaCatalog() (map[string]schemaObject, error) {
	expectedSchemaOnce.Do(func() {
		database, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			expectedSchemaBuildErr = err
			return
		}
		defer database.Close()
		database.SetMaxOpenConns(1)
		database.SetMaxIdleConns(1)
		if _, err := database.Exec("PRAGMA foreign_keys=ON"); err != nil {
			expectedSchemaBuildErr = err
			return
		}
		for _, migration := range migrations {
			if _, err := database.Exec(migration.sql); err != nil {
				expectedSchemaBuildErr = fmt.Errorf("construct schema v%d: %w", migration.version, err)
				return
			}
		}
		expectedSchemaCatalog, expectedSchemaBuildErr = readSchemaCatalog(context.Background(), database)
	})
	if expectedSchemaBuildErr != nil {
		return nil, expectedSchemaBuildErr
	}
	return expectedSchemaCatalog, nil
}

func readSchemaCatalog(ctx context.Context, queryer schemaQueryer) (map[string]schemaObject, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT type, name, tbl_name, sql
		FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL
		ORDER BY type, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	catalog := make(map[string]schemaObject)
	for rows.Next() {
		var objectType, name, tableName, definition string
		if err := rows.Scan(&objectType, &name, &tableName, &definition); err != nil {
			return nil, err
		}
		normalized := normalizeSchemaDefinition(definition)
		catalog[name] = schemaObject{
			objectType: objectType,
			tableName:  tableName,
			digest:     sha256.Sum256([]byte(normalized)),
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return catalog, nil
}

// normalizeSchemaDefinition collapses formatting whitespace outside SQL
// strings and quoted identifiers. Case and literal contents are semantic and
// MUST remain byte-for-byte significant to the manifest digest.
func normalizeSchemaDefinition(definition string) string {
	var normalized strings.Builder
	normalized.Grow(len(definition))
	var quote byte
	spacePending := false
	for index := 0; index < len(definition); index++ {
		character := definition[index]
		if quote != 0 {
			normalized.WriteByte(character)
			if quote == '[' {
				if character == ']' {
					quote = 0
				}
				continue
			}
			if character == quote {
				if index+1 < len(definition) && definition[index+1] == quote {
					normalized.WriteByte(definition[index+1])
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		if isSQLWhitespace(character) {
			spacePending = normalized.Len() > 0
			continue
		}
		if spacePending {
			normalized.WriteByte(' ')
			spacePending = false
		}
		switch character {
		case '\'', '"', '`', '[':
			quote = character
		}
		normalized.WriteByte(character)
	}
	return normalized.String()
}

func isSQLWhitespace(character byte) bool {
	switch character {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}
