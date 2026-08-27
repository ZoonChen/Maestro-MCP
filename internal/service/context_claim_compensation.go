package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

const contextCompensationTimeout = 30 * time.Second

// CompensateContextFailure revokes a claim that could not be delivered with
// all required context. The database changes are one serializable transaction:
// the Task is removed from execution authority, its Lease is released, the
// Worker becomes idle, and the Worktree becomes either a crash-safe cleanup
// intent (a claim created by the rejected call) or quarantined evidence (an
// older assignment whose possible side effects must be preserved).
//
// The Task moves to needs_human rather than directly back to queued because a
// missing or invalid required source cannot be repaired by repeatedly claiming
// the same work. A human may requeue it after repairing the source definition.
func (s *TaskService) CompensateContextFailure(
	ctx context.Context,
	task *model.Task,
	sessionID, workerID, claimIdempotencyKey, contextCode string,
	discardUndeliveredWorktree bool,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("context compensation: service unavailable: %w", store.ErrRecoveryIntegrity)
	}
	if task == nil || task.ProjectID == "" || task.ID == "" || task.ActiveLeaseID == nil ||
		*task.ActiveLeaseID == "" || task.LeaseEpoch <= 0 || task.Version < 0 ||
		task.Status != model.TaskStatusExecuting || sessionID == "" || workerID == "" {
		return fmt.Errorf("context compensation: invalid claim snapshot: %w", store.ErrInvalidParameter)
	}
	if task.AssignedSessionID == nil || *task.AssignedSessionID != sessionID ||
		task.AssignedWorkerID == nil || *task.AssignedWorkerID != workerID {
		return fmt.Errorf("context compensation: claim owner mismatch: %w", store.ErrTaskNotOwned)
	}
	if !isCompensableContextErrorCode(contextCode) {
		return fmt.Errorf("context compensation: invalid context error code: %w", store.ErrInvalidParameter)
	}

	compensationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), contextCompensationTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(compensationCtx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("context compensation: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sessionKey string
	if err := tx.QueryRowContext(compensationCtx,
		`SELECT id FROM agent_sessions
		 WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		task.ProjectID, sessionID,
	).Scan(&sessionKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("context compensation: session: %w", store.ErrSessionNotFound)
		}
		return fmt.Errorf("context compensation: session lookup: %w", err)
	}

	var (
		currentTaskStatus  string
		currentTaskVersion int64
		currentLeaseID     sql.NullString
		currentLeaseEpoch  int64
		assignedSessionKey sql.NullString
		assignedWorkerID   sql.NullString
	)
	if err := tx.QueryRowContext(compensationCtx, `SELECT status, version, active_lease_id,
		lease_epoch, assigned_session_id, assigned_worker_id
		FROM tasks WHERE project_id = ? AND id = ?`, task.ProjectID, task.ID).Scan(
		&currentTaskStatus, &currentTaskVersion, &currentLeaseID, &currentLeaseEpoch,
		&assignedSessionKey, &assignedWorkerID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("context compensation: task: %w", store.ErrTaskNotFound)
		}
		return fmt.Errorf("context compensation: task lookup: %w", err)
	}
	if currentTaskStatus != model.TaskStatusExecuting || currentTaskVersion != task.Version ||
		!currentLeaseID.Valid || currentLeaseID.String != *task.ActiveLeaseID ||
		currentLeaseEpoch != task.LeaseEpoch || !assignedSessionKey.Valid ||
		assignedSessionKey.String != sessionKey || !assignedWorkerID.Valid ||
		assignedWorkerID.String != workerID {
		return fmt.Errorf("context compensation: task claim changed: %w", store.ErrConcurrentConflict)
	}

	var leaseStatus, leaseSessionKey, leaseWorkerID string
	var leaseVersion, leaseEpoch int64
	if err := tx.QueryRowContext(compensationCtx, `SELECT status, version, epoch, session_id, worker_id
		FROM task_leases WHERE project_id = ? AND task_id = ? AND id = ?`,
		task.ProjectID, task.ID, *task.ActiveLeaseID,
	).Scan(&leaseStatus, &leaseVersion, &leaseEpoch, &leaseSessionKey, &leaseWorkerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("context compensation: lease: %w", store.ErrLeaseNotFound)
		}
		return fmt.Errorf("context compensation: lease lookup: %w", err)
	}
	if leaseStatus != model.LeaseStatusActive || leaseEpoch != task.LeaseEpoch ||
		leaseSessionKey != sessionKey || leaseWorkerID != workerID {
		return fmt.Errorf("context compensation: lease authority changed: %w", store.ErrConcurrentConflict)
	}

	var workerStatus string
	var workerVersion int64
	var workerTaskID sql.NullString
	if err := tx.QueryRowContext(compensationCtx, `SELECT status, version, current_task_id
		FROM agent_workers WHERE project_id = ? AND session_id = ? AND id = ?`,
		task.ProjectID, sessionKey, workerID,
	).Scan(&workerStatus, &workerVersion, &workerTaskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("context compensation: worker: %w", store.ErrWorkerNotFound)
		}
		return fmt.Errorf("context compensation: worker lookup: %w", err)
	}
	if workerStatus != model.WorkerStatusBusy || !workerTaskID.Valid || workerTaskID.String != task.ID {
		return fmt.Errorf("context compensation: worker authority changed: %w", store.ErrConcurrentConflict)
	}

	var worktreeID, worktreeVersion, worktreeGeneration int64
	var worktreeStatus string
	var worktreeSessionKey sql.NullString
	if err := tx.QueryRowContext(compensationCtx, `SELECT id, status, version, generation, session_id
		FROM worktrees WHERE project_id = ? AND task_id = ? AND generation = ?`,
		task.ProjectID, task.ID, task.LeaseEpoch,
	).Scan(&worktreeID, &worktreeStatus, &worktreeVersion, &worktreeGeneration, &worktreeSessionKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("context compensation: worktree: %w", store.ErrWorktreeNotFound)
		}
		return fmt.Errorf("context compensation: worktree lookup: %w", err)
	}
	if worktreeStatus != model.WorktreeStatusActive || worktreeGeneration != task.LeaseEpoch ||
		!worktreeSessionKey.Valid || worktreeSessionKey.String != sessionKey {
		return fmt.Errorf("context compensation: worktree authority changed: %w", store.ErrConcurrentConflict)
	}
	worktreeTargetStatus := model.WorktreeStatusQuarantined
	if discardUndeliveredWorktree {
		worktreeTargetStatus = model.WorktreeStatusCleanupPending
	}
	if !model.CanTaskTransition(currentTaskStatus, model.TaskStatusNeedsHuman) ||
		!model.CanWorkerTransition(workerStatus, model.WorkerStatusIdle) ||
		!model.CanWorktreeTransition(worktreeStatus, worktreeTargetStatus) {
		return fmt.Errorf("context compensation: illegal resource transition: %w", store.ErrTaskStateInvalid)
	}

	result, err := tx.ExecContext(compensationCtx, `UPDATE task_leases
		SET status = 'released', version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND task_id = ? AND id = ? AND status = 'active' AND version = ?
		  AND epoch = ? AND session_id = ? AND worker_id = ?`,
		task.ProjectID, task.ID, *task.ActiveLeaseID, leaseVersion,
		task.LeaseEpoch, sessionKey, workerID,
	)
	if err != nil {
		return fmt.Errorf("context compensation: release lease: %w", err)
	}
	if err := requireContextCompensationRow(result, "release lease"); err != nil {
		return err
	}

	reason := "required context rejected: " + contextCode
	result, err = tx.ExecContext(compensationCtx, `UPDATE tasks
		SET status = 'needs_human', assigned_session_id = NULL, assigned_worker_id = NULL,
		    assigned_at = NULL, active_lease_id = NULL, lease_expires_at = NULL,
		    blocker_reason = ?, version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND id = ? AND status = 'executing' AND version = ?
		  AND active_lease_id = ? AND lease_epoch = ?
		  AND assigned_session_id = ? AND assigned_worker_id = ?`,
		reason, task.ProjectID, task.ID, currentTaskVersion, *task.ActiveLeaseID,
		task.LeaseEpoch, sessionKey, workerID,
	)
	if err != nil {
		return fmt.Errorf("context compensation: stop task: %w", err)
	}
	if err := requireContextCompensationRow(result, "stop task"); err != nil {
		return err
	}

	result, err = tx.ExecContext(compensationCtx, `UPDATE agent_workers
		SET current_task_id = NULL, status = 'idle', version = version + 1,
		    last_active = datetime('now')
		WHERE project_id = ? AND session_id = ? AND id = ? AND status = 'busy'
		  AND version = ? AND current_task_id = ?`,
		task.ProjectID, sessionKey, workerID, workerVersion, task.ID,
	)
	if err != nil {
		return fmt.Errorf("context compensation: release worker: %w", err)
	}
	if err := requireContextCompensationRow(result, "release worker"); err != nil {
		return err
	}

	result, err = tx.ExecContext(compensationCtx, `UPDATE worktrees
		SET status = ?, version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND id = ? AND task_id = ? AND status = 'active'
		  AND version = ? AND generation = ? AND session_id = ?`,
		worktreeTargetStatus, task.ProjectID, worktreeID, task.ID, worktreeVersion, task.LeaseEpoch, sessionKey,
	)
	if err != nil {
		return fmt.Errorf("context compensation: release worktree: %w", err)
	}
	if err := requireContextCompensationRow(result, "release worktree"); err != nil {
		return err
	}

	if claimIdempotencyKey != "" {
		result, err = tx.ExecContext(compensationCtx, `DELETE FROM idempotency_records
			WHERE project_id = ? AND scope = ? AND operation = 'task.claim' AND key = ?
			  AND result_ref = ?`,
			task.ProjectID, sessionID+"/"+workerID, claimIdempotencyKey, task.ID,
		)
		if err != nil {
			return fmt.Errorf("context compensation: remove claim idempotency result: %w", err)
		}
		if err := requireContextCompensationRow(result, "remove claim idempotency result"); err != nil {
			return err
		}
	}

	if err := appendStateHistory(compensationCtx, tx, task.ProjectID, "task", task.ID,
		currentTaskStatus, model.TaskStatusNeedsHuman, currentTaskVersion, currentTaskVersion+1,
		"system", reason, *task.ActiveLeaseID); err != nil {
		return err
	}
	if err := appendStateHistory(compensationCtx, tx, task.ProjectID, "lease", *task.ActiveLeaseID,
		leaseStatus, model.LeaseStatusReleased, leaseVersion, leaseVersion+1,
		"system", reason, *task.ActiveLeaseID); err != nil {
		return err
	}
	if err := appendStateHistory(compensationCtx, tx, task.ProjectID, "worker", sessionKey+"/"+workerID,
		workerStatus, model.WorkerStatusIdle, workerVersion, workerVersion+1,
		"system", reason, *task.ActiveLeaseID); err != nil {
		return err
	}
	if err := appendStateHistory(compensationCtx, tx, task.ProjectID, "worktree", fmt.Sprint(worktreeID),
		worktreeStatus, worktreeTargetStatus, worktreeVersion, worktreeVersion+1,
		"system", reason, *task.ActiveLeaseID); err != nil {
		return err
	}

	detail, err := json.Marshal(map[string]any{
		"context_error_code": contextCode,
		"lease_id":           *task.ActiveLeaseID,
		"worker_id":          workerID,
		"worktree_id":        worktreeID,
		"worktree_status":    worktreeTargetStatus,
		"task_status":        model.TaskStatusNeedsHuman,
	})
	if err != nil {
		return fmt.Errorf("context compensation: encode audit detail: %w", err)
	}
	if _, err := tx.ExecContext(compensationCtx, `INSERT INTO activity_log
		(project_id, session_id, task_id, action, detail, created_at)
		VALUES (?, ?, ?, 'context_rejected', ?, datetime('now'))`,
		task.ProjectID, sessionID, task.ID, string(detail),
	); err != nil {
		return fmt.Errorf("context compensation: activity: %w", err)
	}
	if _, err := tx.ExecContext(compensationCtx, `INSERT INTO audit_log
		(session_id, bound_project, target_project, target_task, action, result, detail, created_at)
		VALUES (?, ?, ?, ?, 'task.context.reject', 'ALLOWED', ?, datetime('now'))`,
		sessionID, task.ProjectID, task.ProjectID, task.ID, string(detail),
	); err != nil {
		return fmt.Errorf("context compensation: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("context compensation: commit: %w", err)
	}
	safeEmit(s.eventEmitter, "task.context_rejected", task.ProjectID, map[string]string{
		"task_id": task.ID,
		"code":    contextCode,
	})
	if s.OnFeatureStatusChange != nil {
		s.OnFeatureStatusChange(compensationCtx, task.ProjectID, task.FeatureID)
	}
	return nil
}

func requireContextCompensationRow(result sql.Result, operation string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("context compensation: %s rows: %w", operation, err)
	}
	if rows != 1 {
		return fmt.Errorf("context compensation: %s changed %d rows: %w", operation, rows, store.ErrConcurrentConflict)
	}
	return nil
}

func isCompensableContextErrorCode(code string) bool {
	switch code {
	case ContextErrorRequiredSourceMissing,
		ContextErrorSourceInvalid,
		ContextErrorBuildFailed:
		return true
	default:
		return false
	}
}
