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
const worktreeColumns = `id, task_id, project_id, session_id, worktree_path,
	branch_name, base_commit, status, created_at`

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
		&w.CreatedAt,
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
	const query = `INSERT INTO worktrees (
		task_id, project_id, session_id, worktree_path,
		branch_name, base_commit, status, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	result, err := s.db.ExecContext(ctx, query,
		w.TaskID, projectID, w.SessionID, w.WorktreePath,
		w.BranchName, w.BaseCommit, w.Status, w.CreatedAt,
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
		SELECT %s FROM worktrees
		WHERE task_id = ? AND project_id = ?`, worktreeColumns)

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
	const query = `UPDATE worktrees SET status = ? WHERE id = ? AND project_id = ?`
	result, err := s.db.ExecContext(ctx, query, status, id, projectID)
	if err != nil {
		return fmt.Errorf("worktree update status: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("worktree update status rows: %w", err)
	}
	if n == 0 {
		return ErrWorktreeNotFound
	}
	return nil
}

// ListByStatus returns all worktrees matching the given status (for GC scanning).
func (s *SQLiteWorktreeStore) ListByStatus(ctx context.Context, projectID, status string) ([]*model.Worktree, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM worktrees
		WHERE project_id = ? AND status = ?
		ORDER BY created_at ASC`, worktreeColumns)

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
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, project_id, session_id, worktree_path, branch_name, base_commit, status, created_at
		 FROM worktrees WHERE project_id = ? ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("worktree list by project: %w", err)
	}
	defer rows.Close()

	var worktrees []*model.Worktree
	for rows.Next() {
		var w model.Worktree
		var sessionID sql.NullString
		if err := rows.Scan(&w.ID, &w.TaskID, &w.ProjectID, &sessionID, &w.WorktreePath, &w.BranchName, &w.BaseCommit, &w.Status, &w.CreatedAt); err != nil {
			return nil, fmt.Errorf("worktree list by project scan: %w", err)
		}
		if sessionID.Valid {
			w.SessionID = &sessionID.String
		}
		worktrees = append(worktrees, &w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worktree list by project rows: %w", err)
	}
	return worktrees, nil
}

// Verify interface compliance at compile time.
var _ WorktreeStore = (*SQLiteWorktreeStore)(nil)
