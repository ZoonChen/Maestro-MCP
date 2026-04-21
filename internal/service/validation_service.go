package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// TestExecutionConfig controls the security constraints for test command execution.
type TestExecutionConfig struct {
	DefaultTimeout time.Duration // default 120s
	MaxOutputBytes int           // default 65536 (64KB)
	EnvWhitelist   []string      // allowed environment variables
}

// ValidationService implements the zero-trust validation flow for task submissions.
// It does NOT accept changed_files or test_output from agents — the server runs
// git diff and executes tests itself.
type ValidationService struct {
	taskStore       store.TaskStore
	resultStore     store.TaskResultStore
	validationStore store.ValidationRunStore
	worktreeStore   store.WorktreeStore
	activityStore   store.ActivityLogStore
	projectStore    store.ProjectStore
	db              *sql.DB
	eventEmitter    EventEmitter
	testExecConfig  TestExecutionConfig
}

// NewValidationService creates a new ValidationService with all required dependencies.
func NewValidationService(
	taskStore store.TaskStore,
	resultStore store.TaskResultStore,
	validationStore store.ValidationRunStore,
	worktreeStore store.WorktreeStore,
	activityStore store.ActivityLogStore,
	projectStore store.ProjectStore,
	db *sql.DB,
	eventEmitter EventEmitter,
	testCfg TestExecutionConfig,
) *ValidationService {
	return &ValidationService{
		taskStore:       taskStore,
		resultStore:     resultStore,
		validationStore: validationStore,
		worktreeStore:   worktreeStore,
		activityStore:   activityStore,
		projectStore:    projectStore,
		db:              db,
		eventEmitter:    eventEmitter,
		testExecConfig:  testCfg,
	}
}

