package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlite "modernc.org/sqlite" // pure-Go SQLite driver for database/sql
)

const (
	sqliteBusyCode        = 5
	walSetupRetryWindow   = 5 * time.Second
	walSetupBusyTimeoutMS = 250
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

	// SQLite PRAGMAs below are connection-local. Keep a single connection from
	// the beginning so every later operation observes the same safety settings.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Install a short busy handler before WAL negotiation. PRAGMA journal_mode
	// can still report SQLITE_BUSY during a concurrent open, so the bounded retry
	// below handles that race without allowing one attempt to consume the entire
	// startup window.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", walSetupBusyTimeoutMS)); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	// Enable WAL mode for better concurrent read performance.
	if err := enableWALWithRetry(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	// Enable foreign key enforcement (off by default in SQLite).
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	return &SQLiteDB{db: db}, nil
}

func enableWALWithRetry(db *sql.DB) error {
	deadline := time.Now().Add(walSetupRetryWindow)
	delay := 10 * time.Millisecond
	for {
		_, err := db.Exec("PRAGMA journal_mode=WAL")
		if err == nil {
			return nil
		}
		if !isSQLiteBusy(err) || time.Now().Add(delay).After(deadline) {
			return err
		}
		time.Sleep(delay)
		if delay < 200*time.Millisecond {
			delay *= 2
		}
	}
}

func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteBusyCode
}

// currentSchemaVersion is the expected schema version.
// Increment this when adding migrations to the migrations slice below.
const currentSchemaVersion = 5

// CurrentSchemaVersion is the single source used by migrate/doctor/version.
func CurrentSchemaVersion() int { return currentSchemaVersion }

// ErrSchemaVersionIncompatible identifies a database that a live runtime must
// not mutate. Operators must run the explicit migration command before the
// server or Runner may use an existing, non-current database.
var ErrSchemaVersionIncompatible = errors.New("database schema version incompatible")

// SchemaVersionError records the durable version observed while holding the
// SQLite migration reservation. It unwraps to ErrSchemaVersionIncompatible so
// transport entrypoints can map the failure to a stable public error code.
type SchemaVersionError struct {
	Actual   int
	Expected int
	Empty    bool
}

func (e *SchemaVersionError) Error() string {
	return fmt.Sprintf("database schema version %d, expected %d", e.Actual, e.Expected)
}

func (e *SchemaVersionError) Unwrap() error { return ErrSchemaVersionIncompatible }

// Init creates all tables and indexes. It is idempotent (IF NOT EXISTS).
// Schema versioning: uses PRAGMA user_version for migration tracking.
func (s *SQLiteDB) Init(ctx context.Context) error {
	return s.initializeSchema(ctx, false)
}

// EnsureRuntimeSchema allows a live server or Runner to bootstrap a genuinely
// empty database, but never upgrades an existing database. Version discovery,
// the empty-database decision, and any permitted bootstrap DDL are serialized
// by the same BEGIN IMMEDIATE reservation used by explicit migrations.
func (s *SQLiteDB) EnsureRuntimeSchema(ctx context.Context) error {
	return s.initializeSchema(ctx, true)
}

func (s *SQLiteDB) initializeSchema(ctx context.Context, bootstrapOnly bool) error {
	// BEGIN IMMEDIATE serializes version discovery with schema mutation across
	// both goroutines and independent migrate processes. Reading user_version
	// before obtaining this write reservation allows two migrators to select the
	// same non-idempotent ALTER TABLE migration.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	inTransaction := true
	defer func() {
		if inTransaction {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// Check current schema version only after holding the migration lock.
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if bootstrapOnly && version != currentSchemaVersion {
		empty, err := isEmptySchema(ctx, conn)
		if err != nil {
			return err
		}
		if version != 0 || !empty {
			return &SchemaVersionError{
				Actual:   version,
				Expected: currentSchemaVersion,
				Empty:    empty,
			}
		}
	}

	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}

	// Apply the complete pending sequence and all user_version markers in the
	// same transaction. A failed migration leaves neither partial DDL nor a
	// misleading version, and waiting migrators re-read the committed version.
	for _, m := range migrations {
		if m.version <= version {
			continue
		}
		if _, err := conn.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("migration v%d: %w", m.version, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			return fmt.Errorf("set schema version %d: %w", m.version, err)
		}
		version = m.version
	}

	if err := validateCurrentSchema(ctx, conn); err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migrations through v%d: %w", version, err)
	}
	inTransaction = false

	return nil
}

