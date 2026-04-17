package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// SQLiteProjectStore implements ProjectStore backed by SQLite.
// Project is the top-level isolation boundary, so no projectID parameter is needed.
type SQLiteProjectStore struct {
	db *sql.DB
}

// NewSQLiteProjectStore creates a new ProjectStore backed by the given db.
func NewSQLiteProjectStore(db *sql.DB) *SQLiteProjectStore {
	return &SQLiteProjectStore{db: db}
}

// safeConfig returns a valid JSON string for storing config.
// If the raw message is empty/nil, returns "null".
func safeConfig(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

// now returns the current UTC timestamp in SQLite datetime format.
func now() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}

// Create inserts a new project.
func (s *SQLiteProjectStore) Create(ctx context.Context, p *model.Project) error {
	ts := now()
	p.CreatedAt = ts
	p.UpdatedAt = ts
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, workspace_path, description, status, config, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.WorkspacePath, p.Description, p.Status, safeConfig(p.Config), ts, ts,
	)
	if err != nil {
		return fmt.Errorf("insert project %s: %w", p.ID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("insert project %s: rows affected: %w", p.ID, err)
	}
	if rows == 0 {
		return fmt.Errorf("insert project %s: no rows affected", p.ID)
	}
	return nil
}

// GetByID retrieves a project by ID.
func (s *SQLiteProjectStore) GetByID(ctx context.Context, id string) (*model.Project, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, workspace_path, description, status, config, created_at, updated_at
		 FROM projects WHERE id = ?`, id,
	)
	p, err := scanProject(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrProjectNotFound
		}
		return nil, fmt.Errorf("get project %s: %w", id, err)
	}
	return p, nil
}

// Update updates mutable project fields.
func (s *SQLiteProjectStore) Update(ctx context.Context, p *model.Project) error {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects
		 SET name = ?, workspace_path = ?, description = ?, status = ?, config = ?, updated_at = ?
		 WHERE id = ?`,
		p.Name, p.WorkspacePath, p.Description, p.Status, safeConfig(p.Config), ts, p.ID,
	)
	if err != nil {
		return fmt.Errorf("update project %s: %w", p.ID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update project %s: rows affected: %w", p.ID, err)
	}
	if rows == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// List returns all projects. If includeArchived is false, archived projects are excluded.
func (s *SQLiteProjectStore) List(ctx context.Context, includeArchived bool) ([]*model.Project, error) {
	query := `SELECT id, name, workspace_path, description, status, config, created_at, updated_at
	          FROM projects`
	var args []any
	if !includeArchived {
		query += " WHERE status != 'archived'"
	}
	query += " ORDER BY created_at"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []*model.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return projects, nil
}

// Archive sets project status to 'archived'.
func (s *SQLiteProjectStore) Archive(ctx context.Context, id string) error {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET status = 'archived', updated_at = ? WHERE id = ? AND status != 'archived'`,
		ts, id,
	)
	if err != nil {
		return fmt.Errorf("archive project %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("archive project %s: rows affected: %w", id, err)
	}
	if rows == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// Restore sets project status back to 'active'.
func (s *SQLiteProjectStore) Restore(ctx context.Context, id string) error {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET status = 'active', updated_at = ? WHERE id = ? AND status = 'archived'`,
		ts, id,
	)
	if err != nil {
		return fmt.Errorf("restore project %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("restore project %s: rows affected: %w", id, err)
	}
	if rows == 0 {
		return ErrProjectNotFound
	}
	return nil
}

// FindByPath finds projects whose workspace_path matches the given path.
// Returns a list (may have multiple matches, triggering ErrProjectAmbiguous upstream).
func (s *SQLiteProjectStore) FindByPath(ctx context.Context, workspacePath string) ([]*model.Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, workspace_path, description, status, config, created_at, updated_at
		 FROM projects
		 WHERE workspace_path = ? AND status = 'active'
		 ORDER BY created_at`,
		workspacePath,
	)
	if err != nil {
		return nil, fmt.Errorf("find project by path %s: %w", workspacePath, err)
	}
	defer rows.Close()

	var projects []*model.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find project by path %s: %w", workspacePath, err)
	}
	return projects, nil
}

// scanProject scans a project row from a *sql.Row or *sql.Rows.
// Column order must match the SELECT clause: id, name, workspace_path, description, status, config, created_at, updated_at.
func scanProject(sc scan) (*model.Project, error) {
	var p model.Project
	var configStr string
	err := sc.Scan(
		&p.ID,
		&p.Name,
		&p.WorkspacePath,
		&p.Description,
		&p.Status,
		&configStr,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if configStr == "" {
		p.Config = json.RawMessage("null")
	} else {
		p.Config = json.RawMessage(configStr)
	}
	return &p, nil
}

// scan is the common interface satisfied by both *sql.Row and *sql.Rows.
type scan interface {
	Scan(dest ...any) error
}

// Compile-time interface assertion.
var _ ProjectStore = (*SQLiteProjectStore)(nil)
