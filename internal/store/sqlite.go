package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go SQLite driver for database/sql
)

// SQLiteDB wraps *sql.DB and provides schema initialization.
type SQLiteDB struct {
	db *sql.DB
}

// NewSQLiteDB opens (or creates) a SQLite database at dbPath.
// If dbPath is empty, an in-memory database is used.
// WAL mode and foreign keys are enabled for safety and concurrency.
func NewSQLiteDB(dbPath string) (*SQLiteDB, error) {
	if dbPath == "" {
		dbPath = ":memory:"
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	// Enable WAL mode for better concurrent read performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}

	// Enable foreign key enforcement (off by default in SQLite).
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	// Set busy timeout so concurrent writes wait instead of immediately failing.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	// Configure connection pool for SQLite:
	// - Max 1 open connection (SQLite only allows one writer at a time)
	// - Max 2 idle connections to keep readers warm
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(2)

	return &SQLiteDB{db: db}, nil
}

// currentSchemaVersion is the expected schema version.
// Increment this when adding migrations to the migrations slice below.
const currentSchemaVersion = 1

// Init creates all tables and indexes. It is idempotent (IF NOT EXISTS).
// Schema versioning: uses PRAGMA user_version for migration tracking.
func (s *SQLiteDB) Init(ctx context.Context) error {
	// Check current schema version.
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	// Apply migrations from the current version to the latest.
	if version < currentSchemaVersion {
		for _, m := range migrations {
			if m.version > version {
				if _, err := s.db.ExecContext(ctx, m.sql); err != nil {
					return fmt.Errorf("migration v%d: %w", m.version, err)
				}
			}
		}
		// Update schema version.
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}

	return nil
}

// migration represents a single schema migration.
type migration struct {
	version int
	sql     string
}

// migrations is the ordered list of schema migrations.
// Migration v1 is the initial full schema (all CREATE TABLE IF NOT EXISTS).
var migrations = []migration{
	{
		version: 1,
		sql:     schemaV1,
	},
}

// schemaV1 is the initial schema with all tables.
var schemaV1 = `
-- Project table (top-level entity)
CREATE TABLE IF NOT EXISTS projects (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    workspace_path  TEXT NOT NULL UNIQUE,
    description     TEXT DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'active',
    config          TEXT DEFAULT '{}',
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Feature table
CREATE TABLE IF NOT EXISTS features (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id),
    title       TEXT NOT NULL,
    description TEXT NOT NULL,
    reference_urls TEXT DEFAULT '[]',
    status      TEXT NOT NULL DEFAULT 'planning',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(id, project_id)
);
CREATE INDEX IF NOT EXISTS idx_features_project ON features(project_id);

-- Task table
CREATE TABLE IF NOT EXISTS tasks (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects(id),
    feature_id          TEXT NOT NULL REFERENCES features(id),
    title               TEXT NOT NULL,
    description         TEXT NOT NULL,
    role                TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending',
    allowed_directories TEXT NOT NULL,
    forbidden_patterns  TEXT DEFAULT '[]',
    required_apis       TEXT DEFAULT '[]',
    dependencies        TEXT DEFAULT '[]',
    parent_task_id      TEXT,
    relation_type       TEXT,
    test_requirements   TEXT DEFAULT '{}',
    assigned_session_id TEXT REFERENCES agent_sessions(id),
    assigned_worker_id  TEXT,
    assigned_at         TEXT,
    blocker_reason      TEXT,
    cancel_reason       TEXT,
    merge_commit        TEXT,
    verified_by         TEXT REFERENCES agent_sessions(id),
    verified_at         TEXT,
    priority            TEXT NOT NULL DEFAULT 'normal',
    summary             TEXT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(id, project_id)
);
CREATE INDEX IF NOT EXISTS idx_tasks_project ON tasks(project_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(project_id, status);
CREATE INDEX IF NOT EXISTS idx_tasks_role ON tasks(project_id, role, status);
CREATE INDEX IF NOT EXISTS idx_tasks_feature ON tasks(project_id, feature_id);

-- Task submission results
CREATE TABLE IF NOT EXISTS task_results (
    id               TEXT PRIMARY KEY,
    task_id          TEXT NOT NULL UNIQUE REFERENCES tasks(id),
    project_id       TEXT NOT NULL REFERENCES projects(id),
    base_commit      TEXT NOT NULL,
    changed_files    TEXT NOT NULL,
    test_command     TEXT NOT NULL DEFAULT '',
    test_output      TEXT NOT NULL DEFAULT '',
    coverage         REAL,
    summary          TEXT,
    submitted_at     TEXT NOT NULL DEFAULT (datetime('now')),
    validated_at     TEXT,
    validation_errors TEXT,
    verifier_notes   TEXT
);
CREATE INDEX IF NOT EXISTS idx_task_results_project ON task_results(project_id);

-- Validation run history (append-only)
CREATE TABLE IF NOT EXISTS validation_runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    project_id      TEXT NOT NULL REFERENCES projects(id),
    attempt         INTEGER NOT NULL,
    base_commit     TEXT NOT NULL,
    changed_files   TEXT NOT NULL,
    test_command    TEXT NOT NULL DEFAULT '',
    test_exit_code  INTEGER,
    test_output     TEXT,
    coverage        REAL,
    boundary_ok     INTEGER NOT NULL,
    test_ok         INTEGER NOT NULL,
    coverage_ok     INTEGER NOT NULL,
    summary         TEXT,
    result          TEXT NOT NULL,
    error_code      TEXT,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    log_path        TEXT,
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_validation_runs_task ON validation_runs(project_id, task_id, attempt);

-- Worktree resource state
CREATE TABLE IF NOT EXISTS worktrees (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    project_id      TEXT NOT NULL REFERENCES projects(id),
    session_id      TEXT REFERENCES agent_sessions(id),
    worktree_path   TEXT NOT NULL,
    branch_name     TEXT NOT NULL,
    base_commit     TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'allocated',
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_worktrees_status ON worktrees(project_id, status);
CREATE INDEX IF NOT EXISTS idx_worktrees_stale ON worktrees(status, created_at);

-- API contract index
CREATE TABLE IF NOT EXISTS api_contracts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  TEXT NOT NULL REFERENCES projects(id),
    method      TEXT NOT NULL,
    path        TEXT NOT NULL,
    request_schema  TEXT,
    response_schema TEXT,
    description TEXT,
    source_file TEXT NOT NULL,
    parsed_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(project_id, method, path)
);
CREATE INDEX IF NOT EXISTS idx_contracts_lookup ON api_contracts(project_id, method, path);

-- Agent session table
CREATE TABLE IF NOT EXISTS agent_sessions (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL REFERENCES projects(id),
    role            TEXT NOT NULL,
    client_type     TEXT NOT NULL DEFAULT 'other',
    capacity        INTEGER NOT NULL DEFAULT 1,
    status          TEXT NOT NULL DEFAULT 'online',
    last_heartbeat  TEXT NOT NULL DEFAULT (datetime('now')),
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON agent_sessions(project_id);
CREATE INDEX IF NOT EXISTS idx_sessions_heartbeat ON agent_sessions(status, last_heartbeat);

-- Agent worker table
CREATE TABLE IF NOT EXISTS agent_workers (
    id              TEXT NOT NULL,
    session_id      TEXT NOT NULL REFERENCES agent_sessions(id),
    project_id      TEXT NOT NULL REFERENCES projects(id),
    current_task_id TEXT REFERENCES tasks(id),
    status          TEXT NOT NULL DEFAULT 'idle',
    tasks_completed INTEGER NOT NULL DEFAULT 0,
    last_active     TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (id, session_id)
);
CREATE INDEX IF NOT EXISTS idx_workers_session ON agent_workers(session_id);
CREATE INDEX IF NOT EXISTS idx_workers_task ON agent_workers(current_task_id);

-- Business activity log (append-only)
CREATE TABLE IF NOT EXISTS activity_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  TEXT NOT NULL REFERENCES projects(id),
    session_id  TEXT,
    task_id     TEXT,
    action      TEXT NOT NULL,
    detail      TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_activity_project ON activity_log(project_id, created_at DESC);

-- Security audit log (append-only)
CREATE TABLE IF NOT EXISTS audit_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT,
    bound_project   TEXT NOT NULL,
    target_project  TEXT,
    target_task     TEXT,
    action          TEXT NOT NULL,
    path            TEXT,
    result          TEXT NOT NULL,
    detail          TEXT,
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_denied ON audit_log(result, created_at DESC);
`

// Close closes the underlying database connection.
func (s *SQLiteDB) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for use by individual stores.
func (s *SQLiteDB) DB() *sql.DB {
	return s.db
}
