package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// TestExecutionConfig is trusted server configuration. Profiles and active
// policy identity are never derived from a task request.
type TestExecutionConfig struct {
	DefaultTimeout     time.Duration
	MaxOutputBytes     int
	Profiles           *CommandProfileRegistry
	PolicyVersion      string
	PolicyDigest       string
	AllowHostExecution bool // transitional M0 diagnostic path; default false
}

// ValidationError carries the stable fail-closed error taxonomy to REST/MCP.
type ValidationError struct {
	Code    string
	Message string
	Cause   error
}

func (e *ValidationError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
}

func (e *ValidationError) Unwrap() error { return e.Cause }

type validationEvidence struct {
	Authority          string
	Producer           string
	BaseCommit         string
	SourceCommit       string
	ChangedFiles       string
	ProfileRef         string
	PolicyVersion      string
	PolicyDigest       string
	TestExitCode       *int
	TestOutput         string
	Coverage           *float64
	BoundaryOK         bool
	TestOK             bool
	CoverageOK         bool
	Result             string
	ErrorCode          string
	Duration           time.Duration
	OutputTruncated    bool
	EvidenceDigest     string
	WorkspaceDigest    string
	TaskVersion        int64
	LeaseID            string
	LeaseEpoch         int64
	LeaseVersion       int64
	LeaseExpiresAt     string
	PhysicalSessionID  string
	WorkerVersion      int64
	WorktreeID         int64
	WorktreeGeneration int64
	WorktreeVersion    int64
	WorktreePath       string
	WorkspacePath      string
	ResolvedPolicyJSON string
}

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

