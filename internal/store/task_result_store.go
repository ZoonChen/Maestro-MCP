package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// SQLiteTaskResultStore implements TaskResultStore backed by SQLite.
type SQLiteTaskResultStore struct {
	db *sql.DB
}

// NewSQLiteTaskResultStore creates a new TaskResultStore instance.
func NewSQLiteTaskResultStore(db *sql.DB) *SQLiteTaskResultStore {
	return &SQLiteTaskResultStore{db: db}
}

// taskResultColumns is the ordered column list for SELECT queries on task_results.
// Order matches DDL: id, task_id, project_id, base_commit, changed_files,
// test_command, test_output, coverage, summary, submitted_at, validated_at,
// validation_errors, verifier_notes.
const taskResultColumns = `id, task_id, project_id, base_commit, changed_files,
	test_command, test_output, coverage, summary, submitted_at, validated_at,
	validation_errors, verifier_notes`

// scanTaskResult scans a single row into a TaskResult struct.
func scanTaskResult(scanner interface {
	Scan(dest ...any) error
}) (*model.TaskResult, error) {
	var r model.TaskResult
	var coverage sql.NullFloat64
	var summary, validatedAt, validationErrors, verifierNotes sql.NullString

	err := scanner.Scan(
		&r.ID,
		&r.TaskID,
		&r.ProjectID,
		&r.BaseCommit,
		&r.ChangedFiles,
		&r.TestCommand,
		&r.TestOutput,
		&coverage,
		&summary,
		&r.SubmittedAt,
		&validatedAt,
		&validationErrors,
		&verifierNotes,
	)
	if err != nil {
		return nil, err
	}

	if coverage.Valid {
		r.Coverage = &coverage.Float64
	}
	if summary.Valid {
		r.Summary = &summary.String
	}
	if validatedAt.Valid {
		r.ValidatedAt = &validatedAt.String
	}
	if validationErrors.Valid {
		r.ValidationErrors = &validationErrors.String
	}
	if verifierNotes.Valid {
		r.VerifierNotes = &verifierNotes.String
	}

	return &r, nil
}

// Upsert inserts or replaces a task result.
// Uses INSERT OR REPLACE on task_id (UNIQUE) to upsert.
func (s *SQLiteTaskResultStore) Upsert(ctx context.Context, projectID string, r *model.TaskResult) error {
	const query = `INSERT OR REPLACE INTO task_results (
		id, task_id, project_id, base_commit, changed_files,
		test_command, test_output, coverage, summary,
		submitted_at, validated_at, validation_errors, verifier_notes
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, query,
		r.ID, r.TaskID, projectID, r.BaseCommit, r.ChangedFiles,
		r.TestCommand, r.TestOutput, r.Coverage, r.Summary,
		r.SubmittedAt, r.ValidatedAt, r.ValidationErrors, r.VerifierNotes,
	)
	if err != nil {
		return fmt.Errorf("task result upsert: %w", err)
	}
	return nil
}

// GetByTaskID retrieves the current (latest) task result for a given task.
func (s *SQLiteTaskResultStore) GetByTaskID(ctx context.Context, projectID, taskID string) (*model.TaskResult, error) {
	query := fmt.Sprintf(
		`SELECT %s FROM task_results WHERE task_id = ? AND project_id = ?`,
		taskResultColumns,
	)
	row := s.db.QueryRowContext(ctx, query, taskID, projectID)
	r, err := scanTaskResult(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("task result get by task id: %w", err)
	}
	return r, nil
}

// Verify interface compliance at compile time.
var _ TaskResultStore = (*SQLiteTaskResultStore)(nil)
