// Package service implements the business logic layer for Maestro-MCP.
// TaskService is the largest and most critical service — it implements the
// task state machine and all task-related business logic including atomic
// claim with retry, zero-trust submission, and verification workflows.
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// TaskService implements the task state machine and all task-related business logic.
type TaskService struct {
	taskStore       store.TaskStore
	resultStore     store.TaskResultStore
	validationStore store.ValidationRunStore
	sessionStore    store.SessionStore
	workerStore     store.WorkerStore
	worktreeStore   store.WorktreeStore
	activityStore   store.ActivityLogStore
	auditStore      store.AuditLogStore
	projectStore    store.ProjectStore
	featureStore    store.FeatureStore
	db              *sql.DB // for transactions

	// EventEmitter pushes real-time WebSocket events on state changes.
	eventEmitter EventEmitter

	// OnFeatureStatusChange is called after task state changes that might affect
	// the parent feature's status. If nil, no callback is made.
	OnFeatureStatusChange func(ctx context.Context, projectID, featureID string)
}

// NewTaskService creates a new TaskService with all required store dependencies.
func NewTaskService(
	taskStore store.TaskStore,
	resultStore store.TaskResultStore,
	validationStore store.ValidationRunStore,
	sessionStore store.SessionStore,
	workerStore store.WorkerStore,
	worktreeStore store.WorktreeStore,
	activityStore store.ActivityLogStore,
	auditStore store.AuditLogStore,
	projectStore store.ProjectStore,
	featureStore store.FeatureStore,
	db *sql.DB,
	eventEmitter EventEmitter,
) *TaskService {
	return &TaskService{
		taskStore:       taskStore,
		resultStore:     resultStore,
		validationStore: validationStore,
		sessionStore:    sessionStore,
		workerStore:     workerStore,
		worktreeStore:   worktreeStore,
		activityStore:   activityStore,
		auditStore:      auditStore,
		projectStore:    projectStore,
		featureStore:    featureStore,
		db:              db,
		eventEmitter:    eventEmitter,
	}
}

// ---------------------------------------------------------------------------
// Task CRUD
// ---------------------------------------------------------------------------

// CreateTask validates and creates a new task.
// Sets defaults for status and priority. Checks circular dependencies if the
// task has dependencies defined.
func (s *TaskService) CreateTask(ctx context.Context, projectID string, t *model.Task) error {
	// Validate required fields.
	if t.Title == "" {
		return fmt.Errorf("CreateTask: %w: title is required", store.ErrInvalidParameter)
	}
	if t.Description == "" {
		return fmt.Errorf("CreateTask: %w: description is required", store.ErrInvalidParameter)
	}
	if t.Role == "" {
		return fmt.Errorf("CreateTask: %w: role is required", store.ErrInvalidParameter)
	}
	if t.FeatureID == "" {
		return fmt.Errorf("CreateTask: %w: feature_id is required", store.ErrInvalidParameter)
	}
	if t.AllowedDirectories == "" {
		return fmt.Errorf("CreateTask: %w: allowed_directories is required", store.ErrInvalidParameter)
	}
	if _, err := parseAllowedDirectories(t.AllowedDirectories); err != nil {
		return fmt.Errorf("CreateTask: %w: %v", store.ErrInvalidParameter, err)
	}
	if _, err := parseForbiddenPatterns(string(t.ForbiddenPatterns)); err != nil {
		return fmt.Errorf("CreateTask: %w: %v", store.ErrInvalidParameter, err)
	}
	if err := ValidateTaskTestRequirements(t.TestRequirements); err != nil {
		return fmt.Errorf("CreateTask: %w: %v", store.ErrInvalidParameter, err)
	}

	// Validate queue routing fields before they can affect scheduler ordering.
	if !validTaskRole(t.Role) {
		return fmt.Errorf("CreateTask: %w: invalid role %q, must be one of backend/frontend/devops/verifier/coordinator", store.ErrInvalidParameter, t.Role)
	}

	// Validate feature exists in the project.
	if s.featureStore != nil {
		if _, err := s.featureStore.GetByID(ctx, projectID, t.FeatureID); err != nil {
			return fmt.Errorf("CreateTask: %w: feature %q not found", store.ErrFeatureNotFound, t.FeatureID)
		}
	}

	// Set defaults.
	if t.Status == "" {
		t.Status = model.TaskStatusQueued
	}
	if t.Status != model.TaskStatusDraft && t.Status != model.TaskStatusQueued {
		return fmt.Errorf("CreateTask: %w: new task status must be draft or queued, got %q", store.ErrTaskStateInvalid, t.Status)
	}
	if t.Priority == "" {
		t.Priority = model.PriorityNormal
	}
	if !validTaskPriority(t.Priority) {
		return fmt.Errorf("CreateTask: %w: invalid priority %q", store.ErrInvalidParameter, t.Priority)
	}
	t.ProjectID = projectID

	// Set timestamps.
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	t.CreatedAt = now
	t.UpdatedAt = now

	// Check circular dependencies if the task has dependencies.
	if len(t.Dependencies) > 0 && string(t.Dependencies) != "[]" && string(t.Dependencies) != "null" {
		var deps []model.Dependency
		if err := json.Unmarshal(t.Dependencies, &deps); err != nil {
			return fmt.Errorf("CreateTask: invalid dependencies JSON: %w", err)
		}
		if len(deps) > 0 {
			hasCycle, err := s.taskStore.CheckCircular(ctx, projectID, t.ID, deps)
			if err != nil {
				return fmt.Errorf("CreateTask: check circular: %w", err)
			}
			if hasCycle {
				return fmt.Errorf("CreateTask: %w", store.ErrCircularDependency)
			}
		}
	}

	// Creation changes the claimable queue when the initial state is queued.
	// Persist the task, queue CAS token, activity and audit as one unit so a
	// caller can never observe a new task under an old queue version.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("CreateTask: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertNewTaskTx(ctx, tx, projectID, t); err != nil {
		return fmt.Errorf("CreateTask: %w", err)
	}
	if t.Status == model.TaskStatusQueued {
		if err := bumpProjectQueueVersionTx(ctx, tx, projectID); err != nil {
			return fmt.Errorf("CreateTask: queue version: %w", err)
		}
	}
	detail := fmt.Sprintf(`{"title":%q,"role":%q,"feature_id":%q}`, t.Title, t.Role, t.FeatureID)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, datetime('now'))`,
		projectID, t.ID, model.ActionCreated, detail,
	); err != nil {
		return fmt.Errorf("CreateTask: activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(bound_project, target_project, target_task, action, result, detail, created_at)
		 VALUES (?, ?, ?, 'task.create', 'ALLOWED', ?, datetime('now'))`,
		projectID, projectID, t.ID, detail,
	); err != nil {
		return fmt.Errorf("CreateTask: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("CreateTask: commit: %w", err)
	}

	safeEmit(s.eventEmitter, "task.created", projectID, map[string]string{"task_id": t.ID})

	// Trigger feature status auto-transition if callback is set.
	if s.OnFeatureStatusChange != nil {
		s.OnFeatureStatusChange(ctx, projectID, t.FeatureID)
	}

	return nil
}

// GetTask retrieves a task by ID within a project.
func (s *TaskService) GetTask(ctx context.Context, projectID, taskID string) (*model.Task, error) {
	t, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("GetTask: %w", err)
	}
	return t, nil
}

// GetTaskResult returns the latest TaskResult for a given task.
func (s *TaskService) GetTaskResult(ctx context.Context, projectID, taskID string) (*model.TaskResult, error) {
	r, err := s.resultStore.GetByTaskID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("GetTaskResult: %w", err)
	}
	return r, nil
}

