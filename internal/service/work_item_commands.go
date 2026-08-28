package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// Work-item management commands for the v3 MCP surface: idempotent
// creation keyed by the client's own work-item key, version-guarded
// cancellation and recovery requeue. All authority (project scope, actor)
// arrives from the server-side transport binding, never from payloads.

// DefaultWorkItemFeature is the server-side bucket feature every v3 work
// item is filed under until the Work Graph model replaces the M0
// task/feature split.
const DefaultWorkItemFeature = "work-items"

// CreateWorkItem creates a queued work item under the default feature,
// idempotent by the caller's client key: the same key with the same
// payload replays the created task, the same key with a different payload
// is a conflict, never a silent second item.
func (s *TaskService) CreateWorkItem(
	ctx context.Context, projectID string, task *model.Task, clientKey string,
) (*model.Task, bool, error) {
	requestHash := sha256.Sum256([]byte(task.Title + "\x00" + task.Description + "\x00" +
		task.Role + "\x00" + task.Priority + "\x00" + string(task.Dependencies)))
	hash := hex.EncodeToString(requestHash[:])

	var priorHash, priorTaskID string
	err := s.db.QueryRowContext(ctx, `
		SELECT request_hash, result_ref FROM idempotency_records
		WHERE project_id = ? AND scope = 'work_item' AND operation = 'work_item.create' AND key = ?`,
		projectID, clientKey).Scan(&priorHash, &priorTaskID)
	switch {
	case err == nil:
		if priorHash != hash {
			return nil, true, fmt.Errorf("CreateWorkItem: %w: client key reused with a different payload",
				store.ErrIdempotencyConflict)
		}
		prior, loadErr := s.taskStore.GetByID(ctx, projectID, priorTaskID)
		if loadErr != nil {
			return nil, true, fmt.Errorf("CreateWorkItem: replay load: %w", loadErr)
		}
		return prior, true, nil
	case err != sql.ErrNoRows:
		return nil, false, fmt.Errorf("CreateWorkItem: client key lookup: %w", err)
	}

	// The bucket feature is server-owned: created once per project, never
	// exposed as a creation parameter.
	featureID := DefaultWorkItemFeature
	if _, featureErr := s.featureStore.GetByID(ctx, projectID, featureID); featureErr != nil {
		now := time.Now().UTC().Format(time.RFC3339)
		createErr := s.featureStore.Create(ctx, projectID, &model.Feature{
			ID: featureID, ProjectID: projectID, Title: "Work items",
			Description:   "server-side bucket for v3 work items",
			ReferenceURLs: "[]", Status: model.FeatureStatusActive,
			CreatedAt: now, UpdatedAt: now,
		})
		if createErr != nil {
			return nil, false, fmt.Errorf("CreateWorkItem: ensure bucket feature: %w", createErr)
		}
	}

	task.FeatureID = featureID
	task.Status = model.TaskStatusQueued
	if task.Priority == "" {
		task.Priority = model.PriorityNormal
	}
	if err := s.CreateTask(ctx, projectID, task); err != nil {
		return nil, false, err
	}

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO idempotency_records (project_id, scope, operation, key, request_hash, result_ref)
		VALUES (?, 'work_item', 'work_item.create', ?, ?, ?)`,
		projectID, clientKey, hash, task.ID); err != nil {
		return nil, false, fmt.Errorf("CreateWorkItem: record client key: %w", err)
	}
	return task, false, nil
}

// ReplayWorkItem resolves a previously created work item by client key so
// transport retries return the original result.
func (s *TaskService) ReplayWorkItem(ctx context.Context, projectID, clientKey, expectedPayloadHash string) (*model.Task, bool, error) {
	var requestHash, taskID string
	err := s.db.QueryRowContext(ctx, `
		SELECT request_hash, result_ref FROM idempotency_records
		WHERE project_id = ? AND scope = 'work_item' AND operation = 'work_item.create' AND key = ?`,
		projectID, clientKey).Scan(&requestHash, &taskID)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("ReplayWorkItem: lookup: %w", err)
	}
	if requestHash != expectedPayloadHash {
		return nil, false, fmt.Errorf("ReplayWorkItem: %w", store.ErrIdempotencyConflict)
	}
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return nil, false, fmt.Errorf("ReplayWorkItem: load: %w", err)
	}
	return task, true, nil
}

// CancelWorkItem cancels with the v3 expected-version guard: the caller
// must have observed the exact aggregate version it is cancelling.
func (s *TaskService) CancelWorkItem(
	ctx context.Context, projectID, taskID, sessionID, reason string, expectedVersion int64,
) error {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("CancelWorkItem: %w", err)
	}
	if task.Version != expectedVersion {
		return fmt.Errorf("CancelWorkItem: expected version %d, current %d: %w",
			expectedVersion, task.Version, store.ErrConcurrentConflict)
	}
	return s.CancelTask(ctx, projectID, taskID, sessionID, reason)
}

// RetryWorkItem requeues a failed, blocked or needs-human work item for a
// fresh lease cycle: the workspace must be requeueable and no lease may
// remain active. Mirrors the merge-conflict reopen recipe.
func (s *TaskService) RetryWorkItem(
	ctx context.Context, projectID, taskID, sessionID, reason string, expectedVersion int64,
) (*model.Task, error) {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("RetryWorkItem: %w", err)
	}
	switch task.Status {
	case model.TaskStatusFailed, model.TaskStatusBlocked, model.TaskStatusNeedsHuman:
	default:
		return nil, fmt.Errorf("RetryWorkItem: %w: cannot retry task in status %q",
			store.ErrTaskStateInvalid, task.Status)
	}
	if task.Version != expectedVersion {
		return nil, fmt.Errorf("RetryWorkItem: expected version %d, current %d: %w",
			expectedVersion, task.Version, store.ErrConcurrentConflict)
	}
	if task.ActiveLeaseID != nil {
		return nil, fmt.Errorf("RetryWorkItem: %w: active lease still exists", store.ErrTaskStateInvalid)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("RetryWorkItem: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.ensureTaskWorktreeRequeueableTx(ctx, tx, projectID, task); err != nil {
		return nil, fmt.Errorf("RetryWorkItem: workspace is not requeueable: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = ?, blocker_reason = NULL, cancel_reason = NULL,
		     assigned_session_id = NULL, assigned_worker_id = NULL, assigned_at = NULL,
		     active_lease_id = NULL, lease_expires_at = NULL,
		     version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = ? AND version = ?`,
		model.TaskStatusQueued, taskID, projectID, task.Status, task.Version)
	if err != nil {
		return nil, fmt.Errorf("RetryWorkItem: update: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, fmt.Errorf("RetryWorkItem: %w", store.ErrConcurrentConflict)
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		task.Status, model.TaskStatusQueued, task.Version, task.Version+1,
		sessionID, stateHistoryReason(reason, "retry approved for a fresh lease"),
		fmt.Sprintf("task.retry:%s:v%d", taskID, task.Version)); err != nil {
		return nil, err
	}
	if err := bumpProjectQueueVersionTx(ctx, tx, projectID); err != nil {
		return nil, fmt.Errorf("RetryWorkItem: queue version: %w", err)
	}
	detail, _ := json.Marshal(map[string]string{"reason": reason})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log (project_id, session_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		projectID, sessionID, taskID, model.ActionReopened, string(detail)); err != nil {
		return nil, fmt.Errorf("RetryWorkItem: log activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(bound_project, target_project, target_task, action, result, detail, created_at)
		 VALUES (?, ?, ?, 'task.retry', 'ALLOWED', ?, datetime('now'))`,
		projectID, projectID, taskID, string(detail)); err != nil {
		return nil, fmt.Errorf("RetryWorkItem: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("RetryWorkItem: commit: %w", err)
	}

	retried, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("RetryWorkItem: reload: %w", err)
	}
	safeEmit(s.eventEmitter, "task.retried", projectID, map[string]string{"task_id": taskID})
	return retried, nil
}
