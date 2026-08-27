package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// SQLiteWorktreeStore implements WorktreeStore backed by SQLite.
type SQLiteWorktreeStore struct {
	db *sql.DB
}

// NewWorktreeStore creates a new WorktreeStore instance.
func NewWorktreeStore(db *sql.DB) *SQLiteWorktreeStore {
	return &SQLiteWorktreeStore{db: db}
}

// worktreeColumns is the ordered column list for SELECT queries on worktrees.
// Order matches DDL: id, task_id, project_id, session_id, worktree_path,
// branch_name, base_commit, status, created_at.
const worktreeColumns = `w.id, w.task_id, w.project_id,
	(SELECT COALESCE(s.external_id, s.id) FROM agent_sessions AS s
	 WHERE s.id = w.session_id AND s.project_id = w.project_id),
	w.worktree_path, w.branch_name, w.base_commit, w.status,
	w.generation, w.version, w.created_at, w.updated_at`

// scanWorktree scans a single row into a Worktree struct.
func scanWorktree(scanner interface {
	Scan(dest ...any) error
}) (*model.Worktree, error) {
	var w model.Worktree
	var sessionID sql.NullString

	err := scanner.Scan(
		&w.ID,
		&w.TaskID,
		&w.ProjectID,
		&sessionID,
		&w.WorktreePath,
		&w.BranchName,
		&w.BaseCommit,
		&w.Status,
		&w.Generation,
		&w.Version,
		&w.CreatedAt,
		&w.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	if sessionID.Valid {
		w.SessionID = &sessionID.String
	}

	return &w, nil
}

// Create inserts a new worktree record and returns the auto-incremented ID.
func (s *SQLiteWorktreeStore) Create(ctx context.Context, projectID string, w *model.Worktree) (int64, error) {
	sessionKey, err := resolveNullableSessionKey(ctx, s.db, projectID, w.SessionID)
	if err != nil {
		return 0, fmt.Errorf("worktree create session: %w", err)
	}
	if w.Generation <= 0 {
		w.Generation = 1
	}
	if w.Status == "" {
		w.Status = model.WorktreeStatusAllocated
	}
	if !model.IsWorktreeStatus(w.Status) {
		return 0, fmt.Errorf("worktree create: %w: invalid status %q", ErrTaskStateInvalid, w.Status)
	}
	if w.UpdatedAt == "" {
		w.UpdatedAt = w.CreatedAt
	}
	const query = `INSERT INTO worktrees (
		task_id, project_id, session_id, worktree_path,
		branch_name, base_commit, status, generation, version, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	result, err := s.db.ExecContext(ctx, query,
		w.TaskID, projectID, sessionKey, w.WorktreePath,
		w.BranchName, w.BaseCommit, w.Status, w.Generation, w.Version, w.CreatedAt, w.UpdatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("worktree create: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("worktree create last insert id: %w", err)
	}
	return id, nil
}

// GetByTaskID retrieves the worktree associated with a task (one-to-one).
func (s *SQLiteWorktreeStore) GetByTaskID(ctx context.Context, projectID, taskID string) (*model.Worktree, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM worktrees AS w
		WHERE w.task_id = ? AND w.project_id = ?
		ORDER BY w.generation DESC LIMIT 1`, worktreeColumns)

	row := s.db.QueryRowContext(ctx, query, taskID, projectID)
	w, err := scanWorktree(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrWorktreeNotFound
		}
		return nil, fmt.Errorf("worktree get by task: %w", err)
	}
	return w, nil
}

// UpdateStatus changes the worktree status.
func (s *SQLiteWorktreeStore) UpdateStatus(ctx context.Context, projectID string, id int64, status string) error {
	var current string
	var version int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT status, version FROM worktrees WHERE id = ? AND project_id = ?`, id, projectID,
	).Scan(&current, &version); err != nil {
		if err == sql.ErrNoRows {
			return ErrWorktreeNotFound
		}
		return fmt.Errorf("worktree read status: %w", err)
	}
	if !model.IsWorktreeStatus(status) || !model.CanWorktreeTransition(current, status) {
		return fmt.Errorf("worktree transition %s -> %s: %w", current, status, ErrTaskStateInvalid)
	}
	const query = `UPDATE worktrees
		SET status = ?, version = version + 1, updated_at = datetime('now')
		WHERE id = ? AND project_id = ? AND status = ? AND version = ?`
	result, err := s.db.ExecContext(ctx, query, status, id, projectID, current, version)
	if err != nil {
		return fmt.Errorf("worktree update status: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("worktree update status rows: %w", err)
	}
	if n == 0 {
		return ErrConcurrentConflict
	}
	return nil
}

// ListByStatus returns all worktrees matching the given status (for GC scanning).
func (s *SQLiteWorktreeStore) ListByStatus(ctx context.Context, projectID, status string) ([]*model.Worktree, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM worktrees AS w
		WHERE w.project_id = ? AND w.status = ?
		ORDER BY w.created_at ASC`, worktreeColumns)

	rows, err := s.db.QueryContext(ctx, query, projectID, status)
	if err != nil {
		return nil, fmt.Errorf("worktree list by status: %w", err)
	}
	defer rows.Close()

	var worktrees []*model.Worktree
	for rows.Next() {
		w, err := scanWorktree(rows)
		if err != nil {
			return nil, fmt.Errorf("worktree list by status scan: %w", err)
		}
		worktrees = append(worktrees, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worktree list by status rows: %w", err)
	}
	return worktrees, nil
}

// Delete removes a worktree record by ID scoped to projectID.
func (s *SQLiteWorktreeStore) Delete(ctx context.Context, projectID string, id int64) error {
	const query = `DELETE FROM worktrees WHERE id = ? AND project_id = ?`
	result, err := s.db.ExecContext(ctx, query, id, projectID)
	if err != nil {
		return fmt.Errorf("worktree delete: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("worktree delete rows: %w", err)
	}
	if n == 0 {
		return ErrWorktreeNotFound
	}
	return nil
}

// ListByProject lists all worktrees for a given project.
func (s *SQLiteWorktreeStore) ListByProject(ctx context.Context, projectID string) ([]*model.Worktree, error) {
	query := fmt.Sprintf(`SELECT %s FROM worktrees AS w
		WHERE w.project_id = ? ORDER BY w.created_at DESC`, worktreeColumns)
	rows, err := s.db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("worktree list by project: %w", err)
	}
	defer rows.Close()

	var worktrees []*model.Worktree
	for rows.Next() {
		w, err := scanWorktree(rows)
		if err != nil {
			return nil, fmt.Errorf("worktree list by project scan: %w", err)
		}
		worktrees = append(worktrees, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worktree list by project rows: %w", err)
	}
	return worktrees, nil
}

// Verify interface compliance at compile time.
var _ WorktreeStore = (*SQLiteWorktreeStore)(nil)