// SubmitAndValidate executes the fixed evidence pipeline outside a database
// transaction, appends the immutable outcome, then atomically advances state
// only after all required evidence has passed.
func (s *ValidationService) SubmitAndValidate(ctx context.Context, projectID, taskID, sessionID, workerID string, summary *string) error {
	evidence := validationEvidence{
		Authority:    model.EvidenceAuthorityDiagnostic,
		Producer:     model.EvidenceProducerMaestroLocal,
		ChangedFiles: "[]",
		Result:       "error",
	}

	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("submit and validate get task: %w", err)
	}
	if task.Status != model.TaskStatusExecuting {
		return fmt.Errorf("submit and validate: task %s status is %q, expected %q: %w", taskID, task.Status, model.TaskStatusExecuting, store.ErrTaskStateInvalid)
	}
	if task.AssignedSessionID == nil || *task.AssignedSessionID != sessionID {
		return fmt.Errorf("submit and validate: task %s not owned by session %s: %w", taskID, sessionID, store.ErrTaskNotOwned)
	}
	evidence.TaskVersion = task.Version
	if task.AssignedWorkerID == nil || *task.AssignedWorkerID != workerID {
		return fmt.Errorf("submit and validate: task %s not owned by worker %s: %w", taskID, workerID, store.ErrTaskNotOwned)
	}
	if task.ActiveLeaseID == nil || task.LeaseEpoch <= 0 {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"LEASE_INVALID", "task is not bound to the submitting worker and an active lease", store.ErrTaskNotOwned)
	}
	evidence.LeaseID = *task.ActiveLeaseID
	evidence.LeaseEpoch = task.LeaseEpoch
	physicalSessionID, leaseVersion, workerVersion, leaseExpiresAt, err := s.loadActiveValidationAuthority(ctx, projectID, taskID, sessionID, workerID, task)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"LEASE_INVALID", "the execution lease is missing, expired, stale or inconsistent", errors.Join(store.ErrValidationFailed, err))
	}
	evidence.PhysicalSessionID = physicalSessionID
	evidence.LeaseVersion = leaseVersion
	evidence.WorkerVersion = workerVersion
	evidence.LeaseExpiresAt = leaseExpiresAt

	project, err := s.projectStore.GetByID(ctx, projectID)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"VALIDATION_INPUT_INVALID", "project configuration is unavailable", store.ErrValidationFailed)
	}
	evidence.WorkspacePath = project.WorkspacePath
	worktree, err := s.worktreeStore.GetByTaskID(ctx, projectID, taskID)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"WORKTREE_ERROR", "required worktree evidence is unavailable", errors.Join(store.ErrValidationFailed, err))
	}
	if worktree.Status != model.WorktreeStatusActive || worktree.SessionID == nil || *worktree.SessionID != sessionID ||
		worktree.Generation != task.LeaseEpoch {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"WORKTREE_STALE", "worktree status, owner or generation does not match the active lease", store.ErrValidationFailed)
	}
	evidence.BaseCommit = worktree.BaseCommit
	evidence.WorktreeID = worktree.ID
	evidence.WorktreeGeneration = worktree.Generation
	evidence.WorktreeVersion = worktree.Version
	evidence.WorktreePath = worktree.WorktreePath

	worktreePath, sourceCommit, err := verifyWorktreeRepository(ctx, project.WorkspacePath, worktree.WorktreePath, worktree.BaseCommit)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"BASELINE_UNAVAILABLE", "worktree or baseline SHA validation failed", errors.Join(store.ErrValidationFailed, err))
	}
	evidence.SourceCommit = sourceCommit

	policy, err := resolveValidationPolicy(task, project, s.testExecConfig)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"POLICY_INVALID", "required validation policy is invalid", errors.Join(store.ErrValidationFailed, err))
	}
	evidence.ProfileRef = validationProfileReference(policy)
	evidence.PolicyVersion = s.testExecConfig.PolicyVersion
	evidence.PolicyDigest = s.testExecConfig.PolicyDigest
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"POLICY_INVALID", "resolved validation policy could not be sealed", errors.Join(store.ErrValidationFailed, err))
	}
	evidence.ResolvedPolicyJSON = string(policyJSON)
	profile, err := s.testExecConfig.Profiles.Resolve(policy.ProfileID, policy.ProfileVersion, policy.ProfileDigest)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"PROFILE_NOT_APPROVED", "command profile is missing, stale or unapproved", errors.Join(store.ErrValidationFailed, err))
	}
	// Reserve enough authority for the bounded profile before any command is
	// started. The normal execution Lease is intentionally short, while an
	// approved validation profile may run longer. This CAS also serializes two
	// concurrent submit attempts; only the caller holding the renewed version
	// may execute the profile.
	profileTimeout := time.Duration(profile.TimeoutSeconds) * time.Second
	if s.testExecConfig.DefaultTimeout > 0 && s.testExecConfig.DefaultTimeout < profileTimeout {
		profileTimeout = s.testExecConfig.DefaultTimeout
	}
	leaseVersion, workerVersion, leaseExpiresAt, err = s.extendValidationLease(
		ctx, projectID, taskID, sessionID, workerID, &evidence, profileTimeout+30*time.Second,
	)
	if err != nil {
		return &ValidationError{
			Code: "LEASE_RENEWAL_FAILED", Message: "validation did not start because execution authority could not be renewed",
			Cause: errors.Join(store.ErrValidationFailed, err),
		}
	}
	evidence.LeaseVersion = leaseVersion
	evidence.WorkerVersion = workerVersion
	evidence.LeaseExpiresAt = leaseExpiresAt

	changedFiles, err := getChangedFiles(ctx, worktreePath, worktree.BaseCommit)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"DIFF_FAILED", "Git diff evidence could not be collected", errors.Join(store.ErrValidationFailed, err))
	}
	changedJSON, err := json.Marshal(changedFiles)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"DIFF_FAILED", "Git diff evidence could not be encoded", errors.Join(store.ErrValidationFailed, err))
	}
	evidence.ChangedFiles = string(changedJSON)

	boundary := checkBoundariesInWorktree(worktreePath, changedFiles, task.AllowedDirectories, string(task.ForbiddenPatterns))
	if !boundary.OK {
		detail, marshalErr := json.Marshal(boundary.Violations)
		if marshalErr != nil {
			return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
				"EVIDENCE_ENCODING_FAILED", "change boundary evidence could not be encoded", errors.Join(store.ErrValidationFailed, marshalErr))
		}
		code := boundary.ErrorCode
		if code == "" {
			code = "BOUNDARY_VIOLATION"
		}
		cause := error(store.ErrBoundaryViolation)
		if code == "POLICY_INVALID" {
			cause = store.ErrValidationFailed
		}
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			code, "change boundary validation failed: "+sanitizeDiagnostic(string(detail)), cause)
	}
	evidence.BoundaryOK = true

	preExecutionDigest, err := digestWorkspaceSnapshot(worktreePath, changedFiles)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"EVIDENCE_MISMATCH", "workspace evidence could not be sealed", errors.Join(store.ErrValidationFailed, err))
	}
	evidence.WorkspaceDigest = preExecutionDigest

	execution, execErr := executeCommandProfile(ctx, profile, worktreePath, s.testExecConfig)
	evidence.Duration = execution.Duration
	evidence.TestOutput = execution.Output
	evidence.TestExitCode = &execution.ExitCode
	evidence.OutputTruncated = execution.Truncated
	if execution.Truncated {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"OUTPUT_TRUNCATED", "profile output exceeded the hard limit", store.ErrValidationFailed)
	}
	if execution.TimedOut {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"PROFILE_TIMEOUT", "profile execution timed out", store.ErrValidationFailed)
	}
	if execution.Cancelled || errors.Is(execErr, context.Canceled) {
		evidence.Result = "cancelled"
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"VALIDATION_CANCELLED", "validation was cancelled", store.ErrValidationFailed)
	}
	if execErr != nil {
		var exitErr *exec.ExitError
		if errors.As(execErr, &exitErr) || execution.ExitCode > 0 {
			return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
				"TEST_FAILED", "approved profile returned a non-zero exit status", store.ErrTestExecutionFailed)
		}
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"PROFILE_EXEC_ERROR", "approved profile could not be executed", errors.Join(store.ErrValidationFailed, execErr))
	}
	if execution.ExitCode != 0 {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"TEST_FAILED", "approved profile returned a non-zero exit status", store.ErrTestExecutionFailed)
	}
	evidence.TestOK = true

	coverage, err := parseCoverageEvidence(policy.CoveragePath, policy.CoverageFormat, worktreePath)
	if err != nil {
		code := "COVERAGE_INVALID"
		if errors.Is(err, os.ErrNotExist) {
			code = "COVERAGE_MISSING"
		}
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			code, "coverage evidence is missing or invalid", errors.Join(store.ErrValidationFailed, err))
	}
	evidence.Coverage = &coverage
	if coverage < *policy.MinCoverage {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"COVERAGE_BELOW", fmt.Sprintf("coverage %.2f is below required %.2f", coverage, *policy.MinCoverage), store.ErrCoverageBelowMin)
	}
	evidence.CoverageOK = true

	postFiles, err := getChangedFiles(ctx, worktreePath, worktree.BaseCommit)
	if err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"DIFF_FAILED", "post-execution Git diff evidence could not be collected", errors.Join(store.ErrValidationFailed, err))
	}
	postDigest, err := digestWorkspaceSnapshot(worktreePath, postFiles)
	if err != nil || !equalStringSets(changedFiles, postFiles) || postDigest != preExecutionDigest {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"EVIDENCE_MISMATCH", "workspace changed while validation was running", errors.Join(store.ErrValidationFailed, err))
	}
	_, postSourceCommit, err := verifyWorktreeRepository(ctx, project.WorkspacePath, worktree.WorktreePath, worktree.BaseCommit)
	if err != nil || postSourceCommit != sourceCommit {
		evidence.Result = "stale"
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"EVIDENCE_MISMATCH", "source or target SHA changed while validation was running", errors.Join(store.ErrValidationFailed, err))
	}
	if err := s.verifyPolicyStillCurrent(ctx, projectID, taskID, policy); err != nil {
		evidence.Result = "stale"
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"POLICY_STALE", "validation policy changed while validation was running", errors.Join(store.ErrValidationFailed, err))
	}

	evidence.Result = "passed"
	if err := sealValidationEvidence(projectID, taskID, &evidence); err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"EVIDENCE_SEAL_FAILED", "passed evidence could not be sealed", errors.Join(store.ErrValidationFailed, err))
	}
	if err := s.persistSuccessfulValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence); err != nil {
		return s.failValidation(ctx, projectID, taskID, sessionID, workerID, summary, &evidence,
			"STATE_OR_EVIDENCE_PERSIST_FAILED", "passed evidence could not atomically advance task state", errors.Join(store.ErrValidationFailed, err))
	}
	safeEmit(s.eventEmitter, "validation.passed", projectID, map[string]any{"task_id": taskID})
	safeEmit(s.eventEmitter, "task.submitted", projectID, map[string]any{"task_id": taskID})
	return nil
}

