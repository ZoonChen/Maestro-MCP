package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// SQLiteSessionStore implements SessionStore backed by SQLite.
type SQLiteSessionStore struct {
	db *sql.DB
}

// NewSessionStore creates a new SessionStore instance.
func NewSessionStore(db *sql.DB) *SQLiteSessionStore {
	return &SQLiteSessionStore{db: db}
}

// sessionColumns is the ordered column list for SELECT queries on agent_sessions.
// The physical primary key is intentionally not exposed. external_id is scoped
// by project and is the only Session ID visible above the store boundary.
const sessionColumns = `COALESCE(external_id, id), project_id, role, client_type, capacity, status,
	version, last_heartbeat, created_at`

// scanSession scans a single row into an AgentSession struct.
func scanSession(scanner interface {
	Scan(dest ...any) error
}) (*model.AgentSession, error) {
	var s model.AgentSession
	err := scanner.Scan(
		&s.ID,
		&s.ProjectID,
		&s.Role,
		&s.ClientType,
		&s.Capacity,
		&s.Status,
		&s.Version,
		&s.LastHeartbeat,
		&s.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Create registers a new AgentSession.
func (s *SQLiteSessionStore) Create(ctx context.Context, projectID string, session *model.AgentSession) error {
	const query = `INSERT INTO agent_sessions
		(id, external_id, project_id, role, client_type, capacity, status, version, last_heartbeat, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, query,
		scopedSessionKey(projectID, session.ID),
		session.ID,
		projectID,
		session.Role,
		session.ClientType,
		session.Capacity,
		session.Status,
		session.Version,
		session.LastHeartbeat,
		session.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("session create: %w", err)
	}
	return nil
}

// CreateIfNotExists inserts the session only if the id does not already exist.
// Returns true if the session was created, false if it already existed.
func (s *SQLiteSessionStore) CreateIfNotExists(ctx context.Context, projectID string, session *model.AgentSession) (bool, error) {
	const query = `INSERT OR IGNORE INTO agent_sessions
		(id, external_id, project_id, role, client_type, capacity, status, version, last_heartbeat, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := s.db.ExecContext(ctx, query,
		scopedSessionKey(projectID, session.ID),
		session.ID,
		projectID,
		session.Role,
		session.ClientType,
		session.Capacity,
		session.Status,
		session.Version,
		session.LastHeartbeat,
		session.CreatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("session create if not exists: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("session create if not exists rows: %w", err)
	}
	if affected > 0 {
		return true, nil
	}
	// INSERT OR IGNORE can hide multiple constraint classes. Only an existing
	// logical ID in the same project is an idempotent hit; everything else is a
	// hard conflict and must not be interpreted as success.
	if _, err := s.GetByID(ctx, projectID, session.ID); err != nil {
		return false, fmt.Errorf("session create ignored without matching scoped session: %w", err)
	}
	return false, nil
}

// GetByID retrieves a session by ID scoped to projectID.
func (s *SQLiteSessionStore) GetByID(ctx context.Context, projectID, id string) (*model.AgentSession, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM agent_sessions
		WHERE COALESCE(external_id, id) = ? AND project_id = ?`, sessionColumns)

	row := s.db.QueryRowContext(ctx, query, id, projectID)
	session, err := scanSession(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("session get by id: %w", err)
	}
	return session, nil
}

// List returns all sessions for a project.
func (s *SQLiteSessionStore) List(ctx context.Context, projectID string) ([]*model.AgentSession, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM agent_sessions
		WHERE project_id = ?
		ORDER BY created_at DESC`, sessionColumns)

	rows, err := s.db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("session list: %w", err)
	}
	defer rows.Close()

	var sessions []*model.AgentSession
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("session list scan: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session list rows: %w", err)
	}
	return sessions, nil
}

// UpdateHeartbeat refreshes last_heartbeat to current time.
func (s *SQLiteSessionStore) UpdateHeartbeat(ctx context.Context, projectID, id string) error {
	const query = `UPDATE agent_sessions
		SET last_heartbeat = datetime('now'), version = version + 1
		WHERE COALESCE(external_id, id) = ? AND project_id = ?`
	result, err := s.db.ExecContext(ctx, query, id, projectID)
	if err != nil {
		return fmt.Errorf("session update heartbeat: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("session update heartbeat rows: %w", err)
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// UpdateStatus changes the session status (online/offline).
func (s *SQLiteSessionStore) UpdateStatus(ctx context.Context, projectID, id, status string) error {
	if !model.IsSessionStatus(status) {
		return fmt.Errorf("session update status: %w: %q", ErrTaskStateInvalid, status)
	}
	var current string
	var version int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT status, version FROM agent_sessions WHERE COALESCE(external_id, id) = ? AND project_id = ?`,
		id, projectID,
	).Scan(&current, &version); err != nil {
		if err == sql.ErrNoRows {
			return ErrSessionNotFound
		}
		return fmt.Errorf("session read status: %w", err)
	}
	if !model.CanSessionTransition(current, status) {
		return fmt.Errorf("session transition %s -> %s: %w", current, status, ErrTaskStateInvalid)
	}
	const query = `UPDATE agent_sessions
		SET status = ?, version = version + 1
		WHERE COALESCE(external_id, id) = ? AND project_id = ? AND status = ? AND version = ?`
	result, err := s.db.ExecContext(ctx, query, status, id, projectID, current, version)
	if err != nil {
		return fmt.Errorf("session update status: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("session update status rows: %w", err)
	}
	if n == 0 {
		return ErrConcurrentConflict
	}
	return nil
}

// FindStale scans across all projects for online sessions whose last_heartbeat
// is older than timeoutSec seconds. This method does NOT take projectID -- it is
// called by a background goroutine for cross-project cleanup.
func (s *SQLiteSessionStore) FindStale(ctx context.Context, timeoutSec int) ([]*model.AgentSession, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM agent_sessions
		WHERE status = ? AND last_heartbeat < datetime('now', '-' || ? || ' seconds')`,
		sessionColumns)

	rows, err := s.db.QueryContext(ctx, query, model.SessionStatusOnline, timeoutSec)
	if err != nil {
		return nil, fmt.Errorf("session find stale: %w", err)
	}
	defer rows.Close()

	var sessions []*model.AgentSession
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("session find stale scan: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session find stale rows: %w", err)
	}
	return sessions, nil
}

// Verify interface compliance at compile time.
var _ SessionStore = (*SQLiteSessionStore)(nil)

// Database exposes the adapter connection to the M0 Application Service so it
// can bind all repositories to one transaction. It is deliberately absent from
// the SessionStore port and must not be used by transports.
func (s *SQLiteSessionStore) Database() *sql.DB { return s.db }
