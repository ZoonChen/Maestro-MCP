package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// SQLiteWorkerStore implements WorkerStore backed by SQLite.
type SQLiteWorkerStore struct {
	db *sql.DB
}

// NewWorkerStore creates a new WorkerStore instance.
func NewWorkerStore(db *sql.DB) *SQLiteWorkerStore {
	return &SQLiteWorkerStore{db: db}
}

// workerColumns is the ordered column list for SELECT queries on agent_workers.
// Order matches DDL: id, session_id, project_id, current_task_id, status,
// tasks_completed, last_active.
const workerColumns = `id, session_id, project_id, current_task_id, status,
	tasks_completed, last_active`

// scanWorker scans a single row into an AgentWorker struct.
func scanWorker(scanner interface {
	Scan(dest ...any) error
}) (*model.AgentWorker, error) {
	var w model.AgentWorker
	var currentTaskID sql.NullString

	err := scanner.Scan(
		&w.ID,
		&w.SessionID,
		&w.ProjectID,
		&currentTaskID,
		&w.Status,
		&w.TasksCompleted,
		&w.LastActive,
	)
	if err != nil {
		return nil, err
	}

	if currentTaskID.Valid {
		w.CurrentTaskID = &currentTaskID.String
	}

	return &w, nil
}

// Create registers a new AgentWorker under the given session.
// The DDL PRIMARY KEY is (id, session_id). sessionID comes from the method
// parameter; w.ID maps to the DDL's `id` column.
func (s *SQLiteWorkerStore) Create(ctx context.Context, projectID, sessionID string, w *model.AgentWorker) error {
	const query = `INSERT INTO agent_workers
		(id, session_id, project_id, current_task_id, status, tasks_completed, last_active)
		VALUES (?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, query,
		w.ID,
		sessionID,
		projectID,
		w.CurrentTaskID,
		w.Status,
		w.TasksCompleted,
		w.LastActive,
	)
	if err != nil {
		return fmt.Errorf("worker create: %w", err)
	}
	return nil
}

// GetByID retrieves a worker by its id, scoped to projectID and sessionID.
// The DDL PRIMARY KEY is (id, session_id).
func (s *SQLiteWorkerStore) GetByID(ctx context.Context, projectID, sessionID, workerID string) (*model.AgentWorker, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM agent_workers
		WHERE id = ? AND session_id = ? AND project_id = ?`, workerColumns)

	row := s.db.QueryRowContext(ctx, query, workerID, sessionID, projectID)
	w, err := scanWorker(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrWorkerNotFound
		}
		return nil, fmt.Errorf("worker get by id: %w", err)
	}
	return w, nil
}

// ListBySession returns all workers belonging to a session.
func (s *SQLiteWorkerStore) ListBySession(ctx context.Context, projectID, sessionID string) ([]*model.AgentWorker, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM agent_workers
		WHERE project_id = ? AND session_id = ?
		ORDER BY last_active ASC`, workerColumns)

	rows, err := s.db.QueryContext(ctx, query, projectID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("worker list by session: %w", err)
	}
	defer rows.Close()

	var workers []*model.AgentWorker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, fmt.Errorf("worker list by session scan: %w", err)
		}
		workers = append(workers, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worker list by session rows: %w", err)
	}
	return workers, nil
}

// UpdateCurrentTask sets the worker's current_task_id. Pass empty taskID to
// clear the assignment. Also refreshes last_active.
func (s *SQLiteWorkerStore) UpdateCurrentTask(ctx context.Context, projectID, sessionID, workerID, taskID string) error {
	const query = `UPDATE agent_workers
		SET current_task_id = ?, last_active = datetime('now')
		WHERE id = ? AND session_id = ? AND project_id = ?`

	var taskIDVal *string
	if taskID != "" {
		taskIDVal = &taskID
	}

	result, err := s.db.ExecContext(ctx, query, taskIDVal, workerID, sessionID, projectID)
	if err != nil {
		return fmt.Errorf("worker update current task: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("worker update current task rows: %w", err)
	}
	if n == 0 {
		return ErrWorkerNotFound
	}
	return nil
}

// Delete removes a worker record.
func (s *SQLiteWorkerStore) Delete(ctx context.Context, projectID, sessionID, workerID string) error {
	const query = `DELETE FROM agent_workers WHERE id = ? AND session_id = ? AND project_id = ?`
	result, err := s.db.ExecContext(ctx, query, workerID, sessionID, projectID)
	if err != nil {
		return fmt.Errorf("worker delete: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("worker delete rows: %w", err)
	}
	if n == 0 {
		return ErrWorkerNotFound
	}
	return nil
}

// GetByIdle retrieves an idle worker from the given session (status='idle', no current_task_id).
func (s *SQLiteWorkerStore) GetByIdle(ctx context.Context, projectID, sessionID string) (*model.AgentWorker, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM agent_workers
		WHERE project_id = ? AND session_id = ? AND status = 'idle' AND current_task_id IS NULL
		ORDER BY last_active ASC
		LIMIT 1`, workerColumns)

	row := s.db.QueryRowContext(ctx, query, projectID, sessionID)
	w, err := scanWorker(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrWorkerNotFound
		}
		return nil, fmt.Errorf("worker get idle: %w", err)
	}
	return w, nil
}

// Update updates a worker's status, tasks_completed, and last_active timestamp.
func (s *SQLiteWorkerStore) Update(ctx context.Context, projectID, sessionID string, w *model.AgentWorker) error {
	const query = `UPDATE agent_workers
		SET status = ?, tasks_completed = ?, current_task_id = ?, last_active = datetime('now')
		WHERE id = ? AND session_id = ? AND project_id = ?`

	result, err := s.db.ExecContext(ctx, query,
		w.Status, w.TasksCompleted, w.CurrentTaskID,
		w.ID, sessionID, projectID,
	)
	if err != nil {
		return fmt.Errorf("worker update: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("worker update rows affected: %w", err)
	}
	if n == 0 {
		return ErrWorkerNotFound
	}
	return nil
}

// CountBySession returns the number of workers under a session (for capacity checks).
func (s *SQLiteWorkerStore) CountBySession(ctx context.Context, projectID, sessionID string) (int, error) {
	const query = `SELECT COUNT(*) FROM agent_workers WHERE project_id = ? AND session_id = ?`
	var count int
	err := s.db.QueryRowContext(ctx, query, projectID, sessionID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("worker count by session: %w", err)
	}
	return count, nil
}

// Verify interface compliance at compile time.
var _ WorkerStore = (*SQLiteWorkerStore)(nil)