func (s *ValidationService) failValidation(ctx context.Context, projectID, taskID, sessionID, workerID string, summary *string, evidence *validationEvidence, code, message string, cause error) error {
	if evidence.Result == "" || evidence.Result == "passed" {
		evidence.Result = "error"
	}
	if code == "BOUNDARY_VIOLATION" || code == "TEST_FAILED" || code == "COVERAGE_BELOW" {
		evidence.Result = "failed"
	}
	evidence.ErrorCode = code
	if err := sealValidationEvidence(projectID, taskID, evidence); err != nil {
		return &ValidationError{Code: "EVIDENCE_SEAL_FAILED", Message: "validation failure evidence could not be sealed; state was not advanced", Cause: errors.Join(store.ErrValidationFailed, err)}
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.persistFailedValidation(persistCtx, projectID, taskID, sessionID, workerID, summary, evidence); err != nil {
		return &ValidationError{Code: "EVIDENCE_PERSIST_FAILED", Message: "validation failure evidence could not be persisted; state was not advanced", Cause: errors.Join(store.ErrValidationFailed, err)}
	}
	safeEmit(s.eventEmitter, "validation.failed", projectID, map[string]any{"task_id": taskID, "error_code": code})
	return &ValidationError{Code: code, Message: message, Cause: cause}
}

func (s *ValidationService) persistFailedValidation(ctx context.Context, projectID, taskID, sessionID, workerID string, summary *string, evidence *validationEvidence) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Failure Evidence and execution authority are reconciled as one state
	// change. Leaving a failed validation in executing with a live Lease would
	// let the same Worker continue mutating a workspace after the platform had
	// already rejected its result.
	var physicalSessionID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM agent_sessions
		WHERE project_id = ? AND COALESCE(external_id, id) = ?`, projectID, sessionID).Scan(&physicalSessionID); err != nil {
		return fmt.Errorf("validation failure session scope: %w", err)
	}
	var (
		currentStatus, currentWorkerID    string
		currentSessionID                  sql.NullString
		currentLeaseID                    sql.NullString
		currentVersion, currentLeaseEpoch int64
	)
	if err := tx.QueryRowContext(ctx, `SELECT status, version, assigned_session_id,
		COALESCE(assigned_worker_id, ''), active_lease_id, lease_epoch FROM tasks
		WHERE project_id = ? AND id = ?`, projectID, taskID).Scan(
		&currentStatus, &currentVersion, &currentSessionID, &currentWorkerID, &currentLeaseID,
		&currentLeaseEpoch,
	); err != nil {
		return fmt.Errorf("validation failure task snapshot: %w", err)
	}
	leaseMatches := (currentLeaseID.Valid && currentLeaseID.String == evidence.LeaseID) ||
		(!currentLeaseID.Valid && evidence.LeaseID == "")
	if currentStatus != model.TaskStatusExecuting || currentVersion < evidence.TaskVersion ||
		!currentSessionID.Valid || currentSessionID.String != physicalSessionID ||
		currentWorkerID != workerID || !leaseMatches {
		return fmt.Errorf("validation failure authority changed: %w", store.ErrConcurrentConflict)
	}
	if currentVersion > evidence.TaskVersion {
		if evidence.LeaseID == "" || requireExecutionAuthorityVersionChain(
			ctx, tx, projectID, taskID, evidence.TaskVersion, currentVersion, evidence.LeaseID,
		) != nil {
			return fmt.Errorf("validation failure task version chain changed: %w", store.ErrConcurrentConflict)
		}
	}

	var leaseStatus, leaseSessionID, leaseWorkerID string
	var leaseVersion, leaseEpoch int64
	if currentLeaseID.Valid {
		if err := tx.QueryRowContext(ctx, `SELECT status, version, epoch, session_id, worker_id
			FROM task_leases WHERE project_id = ? AND task_id = ? AND id = ?`,
			projectID, taskID, currentLeaseID.String,
		).Scan(&leaseStatus, &leaseVersion, &leaseEpoch, &leaseSessionID, &leaseWorkerID); err != nil {
			return fmt.Errorf("validation failure lease snapshot: %w", err)
		}
		if leaseSessionID != physicalSessionID || leaseWorkerID != workerID {
			return fmt.Errorf("validation failure lease ownership changed: %w", store.ErrRecoveryIntegrity)
		}
	}
	var workerStatus string
	var workerVersion int64
	var workerTask sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, current_task_id, version FROM agent_workers
		WHERE project_id = ? AND session_id = ? AND id = ?`,
		projectID, physicalSessionID, workerID,
	).Scan(&workerStatus, &workerTask, &workerVersion); err != nil {
		return fmt.Errorf("validation failure worker snapshot: %w", err)
	}
	if workerTask.Valid && workerTask.String != taskID {
		return fmt.Errorf("validation failure worker owns another task: %w", store.ErrRecoveryIntegrity)
	}

	if _, err := insertValidationRun(ctx, tx, projectID, taskID, summary, evidence); err != nil {
		return err
	}
	targetStatus := validationFailureStatus(evidence.ErrorCode)
	updateQuery := `UPDATE tasks
		SET status = ?, assigned_session_id = NULL, assigned_worker_id = NULL,
		    assigned_at = NULL, active_lease_id = NULL, lease_expires_at = NULL,
		    version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND id = ? AND status = ? AND version = ?`
	updateArgs := []any{targetStatus, projectID, taskID, model.TaskStatusExecuting, currentVersion}
	if currentLeaseID.Valid {
		updateQuery += ` AND active_lease_id = ?`
		updateArgs = append(updateArgs, currentLeaseID.String)
	} else {
		updateQuery += ` AND active_lease_id IS NULL`
	}
	updated, err := tx.ExecContext(ctx, updateQuery, updateArgs...)
	if err != nil {
		return fmt.Errorf("validation failure transition: %w", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("validation failure transition CAS: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	causationID := evidence.EvidenceDigest
	if currentLeaseID.Valid {
		causationID = currentLeaseID.String
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		model.TaskStatusExecuting, targetStatus, currentVersion, currentVersion+1,
		sessionID, "zero-trust validation rejected execution", causationID); err != nil {
		return err
	}
	if currentLeaseID.Valid && leaseStatus == model.LeaseStatusActive {
		updated, err = tx.ExecContext(ctx, `UPDATE task_leases
			SET status = 'released', version = version + 1, updated_at = datetime('now')
			WHERE project_id = ? AND task_id = ? AND id = ? AND status = 'active'
			  AND version = ? AND epoch = ? AND session_id = ? AND worker_id = ?`,
			projectID, taskID, currentLeaseID.String, leaseVersion, leaseEpoch,
			physicalSessionID, workerID)
		if err != nil {
			return fmt.Errorf("validation failure release lease: %w", err)
		}
		if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("validation failure release lease CAS: %w", errors.Join(store.ErrConcurrentConflict, err))
		}
		if err := appendStateHistory(ctx, tx, projectID, "lease", currentLeaseID.String,
			model.LeaseStatusActive, model.LeaseStatusReleased, leaseVersion, leaseVersion+1,
			sessionID, "zero-trust validation released execution authority", causationID); err != nil {
			return err
		}
	}
	if workerTask.Valid {
		targetWorkerStatus := model.WorkerStatusIdle
		if workerStatus == model.WorkerStatusLost {
			targetWorkerStatus = model.WorkerStatusLost
		} else if workerStatus != model.WorkerStatusBusy {
			return fmt.Errorf("validation failure worker status is %s: %w", workerStatus, store.ErrRecoveryIntegrity)
		}
		updated, err = tx.ExecContext(ctx, `UPDATE agent_workers
			SET current_task_id = NULL, status = ?, version = version + 1, last_active = datetime('now')
			WHERE project_id = ? AND session_id = ? AND id = ? AND current_task_id = ?
			  AND status = ? AND version = ?`,
			targetWorkerStatus, projectID, physicalSessionID, workerID, taskID, workerStatus, workerVersion)
		if err != nil {
			return fmt.Errorf("validation failure release worker: %w", err)
		}
		if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("validation failure release worker CAS: %w", errors.Join(store.ErrConcurrentConflict, err))
		}
		if err := appendStateHistory(ctx, tx, projectID, "worker", physicalSessionID+"/"+workerID,
			workerStatus, targetWorkerStatus, workerVersion, workerVersion+1,
			sessionID, "zero-trust validation released worker", causationID); err != nil {
			return err
		}
	}

	var worktreeID, worktreeVersion, worktreeGeneration int64
	var worktreeStatus string
	if evidence.WorktreeID > 0 {
		err = tx.QueryRowContext(ctx, `SELECT id, status, version, generation FROM worktrees
			WHERE project_id = ? AND task_id = ? AND id = ? AND generation = ?`,
			projectID, taskID, evidence.WorktreeID, evidence.WorktreeGeneration,
		).Scan(&worktreeID, &worktreeStatus, &worktreeVersion, &worktreeGeneration)
		if err == nil && (worktreeVersion != evidence.WorktreeVersion || worktreeGeneration != evidence.WorktreeGeneration) {
			return fmt.Errorf("validation failure evidence workspace changed: %w", store.ErrConcurrentConflict)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("validation failure evidence workspace is missing: %w", store.ErrConcurrentConflict)
		}
	} else {
		// Failures discovered before workspace evidence was captured may only
		// quarantine the workspace bound to the exact current Lease generation.
		authorityGeneration := currentLeaseEpoch
		if currentLeaseID.Valid {
			authorityGeneration = leaseEpoch
		}
		err = tx.QueryRowContext(ctx, `SELECT id, status, version, generation FROM worktrees
			WHERE project_id = ? AND task_id = ? AND generation = ?`,
			projectID, taskID, authorityGeneration,
		).Scan(&worktreeID, &worktreeStatus, &worktreeVersion, &worktreeGeneration)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("validation failure workspace snapshot: %w", err)
	}
	if err == nil && (worktreeStatus == model.WorktreeStatusAllocated ||
		worktreeStatus == model.WorktreeStatusActive || worktreeStatus == model.WorktreeStatusSealed ||
		worktreeStatus == model.WorktreeStatusSubmitted) {
		updated, err = tx.ExecContext(ctx, `UPDATE worktrees SET status = 'quarantined',
			version = version + 1, updated_at = datetime('now')
			WHERE id = ? AND project_id = ? AND task_id = ? AND status = ?
			  AND version = ? AND generation = ?`,
			worktreeID, projectID, taskID, worktreeStatus, worktreeVersion, worktreeGeneration)
		if err != nil {
			return fmt.Errorf("validation failure quarantine workspace: %w", err)
		}
		if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("validation failure workspace CAS: %w", errors.Join(store.ErrConcurrentConflict, err))
		}
		if err := appendStateHistory(ctx, tx, projectID, "worktree", fmt.Sprint(worktreeID),
			worktreeStatus, model.WorktreeStatusQuarantined, worktreeVersion, worktreeVersion+1,
			sessionID, "zero-trust validation rejected workspace", evidence.EvidenceDigest); err != nil {
			return err
		}
	}
	detail := validationAuditDetail(workerID, evidence)
	if _, err := tx.ExecContext(ctx, `INSERT INTO activity_log
		(project_id, session_id, task_id, action, detail, created_at)
		VALUES (?, ?, ?, 'validation_rejected', ?, datetime('now'))`, projectID, sessionID, taskID, detail); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log
		(session_id, bound_project, target_project, target_task, action, result, detail, created_at)
		VALUES (?, ?, ?, ?, 'validation.evaluate', 'DENIED', ?, datetime('now'))`, sessionID, projectID, projectID, taskID, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func validationFailureStatus(code string) string {
	switch code {
	case "BOUNDARY_VIOLATION", "TEST_FAILED", "COVERAGE_BELOW":
		return model.TaskStatusFailed
	default:
		return model.TaskStatusNeedsHuman
	}
}

// requireExecutionAuthorityVersionChain permits a validation result to survive
// heartbeats on the same Lease without treating arbitrary Task edits as an
// authority refresh. Every intervening version must be represented by one
// contiguous executing -> executing history row caused by that exact Lease.
func requireExecutionAuthorityVersionChain(
	ctx context.Context,
	tx *sql.Tx,
	projectID, taskID string,
	fromVersion, toVersion int64,
	leaseID string,
) error {
	if toVersion == fromVersion {
		return nil
	}
	if tx == nil || fromVersion < 0 || toVersion < fromVersion || leaseID == "" {
		return store.ErrConcurrentConflict
	}
	var count, distinctFrom, minFrom, maxTo int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT from_version),
		COALESCE(MIN(from_version), -1), COALESCE(MAX(to_version), -1)
		FROM state_history WHERE project_id = ? AND aggregate_type = 'task' AND aggregate_id = ?
		  AND from_version >= ? AND to_version <= ?
		  AND from_status = 'executing' AND to_status = 'executing' AND causation_id = ?`,
		projectID, taskID, fromVersion, toVersion, leaseID,
	).Scan(&count, &distinctFrom, &minFrom, &maxTo); err != nil {
		return err
	}
	expected := toVersion - fromVersion
	if count != expected || distinctFrom != expected || minFrom != fromVersion || maxTo != toVersion {
		return store.ErrConcurrentConflict
	}
	return nil
}

