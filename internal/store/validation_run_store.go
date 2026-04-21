package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// SQLiteValidationRunStore implements ValidationRunStore backed by SQLite.
type SQLiteValidationRunStore struct {
	db *sql.DB
}

// NewSQLiteValidationRunStore creates a new ValidationRunStore instance.
func NewSQLiteValidationRunStore(db *sql.DB) *SQLiteValidationRunStore {
	return &SQLiteValidationRunStore{db: db}
}

// validationRunColumns is the ordered column list for SELECT queries on validation_runs.
// Order matches DDL: id, task_id, project_id, attempt, base_commit, changed_files,
// test_command, test_exit_code, test_output, coverage, boundary_ok, test_ok, coverage_ok,
// summary, result, error_code, duration_ms, log_path, created_at.
const validationRunColumns = `id, task_id, project_id, attempt, base_commit, changed_files,
	test_command, test_exit_code, test_output, coverage, boundary_ok, test_ok, coverage_ok,
	summary, result, error_code, duration_ms, log_path, created_at`

// scanValidationRun scans a single row into a ValidationRun struct.
func scanValidationRun(scanner interface {
	Scan(dest ...any) error
}) (*model.ValidationRun, error) {
	var v model.ValidationRun
	var testExitCode sql.NullInt64
	var testOutput, summary, errorCode, logPath sql.NullString
	var coverage sql.NullFloat64
	var boundaryOK, testOK, coverageOK int

	err := scanner.Scan(
		&v.ID,
		&v.TaskID,
		&v.ProjectID,
		&v.Attempt,
		&v.BaseCommit,
		&v.ChangedFiles,
		&v.TestCommand,
		&testExitCode,
		&testOutput,
		&coverage,
		&boundaryOK,
		&testOK,
		&coverageOK,
		&summary,
		&v.Result,
		&errorCode,
		&v.DurationMs,
		&logPath,
		&v.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	v.BoundaryOK = boundaryOK != 0
	v.TestOK = testOK != 0
	v.CoverageOK = coverageOK != 0

	if testExitCode.Valid {
		code := int(testExitCode.Int64)
		v.TestExitCode = &code
	}
	if testOutput.Valid {
		v.TestOutput = &testOutput.String
	}
	if coverage.Valid {
		v.Coverage = &coverage.Float64
	}
	if summary.Valid {
		v.Summary = &summary.String
	}
	if errorCode.Valid {
		v.ErrorCode = &errorCode.String
	}
	if logPath.Valid {
		v.LogPath = &logPath.String
	}

	return &v, nil
}

// Create appends a new validation run record. Returns the auto-incremented ID.
func (s *SQLiteValidationRunStore) Create(ctx context.Context, projectID string, r *model.ValidationRun) (int64, error) {
	const query = `INSERT INTO validation_runs (
		task_id, project_id, attempt, base_commit, changed_files,
		test_command, test_exit_code, test_output, coverage,
		boundary_ok, test_ok, coverage_ok,
		summary, result, error_code, duration_ms, log_path, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	var testExitCode sql.NullInt64
	if r.TestExitCode != nil {
		testExitCode = sql.NullInt64{Int64: int64(*r.TestExitCode), Valid: true}
	}

	boundaryOK := 0
	if r.BoundaryOK {
		boundaryOK = 1
	}
	testOK := 0
	if r.TestOK {
		testOK = 1
	}
	coverageOK := 0
	if r.CoverageOK {
		coverageOK = 1
	}

	result, err := s.db.ExecContext(ctx, query,
		r.TaskID, projectID, r.Attempt, r.BaseCommit, r.ChangedFiles,
		r.TestCommand, testExitCode, r.TestOutput, r.Coverage,
		boundaryOK, testOK, coverageOK,
		r.Summary, r.Result, r.ErrorCode, r.DurationMs, r.LogPath, r.CreatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("validation run create: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("validation run last insert id: %w", err)
	}
	return id, nil
}

// ListByTask returns all validation runs for a task, ordered by attempt ascending.
func (s *SQLiteValidationRunStore) ListByTask(ctx context.Context, projectID, taskID string) ([]*model.ValidationRun, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM validation_runs
		WHERE project_id = ? AND task_id = ?
		ORDER BY attempt ASC`, validationRunColumns)

	rows, err := s.db.QueryContext(ctx, query, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("validation run list by task: %w", err)
	}
	defer rows.Close()

	var runs []*model.ValidationRun
	for rows.Next() {
		v, err := scanValidationRun(rows)
		if err != nil {
			return nil, fmt.Errorf("validation run list scan: %w", err)
		}
		runs = append(runs, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("validation run list rows: %w", err)
	}
	return runs, nil
}

// LatestByTask returns the most recent validation run for a task.
func (s *SQLiteValidationRunStore) LatestByTask(ctx context.Context, projectID, taskID string) (*model.ValidationRun, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM validation_runs
		WHERE project_id = ? AND task_id = ?
		ORDER BY id DESC
		LIMIT 1`, validationRunColumns)

	row := s.db.QueryRowContext(ctx, query, projectID, taskID)
	v, err := scanValidationRun(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("validation run latest by task: %w", err)
	}
	return v, nil
}

// Verify interface compliance at compile time.
var _ ValidationRunStore = (*SQLiteValidationRunStore)(nil)
