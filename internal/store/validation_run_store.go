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
// Order matches the immutable Evidence shape in schema v4.
const validationRunColumns = `id, task_id, project_id, attempt, base_commit, source_commit, changed_files,
	test_command, profile_ref, policy_version, policy_digest, evidence_digest, workspace_digest,
	authority, producer, pipeline_id, job_id,
	test_exit_code, test_output, output_truncated, coverage, boundary_ok, test_ok, coverage_ok,
	summary, result, error_code, duration_ms, log_path, created_at`

// scanValidationRun scans a single row into a ValidationRun struct.
func scanValidationRun(scanner interface {
	Scan(dest ...any) error
}) (*model.ValidationRun, error) {
	var v model.ValidationRun
	var testExitCode sql.NullInt64
	var testOutput, summary, errorCode, logPath, pipelineID, jobID sql.NullString
	var coverage sql.NullFloat64
	var boundaryOK, testOK, coverageOK, outputTruncated int

	err := scanner.Scan(
		&v.ID,
		&v.TaskID,
		&v.ProjectID,
		&v.Attempt,
		&v.BaseCommit,
		&v.SourceCommit,
		&v.ChangedFiles,
		&v.TestCommand,
		&v.ProfileRef,
		&v.PolicyVersion,
		&v.PolicyDigest,
		&v.EvidenceDigest,
		&v.WorkspaceDigest,
		&v.Authority,
		&v.Producer,
		&pipelineID,
		&jobID,
		&testExitCode,
		&testOutput,
		&outputTruncated,
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
	v.OutputTruncated = outputTruncated != 0

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
	if pipelineID.Valid {
		v.PipelineID = &pipelineID.String
	}
	if jobID.Valid {
		v.JobID = &jobID.String
	}

	return &v, nil
}

// Create appends a new validation run record. Returns the auto-incremented ID.
func (s *SQLiteValidationRunStore) Create(ctx context.Context, projectID string, r *model.ValidationRun) (int64, error) {
	if r == nil {
		return 0, fmt.Errorf("validation run create: nil evidence: %w", ErrInvalidParameter)
	}
	// M0 exposes only the local diagnostic append path. Authority and producer
	// are server-owned; attempting to smuggle CI authority through this Store is
	// rejected rather than silently downgraded.
	if (r.Authority != "" && r.Authority != model.EvidenceAuthorityDiagnostic) ||
		(r.Producer != "" && r.Producer != model.EvidenceProducerMaestroLocal) ||
		r.PipelineID != nil || r.JobID != nil {
		return 0, fmt.Errorf("validation run create: local evidence authority is fixed: %w", ErrInvalidParameter)
	}
	const query = `INSERT INTO validation_runs (
		task_id, project_id, attempt, base_commit, source_commit, changed_files,
		test_command, profile_ref, policy_version, policy_digest, evidence_digest, workspace_digest,
		authority, producer, pipeline_id, job_id,
		test_exit_code, test_output, output_truncated, coverage,
		boundary_ok, test_ok, coverage_ok,
		summary, result, error_code, duration_ms, log_path, created_at
	) VALUES (
		?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?,
		'diagnostic', 'maestro-local', NULL, NULL,
		?, ?, ?, ?,
		?, ?, ?,
		?, ?, ?, ?, ?, ?
	)`

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
	outputTruncated := 0
	if r.OutputTruncated {
		outputTruncated = 1
	}

	result, err := s.db.ExecContext(ctx, query,
		r.TaskID, projectID, r.Attempt, r.BaseCommit, r.SourceCommit, r.ChangedFiles,
		r.TestCommand, r.ProfileRef, r.PolicyVersion, r.PolicyDigest, r.EvidenceDigest, r.WorkspaceDigest,
		testExitCode, r.TestOutput, outputTruncated, r.Coverage,
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
	r.Authority = model.EvidenceAuthorityDiagnostic
	r.Producer = model.EvidenceProducerMaestroLocal
	r.PipelineID = nil
	r.JobID = nil
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