// SubmitAndValidate implements the main submit_task_result flow:
//  1. Validate task status is "in_progress" and assigned_session_id matches
//  2. Get worktree for the task (base_commit, worktree_path)
//  3. Run git diff for changed files, boundary check, and execute tests
//  4. Create a ValidationRun record with attempt = N+1
//  5. Upsert a TaskResult record
//  6. Update task status to "submitted"
//  7. Log activity (action="submitted")
//
// All operations are wrapped in a single database transaction.
func (s *ValidationService) SubmitAndValidate(ctx context.Context, projectID, taskID, sessionID, workerID string, summary *string) error {
	// Step 1: Get and validate task.
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("submit and validate get task: %w", err)
	}

	if task.Status != model.TaskStatusInProgress {
		return fmt.Errorf("submit and validate: task %s status is %q, expected %q: %w",
			taskID, task.Status, model.TaskStatusInProgress, store.ErrTaskStateInvalid)
	}

	if task.AssignedSessionID == nil || *task.AssignedSessionID != sessionID {
		return fmt.Errorf("submit and validate: task %s not owned by session %s: %w",
			taskID, sessionID, store.ErrTaskNotOwned)
	}

	// Load project for test_requirements fallback.
	project, projErr := s.projectStore.GetByID(ctx, projectID)
	if projErr != nil {
		slog.Error("SubmitAndValidate: failed to load project for config fallback", "project_id", projectID, "error", projErr)
	}

	// Step 2: Get worktree for base_commit and worktree_path.
	worktree, err := s.worktreeStore.GetByTaskID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("submit and validate get worktree: %w", err)
	}

	baseCommit := worktree.BaseCommit
	worktreePath := worktree.WorktreePath

	// Resolve test requirements with fallback chain: Task > Project config > defaults.
	testReqs := resolveTestRequirements(task, project)
	testCommand := ""
	if testReqs != nil {
		testCommand = testReqs.Command
	}

	// Step 3: Real git diff, boundary checks, and optional test execution.
	var changedFilesList []string
	changedFilesList, err = getChangedFiles(ctx, worktreePath, baseCommit)
	if err != nil {
		slog.Error("SubmitAndValidate: failed to get changed files", "task_id", taskID, "error", err)
		changedFilesList = []string{}
	}

	changedFilesBytes, _ := json.Marshal(changedFilesList) //nolint:errchkjson // []string is safe
	changedFiles := string(changedFilesBytes)

	// Boundary check: verify all changed files are within allowed directories
	// and do not match forbidden patterns.
	boundaryResult := checkBoundaries(
		changedFilesList,
		task.AllowedDirectories,
		string(task.ForbiddenPatterns),
	)

	if !boundaryResult.OK {
		violationsJSON, _ := json.Marshal(boundaryResult.Violations) //nolint:errchkjson // []string is safe
		safeEmit(s.eventEmitter, "validation.failed", projectID, map[string]interface{}{"task_id": taskID, "reason": "boundary_violation"})

		// Record the boundary violation as a rejected ValidationRun for audit trail.
		existingRuns, _ := s.validationStore.ListByTask(ctx, projectID, taskID)
		attempt := len(existingRuns) + 1
		violationDetail := string(violationsJSON)
		const insertViolationRun = `INSERT INTO validation_runs (
			project_id, task_id, attempt, changed_files, test_command,
			test_exit_code, test_output, coverage,
			boundary_ok, test_ok, coverage_ok,
			summary, result, error_code, duration_ms
		) VALUES (?, ?, ?, ?, ?, NULL, '',  '', 0, 0, 0, ?, 'rejected',  'BOUNDARY_VIOLATION', 0)`
		_, _ = s.db.ExecContext(ctx, insertViolationRun,
			projectID, taskID, attempt, changedFiles, "", summary)

		// Log activity for observability.
		detail := fmt.Sprintf(`{"session_id":"%s","worker_id":"%s","attempt":%d,"reason":"boundary_violation"}`, sessionID, workerID, attempt)
		const insertActivitySQL = `INSERT INTO activity_log
			(project_id, session_id, task_id, action, detail, created_at)
			VALUES (?, ?, ?, 'validation_rejected', ?, datetime('now'))`
		_, _ = s.db.ExecContext(ctx, insertActivitySQL,
			projectID, sessionID, taskID, detail)

		return fmt.Errorf("SubmitAndValidate: boundary violation for task %s: %s: %w",
			taskID, violationDetail, store.ErrBoundaryViolation)
	}

	// Execute tests if a test command is configured.
	var testOutput string
	var testExitCode sql.NullInt64
	testOK := 0 //nolint:wastedassign // reassigned after test execution
	coverageOK := 0
	var coverage string
	var durationMs int64

	if testCommand != "" {
		start := time.Now()

		// Apply configurable timeout (default 120s) to prevent runaway tests.
		timeout := s.testExecConfig.DefaultTimeout
		if timeout <= 0 {
			timeout = 120 * time.Second
		}
		testCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// Parse test command into argv to avoid sh -c shell injection.
		parts := strings.Fields(testCommand)
		cmd := exec.CommandContext(testCtx, parts[0], parts[1:]...) //nolint:gosec // test_command is validated at task creation time
		cmd.Dir = worktreePath

		// Filter environment variables to whitelist only, preventing leakage
		// of sensitive env vars (tokens, keys) into test subprocesses.
		whitelist := s.testExecConfig.EnvWhitelist
		if len(whitelist) == 0 {
			whitelist = []string{"PATH", "HOME", "GOPATH", "NODE_PATH", "PYTHONPATH", "USER", "LANG", "LC_ALL", "TMPDIR", "TEMP", "CI"}
		}
		cmd.Env = filterEnv(os.Environ(), whitelist)

		var outputBuf bytes.Buffer
		cmd.Stdout = &outputBuf
		cmd.Stderr = &outputBuf

		exitErr := cmd.Run()
		durationMs = time.Since(start).Milliseconds()
		testOutput = outputBuf.String()

		// Truncate output if it exceeds the configured limit to prevent
		// unbounded memory usage from verbose test output.
		maxBytes := s.testExecConfig.MaxOutputBytes
		if maxBytes <= 0 {
			maxBytes = 65536
		}
		if len(testOutput) > maxBytes {
			half := maxBytes / 2
			head := testOutput[:half]
			tail := testOutput[len(testOutput)-half:]
			testOutput = head + "\n... [TRUNCATED] ...\n" + tail
		}

		if exitErr != nil {
			if exitError, ok := exitErr.(*exec.ExitError); ok {
				testExitCode = sql.NullInt64{Int64: int64(exitError.ExitCode()), Valid: true}
			} else {
				testExitCode = sql.NullInt64{Int64: 1, Valid: true}
			}
			testOK = 0
		} else {
			testExitCode = sql.NullInt64{Int64: 0, Valid: true}
			testOK = 1
			coverageOK = 1

			// Parse coverage if a coverage format and path are configured.
			if testReqs != nil && testReqs.CoverageFormat != "" && testReqs.CoveragePath != "" {
				cov := parseCoverage(testReqs.CoveragePath, testReqs.CoverageFormat, worktreePath)
				if cov >= 0 {
					coverage = fmt.Sprintf("%.1f", cov)
					if testReqs.MinCoverage > 0 && cov < testReqs.MinCoverage {
						coverageOK = 0
					}
				}
			}
		}
	} else {
		testOutput = ""
		testOK = 1 // No tests required = pass by default.
		coverageOK = 1
	}

	// Begin transaction for atomic recording.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("submit and validate begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Step 4: Count existing validation runs to determine attempt number.
	existingRuns, err := s.validationStore.ListByTask(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("submit and validate list existing runs: %w", err)
	}
	attempt := len(existingRuns) + 1

	// Determine validation result before recording.
	validationResult := model.TaskStatusSubmitted
	var validationErr error
	if testCommand != "" && testOK == 0 {
		validationResult = "rejected"
		validationErr = fmt.Errorf("test execution failed: %w", store.ErrTestExecutionFailed)
	} else if testCommand != "" && coverageOK == 0 && testReqs != nil && testReqs.MinCoverage > 0 {
		validationResult = "rejected"
		validationErr = fmt.Errorf("coverage below minimum threshold: %w", store.ErrCoverageBelowMin)
	}

	validationRun := &model.ValidationRun{
		TaskID:       taskID,
		ProjectID:    projectID,
		Attempt:      attempt,
		BaseCommit:   baseCommit,
		ChangedFiles: changedFiles,
		TestCommand:  testCommand,
		TestOutput:   &testOutput,
		Result:       validationResult,
		Summary:      summary,
		DurationMs:   int(durationMs),
		CreatedAt:    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}

	// Use the transaction for the validation run insert.
	const insertRunSQL = `INSERT INTO validation_runs (
		task_id, project_id, attempt, base_commit, changed_files,
		test_command, test_exit_code, test_output, coverage,
		boundary_ok, test_ok, coverage_ok,
		summary, result, error_code, duration_ms, log_path, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`

	boundaryOK := 1 // Boundary check already passed (early return on violation)

	result, err := tx.ExecContext(ctx, insertRunSQL,
		validationRun.TaskID, projectID, validationRun.Attempt,
		validationRun.BaseCommit, validationRun.ChangedFiles,
		validationRun.TestCommand, testExitCode, validationRun.TestOutput,
		coverage,
		boundaryOK, testOK, coverageOK,
		validationRun.Summary, validationRun.Result,
		validationRun.ErrorCode, validationRun.DurationMs, validationRun.LogPath,
	)
	if err != nil {
		return fmt.Errorf("submit and validate insert validation run: %w", err)
	}
	runID, _ := result.LastInsertId()
	validationRun.ID = runID

	// Step 5: Upsert TaskResult.
	taskResult := &model.TaskResult{
		ID:           taskID,
		TaskID:       taskID,
		ProjectID:    projectID,
		BaseCommit:   baseCommit,
		ChangedFiles: changedFiles,
		TestCommand:  testCommand,
		TestOutput:   testOutput,
		Summary:      summary,
		SubmittedAt:  time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}

	const upsertResultSQL = `INSERT OR REPLACE INTO task_results (
		id, task_id, project_id, base_commit, changed_files,
		test_command, test_output, coverage, summary,
		submitted_at, validated_at, validation_errors, verifier_notes
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err = tx.ExecContext(ctx, upsertResultSQL,
		taskResult.ID, taskResult.TaskID, projectID,
		taskResult.BaseCommit, taskResult.ChangedFiles,
		taskResult.TestCommand, taskResult.TestOutput,
		taskResult.Coverage, taskResult.Summary,
		taskResult.SubmittedAt, taskResult.ValidatedAt,
		taskResult.ValidationErrors, taskResult.VerifierNotes,
	)
	if err != nil {
		return fmt.Errorf("submit and validate upsert task result: %w", err)
	}

	// Step 6: Update task status to "submitted" only if validation passed.
	if validationResult == model.TaskStatusSubmitted {
		const updateStatusSQL = `UPDATE tasks SET status = ?, updated_at = datetime('now') WHERE id = ? AND project_id = ?`
		_, err = tx.ExecContext(ctx, updateStatusSQL, model.TaskStatusSubmitted, taskID, projectID)
		if err != nil {
			return fmt.Errorf("submit and validate update task status: %w", err)
		}
	}

	// Step 7: Log activity.
	detail := fmt.Sprintf(`{"session_id":"%s","worker_id":"%s","attempt":%d}`, sessionID, workerID, attempt)
	const insertActivitySQL = `INSERT INTO activity_log
		(project_id, session_id, task_id, action, detail, created_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))`

	_, err = tx.ExecContext(ctx, insertActivitySQL,
		projectID, sessionID, taskID, model.ActionSubmitted, detail,
	)
	if err != nil {
		return fmt.Errorf("submit and validate log activity: %w", err)
	}

	// Update worktree status to "submitted".
	const updateWorktreeSQL = `UPDATE worktrees SET status = ? WHERE task_id = ? AND project_id = ?`
	_, err = tx.ExecContext(ctx, updateWorktreeSQL, model.WorktreeStatusSubmitted, taskID, projectID)
	if err != nil {
		return fmt.Errorf("submit and validate update worktree status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("submit and validate commit: %w", err)
	}
	committed = true
	if validationResult == model.TaskStatusSubmitted {
		safeEmit(s.eventEmitter, "validation.passed", projectID, map[string]interface{}{"task_id": taskID, "attempt": attempt})
		safeEmit(s.eventEmitter, "task.submitted", projectID, map[string]interface{}{"task_id": taskID, "attempt": attempt})
	} else {
		safeEmit(s.eventEmitter, "validation.failed", projectID, map[string]interface{}{"task_id": taskID, "attempt": attempt})
	}
	return validationErr
}

// GetValidationHistory returns all validation runs for a task, ordered by
// attempt number ascending (earliest first).
func (s *ValidationService) GetValidationHistory(ctx context.Context, projectID, taskID string) ([]*model.ValidationRun, error) {
	runs, err := s.validationStore.ListByTask(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("get validation history: %w", err)
	}
	return runs, nil
}

// resolveTestRequirements implements the fallback chain: Task > Project > Global defaults.
func resolveTestRequirements(task *model.Task, project *model.Project) *model.TestRequirements {
	// Level 1: Task-level test_requirements
	if task.TestRequirements != nil && string(task.TestRequirements) != "" && string(task.TestRequirements) != "{}" {
		var reqs model.TestRequirements
		if err := json.Unmarshal(task.TestRequirements, &reqs); err == nil && reqs.Command != "" {
			return &reqs
		}
	}

	// Level 2: Project config
	if project != nil && project.Config != nil {
		var cfg model.ProjectConfig
		if err := json.Unmarshal(project.Config, &cfg); err == nil {
			if cfg.DefaultTestCommand != nil && *cfg.DefaultTestCommand != "" {
				return &model.TestRequirements{
					Command:        *cfg.DefaultTestCommand,
					CoverageFormat: ptrToStr(cfg.DefaultCoverageFormat),
					CoveragePath:   ptrToStr(cfg.DefaultCoveragePath),
					MinCoverage:    ptrToFloat64(cfg.DefaultMinCoverage),
				}
			}
		}
	}

	// Level 3: No test requirements — return nil (no tests needed).
	return nil
}

func ptrToStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrToFloat64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// filterEnv returns only environment variables whose keys match the whitelist.
func filterEnv(env []string, whitelist []string) []string {
	allowed := make(map[string]bool, len(whitelist))
	for _, k := range whitelist {
		allowed[k] = true
	}
	var result []string
	for _, e := range env {
		if idx := strings.IndexByte(e, '='); idx > 0 {
			if allowed[e[:idx]] {
				result = append(result, e)
			}
		}
	}
	return result
}