// loadActiveValidationAuthority resolves the external session identifier to
// its project-scoped physical key and proves that the exact task Lease and
// worker reservation are still live. Request parameters are never used as
// authority without this database binding.
func (s *ValidationService) loadActiveValidationAuthority(
	ctx context.Context,
	projectID, taskID, sessionID, workerID string,
	task *model.Task,
) (physicalSessionID string, leaseVersion, workerVersion int64, leaseExpiresAt string, err error) {
	if task == nil || task.ActiveLeaseID == nil || task.AssignedWorkerID == nil ||
		*task.AssignedWorkerID != workerID || task.LeaseEpoch <= 0 {
		return "", 0, 0, "", store.ErrLeaseNotFound
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT sess.id, lease.version, worker.version, lease.expires_at
		FROM agent_sessions AS sess
		JOIN task_leases AS lease
		  ON lease.project_id = sess.project_id AND lease.session_id = sess.id
		JOIN agent_workers AS worker
		  ON worker.project_id = sess.project_id AND worker.session_id = sess.id
		 AND worker.id = lease.worker_id
		WHERE sess.project_id = ? AND COALESCE(sess.external_id, sess.id) = ?
		  AND sess.status = ?
		  AND lease.id = ? AND lease.task_id = ? AND lease.worker_id = ?
		  AND lease.epoch = ? AND lease.status = ?
		  AND julianday(lease.expires_at) > julianday('now')
		  AND worker.status = ? AND worker.current_task_id = ?`,
		projectID, sessionID, model.SessionStatusOnline,
		*task.ActiveLeaseID, taskID, workerID, task.LeaseEpoch, model.LeaseStatusActive,
		model.WorkerStatusBusy, taskID,
	).Scan(&physicalSessionID, &leaseVersion, &workerVersion, &leaseExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, 0, "", store.ErrLeaseExpired
		}
		return "", 0, 0, "", err
	}
	if task.LeaseExpiresAt == nil || *task.LeaseExpiresAt != leaseExpiresAt {
		return "", 0, 0, "", store.ErrLeaseVersionMismatch
	}
	return physicalSessionID, leaseVersion, workerVersion, leaseExpiresAt, nil
}

// extendValidationLease atomically reserves enough time for one approved
// profile invocation. It does not broaden owner, epoch, workspace, or command
// authority. Later explicit heartbeats may advance Lease/Worker versions; the
// success transaction accepts only monotonic advances on the same live Lease.
func (s *ValidationService) extendValidationLease(
	ctx context.Context,
	projectID, taskID, sessionID, workerID string,
	evidence *validationEvidence,
	minimumWindow time.Duration,
) (leaseVersion, workerVersion int64, expiresAt string, err error) {
	if evidence == nil || evidence.LeaseID == "" || evidence.LeaseEpoch <= 0 ||
		evidence.PhysicalSessionID == "" || minimumWindow <= 0 {
		return 0, 0, "", store.ErrInvalidParameter
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, 0, "", fmt.Errorf("extend validation lease begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		currentTaskStatus, currentSessionID, currentWorkerID string
		currentTaskVersion, currentEpoch                     int64
		currentLeaseID, currentTaskExpiry                    sql.NullString
		currentLeaseVersion, currentWorkerVersion            int64
		currentLeaseExpiry                                   string
		currentSessionVersion                                int64
	)
	if err := tx.QueryRowContext(ctx, `SELECT status, version, assigned_session_id,
		COALESCE(assigned_worker_id, ''), lease_epoch, active_lease_id, lease_expires_at
		FROM tasks WHERE project_id = ? AND id = ?`, projectID, taskID).Scan(
		&currentTaskStatus, &currentTaskVersion, &currentSessionID, &currentWorkerID,
		&currentEpoch, &currentLeaseID, &currentTaskExpiry,
	); err != nil {
		return 0, 0, "", fmt.Errorf("extend validation lease task: %w", err)
	}
	if currentTaskStatus != model.TaskStatusExecuting || currentTaskVersion != evidence.TaskVersion ||
		currentSessionID != evidence.PhysicalSessionID || currentWorkerID != workerID ||
		currentEpoch != evidence.LeaseEpoch || !currentLeaseID.Valid || currentLeaseID.String != evidence.LeaseID ||
		!currentTaskExpiry.Valid || currentTaskExpiry.String != evidence.LeaseExpiresAt {
		return 0, 0, "", store.ErrConcurrentConflict
	}
	if err := tx.QueryRowContext(ctx, `SELECT version, expires_at FROM task_leases
		WHERE project_id = ? AND task_id = ? AND id = ? AND session_id = ? AND worker_id = ?
		  AND epoch = ? AND status = 'active' AND julianday(expires_at) > julianday('now')`,
		projectID, taskID, evidence.LeaseID, evidence.PhysicalSessionID, workerID, evidence.LeaseEpoch,
	).Scan(&currentLeaseVersion, &currentLeaseExpiry); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, "", store.ErrLeaseExpired
		}
		return 0, 0, "", fmt.Errorf("extend validation lease authority: %w", err)
	}
	if currentLeaseVersion != evidence.LeaseVersion || currentLeaseExpiry != evidence.LeaseExpiresAt {
		return 0, 0, "", store.ErrLeaseVersionMismatch
	}
	if err := tx.QueryRowContext(ctx, `SELECT version FROM agent_workers
		WHERE project_id = ? AND session_id = ? AND id = ? AND status = 'busy' AND current_task_id = ?`,
		projectID, evidence.PhysicalSessionID, workerID, taskID,
	).Scan(&currentWorkerVersion); err != nil {
		return 0, 0, "", fmt.Errorf("extend validation lease worker: %w", err)
	}
	if currentWorkerVersion != evidence.WorkerVersion {
		return 0, 0, "", store.ErrConcurrentConflict
	}
	if err := tx.QueryRowContext(ctx, `SELECT version FROM agent_sessions
		WHERE project_id = ? AND id = ? AND status = 'online'`,
		projectID, evidence.PhysicalSessionID,
	).Scan(&currentSessionVersion); err != nil {
		return 0, 0, "", fmt.Errorf("extend validation session: %w", err)
	}

	desired := time.Now().UTC().Add(minimumWindow)
	if current, parseErr := time.Parse("2006-01-02 15:04:05", currentLeaseExpiry); parseErr == nil && current.After(desired) {
		desired = current
	}
	expiresAt = desired.Format("2006-01-02 15:04:05")
	updated, err := tx.ExecContext(ctx, `UPDATE task_leases
		SET expires_at = ?, version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND task_id = ? AND id = ? AND session_id = ? AND worker_id = ?
		  AND epoch = ? AND status = 'active' AND version = ?
		  AND julianday(expires_at) > julianday('now')`,
		expiresAt, projectID, taskID, evidence.LeaseID, evidence.PhysicalSessionID, workerID,
		evidence.LeaseEpoch, currentLeaseVersion,
	)
	if err != nil {
		return 0, 0, "", fmt.Errorf("extend validation lease CAS: %w", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		return 0, 0, "", fmt.Errorf("extend validation lease changed: %w", errors.Join(store.ErrLeaseVersionMismatch, err))
	}
	if err := appendStateHistory(ctx, tx, projectID, "lease", evidence.LeaseID,
		model.LeaseStatusActive, model.LeaseStatusActive,
		currentLeaseVersion, currentLeaseVersion+1, sessionID,
		"validation authority extended for bounded execution", evidence.LeaseID); err != nil {
		return 0, 0, "", err
	}
	updated, err = tx.ExecContext(ctx, `UPDATE tasks SET lease_expires_at = ?,
		version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND id = ? AND status = 'executing' AND version = ?
		  AND assigned_session_id = ? AND assigned_worker_id = ? AND lease_epoch = ?
		  AND active_lease_id = ? AND lease_expires_at = ?`,
		expiresAt, projectID, taskID, currentTaskVersion, evidence.PhysicalSessionID, workerID,
		evidence.LeaseEpoch, evidence.LeaseID, currentLeaseExpiry,
	)
	if err != nil {
		return 0, 0, "", fmt.Errorf("extend validation task deadline: %w", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		return 0, 0, "", fmt.Errorf("extend validation task changed: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		model.TaskStatusExecuting, model.TaskStatusExecuting,
		currentTaskVersion, currentTaskVersion+1, sessionID,
		"validation authority deadline extended", evidence.LeaseID); err != nil {
		return 0, 0, "", err
	}
	updated, err = tx.ExecContext(ctx, `UPDATE agent_workers
		SET version = version + 1, last_active = datetime('now')
		WHERE project_id = ? AND session_id = ? AND id = ? AND status = 'busy'
		  AND current_task_id = ? AND version = ?`,
		projectID, evidence.PhysicalSessionID, workerID, taskID, currentWorkerVersion,
	)
	if err != nil {
		return 0, 0, "", fmt.Errorf("extend validation worker CAS: %w", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		return 0, 0, "", fmt.Errorf("extend validation worker changed: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	if err := appendStateHistory(ctx, tx, projectID, "worker", evidence.PhysicalSessionID+"/"+workerID,
		model.WorkerStatusBusy, model.WorkerStatusBusy,
		currentWorkerVersion, currentWorkerVersion+1, sessionID,
		"validation worker authority extended", evidence.LeaseID); err != nil {
		return 0, 0, "", err
	}
	updated, err = tx.ExecContext(ctx, `UPDATE agent_sessions
		SET last_heartbeat = datetime('now'), version = version + 1
		WHERE project_id = ? AND id = ? AND status = 'online' AND version = ?`,
		projectID, evidence.PhysicalSessionID, currentSessionVersion)
	if err != nil {
		return 0, 0, "", fmt.Errorf("extend validation session heartbeat: %w", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		return 0, 0, "", fmt.Errorf("extend validation session changed: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	if err := appendStateHistory(ctx, tx, projectID, "session", evidence.PhysicalSessionID,
		model.SessionStatusOnline, model.SessionStatusOnline,
		currentSessionVersion, currentSessionVersion+1, sessionID,
		"validation session authority extended", evidence.LeaseID); err != nil {
		return 0, 0, "", err
	}
	detail, err := json.Marshal(map[string]any{
		"lease_id": evidence.LeaseID, "lease_epoch": evidence.LeaseEpoch,
		"lease_version": currentLeaseVersion + 1, "expires_at": expiresAt,
		"profile_ref": evidence.ProfileRef,
	})
	if err != nil {
		return 0, 0, "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log
		(session_id, bound_project, target_project, target_task, action, result, detail, created_at)
		VALUES (?, ?, ?, ?, 'validation.lease_extend', 'ALLOWED', ?, datetime('now'))`,
		sessionID, projectID, projectID, taskID, string(detail)); err != nil {
		return 0, 0, "", fmt.Errorf("extend validation audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, "", fmt.Errorf("extend validation lease commit: %w", err)
	}
	evidence.TaskVersion = currentTaskVersion + 1
	return currentLeaseVersion + 1, currentWorkerVersion + 1, expiresAt, nil
}

func (s *ValidationService) persistSuccessfulValidation(ctx context.Context, projectID, taskID, sessionID, workerID string, summary *string, evidence *validationEvidence) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Re-read every authority and policy input inside the success transaction.
	// This closes the gap between filesystem validation and the state change: a
	// concurrent task edit, re-lease, policy edit or worktree replacement makes
	// the CAS fail and the complete transaction (including Evidence) rolls back.
	var currentWorkspacePath string
	var currentProjectConfig string
	if err := tx.QueryRowContext(ctx,
		`SELECT workspace_path, config FROM projects WHERE id = ? AND status = ?`,
		projectID, model.ProjectStatusActive,
	).Scan(&currentWorkspacePath, &currentProjectConfig); err != nil {
		return fmt.Errorf("validation success project snapshot: %w", err)
	}
	if currentWorkspacePath != evidence.WorkspacePath {
		return fmt.Errorf("project workspace changed: %w", store.ErrConcurrentConflict)
	}

	var physicalSessionID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM agent_sessions
		 WHERE project_id = ? AND COALESCE(external_id, id) = ? AND status = ?`,
		projectID, sessionID, model.SessionStatusOnline,
	).Scan(&physicalSessionID); err != nil {
		return fmt.Errorf("validation success session scope: %w", err)
	}
	if physicalSessionID != evidence.PhysicalSessionID {
		return fmt.Errorf("physical session binding changed: %w", store.ErrConcurrentConflict)
	}

	var (
		currentStatus           string
		currentVersion          int64
		currentAssignedSession  sql.NullString
		currentAssignedWorker   sql.NullString
		currentLeaseEpoch       int64
		currentActiveLeaseID    sql.NullString
		currentLeaseExpiresAt   sql.NullString
		currentTestRequirements string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT status, version, assigned_session_id, assigned_worker_id,
		       lease_epoch, active_lease_id, lease_expires_at, test_requirements
		FROM tasks WHERE id = ? AND project_id = ?`, taskID, projectID,
	).Scan(
		&currentStatus, &currentVersion, &currentAssignedSession, &currentAssignedWorker,
		&currentLeaseEpoch, &currentActiveLeaseID, &currentLeaseExpiresAt, &currentTestRequirements,
	); err != nil {
		return fmt.Errorf("validation success task snapshot: %w", err)
	}
	if currentStatus != model.TaskStatusExecuting || currentVersion < evidence.TaskVersion ||
		!currentAssignedSession.Valid || currentAssignedSession.String != physicalSessionID ||
		!currentAssignedWorker.Valid || currentAssignedWorker.String != workerID ||
		currentLeaseEpoch != evidence.LeaseEpoch || !currentActiveLeaseID.Valid || currentActiveLeaseID.String != evidence.LeaseID ||
		!currentLeaseExpiresAt.Valid {
		return fmt.Errorf("task execution authority changed: %w", store.ErrConcurrentConflict)
	}
	if err := requireExecutionAuthorityVersionChain(
		ctx, tx, projectID, taskID, evidence.TaskVersion, currentVersion, evidence.LeaseID,
	); err != nil {
		return fmt.Errorf("task execution authority version chain changed: %w", err)
	}

	currentPolicy, err := resolveValidationPolicy(
		&model.Task{TestRequirements: json.RawMessage(currentTestRequirements)},
		&model.Project{Config: json.RawMessage(currentProjectConfig)},
		s.testExecConfig,
	)
	if err != nil {
		return fmt.Errorf("validation success policy snapshot: %w", err)
	}
	currentPolicyJSON, err := json.Marshal(currentPolicy)
	if err != nil || string(currentPolicyJSON) != evidence.ResolvedPolicyJSON ||
		s.testExecConfig.PolicyVersion != evidence.PolicyVersion ||
		s.testExecConfig.PolicyDigest != evidence.PolicyDigest {
		return fmt.Errorf("resolved policy changed: %w", errors.Join(store.ErrConcurrentConflict, err))
	}

	var currentLeaseVersion, currentLeaseEpochFromRow int64
	var currentLeaseExpiresFromRow string
	if err := tx.QueryRowContext(ctx, `
		SELECT version, epoch, expires_at FROM task_leases
		WHERE id = ? AND project_id = ? AND task_id = ? AND session_id = ? AND worker_id = ?
		  AND status = ? AND julianday(expires_at) > julianday('now')`,
		evidence.LeaseID, projectID, taskID, physicalSessionID, workerID, model.LeaseStatusActive,
	).Scan(&currentLeaseVersion, &currentLeaseEpochFromRow, &currentLeaseExpiresFromRow); err != nil {
		return fmt.Errorf("validation success lease snapshot: %w", err)
	}
	if currentLeaseVersion < evidence.LeaseVersion || currentLeaseEpochFromRow != evidence.LeaseEpoch ||
		currentLeaseExpiresFromRow != currentLeaseExpiresAt.String {
		return fmt.Errorf("lease changed: %w", store.ErrConcurrentConflict)
	}

	var currentWorkerVersion int64
	if err := tx.QueryRowContext(ctx, `
		SELECT version FROM agent_workers
		WHERE project_id = ? AND session_id = ? AND id = ? AND status = ? AND current_task_id = ?`,
		projectID, physicalSessionID, workerID, model.WorkerStatusBusy, taskID,
	).Scan(&currentWorkerVersion); err != nil {
		return fmt.Errorf("validation success worker snapshot: %w", err)
	}
	if currentWorkerVersion < evidence.WorkerVersion {
		return fmt.Errorf("worker reservation changed: %w", store.ErrConcurrentConflict)
	}

	var (
		currentWorktreeStatus     string
		currentWorktreeGeneration int64
		currentWorktreeVersion    int64
		currentWorktreePath       string
		currentBaseCommit         string
		currentWorktreeSession    sql.NullString
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT status, generation, version, worktree_path, base_commit, session_id
		FROM worktrees WHERE id = ? AND project_id = ? AND task_id = ?`,
		evidence.WorktreeID, projectID, taskID,
	).Scan(
		&currentWorktreeStatus, &currentWorktreeGeneration, &currentWorktreeVersion,
		&currentWorktreePath, &currentBaseCommit, &currentWorktreeSession,
	); err != nil {
		return fmt.Errorf("validation success worktree snapshot: %w", err)
	}
	if currentWorktreeStatus != model.WorktreeStatusActive || currentWorktreeGeneration != evidence.WorktreeGeneration ||
		currentWorktreeVersion != evidence.WorktreeVersion || currentWorktreePath != evidence.WorktreePath ||
		currentBaseCommit != evidence.BaseCommit || !currentWorktreeSession.Valid || currentWorktreeSession.String != physicalSessionID {
		return fmt.Errorf("worktree allocation changed: %w", store.ErrConcurrentConflict)
	}
	if _, err := insertValidationRun(ctx, tx, projectID, taskID, summary, evidence); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `INSERT INTO task_results (
		id, task_id, project_id, base_commit, changed_files, test_command,
		test_output, coverage, summary, submitted_at, validated_at,
		validation_errors, verifier_notes
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
	ON CONFLICT(task_id) DO UPDATE SET
		base_commit=excluded.base_commit, changed_files=excluded.changed_files,
		test_command=excluded.test_command, test_output=excluded.test_output,
		coverage=excluded.coverage, summary=excluded.summary,
		submitted_at=excluded.submitted_at, validated_at=excluded.validated_at,
		validation_errors=NULL`,
		taskID, taskID, projectID, evidence.BaseCommit, evidence.ChangedFiles,
		evidence.ProfileRef, evidence.TestOutput, evidence.Coverage, summary, now, now)
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE tasks
		SET status = ?, active_lease_id = NULL, lease_expires_at = NULL,
		    version = version + 1, updated_at = datetime('now')
		WHERE id = ? AND project_id = ? AND status = ? AND version = ?
		  AND assigned_session_id = ? AND assigned_worker_id = ?
		  AND lease_epoch = ? AND active_lease_id = ?`,
		model.TaskStatusValidating, taskID, projectID, model.TaskStatusExecuting, currentVersion,
		physicalSessionID, workerID, evidence.LeaseEpoch, evidence.LeaseID)
	if err != nil {
		return err
	}
	rows, err := updated.RowsAffected()
	if err != nil || rows != 1 {
		return store.ErrConcurrentConflict
	}
	updated, err = tx.ExecContext(ctx, `UPDATE task_leases
		SET status = ?, version = version + 1, updated_at = datetime('now')
		WHERE id = ? AND project_id = ? AND task_id = ? AND session_id = ? AND worker_id = ?
		  AND epoch = ? AND status = ? AND version = ?`,
		model.LeaseStatusCompleted, evidence.LeaseID, projectID, taskID, physicalSessionID, workerID,
		evidence.LeaseEpoch, model.LeaseStatusActive, currentLeaseVersion)
	if err != nil {
		return err
	}
	rows, err = updated.RowsAffected()
	if err != nil || rows != 1 {
		return store.ErrConcurrentConflict
	}
	if err := appendStateHistory(ctx, tx, projectID, "lease", evidence.LeaseID,
		model.LeaseStatusActive, model.LeaseStatusCompleted,
		currentLeaseVersion, currentLeaseVersion+1, sessionID,
		"zero-trust validation completed execution authority", evidence.LeaseID); err != nil {
		return err
	}
	updated, err = tx.ExecContext(ctx, `UPDATE agent_workers
		SET current_task_id = NULL, status = ?, version = version + 1,
		    tasks_completed = tasks_completed + 1, last_active = datetime('now')
		WHERE project_id = ? AND session_id = ? AND id = ? AND current_task_id = ?
		  AND status = ? AND version = ?`,
		model.WorkerStatusIdle, projectID, physicalSessionID, workerID, taskID,
		model.WorkerStatusBusy, currentWorkerVersion)
	if err != nil {
		return err
	}
	rows, err = updated.RowsAffected()
	if err != nil || rows != 1 {
		return store.ErrConcurrentConflict
	}
	if err := appendStateHistory(ctx, tx, projectID, "worker", physicalSessionID+"/"+workerID,
		model.WorkerStatusBusy, model.WorkerStatusIdle,
		currentWorkerVersion, currentWorkerVersion+1, sessionID,
		"zero-trust validation released worker", evidence.LeaseID); err != nil {
		return err
	}
	updated, err = tx.ExecContext(ctx, `UPDATE worktrees
		SET status = ?, version = version + 1, updated_at = datetime('now')
		WHERE id = ? AND project_id = ? AND task_id = ? AND status = ?
		  AND generation = ? AND version = ?`,
		model.WorktreeStatusSealed, evidence.WorktreeID, projectID, taskID, model.WorktreeStatusActive,
		evidence.WorktreeGeneration, evidence.WorktreeVersion)
	if err != nil {
		return err
	}
	rows, err = updated.RowsAffected()
	if err != nil || rows != 1 {
		return store.ErrConcurrentConflict
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		model.TaskStatusExecuting, model.TaskStatusValidating,
		currentVersion, currentVersion+1, sessionID,
		"zero-trust validation evidence sealed", evidence.LeaseID,
	); err != nil {
		return err
	}
	if err := appendStateHistory(ctx, tx, projectID, "worktree", fmt.Sprint(evidence.WorktreeID),
		model.WorktreeStatusActive, model.WorktreeStatusSealed,
		evidence.WorktreeVersion, evidence.WorktreeVersion+1, sessionID,
		"workspace generation sealed", evidence.EvidenceDigest,
	); err != nil {
		return err
	}
	detail := validationAuditDetail(workerID, evidence)
	if _, err := tx.ExecContext(ctx, `INSERT INTO activity_log
		(project_id, session_id, task_id, action, detail, created_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))`, projectID, sessionID, taskID, model.ActionSubmitted, detail); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log
		(session_id, bound_project, target_project, target_task, action, result, detail, created_at)
		VALUES (?, ?, ?, ?, 'validation.evaluate', 'ALLOWED', ?, datetime('now'))`, sessionID, projectID, projectID, taskID, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func insertValidationRun(ctx context.Context, tx *sql.Tx, projectID, taskID string, summary *string, evidence *validationEvidence) (int64, error) {
	if evidence == nil || evidence.Authority != model.EvidenceAuthorityDiagnostic ||
		evidence.Producer != model.EvidenceProducerMaestroLocal {
		return 0, fmt.Errorf("local validation authority must be %s/%s: %w",
			model.EvidenceAuthorityDiagnostic, model.EvidenceProducerMaestroLocal, store.ErrInvalidParameter)
	}
	if err := sealValidationEvidence(projectID, taskID, evidence); err != nil {
		return 0, err
	}
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), 0) + 1 FROM validation_runs WHERE project_id = ? AND task_id = ?`, projectID, taskID).Scan(&attempt); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO validation_runs (
		task_id, project_id, attempt, base_commit, source_commit, changed_files,
		test_command, profile_ref, policy_version, policy_digest, evidence_digest, workspace_digest,
		authority, producer, pipeline_id, job_id,
		test_exit_code, test_output, output_truncated, coverage,
		boundary_ok, test_ok, coverage_ok,
		summary, result, error_code, duration_ms, log_path, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, datetime('now'))`,
		taskID, projectID, attempt, evidence.BaseCommit, evidence.SourceCommit, evidence.ChangedFiles,
		evidence.ProfileRef, evidence.ProfileRef, evidence.PolicyVersion, evidence.PolicyDigest, evidence.EvidenceDigest, evidence.WorkspaceDigest,
		evidence.Authority, evidence.Producer,
		evidence.TestExitCode, evidence.TestOutput, boolInt(evidence.OutputTruncated), evidence.Coverage,
		boolInt(evidence.BoundaryOK), boolInt(evidence.TestOK), boolInt(evidence.CoverageOK),
		summary, evidence.Result, nullString(evidence.ErrorCode), evidence.Duration.Milliseconds())
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// sealValidationEvidence creates the immutable digest stored with each
// validation run. The raw output remains bounded in the row; the digest binds
// its hash together with exact SHAs, policy/profile identity and execution
// authority without duplicating potentially sensitive output in audit logs.
func sealValidationEvidence(projectID, taskID string, evidence *validationEvidence) error {
	if evidence == nil {
		return fmt.Errorf("validation evidence is nil")
	}
	outputDigest := sha256.Sum256([]byte(evidence.TestOutput))
	canonical := struct {
		Authority          string   `json:"authority"`
		Producer           string   `json:"producer"`
		ProjectID          string   `json:"project_id"`
		TaskID             string   `json:"task_id"`
		TargetSHA          string   `json:"target_sha"`
		SourceSHA          string   `json:"source_sha"`
		ChangedFiles       string   `json:"changed_files"`
		ProfileRef         string   `json:"profile_ref"`
		PolicyVersion      string   `json:"policy_version"`
		PolicyDigest       string   `json:"policy_digest"`
		WorkspaceDigest    string   `json:"workspace_digest"`
		TestExitCode       *int     `json:"test_exit_code"`
		TestOutputDigest   string   `json:"test_output_digest"`
		OutputTruncated    bool     `json:"output_truncated"`
		Coverage           *float64 `json:"coverage"`
		BoundaryOK         bool     `json:"boundary_ok"`
		TestOK             bool     `json:"test_ok"`
		CoverageOK         bool     `json:"coverage_ok"`
		Result             string   `json:"result"`
		ErrorCode          string   `json:"error_code"`
		TaskVersion        int64    `json:"task_version"`
		LeaseID            string   `json:"lease_id"`
		LeaseEpoch         int64    `json:"lease_epoch"`
		LeaseVersion       int64    `json:"lease_version"`
		WorktreeID         int64    `json:"worktree_id"`
		WorktreeGeneration int64    `json:"worktree_generation"`
		WorktreeVersion    int64    `json:"worktree_version"`
	}{
		Authority:          evidence.Authority,
		Producer:           evidence.Producer,
		ProjectID:          projectID,
		TaskID:             taskID,
		TargetSHA:          evidence.BaseCommit,
		SourceSHA:          evidence.SourceCommit,
		ChangedFiles:       evidence.ChangedFiles,
		ProfileRef:         evidence.ProfileRef,
		PolicyVersion:      evidence.PolicyVersion,
		PolicyDigest:       evidence.PolicyDigest,
		WorkspaceDigest:    evidence.WorkspaceDigest,
		TestExitCode:       evidence.TestExitCode,
		TestOutputDigest:   "sha256:" + hex.EncodeToString(outputDigest[:]),
		OutputTruncated:    evidence.OutputTruncated,
		Coverage:           evidence.Coverage,
		BoundaryOK:         evidence.BoundaryOK,
		TestOK:             evidence.TestOK,
		CoverageOK:         evidence.CoverageOK,
		Result:             evidence.Result,
		ErrorCode:          evidence.ErrorCode,
		TaskVersion:        evidence.TaskVersion,
		LeaseID:            evidence.LeaseID,
		LeaseEpoch:         evidence.LeaseEpoch,
		LeaseVersion:       evidence.LeaseVersion,
		WorktreeID:         evidence.WorktreeID,
		WorktreeGeneration: evidence.WorktreeGeneration,
		WorktreeVersion:    evidence.WorktreeVersion,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return fmt.Errorf("marshal validation evidence: %w", err)
	}
	digest := sha256.Sum256(encoded)
	evidence.EvidenceDigest = "sha256:" + hex.EncodeToString(digest[:])
	return nil
}

func (s *ValidationService) verifyPolicyStillCurrent(ctx context.Context, projectID, taskID string, expected validationPolicy) error {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return err
	}
	project, err := s.projectStore.GetByID(ctx, projectID)
	if err != nil {
		return err
	}
	current, err := resolveValidationPolicy(task, project, s.testExecConfig)
	if err != nil {
		return err
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		return fmt.Errorf("marshal expected validation policy: %w", err)
	}
	currentJSON, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("marshal current validation policy: %w", err)
	}
	if !bytes.Equal(expectedJSON, currentJSON) {
		return fmt.Errorf("resolved policy changed")
	}
	return nil
}

func (s *ValidationService) GetValidationHistory(ctx context.Context, projectID, taskID string) ([]*model.ValidationRun, error) {
	runs, err := s.validationStore.ListByTask(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("get validation history: %w", err)
	}
	return runs, nil
}

func digestWorkspaceSnapshot(worktreePath string, changedFiles []string) (string, error) {
	files := append([]string(nil), changedFiles...)
	sort.Strings(files)
	hash := sha256.New()
	var total int64
	for _, file := range files {
		resolved, err := resolvePathWithinRoot(worktreePath, file, false)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(hash, "%d:%s:", len(file), file)
		info, err := os.Lstat(resolved)
		if os.IsNotExist(err) {
			_, _ = hash.Write([]byte("deleted\x00"))
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxValidationFileBytes {
			return "", fmt.Errorf("changed path %s is not a bounded regular file", file)
		}
		total += info.Size()
		if total > maxValidationTotalBytes {
			return "", fmt.Errorf("changed file snapshot exceeds %d bytes", maxValidationTotalBytes)
		}
		f, err := os.Open(resolved)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(hash, io.LimitReader(f, info.Size()+1))
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			return "", errors.Join(copyErr, closeErr)
		}
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]string(nil), left...)
	b := append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func validationAuditDetail(workerID string, evidence *validationEvidence) string {
	detail, err := json.Marshal(map[string]any{
		"authority":      evidence.Authority,
		"producer":       evidence.Producer,
		"worker_id":      workerID,
		"source_sha":     evidence.SourceCommit,
		"target_sha":     evidence.BaseCommit,
		"profile":        evidence.ProfileRef,
		"policy_version": evidence.PolicyVersion,
		"policy_digest":  evidence.PolicyDigest,
		"error_code":     evidence.ErrorCode,
	})
	if err != nil {
		return `{}`
	}
	return string(detail)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