// ListTasks returns tasks matching the filter within a project.
func (s *TaskService) ListTasks(ctx context.Context, projectID string, filter store.TaskFilter) ([]*model.Task, error) {
	tasks, err := s.taskStore.List(ctx, projectID, filter)
	if err != nil {
		return nil, fmt.Errorf("ListTasks: %w", err)
	}
	return tasks, nil
}

// UpdateTask updates mutable task fields (title, description, allowed_directories, priority, dependencies).
// It validates that the task exists within the project before delegating to the store.
// If dependencies are changed, checks for circular dependencies.
func (s *TaskService) UpdateTask(ctx context.Context, projectID string, t *model.Task) error {
	// Verify the task exists in this project.
	existing, err := s.taskStore.GetByID(ctx, projectID, t.ID)
	if err != nil {
		return fmt.Errorf("UpdateTask: %w", err)
	}

	if _, err := parseAllowedDirectories(t.AllowedDirectories); err != nil {
		return fmt.Errorf("UpdateTask: %w: %v", store.ErrInvalidParameter, err)
	}
	if _, err := parseForbiddenPatterns(string(t.ForbiddenPatterns)); err != nil {
		return fmt.Errorf("UpdateTask: %w: %v", store.ErrInvalidParameter, err)
	}
	if err := ValidateTaskTestRequirements(t.TestRequirements); err != nil {
		return fmt.Errorf("UpdateTask: %w: %v", store.ErrInvalidParameter, err)
	}
	if !validTaskRole(t.Role) {
		return fmt.Errorf("UpdateTask: %w: invalid role %q", store.ErrInvalidParameter, t.Role)
	}
	if !validTaskPriority(t.Priority) {
		return fmt.Errorf("UpdateTask: %w: invalid priority %q", store.ErrInvalidParameter, t.Priority)
	}

	// Status-based field edit restrictions.
	switch existing.Status {
	case model.TaskStatusQueued, model.TaskStatusBlocked:
		// Mutable planning fields are editable. Status and execution authority
		// always remain server-owned and are not written by this use case.
		t.Status = existing.Status
	case model.TaskStatusExecuting:
		// In progress: only description and summary are editable (PRD task-management.md).
		t.Title = existing.Title
		t.Role = existing.Role
		t.Priority = existing.Priority
		t.FeatureID = existing.FeatureID
		t.AllowedDirectories = existing.AllowedDirectories
		t.ForbiddenPatterns = existing.ForbiddenPatterns
		t.RequiredAPIs = existing.RequiredAPIs
		t.TestRequirements = existing.TestRequirements
		t.Dependencies = existing.Dependencies
		// Keep status as-is (don't allow status changes through update).
		t.Status = existing.Status
	case model.TaskStatusValidating, model.TaskStatusReadyForHumanMerge, model.TaskStatusDone:
		return fmt.Errorf("UpdateTask: %w: cannot update task in status %q", store.ErrTaskStateInvalid, existing.Status)
	case model.TaskStatusCancelled:
		return fmt.Errorf("UpdateTask: %w: cannot update cancelled task", store.ErrTaskAlreadyCancelled)
	case model.TaskStatusNeedsHuman:
		return fmt.Errorf("UpdateTask: %w: cannot update needs_human task without an authorized recovery action", store.ErrTaskStateInvalid)
	default:
		return fmt.Errorf("UpdateTask: %w: unknown status %q", store.ErrTaskStateInvalid, existing.Status)
	}

	// Check circular dependencies if dependencies field has changed.
	if len(t.Dependencies) > 0 && string(t.Dependencies) != "[]" && string(t.Dependencies) != "null" {
		var deps []model.Dependency
		if err := json.Unmarshal(t.Dependencies, &deps); err != nil {
			return fmt.Errorf("UpdateTask: invalid dependencies JSON: %w", err)
		}
		if len(deps) > 0 {
			hasCycle, err := s.taskStore.CheckCircular(ctx, projectID, t.ID, deps)
			if err != nil {
				return fmt.Errorf("UpdateTask: check circular: %w", err)
			}
			if hasCycle {
				return fmt.Errorf("UpdateTask: %w", store.ErrCircularDependency)
			}
		}
	} else if len(t.Dependencies) == 0 {
		// No dependencies in the update — keep existing dependencies.
		t.Dependencies = existing.Dependencies
	}

	queueChanged := queuedTaskOrderingChanged(existing, t)
	detail := fmt.Sprintf(`{"title":%q,"priority":%q}`, t.Title, t.Priority)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("UpdateTask: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET
		title = ?, description = ?, role = ?, priority = ?, feature_id = ?,
		allowed_directories = ?, forbidden_patterns = ?, required_apis = ?,
		dependencies = ?, test_requirements = ?, parent_task_id = ?, relation_type = ?,
		summary = ?, version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND id = ? AND status = ? AND version = ?`,
		t.Title, t.Description, t.Role, t.Priority, t.FeatureID,
		t.AllowedDirectories, t.ForbiddenPatterns, t.RequiredAPIs,
		t.Dependencies, t.TestRequirements, t.ParentTaskID, t.RelationType,
		t.Summary, projectID, t.ID, existing.Status, t.Version,
	)
	if err != nil {
		return fmt.Errorf("UpdateTask: update: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("UpdateTask: rows affected: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("UpdateTask: %w", store.ErrConcurrentConflict)
	}
	if queueChanged {
		if err := bumpProjectQueueVersionTx(ctx, tx, projectID); err != nil {
			return fmt.Errorf("UpdateTask: queue version: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, datetime('now'))`,
		projectID, t.ID, model.ActionUpdated, detail,
	); err != nil {
		return fmt.Errorf("UpdateTask: activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(bound_project, target_project, target_task, action, result, detail, created_at)
		 VALUES (?, ?, ?, 'task.update', 'ALLOWED', ?, datetime('now'))`,
		projectID, projectID, t.ID,
		fmt.Sprintf(`{"queue_changed":%t,"from_version":%d,"to_version":%d}`, queueChanged, t.Version, t.Version+1),
	); err != nil {
		return fmt.Errorf("UpdateTask: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("UpdateTask: commit: %w", err)
	}
	t.Version++

	return nil
}

// ClaimTask leases a specific queued task. The queue-level API is preferred;
// this compatibility method verifies that the requested task is still the next
// eligible task before delegating to the same atomic Lease workflow.
func (s *TaskService) ClaimTask(ctx context.Context, projectID, taskID, sessionID, workerID string) (*model.Task, error) {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("ClaimTask: %w", err)
	}
	if task.Status != model.TaskStatusQueued {
		return nil, fmt.Errorf("ClaimTask: %w", store.ErrConcurrentConflict)
	}
	var queueVersion int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT version FROM project_queue_versions WHERE project_id = ?), 0)`,
		projectID,
	).Scan(&queueVersion); err != nil {
		return nil, fmt.Errorf("ClaimTask: read queue version: %w", err)
	}
	claimed, err := s.claimNextTask(ctx, projectID, sessionID, task.Role, workerID, "", queueVersion, "", taskID, nil)
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// ---------------------------------------------------------------------------
// State transitions
// ---------------------------------------------------------------------------

// CancelTask requests cancellation of a cancellable task.
func (s *TaskService) CancelTask(ctx context.Context, projectID, taskID, sessionID, reason string) error {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("CancelTask: %w", err)
	}

	if task.Status != model.TaskStatusQueued && task.Status != model.TaskStatusExecuting &&
		task.Status != model.TaskStatusBlocked && task.Status != model.TaskStatusNeedsHuman {
		return fmt.Errorf("CancelTask: %w: cannot cancel task in status %q", store.ErrTaskStateInvalid, task.Status)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("CancelTask: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	toStatus := model.TaskStatusCancelled
	if task.Status != model.TaskStatusQueued {
		toStatus = model.TaskStatusCancelling
	}
	if task.Status == model.TaskStatusExecuting {
		if task.ActiveLeaseID == nil {
			return fmt.Errorf("CancelTask: executing task has no active lease: %w", store.ErrRecoveryIntegrity)
		}
		var active int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM task_leases WHERE project_id = ? AND task_id = ? AND id = ? AND status = 'active'`,
			projectID, taskID, *task.ActiveLeaseID,
		).Scan(&active); err != nil {
			return fmt.Errorf("CancelTask: lease check: %w", err)
		}
		if active != 1 {
			return fmt.Errorf("CancelTask: active lease missing: %w", store.ErrRecoveryIntegrity)
		}
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, cancel_reason = ?, version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = ? AND version = ?`,
		toStatus, reason, taskID, projectID, task.Status, task.Version)
	if err != nil {
		return fmt.Errorf("CancelTask: update: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("CancelTask: %w", store.ErrConcurrentConflict)
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID, task.Status, toStatus,
		task.Version, task.Version+1, sessionID, "cancellation requested",
		fmt.Sprintf("task.cancel:%s:v%d", taskID, task.Version)); err != nil {
		return err
	}
	finalStatus := toStatus
	finalVersion := task.Version + 1
	// queued/blocked/needs_human have no running side effect. For stopped states
	// the same transaction is the cancellation acknowledgement. Executing work
	// remains cancelling until the lease expires or a Runner acknowledgement is
	// introduced; it must never jump directly to cancelled.
	if toStatus == model.TaskStatusCancelling && task.Status != model.TaskStatusExecuting {
		result, err = tx.ExecContext(ctx,
			`UPDATE tasks SET status = 'cancelled', assigned_session_id = NULL,
			     assigned_worker_id = NULL, assigned_at = NULL, active_lease_id = NULL,
			     lease_expires_at = NULL, version = version + 1, updated_at = datetime('now')
			 WHERE project_id = ? AND id = ? AND status = 'cancelling' AND version = ?`,
			projectID, taskID, task.Version+1,
		)
		if err != nil {
			return fmt.Errorf("CancelTask: acknowledge stopped task: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return store.ErrConcurrentConflict
		}
		if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
			model.TaskStatusCancelling, model.TaskStatusCancelled,
			task.Version+1, task.Version+2, sessionID, "stopped task cancellation acknowledged",
			fmt.Sprintf("task.cancel:%s:v%d", taskID, task.Version)); err != nil {
			return err
		}
		finalStatus, finalVersion = model.TaskStatusCancelled, task.Version+2
	}
	if finalStatus == model.TaskStatusCancelled {
		if err := s.releaseTaskResourcesTx(ctx, tx, projectID, task, model.WorktreeStatusCleanupPending,
			sessionID, "task cancellation released owned resources",
			fmt.Sprintf("task.cancel:%s:v%d", taskID, task.Version)); err != nil {
			return fmt.Errorf("CancelTask: release resources: %w", err)
		}
	}
	if task.Status == model.TaskStatusQueued {
		if err := bumpProjectQueueVersionTx(ctx, tx, projectID); err != nil {
			return fmt.Errorf("CancelTask: queue version: %w", err)
		}
	}
	detail := fmt.Sprintf(`{"reason":%q,"previous_status":%q}`, reason, task.Status)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, session_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		projectID, sessionID, taskID,
		map[bool]string{true: model.ActionCancelled, false: "cancellation_requested"}[finalStatus == model.TaskStatusCancelled], detail); err != nil {
		return fmt.Errorf("CancelTask: log activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(session_id, bound_project, target_project, target_task, action, result, detail, created_at)
		 VALUES (?, ?, ?, ?, 'task.cancel', 'ALLOWED', ?, datetime('now'))`,
		sessionID, projectID, projectID, taskID,
		fmt.Sprintf(`{"to_status":%q,"version":%d}`, finalStatus, finalVersion)); err != nil {
		return fmt.Errorf("CancelTask: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("CancelTask: commit: %w", err)
	}
	if s.OnFeatureStatusChange != nil {
		s.OnFeatureStatusChange(ctx, projectID, task.FeatureID)
	}
	safeEmit(s.eventEmitter, "task."+finalStatus, projectID, map[string]string{"task_id": taskID})
	return nil
}

// ReportBlocker transitions a task from executing to blocked.
func (s *TaskService) ReportBlocker(ctx context.Context, projectID, taskID, sessionID, reason string) error {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("ReportBlocker: %w", err)
	}

	if task.Status != model.TaskStatusExecuting {
		return fmt.Errorf("ReportBlocker: %w: task must be executing, got %q", store.ErrTaskStateInvalid, task.Status)
	}
	if task.AssignedSessionID == nil || *task.AssignedSessionID != sessionID ||
		task.AssignedWorkerID == nil || task.ActiveLeaseID == nil {
		return fmt.Errorf("ReportBlocker: %w", store.ErrTaskNotOwned)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("ReportBlocker: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sessionKey string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM agent_sessions WHERE project_id = ? AND COALESCE(external_id, id) = ? AND status = 'online'`,
		projectID, sessionID,
	).Scan(&sessionKey); err != nil {
		return fmt.Errorf("ReportBlocker: owning session: %w", store.ErrTaskNotOwned)
	}
	var leaseEpoch, leaseVersion, workerVersion int64
	if err := tx.QueryRowContext(ctx,
		`SELECT l.epoch, l.version, w.version FROM task_leases AS l
		 JOIN agent_workers AS w ON w.project_id = l.project_id AND w.session_id = l.session_id
		   AND w.id = l.worker_id
		 WHERE l.project_id = ? AND l.task_id = ? AND l.id = ? AND l.session_id = ?
		   AND l.worker_id = ? AND l.status = 'active' AND julianday(l.expires_at) > julianday('now')
		   AND w.status = 'busy' AND w.current_task_id = l.task_id`,
		projectID, taskID, *task.ActiveLeaseID, sessionKey, *task.AssignedWorkerID,
	).Scan(&leaseEpoch, &leaseVersion, &workerVersion); err != nil || leaseEpoch != task.LeaseEpoch {
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("ReportBlocker: lease: %w", err)
		}
		return fmt.Errorf("ReportBlocker: %w", store.ErrLeaseExpired)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'blocked', blocker_reason = ?, assigned_session_id = NULL,
		     assigned_worker_id = NULL, assigned_at = NULL, active_lease_id = NULL,
		     lease_expires_at = NULL, version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = 'executing' AND version = ? AND active_lease_id = ?`,
		reason, taskID, projectID, task.Version, *task.ActiveLeaseID)
	if err != nil {
		return fmt.Errorf("ReportBlocker: update: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("ReportBlocker: %w", store.ErrConcurrentConflict)
	}
	result, err = tx.ExecContext(ctx,
		`UPDATE task_leases SET status = 'released', version = version + 1, updated_at = datetime('now')
		 WHERE project_id = ? AND task_id = ? AND id = ? AND session_id = ? AND worker_id = ?
		   AND status = 'active' AND epoch = ? AND version = ?`,
		projectID, taskID, *task.ActiveLeaseID, sessionKey, *task.AssignedWorkerID,
		task.LeaseEpoch, leaseVersion,
	)
	if err != nil {
		return fmt.Errorf("ReportBlocker: release lease: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return store.ErrConcurrentConflict
	}
	if err := appendStateHistory(ctx, tx, projectID, "lease", *task.ActiveLeaseID,
		model.LeaseStatusActive, model.LeaseStatusReleased, leaseVersion, leaseVersion+1,
		sessionID, "execution blocked and authority released", *task.ActiveLeaseID); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx,
		`UPDATE agent_workers SET current_task_id = NULL, status = 'idle', version = version + 1,
		     last_active = datetime('now')
		 WHERE project_id = ? AND session_id = ? AND id = ? AND current_task_id = ?
		   AND status = 'busy' AND version = ?`,
		projectID, sessionKey, *task.AssignedWorkerID, taskID, workerVersion,
	)
	if err != nil {
		return fmt.Errorf("ReportBlocker: release worker: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("ReportBlocker: worker changed: %w", store.ErrConcurrentConflict)
	}
	if err := appendStateHistory(ctx, tx, projectID, "worker", sessionKey+"/"+*task.AssignedWorkerID,
		model.WorkerStatusBusy, model.WorkerStatusIdle, workerVersion, workerVersion+1,
		sessionID, "execution blocked and worker released", *task.ActiveLeaseID); err != nil {
		return err
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		model.TaskStatusExecuting, model.TaskStatusBlocked, task.Version, task.Version+1,
		sessionID, reason, *task.ActiveLeaseID); err != nil {
		return err
	}
	detail := fmt.Sprintf(`{"reason":%q}`, reason)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, session_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		projectID, sessionID, taskID, model.ActionBlocked, detail); err != nil {
		return fmt.Errorf("ReportBlocker: log activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(session_id, bound_project, target_project, target_task, action, result, detail, created_at)
		 VALUES (?, ?, ?, ?, 'task.block', 'ALLOWED', ?, datetime('now'))`,
		sessionID, projectID, projectID, taskID, detail); err != nil {
		return fmt.Errorf("ReportBlocker: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ReportBlocker: commit: %w", err)
	}

	safeEmit(s.eventEmitter, "task.blocked", projectID, map[string]string{"task_id": taskID})

	return nil
}

// ResolveBlocker transitions a blocked task back to work.
// A blocked task always returns to queued. Reusing an old execution assignment
// would bypass a fresh lease, so reassign=true is rejected.
func (s *TaskService) ResolveBlocker(ctx context.Context, projectID, taskID string, reassign bool, resolution string) error {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("ResolveBlocker: %w", err)
	}

	if task.Status != model.TaskStatusBlocked {
		return fmt.Errorf("ResolveBlocker: %w: task must be blocked, got %q", store.ErrTaskStateInvalid, task.Status)
	}
	if task.ActiveLeaseID != nil {
		return fmt.Errorf("ResolveBlocker: blocked task still has active lease: %w", store.ErrRecoveryIntegrity)
	}

	if reassign {
		return fmt.Errorf("ResolveBlocker: reassign requires a fresh lease: %w", store.ErrInvalidParameter)
	}
	newStatus := model.TaskStatusQueued

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("ResolveBlocker: begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()
	if err := s.ensureTaskWorktreeRequeueableTx(ctx, tx, projectID, task); err != nil {
		return fmt.Errorf("ResolveBlocker: workspace is not requeueable: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, assigned_session_id = NULL, assigned_worker_id = NULL,
		     assigned_at = NULL, blocker_reason = NULL, version = version + 1,
		     updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = ? AND version = ? AND active_lease_id IS NULL`,
		newStatus, taskID, projectID, model.TaskStatusBlocked, task.Version)
	if err != nil {
		return fmt.Errorf("ResolveBlocker: update: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("ResolveBlocker: %w", store.ErrConcurrentConflict)
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		model.TaskStatusBlocked, model.TaskStatusQueued, task.Version, task.Version+1,
		"coordinator", stateHistoryReason(resolution, "blocker resolved"),
		fmt.Sprintf("task.resolve-blocker:%s:v%d", taskID, task.Version)); err != nil {
		return err
	}
	if err := bumpProjectQueueVersionTx(ctx, tx, projectID); err != nil {
		return fmt.Errorf("ResolveBlocker: queue version: %w", err)
	}

	detail := fmt.Sprintf(`{"reassign":%v,"resolution":%q,"previous_blocker_reason":%q}`, reassign, resolution, ptrStr(task.BlockerReason))
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, session_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		projectID, task.AssignedSessionID, taskID, model.ActionUnblocked, detail); err != nil {
		return fmt.Errorf("ResolveBlocker: log activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(bound_project, target_project, target_task, action, result, detail, created_at)
		 VALUES (?, ?, ?, 'task.resolve_blocker', 'ALLOWED', ?, datetime('now'))`,
		projectID, projectID, taskID, detail); err != nil {
		return fmt.Errorf("ResolveBlocker: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ResolveBlocker: commit: %w", err)
	}

	safeEmit(s.eventEmitter, "task.unblocked", projectID, map[string]string{"task_id": taskID})

	return nil
}

// ---------------------------------------------------------------------------
// Submit task result
// ---------------------------------------------------------------------------

// SubmitTaskResult is retained only for source compatibility with the v2
// in-process API. It is deliberately disabled because accepting caller-supplied
// results would create a validating task without immutable Git/policy/profile
// Evidence. All transports use ValidationService.SubmitAndValidate instead.
func (s *TaskService) SubmitTaskResult(ctx context.Context, projectID, taskID, sessionID, _ string, _ *model.TaskResult) error {
	s.logAudit(ctx, projectID, "task.submit_result", "DENIED", &sessionID, &taskID)
	return fmt.Errorf("SubmitTaskResult: caller-supplied result path is disabled; use zero-trust validation: %w", store.ErrOperationDisabled)
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

// GetVerificationTask atomically leases a validating task to an independent
// verifier. The original execution assignment remains attribution only; the
// active lease is the sole authority for submitting the verdict.
func (s *TaskService) GetVerificationTask(ctx context.Context, projectID, verifierSessionID, verifierWorkerID string) (*model.Task, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("GetVerificationTask: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var verifierKey, role, sessionStatus string
	if err := tx.QueryRowContext(ctx,
		`SELECT id, role, status FROM agent_sessions
		 WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		projectID, verifierSessionID,
	).Scan(&verifierKey, &role, &sessionStatus); err != nil {
		if err == sql.ErrNoRows {
			return nil, store.ErrSessionNotFound
		}
		return nil, fmt.Errorf("GetVerificationTask: verifier session: %w", err)
	}
	if role != model.RoleVerifier || sessionStatus != model.SessionStatusOnline {
		return nil, fmt.Errorf("GetVerificationTask: verifier identity/status: %w", store.ErrTaskNotOwned)
	}

	var workerVersion int64
	var workerStatus string
	var currentTask sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT status, current_task_id, version FROM agent_workers
		 WHERE project_id = ? AND session_id = ? AND id = ?`,
		projectID, verifierKey, verifierWorkerID,
	).Scan(&workerStatus, &currentTask, &workerVersion); err != nil {
		if err == sql.ErrNoRows {
			return nil, store.ErrWorkerNotFound
		}
		return nil, fmt.Errorf("GetVerificationTask: verifier worker: %w", err)
	}
	if workerStatus != model.WorkerStatusIdle || currentTask.Valid {
		return nil, fmt.Errorf("GetVerificationTask: verifier worker is not idle: %w", store.ErrConcurrentConflict)
	}

	var taskID string
	var taskVersion, priorEpoch int64
	if err := tx.QueryRowContext(ctx, `
		SELECT id, version, lease_epoch FROM tasks
		WHERE project_id = ? AND status = 'validating'
		  AND verified_by IS NULL AND active_lease_id IS NULL
		  AND (assigned_session_id IS NULL OR assigned_session_id <> ?)
		ORDER BY created_at ASC
		LIMIT 1`, projectID, verifierKey,
	).Scan(&taskID, &taskVersion, &priorEpoch); err != nil {
		if err == sql.ErrNoRows {
			return nil, store.ErrNoAvailableTask
		}
		return nil, fmt.Errorf("GetVerificationTask: select validating task: %w", err)
	}

	leaseID := fmt.Sprintf("verify-%d-%s", time.Now().UTC().UnixNano(), verifierWorkerID)
	leaseEpoch := priorEpoch + 1
	expiresAt := time.Now().UTC().Add(taskLeaseDuration).Format("2006-01-02 15:04:05")
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_leases
		 (id, project_id, task_id, session_id, worker_id, epoch, status, version, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'active', 1, ?, datetime('now'), datetime('now'))`,
		leaseID, projectID, taskID, verifierKey, verifierWorkerID, leaseEpoch, expiresAt,
	); err != nil {
		return nil, fmt.Errorf("GetVerificationTask: create verifier lease: %w", err)
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET verified_by = ?, verified_at = NULL, lease_epoch = ?, active_lease_id = ?,
		     lease_expires_at = ?, version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = 'validating' AND version = ?
		   AND verified_by IS NULL AND active_lease_id IS NULL`,
		verifierKey, leaseEpoch, leaseID, expiresAt, taskID, projectID, taskVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("GetVerificationTask: reserve task: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, store.ErrConcurrentConflict
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		model.TaskStatusValidating, model.TaskStatusValidating, taskVersion, taskVersion+1,
		verifierSessionID, "verification lease accepted", leaseID); err != nil {
		return nil, err
	}

	result, err = tx.ExecContext(ctx,
		`UPDATE agent_workers SET current_task_id = ?, status = 'busy', version = version + 1,
		     last_active = datetime('now')
		 WHERE project_id = ? AND session_id = ? AND id = ? AND status = 'idle'
		   AND current_task_id IS NULL AND version = ?`,
		taskID, projectID, verifierKey, verifierWorkerID, workerVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("GetVerificationTask: reserve verifier worker: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, store.ErrConcurrentConflict
	}
	if err := appendStateHistory(ctx, tx, projectID, "worker", verifierKey+"/"+verifierWorkerID,
		model.WorkerStatusIdle, model.WorkerStatusBusy, workerVersion, workerVersion+1,
		verifierSessionID, "verification lease accepted", leaseID); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, session_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		projectID, verifierSessionID, taskID, model.ActionVerifying,
		fmt.Sprintf(`{"verifier_worker_id":%q,"lease_id":%q,"epoch":%d}`, verifierWorkerID, leaseID, leaseEpoch)); err != nil {
		return nil, fmt.Errorf("GetVerificationTask: log activity: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("GetVerificationTask: commit: %w", err)
	}
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("GetVerificationTask: get validating task: %w", err)
	}
	safeEmit(s.eventEmitter, "task.verifying", projectID, map[string]string{"task_id": taskID, "lease_id": leaseID})
	return task, nil
}

// SubmitVerification handles a verifier submitting their verdict on a task.
// A failed verdict is terminal for this attempt. Re-queueing requires an
// explicit recovery operation that creates a fresh execution lease.
func (s *TaskService) SubmitVerification(ctx context.Context, projectID, verifierSessionID, verifierWorkerID, taskID string, passed bool, notes string) error {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("SubmitVerification: %w", err)
	}

	if task.Status != model.TaskStatusValidating || task.VerifiedBy == nil || *task.VerifiedBy != verifierSessionID || task.ActiveLeaseID == nil {
		return fmt.Errorf("SubmitVerification: %w: verifier does not own an active validating lease", store.ErrTaskNotOwned)
	}

	// A human/agent verdict can never manufacture missing quality evidence. For
	// a passing verdict, recalculate the sealed workspace snapshot before the
	// transaction and bind it to the latest immutable passed ValidationRun.
	var workspaceEvidence *verificationWorkspaceEvidence
	if passed {
		workspaceEvidence, err = s.captureVerificationWorkspaceEvidence(ctx, projectID, taskID)
		if err != nil {
			return fmt.Errorf("SubmitVerification: passed evidence is missing or stale: %w", errors.Join(store.ErrValidationFailed, err))
		}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("SubmitVerification: begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()
	var verifierKey string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM agent_sessions WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		projectID, verifierSessionID,
	).Scan(&verifierKey); err != nil {
		return fmt.Errorf("SubmitVerification: verifier scope: %w", err)
	}
	var leaseEpoch, leaseVersion, workerVersion int64
	if err := tx.QueryRowContext(ctx,
		`SELECT l.epoch, l.version, worker.version FROM task_leases AS l
		 JOIN agent_sessions AS sess ON sess.project_id = l.project_id AND sess.id = l.session_id
		 JOIN agent_workers AS worker ON worker.project_id = l.project_id
		   AND worker.session_id = l.session_id AND worker.id = l.worker_id
		 WHERE l.id = ? AND l.project_id = ? AND l.task_id = ?
		   AND l.session_id = ? AND l.worker_id = ? AND l.status = 'active'
		   AND sess.status = 'online' AND worker.status = 'busy' AND worker.current_task_id = l.task_id
		   AND julianday(l.expires_at) > julianday('now')`,
		*task.ActiveLeaseID, projectID, taskID, verifierKey, verifierWorkerID,
	).Scan(&leaseEpoch, &leaseVersion, &workerVersion); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("SubmitVerification: %w", store.ErrLeaseExpired)
		}
		return fmt.Errorf("SubmitVerification: validate lease: %w", err)
	}
	if leaseEpoch != task.LeaseEpoch {
		return fmt.Errorf("SubmitVerification: lease epoch mismatch: %w", store.ErrLeaseVersionMismatch)
	}
	if passed {
		if err := requireLatestPassedValidationEvidence(ctx, tx, projectID, taskID, workspaceEvidence); err != nil {
			return fmt.Errorf("SubmitVerification: passed evidence is missing or stale: %w", errors.Join(store.ErrValidationFailed, err))
		}
	}

	newStatus, action := model.TaskStatusFailed, model.ActionRejected
	if passed {
		newStatus, action = model.TaskStatusReadyForHumanMerge, model.ActionApproved
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, verified_at = datetime('now'), active_lease_id = NULL,
		     lease_expires_at = NULL, version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = 'validating' AND version = ?
		   AND verified_by = ? AND active_lease_id = ?`,
		newStatus, taskID, projectID, task.Version, verifierKey, *task.ActiveLeaseID,
	)
	if err != nil {
		return fmt.Errorf("SubmitVerification: update verdict: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("SubmitVerification: %w", store.ErrConcurrentConflict)
	}
	result, err = tx.ExecContext(ctx,
		`UPDATE task_leases SET status = 'completed', version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND task_id = ? AND session_id = ? AND worker_id = ?
		   AND epoch = ? AND status = 'active' AND version = ?`,
		*task.ActiveLeaseID, projectID, taskID, verifierKey, verifierWorkerID,
		task.LeaseEpoch, leaseVersion,
	)
	if err != nil {
		return fmt.Errorf("SubmitVerification: complete lease: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("SubmitVerification: lease changed: %w", store.ErrConcurrentConflict)
	}
	if err := appendStateHistory(ctx, tx, projectID, "lease", *task.ActiveLeaseID,
		model.LeaseStatusActive, model.LeaseStatusCompleted, leaseVersion, leaseVersion+1,
		verifierSessionID, "verification verdict submitted", *task.ActiveLeaseID); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx,
		`UPDATE agent_workers SET current_task_id = NULL, status = 'idle', version = version + 1,
		     tasks_completed = tasks_completed + 1, last_active = datetime('now')
		 WHERE project_id = ? AND session_id = ? AND id = ? AND current_task_id = ?
		   AND status = 'busy' AND version = ?`,
		projectID, verifierKey, verifierWorkerID, taskID, workerVersion,
	)
	if err != nil {
		return fmt.Errorf("SubmitVerification: release verifier worker: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("SubmitVerification: verifier worker changed: %w", store.ErrConcurrentConflict)
	}
	if err := appendStateHistory(ctx, tx, projectID, "worker", verifierKey+"/"+verifierWorkerID,
		model.WorkerStatusBusy, model.WorkerStatusIdle, workerVersion, workerVersion+1,
		verifierSessionID, "verification verdict submitted", *task.ActiveLeaseID); err != nil {
		return err
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		model.TaskStatusValidating, newStatus, task.Version, task.Version+1,
		verifierSessionID, "verification verdict", *task.ActiveLeaseID); err != nil {
		return err
	}
	if notes != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE task_results SET verifier_notes = ? WHERE project_id = ? AND task_id = ?`,
			notes, projectID, taskID,
		); err != nil {
			return fmt.Errorf("SubmitVerification: store notes: %w", err)
		}
	}

	// Log activity.
	detail := fmt.Sprintf(`{"passed":%v,"verifier_session_id":%q,"notes":%q}`, passed, verifierSessionID, notes)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, session_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		projectID, verifierSessionID, taskID, action, detail); err != nil {
		return fmt.Errorf("SubmitVerification: log activity: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("SubmitVerification: commit: %w", err)
	}

	if s.OnFeatureStatusChange != nil {
		s.OnFeatureStatusChange(ctx, projectID, task.FeatureID)
	}

	if passed {
		safeEmit(s.eventEmitter, "task.approved", projectID, map[string]string{"task_id": taskID})
	} else {
		safeEmit(s.eventEmitter, "task.rejected", projectID, map[string]string{"task_id": taskID})
	}

	return nil
}

type verificationWorkspaceEvidence struct {
	WorktreeID      int64
	WorktreeVersion int64
	BaseCommit      string
	SourceCommit    string
	ChangedFiles    string
	WorkspaceDigest string
}

func (s *TaskService) captureVerificationWorkspaceEvidence(ctx context.Context, projectID, taskID string) (*verificationWorkspaceEvidence, error) {
	project, err := s.projectStore.GetByID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	worktree, err := s.worktreeStore.GetByTaskID(ctx, projectID, taskID)
	if err != nil {
		return nil, err
	}
	if worktree.Status != model.WorktreeStatusSealed {
		return nil, fmt.Errorf("worktree status is %q, expected %q", worktree.Status, model.WorktreeStatusSealed)
	}
	canonicalWorktree, sourceCommit, err := verifyWorktreeRepository(ctx, project.WorkspacePath, worktree.WorktreePath, worktree.BaseCommit)
	if err != nil {
		return nil, err
	}
	changedFiles, err := getChangedFiles(ctx, canonicalWorktree, worktree.BaseCommit)
	if err != nil {
		return nil, err
	}
	changedJSON, err := json.Marshal(changedFiles)
	if err != nil {
		return nil, err
	}
	workspaceDigest, err := digestWorkspaceSnapshot(canonicalWorktree, changedFiles)
	if err != nil {
		return nil, err
	}
	return &verificationWorkspaceEvidence{
		WorktreeID:      worktree.ID,
		WorktreeVersion: worktree.Version,
		BaseCommit:      worktree.BaseCommit,
		SourceCommit:    sourceCommit,
		ChangedFiles:    string(changedJSON),
		WorkspaceDigest: workspaceDigest,
	}, nil
}

func requireLatestPassedValidationEvidence(ctx context.Context, tx *sql.Tx, projectID, taskID string, workspace *verificationWorkspaceEvidence) error {
	if workspace == nil {
		return fmt.Errorf("workspace evidence is absent")
	}
	var (
		baseCommit, sourceCommit, changedFiles             string
		profileRef, policyVersion, policyDigest            string
		evidenceDigest, workspaceDigest, result, errorCode string
		authority, producer                                string
		boundaryOK, testOK, coverageOK, outputTruncated    int
		worktreeStatus                                     string
		worktreeVersion                                    int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT vr.base_commit, vr.source_commit, vr.changed_files,
		       vr.profile_ref, vr.policy_version, vr.policy_digest,
		       vr.evidence_digest, vr.workspace_digest, vr.authority, vr.producer, vr.result,
		       COALESCE(vr.error_code, ''), vr.boundary_ok, vr.test_ok,
		       vr.coverage_ok, vr.output_truncated,
		       wt.status, wt.version
		FROM validation_runs AS vr
		JOIN worktrees AS wt
		  ON wt.project_id = vr.project_id AND wt.task_id = vr.task_id AND wt.id = ?
		WHERE vr.id = (
		  SELECT latest.id FROM validation_runs AS latest
		  WHERE latest.project_id = ? AND latest.task_id = ?
		  ORDER BY latest.attempt DESC, latest.id DESC LIMIT 1
		) AND vr.project_id = ? AND vr.task_id = ?`,
		workspace.WorktreeID, projectID, taskID, projectID, taskID,
	).Scan(
		&baseCommit, &sourceCommit, &changedFiles,
		&profileRef, &policyVersion, &policyDigest,
		&evidenceDigest, &workspaceDigest, &authority, &producer, &result, &errorCode,
		&boundaryOK, &testOK, &coverageOK, &outputTruncated,
		&worktreeStatus, &worktreeVersion,
	)
	if err != nil {
		return err
	}
	if result != "passed" || errorCode != "" || boundaryOK != 1 || testOK != 1 || coverageOK != 1 || outputTruncated != 0 {
		return fmt.Errorf("latest validation run is not a complete pass")
	}
	if authority != model.EvidenceAuthorityMergeGate || strings.TrimSpace(producer) == "" ||
		producer == model.EvidenceProducerMaestroLocal {
		return fmt.Errorf("latest validation evidence lacks merge authority: authority=%q producer=%q", authority, producer)
	}
	if !gitSHARe.MatchString(baseCommit) || !gitSHARe.MatchString(sourceCommit) ||
		profileRef == "" || policyVersion == "" || !imageDigestRe.MatchString(policyDigest) ||
		!imageDigestRe.MatchString(evidenceDigest) || !imageDigestRe.MatchString(workspaceDigest) {
		return fmt.Errorf("latest validation evidence identity is incomplete or malformed")
	}
	if baseCommit != workspace.BaseCommit || sourceCommit != workspace.SourceCommit ||
		changedFiles != workspace.ChangedFiles || workspaceDigest != workspace.WorkspaceDigest ||
		worktreeStatus != model.WorktreeStatusSealed || worktreeVersion != workspace.WorktreeVersion {
		return fmt.Errorf("sealed workspace no longer matches validation evidence")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Merge conflict resolution
// ---------------------------------------------------------------------------

// ResolveMergeConflict handles a needs_human task with one of two deterministic
// M0 actions: reopen it for a fresh Lease or cancel it. Creating follow-up work
// is deliberately disabled until the v3 mutation contract can require a parent
// version, an idempotency key, a unique relation, and an authenticated actor.
func (s *TaskService) ResolveMergeConflict(ctx context.Context, projectID, taskID string, action string, reason string) error {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("ResolveMergeConflict: %w", err)
	}

	if task.Status != model.TaskStatusNeedsHuman {
		return fmt.Errorf("ResolveMergeConflict: %w: task must be needs_human, got %q", store.ErrTaskStateInvalid, task.Status)
	}

	switch action {
	case "reopen":
		return s.resolveMergeConflictReopen(ctx, projectID, task, reason)
	case "cancel":
		return s.resolveMergeConflictCancel(ctx, projectID, task, reason)
	case "followup":
		s.logAudit(ctx, projectID, "task.followup", "DENIED", nil, &taskID)
		return fmt.Errorf("ResolveMergeConflict: followup requires the v3 idempotent workflow: %w", store.ErrOperationDisabled)
	default:
		return fmt.Errorf("ResolveMergeConflict: %w: unknown action %q, must be reopen/cancel", store.ErrInvalidParameter, action)
	}
}

func (s *TaskService) resolveMergeConflictReopen(ctx context.Context, projectID string, task *model.Task, reason string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("ResolveMergeConflict: reopen: begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()
	if task.ActiveLeaseID != nil {
		return fmt.Errorf("ResolveMergeConflict: needs_human task has active lease: %w", store.ErrRecoveryIntegrity)
	}
	if err := s.ensureTaskWorktreeRequeueableTx(ctx, tx, projectID, task); err != nil {
		return fmt.Errorf("ResolveMergeConflict: workspace is not requeueable: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'queued', assigned_session_id = NULL, assigned_worker_id = NULL,
		     assigned_at = NULL, active_lease_id = NULL, lease_expires_at = NULL,
		     version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = 'needs_human' AND version = ?`,
		task.ID, projectID, task.Version)
	if err != nil {
		return fmt.Errorf("ResolveMergeConflict: reopen update: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("ResolveMergeConflict: reopen rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("ResolveMergeConflict: %w", store.ErrConcurrentConflict)
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", task.ID,
		model.TaskStatusNeedsHuman, model.TaskStatusQueued, task.Version, task.Version+1,
		"coordinator", stateHistoryReason(reason, "recovery approved for a fresh lease"),
		fmt.Sprintf("task.reopen:%s:v%d", task.ID, task.Version)); err != nil {
		return err
	}
	if err := bumpProjectQueueVersionTx(ctx, tx, projectID); err != nil {
		return fmt.Errorf("ResolveMergeConflict: queue version: %w", err)
	}

	detail := fmt.Sprintf(`{"resolution":"reopen","reason":%q}`, reason)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, session_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		projectID, task.AssignedSessionID, task.ID, model.ActionReopened, detail); err != nil {
		return fmt.Errorf("ResolveMergeConflict: reopen log activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(bound_project, target_project, target_task, action, result, detail, created_at)
		 VALUES (?, ?, ?, 'task.recover', 'ALLOWED', ?, datetime('now'))`,
		projectID, projectID, task.ID, detail); err != nil {
		return fmt.Errorf("ResolveMergeConflict: reopen audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ResolveMergeConflict: reopen commit: %w", err)
	}

	safeEmit(s.eventEmitter, "task.reopened", projectID, map[string]string{"task_id": task.ID})

	return nil
}

func (s *TaskService) resolveMergeConflictCancel(ctx context.Context, projectID string, task *model.Task, reason string) error {
	return s.CancelTask(ctx, projectID, task.ID, "coordinator", reason)
}

// MergeTask is retained only as a compatibility symbol for old in-process
// callers. M0 is fail-closed: Maestro cannot locally merge or infer completion.
func (s *TaskService) MergeTask(ctx context.Context, projectID, taskID, sessionID string) error {
	s.logAudit(ctx, projectID, "task.merge", "DENIED", &sessionID, &taskID)
	return fmt.Errorf("MergeTask: final merge is human-only: %w", store.ErrOperationDisabled)
}

// ConfirmMergedFact is deliberately disabled in M0. A caller-supplied fact ID
// and SHA are not proof that GitLab accepted a human merge. M2 may replace this
// compatibility symbol only with a transaction that references a persisted,
// signature-verified Webhook Inbox or reconciliation fact.
func (s *TaskService) ConfirmMergedFact(ctx context.Context, projectID, taskID, _, _ string) error {
	s.logAudit(ctx, projectID, "task.confirm_merged_fact", "DENIED", nil, &taskID)
	return fmt.Errorf("ConfirmMergedFact: verified GitLab merge facts are unavailable in M0: %w", store.ErrOperationDisabled)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// releaseTaskResourcesTx releases DB-owned worker/worktree state in the same
// transaction as the task transition. Physical Git cleanup is a later,
// idempotent cleanup_pending operation and is never claimed as complete here.
func (s *TaskService) releaseTaskResourcesTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID string,
	task *model.Task,
	worktreeStatus, actor, reason, causationID string,
) error {
	if tx == nil || task == nil || strings.TrimSpace(actor) == "" ||
		strings.TrimSpace(reason) == "" || strings.TrimSpace(causationID) == "" {
		return fmt.Errorf("resource release authority is incomplete: %w", store.ErrInvalidParameter)
	}
	if task.AssignedWorkerID != nil && task.AssignedSessionID != nil {
		var sessionKey string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM agent_sessions
			WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
			projectID, *task.AssignedSessionID).Scan(&sessionKey); err != nil {
			return fmt.Errorf("resolve assigned session: %w", err)
		}
		var workerStatus string
		var currentTask sql.NullString
		var workerVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT status, current_task_id, version FROM agent_workers
			WHERE project_id = ? AND session_id = ? AND id = ?`,
			projectID, sessionKey, *task.AssignedWorkerID,
		).Scan(&workerStatus, &currentTask, &workerVersion); err != nil {
			return fmt.Errorf("read assigned worker: %w", err)
		}
		if currentTask.Valid && currentTask.String == task.ID && workerStatus == model.WorkerStatusBusy {
			result, err := tx.ExecContext(ctx,
				`UPDATE agent_workers SET current_task_id = NULL, status = 'idle', version = version + 1,
			     last_active = datetime('now')
			 WHERE project_id = ? AND session_id = ? AND id = ? AND current_task_id = ?
			   AND status = 'busy' AND version = ?`,
				projectID, sessionKey, *task.AssignedWorkerID, task.ID, workerVersion,
			)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				return errors.Join(store.ErrConcurrentConflict, err)
			}
			if err := appendStateHistory(ctx, tx, projectID, "worker", sessionKey+"/"+*task.AssignedWorkerID,
				model.WorkerStatusBusy, model.WorkerStatusIdle, workerVersion, workerVersion+1,
				actor, reason, causationID); err != nil {
				return err
			}
		} else if currentTask.Valid || (workerStatus != model.WorkerStatusIdle && workerStatus != model.WorkerStatusLost) {
			return fmt.Errorf("assigned worker authority is inconsistent: %w", store.ErrRecoveryIntegrity)
		}
	}
	if worktreeStatus != "" {
		if !model.IsWorktreeStatus(worktreeStatus) {
			return fmt.Errorf("invalid worktree release status %q: %w", worktreeStatus, store.ErrTaskStateInvalid)
		}
		var worktreeID, worktreeVersion, generation int64
		var currentStatus string
		err := tx.QueryRowContext(ctx, `SELECT id, status, version, generation FROM worktrees
			WHERE project_id = ? AND task_id = ?`, projectID, task.ID).Scan(
			&worktreeID, &currentStatus, &worktreeVersion, &generation)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			targetStatus := worktreeStatus
			if currentStatus == model.WorktreeStatusSealed || currentStatus == model.WorktreeStatusSubmitted {
				targetStatus = model.WorktreeStatusQuarantined
			}
			if currentStatus == model.WorktreeStatusQuarantined || currentStatus == targetStatus {
				return nil
			}
			if !model.CanWorktreeTransition(currentStatus, targetStatus) {
				return fmt.Errorf("cannot release worktree %s -> %s: %w", currentStatus, targetStatus, store.ErrTaskStateInvalid)
			}
			result, err := tx.ExecContext(ctx, `UPDATE worktrees SET status = ?, version = version + 1,
				updated_at = datetime('now') WHERE project_id = ? AND task_id = ? AND id = ?
				AND status = ? AND version = ? AND generation = ?`,
				targetStatus, projectID, task.ID, worktreeID, currentStatus, worktreeVersion, generation)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				return errors.Join(store.ErrConcurrentConflict, err)
			}
			if err := appendStateHistory(ctx, tx, projectID, "worktree", fmt.Sprint(worktreeID),
				currentStatus, targetStatus, worktreeVersion, worktreeVersion+1,
				actor, reason, causationID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureTaskWorktreeRequeueableTx prevents an administrative action from
// publishing a queued Task that a fresh claim can never safely execute. Only
// an active workspace from a durably closed prior Lease may be rebound. A
// cleanup intent must finish first; sealed, submitted, quarantined and other
// evidence states require an explicit recovery workflow instead of requeue.
func (s *TaskService) ensureTaskWorktreeRequeueableTx(ctx context.Context, tx *sql.Tx, projectID string, task *model.Task) error {
	var worktreeID, generation int64
	var status string
	err := tx.QueryRowContext(ctx, `SELECT id, status, generation FROM worktrees
		WHERE project_id = ? AND task_id = ?`, projectID, task.ID).Scan(&worktreeID, &status, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == model.WorktreeStatusCleanupPending {
		return fmt.Errorf("worktree %d cleanup is pending; run GC before retry: %w", worktreeID, store.ErrConcurrentConflict)
	}
	if status != model.WorktreeStatusActive || generation <= 0 {
		return fmt.Errorf("worktree %d in %s cannot be rebound: %w", worktreeID, status, store.ErrRecoveryIntegrity)
	}
	var priorLeaseStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM task_leases
		WHERE project_id = ? AND task_id = ? AND epoch = ?`,
		projectID, task.ID, generation).Scan(&priorLeaseStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("worktree %d has no generation lease: %w", worktreeID, store.ErrRecoveryIntegrity)
		}
		return err
	}
	if priorLeaseStatus == model.LeaseStatusActive {
		return fmt.Errorf("worktree %d generation lease remains active: %w", worktreeID, store.ErrRecoveryIntegrity)
	}
	return nil
}

func stateHistoryReason(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// ptrStr returns the dereferenced string value or empty string if nil.
func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// logAudit writes a security audit entry for a task-level operation.
func (s *TaskService) logAudit(ctx context.Context, boundProject, action, result string, sessionID, taskID *string) {
	entry := &model.AuditLog{
		SessionID:    sessionID,
		BoundProject: boundProject,
		TargetTask:   taskID,
		Action:       action,
		Result:       result,
		CreatedAt:    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if err := s.auditStore.Create(ctx, entry); err != nil {
		slog.Error("logAudit: failed to write audit entry", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Admin operations
// ---------------------------------------------------------------------------

// ForceRollback re-queues only stopped/recoverable states.
// This is an admin escape hatch that clears session/worker assignment and
// marks the worktree as abandoned. Allowed states: in_progress, submitted,
// verifying, ready_to_merge, blocked.
func (s *TaskService) ForceRollback(ctx context.Context, projectID, taskID, sessionID string) error {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("ForceRollback: %w", err)
	}

	// Validate status is rollbackable (non-terminal).
	switch task.Status {
	case model.TaskStatusFailed, model.TaskStatusNeedsHuman, model.TaskStatusBlocked:
		// OK to rollback.
	default:
		return fmt.Errorf("ForceRollback: %w: cannot rollback task in status %q", store.ErrTaskStateInvalid, task.Status)
	}

	previousStatus := task.Status
	if task.ActiveLeaseID != nil {
		return fmt.Errorf("ForceRollback: stopped task still has active lease: %w", store.ErrRecoveryIntegrity)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("ForceRollback: begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()
	if err := s.ensureTaskWorktreeRequeueableTx(ctx, tx, projectID, task); err != nil {
		return fmt.Errorf("ForceRollback: workspace is not requeueable: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, assigned_session_id = NULL, assigned_worker_id = NULL,
		     assigned_at = NULL, blocker_reason = NULL, verified_by = NULL, verified_at = NULL,
		     version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = ? AND version = ? AND active_lease_id IS NULL`,
		model.TaskStatusQueued, taskID, projectID, previousStatus, task.Version)
	if err != nil {
		return fmt.Errorf("ForceRollback: update: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("ForceRollback: rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("ForceRollback: %w", store.ErrConcurrentConflict)
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		previousStatus, model.TaskStatusQueued, task.Version, task.Version+1,
		sessionID, "authorized recovery", fmt.Sprintf("task.force-rollback:%s:v%d", taskID, task.Version)); err != nil {
		return err
	}
	if err := bumpProjectQueueVersionTx(ctx, tx, projectID); err != nil {
		return fmt.Errorf("ForceRollback: queue version: %w", err)
	}

	// Log activity.
	detail := fmt.Sprintf(`{"previous_status":%q,"rolled_back_by":%q}`, previousStatus, sessionID)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, session_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		projectID, sessionID, taskID, model.ActionReopened, detail); err != nil {
		return fmt.Errorf("ForceRollback: log activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(session_id, bound_project, target_project, target_task, action, result, detail, created_at)
		 VALUES (?, ?, ?, ?, 'task.force_rollback', 'ALLOWED', ?, datetime('now'))`,
		sessionID, projectID, projectID, taskID, detail); err != nil {
		return fmt.Errorf("ForceRollback: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ForceRollback: commit: %w", err)
	}

	safeEmit(s.eventEmitter, "task.reopened", projectID, map[string]string{"task_id": taskID})

	return nil
}

// GetTaskDiff returns the git diff for a task's worktree relative to its base commit.
// Returns an empty string if no worktree exists.
func (s *TaskService) GetTaskDiff(ctx context.Context, projectID, taskID string) (string, error) {
	// Verify task exists.
	if _, err := s.taskStore.GetByID(ctx, projectID, taskID); err != nil {
		return "", fmt.Errorf("GetTaskDiff: %w", err)
	}

	// Get worktree.
	wt, err := s.worktreeStore.GetByTaskID(ctx, projectID, taskID)
	if err != nil {
		return "", fmt.Errorf("GetTaskDiff: no worktree for task %s: %w", taskID, err)
	}
	project, err := s.projectStore.GetByID(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("GetTaskDiff: project: %w", err)
	}
	canonicalWorktree, _, err := verifyWorktreeRepository(ctx, project.WorkspacePath, wt.WorktreePath, wt.BaseCommit)
	if err != nil {
		return "", fmt.Errorf("GetTaskDiff: untrusted worktree: %w", err)
	}

	// Run git diff.
	files, err := getChangedFiles(ctx, canonicalWorktree, wt.BaseCommit)
	if err != nil {
		return "", fmt.Errorf("GetTaskDiff: git diff failed: %w", err)
	}

	diffOutput := ""
	for _, f := range files {
		diffOutput += f + "\n"
	}

	return diffOutput, nil
}