func isEmptySchema(ctx context.Context, conn *sql.Conn) (bool, error) {
	var empty bool
	if err := conn.QueryRowContext(ctx, `SELECT NOT EXISTS (
		SELECT 1 FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'
	)`).Scan(&empty); err != nil {
		return false, fmt.Errorf("inspect database schema: %w", err)
	}
	return empty, nil
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
	{
		version: 2,
		sql:     schemaV2,
	},
	{
		version: 3,
		sql:     schemaV3,
	},
	{
		version: 4,
		sql:     schemaV4,
	},
	{
		version: 5,
		sql:     schemaV5,
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

// schemaV2 adds the M0 concurrency substrate without pretending SQLite is the
// final v3 control-plane database. Project-scope triggers compensate for the
// v1 single-column foreign keys; PostgreSQL replaces them with composite FKs.
var schemaV2 = `
ALTER TABLE tasks ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN lease_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN active_lease_id TEXT;
ALTER TABLE tasks ADD COLUMN lease_expires_at TEXT;
ALTER TABLE tasks ADD COLUMN merged_fact_id TEXT;

UPDATE tasks SET status = CASE status
    WHEN 'pending' THEN 'queued'
    WHEN 'in_progress' THEN 'executing'
    WHEN 'submitted' THEN 'validating'
    WHEN 'verifying' THEN 'validating'
    WHEN 'ready_to_merge' THEN 'ready_for_human_merge'
    WHEN 'merge_conflicted' THEN 'needs_human'
    ELSE status
END;

ALTER TABLE agent_sessions ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agent_sessions ADD COLUMN external_id TEXT;
UPDATE agent_sessions SET external_id = id WHERE external_id IS NULL;
ALTER TABLE agent_workers ADD COLUMN version INTEGER NOT NULL DEFAULT 0;

ALTER TABLE worktrees ADD COLUMN generation INTEGER NOT NULL DEFAULT 1;
ALTER TABLE worktrees ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE worktrees ADD COLUMN updated_at TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z';
UPDATE worktrees SET updated_at = created_at WHERE updated_at = '1970-01-01T00:00:00Z';

CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_project_id
    ON agent_sessions(project_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_project_external_id
    ON agent_sessions(project_id, external_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_project_logical_id
    ON agent_sessions(project_id, COALESCE(external_id, id));
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_project_id
    ON tasks(project_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_workers_project_session_id
    ON agent_workers(project_id, session_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_worktrees_generation
    ON worktrees(project_id, task_id, generation);

CREATE TABLE IF NOT EXISTS task_leases (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id),
    task_id     TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    worker_id   TEXT NOT NULL,
    epoch       INTEGER NOT NULL CHECK (epoch > 0),
    status      TEXT NOT NULL CHECK (status IN ('active','completed','released','expired','cancelled')),
    version     INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    expires_at  TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(project_id, task_id, epoch),
    FOREIGN KEY(project_id, task_id) REFERENCES tasks(project_id, id),
    FOREIGN KEY(project_id, session_id) REFERENCES agent_sessions(project_id, id),
    FOREIGN KEY(project_id, session_id, worker_id)
        REFERENCES agent_workers(project_id, session_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_task_leases_one_active
    ON task_leases(project_id, task_id) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_task_leases_expiry
    ON task_leases(status, expires_at);

CREATE TABLE IF NOT EXISTS idempotency_records (
    project_id    TEXT NOT NULL REFERENCES projects(id),
    scope         TEXT NOT NULL,
    operation     TEXT NOT NULL,
    key           TEXT NOT NULL,
    request_hash  TEXT NOT NULL,
    result_ref    TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at    TEXT,
    PRIMARY KEY(project_id, scope, operation, key)
);

CREATE TABLE IF NOT EXISTS project_queue_versions (
    project_id TEXT PRIMARY KEY REFERENCES projects(id),
    version    INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0)
);
INSERT OR IGNORE INTO project_queue_versions(project_id, version)
    SELECT id, 0 FROM projects;

CREATE TABLE IF NOT EXISTS state_history (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id     TEXT NOT NULL REFERENCES projects(id),
    aggregate_type TEXT NOT NULL,
    aggregate_id   TEXT NOT NULL,
    from_status    TEXT NOT NULL,
    to_status      TEXT NOT NULL,
    from_version   INTEGER NOT NULL,
    to_version     INTEGER NOT NULL,
    actor_id       TEXT,
    reason         TEXT NOT NULL,
    causation_id   TEXT,
    created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_state_history_aggregate
    ON state_history(project_id, aggregate_type, aggregate_id, id);

CREATE TABLE IF NOT EXISTS runtime_state (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Fail closed if an old global identifier is used across a project boundary.
CREATE TRIGGER IF NOT EXISTS trg_tasks_project_scope_insert
BEFORE INSERT ON tasks
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM features f WHERE f.id = NEW.feature_id AND f.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_FEATURE') END;
    SELECT CASE WHEN NEW.assigned_session_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM agent_sessions s WHERE s.id = NEW.assigned_session_id AND s.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_SESSION') END;
    SELECT CASE WHEN NEW.verified_by IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM agent_sessions s WHERE s.id = NEW.verified_by AND s.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_VERIFIER') END;
END;

CREATE TRIGGER IF NOT EXISTS trg_tasks_project_scope_update
BEFORE UPDATE OF project_id, feature_id, assigned_session_id, verified_by ON tasks
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM features f WHERE f.id = NEW.feature_id AND f.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_FEATURE') END;
    SELECT CASE WHEN NEW.assigned_session_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM agent_sessions s WHERE s.id = NEW.assigned_session_id AND s.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_SESSION') END;
    SELECT CASE WHEN NEW.verified_by IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM agent_sessions s WHERE s.id = NEW.verified_by AND s.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_VERIFIER') END;
END;

CREATE TRIGGER IF NOT EXISTS trg_workers_project_scope_insert
BEFORE INSERT ON agent_workers
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM agent_sessions s WHERE s.id = NEW.session_id AND s.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_SESSION') END;
    SELECT CASE WHEN NEW.current_task_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = NEW.current_task_id AND t.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_TASK') END;
END;

CREATE TRIGGER IF NOT EXISTS trg_workers_project_scope_update
BEFORE UPDATE OF project_id, session_id, current_task_id ON agent_workers
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM agent_sessions s WHERE s.id = NEW.session_id AND s.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_SESSION') END;
    SELECT CASE WHEN NEW.current_task_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = NEW.current_task_id AND t.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_TASK') END;
END;

CREATE TRIGGER IF NOT EXISTS trg_worktrees_project_scope_insert
BEFORE INSERT ON worktrees
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = NEW.task_id AND t.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_TASK') END;
    SELECT CASE WHEN NEW.session_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM agent_sessions s WHERE s.id = NEW.session_id AND s.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_SESSION') END;
END;

CREATE TRIGGER IF NOT EXISTS trg_task_results_project_scope_insert
BEFORE INSERT ON task_results
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = NEW.task_id AND t.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_TASK') END;
END;

CREATE TRIGGER IF NOT EXISTS trg_task_results_project_scope_update
BEFORE UPDATE OF project_id, task_id ON task_results
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = NEW.task_id AND t.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_TASK') END;
END;

CREATE TRIGGER IF NOT EXISTS trg_validation_runs_project_scope_insert
BEFORE INSERT ON validation_runs
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM tasks t WHERE t.id = NEW.task_id AND t.project_id = NEW.project_id
    ) THEN RAISE(ABORT, 'PROJECT_SCOPE_TASK') END;
END;

CREATE TRIGGER IF NOT EXISTS trg_tasks_valid_status_insert
BEFORE INSERT ON tasks
WHEN NEW.status NOT IN (
    'draft','queued','leased','executing','validating','ready_for_human_merge',
    'done','blocked','cancelling','cancelled','failed','needs_human'
)
BEGIN SELECT RAISE(ABORT, 'INVALID_TASK_STATUS'); END;

CREATE TRIGGER IF NOT EXISTS trg_tasks_valid_status_update
BEFORE UPDATE OF status ON tasks
WHEN NEW.status NOT IN (
    'draft','queued','leased','executing','validating','ready_for_human_merge',
    'done','blocked','cancelling','cancelled','failed','needs_human'
)
BEGIN SELECT RAISE(ABORT, 'INVALID_TASK_STATUS'); END;

CREATE TRIGGER IF NOT EXISTS trg_sessions_valid_status_insert
BEFORE INSERT ON agent_sessions
WHEN NEW.status NOT IN ('online','offline')
BEGIN SELECT RAISE(ABORT, 'INVALID_SESSION_STATUS'); END;

CREATE TRIGGER IF NOT EXISTS trg_sessions_valid_status_update
BEFORE UPDATE OF status ON agent_sessions
WHEN NEW.status NOT IN ('online','offline')
BEGIN SELECT RAISE(ABORT, 'INVALID_SESSION_STATUS'); END;

CREATE TRIGGER IF NOT EXISTS trg_workers_valid_status_insert
BEFORE INSERT ON agent_workers
WHEN NEW.status NOT IN ('idle','reserved','busy','lost')
BEGIN SELECT RAISE(ABORT, 'INVALID_WORKER_STATUS'); END;

CREATE TRIGGER IF NOT EXISTS trg_workers_valid_status_update
BEFORE UPDATE OF status ON agent_workers
WHEN NEW.status NOT IN ('idle','reserved','busy','lost')
BEGIN SELECT RAISE(ABORT, 'INVALID_WORKER_STATUS'); END;

CREATE TRIGGER IF NOT EXISTS trg_worktrees_valid_status_insert
BEFORE INSERT ON worktrees
WHEN NEW.status NOT IN (
    'allocated','active','sealed','submitted','stale','merged','abandoned',
    'quarantined','cleanup_pending'
)
BEGIN SELECT RAISE(ABORT, 'INVALID_WORKTREE_STATUS'); END;

CREATE TRIGGER IF NOT EXISTS trg_worktrees_valid_status_update
BEFORE UPDATE OF status ON worktrees
WHEN NEW.status NOT IN (
    'allocated','active','sealed','submitted','stale','merged','abandoned',
    'quarantined','cleanup_pending'
)
BEGIN SELECT RAISE(ABORT, 'INVALID_WORKTREE_STATUS'); END;
`

// schemaV3 binds validation Evidence to its exact code/policy/profile inputs
// and makes the three security histories append-only at the database boundary.
// It is a separate migration so databases initialized by early M0 builds at
// user_version=2 cannot silently miss these invariants.
var schemaV3 = `
ALTER TABLE validation_runs ADD COLUMN source_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE validation_runs ADD COLUMN policy_version TEXT NOT NULL DEFAULT '';
ALTER TABLE validation_runs ADD COLUMN policy_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE validation_runs ADD COLUMN profile_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE validation_runs ADD COLUMN evidence_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE validation_runs ADD COLUMN workspace_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE validation_runs ADD COLUMN output_truncated INTEGER NOT NULL DEFAULT 0
    CHECK (output_truncated IN (0, 1));

CREATE INDEX IF NOT EXISTS idx_validation_runs_evidence_digest
    ON validation_runs(project_id, task_id, evidence_digest);

CREATE TRIGGER IF NOT EXISTS trg_validation_runs_append_only_update
BEFORE UPDATE ON validation_runs
BEGIN SELECT RAISE(ABORT, 'APPEND_ONLY_VALIDATION_EVIDENCE'); END;
CREATE TRIGGER IF NOT EXISTS trg_validation_runs_append_only_delete
BEFORE DELETE ON validation_runs
BEGIN SELECT RAISE(ABORT, 'APPEND_ONLY_VALIDATION_EVIDENCE'); END;

CREATE TRIGGER IF NOT EXISTS trg_state_history_append_only_update
BEFORE UPDATE ON state_history
BEGIN SELECT RAISE(ABORT, 'APPEND_ONLY_STATE_HISTORY'); END;
CREATE TRIGGER IF NOT EXISTS trg_state_history_append_only_delete
BEFORE DELETE ON state_history
BEGIN SELECT RAISE(ABORT, 'APPEND_ONLY_STATE_HISTORY'); END;

CREATE TRIGGER IF NOT EXISTS trg_audit_log_append_only_update
BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'APPEND_ONLY_AUDIT_LOG'); END;
CREATE TRIGGER IF NOT EXISTS trg_audit_log_append_only_delete
BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'APPEND_ONLY_AUDIT_LOG'); END;

-- Defense in depth: every status-changing SQL statement must follow the
-- canonical graph and advance the optimistic version exactly once. The
-- historical local merge-fact exception is removed by schemaV5.
CREATE TRIGGER IF NOT EXISTS trg_tasks_legal_transition_update
BEFORE UPDATE OF status ON tasks
WHEN OLD.status <> NEW.status AND NOT (
       (OLD.status = 'draft' AND NEW.status = 'queued')
    OR (OLD.status = 'queued' AND NEW.status IN ('leased','cancelled'))
    OR (OLD.status = 'leased' AND NEW.status IN ('executing','queued','cancelling'))
    OR (OLD.status = 'executing' AND NEW.status IN ('validating','blocked','cancelling','failed','needs_human','queued'))
    OR (OLD.status = 'validating' AND NEW.status IN ('ready_for_human_merge','failed','needs_human'))
    OR (OLD.status = 'ready_for_human_merge' AND NEW.status IN ('validating','needs_human'))
    OR (OLD.status = 'blocked' AND NEW.status IN ('queued','cancelling','needs_human'))
    OR (OLD.status = 'cancelling' AND NEW.status IN ('cancelled','needs_human'))
    OR (OLD.status = 'failed' AND NEW.status IN ('queued','needs_human'))
    OR (OLD.status = 'needs_human' AND NEW.status IN ('queued','cancelling'))
    OR (OLD.status = 'ready_for_human_merge' AND NEW.status = 'done'
        AND NEW.merged_fact_id IS NOT NULL AND length(NEW.merged_fact_id) BETWEEN 1 AND 256
        AND NEW.merge_commit IS NOT NULL AND length(NEW.merge_commit) IN (40,64)
        AND NEW.merge_commit NOT GLOB '*[^0-9a-f]*')
)
BEGIN SELECT RAISE(ABORT, 'ILLEGAL_TASK_TRANSITION'); END;

CREATE TRIGGER IF NOT EXISTS trg_tasks_transition_version_update
BEFORE UPDATE OF status ON tasks
WHEN OLD.status <> NEW.status AND NEW.version <> OLD.version + 1
BEGIN SELECT RAISE(ABORT, 'TASK_VERSION_NOT_INCREMENTED'); END;

CREATE TRIGGER IF NOT EXISTS trg_tasks_merged_fact_immutable
BEFORE UPDATE OF merged_fact_id, merge_commit ON tasks
WHEN OLD.merged_fact_id IS NOT NULL
 AND (NEW.merged_fact_id IS NOT OLD.merged_fact_id OR NEW.merge_commit IS NOT OLD.merge_commit)
BEGIN SELECT RAISE(ABORT, 'MERGED_FACT_IMMUTABLE'); END;
`

// schemaV4 makes Evidence authority an immutable, explicit fact. Existing M0
// ValidationRun rows are local diagnostics and are migrated conservatively;
// they must never become merge authority merely because the schema changed.
var schemaV4 = `
ALTER TABLE validation_runs ADD COLUMN authority TEXT NOT NULL DEFAULT 'diagnostic'
    CHECK (authority IN ('diagnostic', 'merge_gate'));
ALTER TABLE validation_runs ADD COLUMN producer TEXT NOT NULL DEFAULT 'maestro-local'
    CHECK (length(trim(producer)) > 0);
ALTER TABLE validation_runs ADD COLUMN pipeline_id TEXT;
ALTER TABLE validation_runs ADD COLUMN job_id TEXT;

CREATE INDEX IF NOT EXISTS idx_validation_runs_authority
    ON validation_runs(project_id, task_id, authority, attempt);

-- Local Evidence has a single server-owned identity and cannot carry forged
-- CI coordinates. Conversely, merge authority cannot claim the local producer.
CREATE TRIGGER IF NOT EXISTS trg_validation_runs_authority_insert
BEFORE INSERT ON validation_runs
WHEN (
    NEW.authority = 'diagnostic' AND (
        NEW.producer <> 'maestro-local' OR
        NEW.pipeline_id IS NOT NULL OR NEW.job_id IS NOT NULL
    )
) OR (
    NEW.authority = 'merge_gate' AND NEW.producer = 'maestro-local'
)
BEGIN SELECT RAISE(ABORT, 'INVALID_EVIDENCE_AUTHORITY'); END;
`

// schemaV5 closes the M0 local-completion escape hatch. Existing done rows are
// retained as history, but no SQL write may create another done task;
// a later GitLab-authoritative migration can replace this trigger with rules
// bound to durable, verified merge events.
var schemaV5 = `
CREATE TRIGGER IF NOT EXISTS trg_tasks_m0_no_done_insert
BEFORE INSERT ON tasks
WHEN NEW.status = 'done'
BEGIN SELECT RAISE(ABORT, 'M0_DONE_REQUIRES_VERIFIED_MERGE_FACT'); END;

DROP TRIGGER IF EXISTS trg_tasks_legal_transition_update;
CREATE TRIGGER trg_tasks_legal_transition_update
BEFORE UPDATE OF status ON tasks
WHEN OLD.status <> NEW.status AND NOT (
       (OLD.status = 'draft' AND NEW.status = 'queued')
    OR (OLD.status = 'queued' AND NEW.status IN ('leased','cancelled'))
    OR (OLD.status = 'leased' AND NEW.status IN ('executing','queued','cancelling'))
    OR (OLD.status = 'executing' AND NEW.status IN ('validating','blocked','cancelling','failed','needs_human','queued'))
    OR (OLD.status = 'validating' AND NEW.status IN ('ready_for_human_merge','failed','needs_human'))
    OR (OLD.status = 'ready_for_human_merge' AND NEW.status IN ('validating','needs_human'))
    OR (OLD.status = 'blocked' AND NEW.status IN ('queued','cancelling','needs_human'))
    OR (OLD.status = 'cancelling' AND NEW.status IN ('cancelled','needs_human'))
    OR (OLD.status = 'failed' AND NEW.status IN ('queued','needs_human'))
    OR (OLD.status = 'needs_human' AND NEW.status IN ('queued','cancelling'))
)
BEGIN SELECT RAISE(ABORT, 'ILLEGAL_TASK_TRANSITION'); END;
`

// Close closes the underlying database connection.
func (s *SQLiteDB) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for use by individual stores.
func (s *SQLiteDB) DB() *sql.DB {
	return s.db
}
