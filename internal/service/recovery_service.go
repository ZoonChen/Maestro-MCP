package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// RecoveryService reconciles durable runtime authority before readiness. It is
// fail-closed: a partial or unverifiable recovery prevents service startup.
type RecoveryService struct {
	db *sql.DB
}

// NewRecoveryService keeps projectStore for constructor compatibility. Calling
// a base-DB repository while holding this transaction would deadlock SQLite's
// single M0 connection, so recovery deliberately uses transaction-bound SQL.
func NewRecoveryService(db *sql.DB, _ store.ProjectStore) *RecoveryService {
	return &RecoveryService{db: db}
}

// Run performs one atomic startup reconciliation. A restart invalidates all
// process-local execution authority. Leased-only work is safe to queue;
// executing work and an interrupted verifier require human reconciliation.
func (s *RecoveryService) Run(ctx context.Context) error {
	if s.db == nil {
		return fmt.Errorf("startup recovery: database unavailable: %w", store.ErrRecoveryIntegrity)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("startup recovery: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	type recoverySession struct {
		id, projectID, status string
		version               int64
	}
	sessionRows, err := tx.QueryContext(ctx, `SELECT id, project_id, status, version
		FROM agent_sessions WHERE status = 'online' ORDER BY project_id, id`)
	if err != nil {
		return fmt.Errorf("startup recovery: list sessions: %w", err)
	}
	var sessions []recoverySession
	for sessionRows.Next() {
		var session recoverySession
		if err := sessionRows.Scan(&session.id, &session.projectID, &session.status, &session.version); err != nil {
			_ = sessionRows.Close()
			return fmt.Errorf("startup recovery: scan session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := sessionRows.Err(); err != nil {
		_ = sessionRows.Close()
		return fmt.Errorf("startup recovery: session rows: %w", err)
	}
	if err := sessionRows.Close(); err != nil {
		return fmt.Errorf("startup recovery: close session rows: %w", err)
	}
	for _, session := range sessions {
		if !model.CanSessionTransition(session.status, model.SessionStatusOffline) {
			return fmt.Errorf("startup recovery: session %s transition %s -> offline: %w",
				session.id, session.status, store.ErrRecoveryIntegrity)
		}
		result, err := tx.ExecContext(ctx, `UPDATE agent_sessions
			SET status = 'offline', version = version + 1
			WHERE project_id = ? AND id = ? AND status = ? AND version = ?`,
			session.projectID, session.id, session.status, session.version)
		if err != nil {
			return fmt.Errorf("startup recovery: session %s offline: %w", session.id, err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("startup recovery: session %s changed: %w", session.id, errors.Join(store.ErrConcurrentConflict, err))
		}
		causationID := fmt.Sprintf("startup-recovery:session:%s:%d", session.id, session.version)
		if err := appendStateHistory(ctx, tx, session.projectID, "session", session.id,
			session.status, model.SessionStatusOffline, session.version, session.version+1,
			"system", "restart invalidated process-local session authority", causationID); err != nil {
			return err
		}
	}

	type recoveryLease struct {
		id, projectID, taskID, sessionID, workerID, status, expiresAt string
		version, epoch                                                int64
	}
	leaseRows, err := tx.QueryContext(ctx, `SELECT id, project_id, task_id, session_id,
		worker_id, status, version, epoch, expires_at
		FROM task_leases WHERE status = 'active' ORDER BY project_id, task_id, epoch`)
	if err != nil {
		return fmt.Errorf("startup recovery: list leases: %w", err)
	}
	var leases []recoveryLease
	for leaseRows.Next() {
		var lease recoveryLease
		if err := leaseRows.Scan(&lease.id, &lease.projectID, &lease.taskID, &lease.sessionID,
			&lease.workerID, &lease.status, &lease.version, &lease.epoch, &lease.expiresAt); err != nil {
			_ = leaseRows.Close()
			return fmt.Errorf("startup recovery: scan lease: %w", err)
		}
		leases = append(leases, lease)
	}
	if err := leaseRows.Err(); err != nil {
		_ = leaseRows.Close()
		return fmt.Errorf("startup recovery: lease rows: %w", err)
	}
	if err := leaseRows.Close(); err != nil {
		return fmt.Errorf("startup recovery: close lease rows: %w", err)
	}
	for _, lease := range leases {
		result, err := tx.ExecContext(ctx, `UPDATE task_leases
			SET status = 'expired', version = version + 1, updated_at = datetime('now')
			WHERE id = ? AND project_id = ? AND task_id = ? AND session_id = ? AND worker_id = ?
			  AND status = ? AND version = ? AND epoch = ? AND expires_at = ?`,
			lease.id, lease.projectID, lease.taskID, lease.sessionID, lease.workerID,
			lease.status, lease.version, lease.epoch, lease.expiresAt)
		if err != nil {
			return fmt.Errorf("startup recovery: expire lease %s: %w", lease.id, err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("startup recovery: lease %s changed: %w", lease.id, errors.Join(store.ErrConcurrentConflict, err))
		}
		if err := appendStateHistory(ctx, tx, lease.projectID, "lease", lease.id,
			lease.status, model.LeaseStatusExpired, lease.version, lease.version+1,
			"system", "restart invalidated process-local lease authority", lease.id); err != nil {
			return err
		}
	}

	type recoveryTask struct {
		id, projectID, status string
		version               int64
		leaseID               sql.NullString
		verifiedBy            sql.NullString
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, project_id, status, version, active_lease_id, verified_by
		FROM tasks
		WHERE status IN ('leased','executing','validating','cancelling')
		  AND (status <> 'validating' OR active_lease_id IS NOT NULL OR verified_by IS NOT NULL)
		ORDER BY project_id, id`)
	if err != nil {
		return fmt.Errorf("startup recovery: list interrupted tasks: %w", err)
	}
	var interrupted []recoveryTask
	for rows.Next() {
		var task recoveryTask
		if err := rows.Scan(&task.id, &task.projectID, &task.status, &task.version, &task.leaseID, &task.verifiedBy); err != nil {
			_ = rows.Close()
			return fmt.Errorf("startup recovery: scan task: %w", err)
		}
		interrupted = append(interrupted, task)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("startup recovery: task rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("startup recovery: close task rows: %w", err)
	}

	queueProjects := make(map[string]struct{})
	for _, task := range interrupted {
		toStatus := model.TaskStatusNeedsHuman
		reason := "restart interrupted work with possible side effects"
		worktreeStatus := model.WorktreeStatusQuarantined
		switch task.status {
		case model.TaskStatusLeased:
			toStatus = model.TaskStatusQueued
			reason = "restart expired lease before execution"
			worktreeStatus = model.WorktreeStatusCleanupPending
		case model.TaskStatusCancelling:
			toStatus = model.TaskStatusCancelled
			reason = "restart invalidated execution authority and completed pending cancellation"
			worktreeStatus = model.WorktreeStatusCleanupPending
		}
		if !model.CanTaskTransition(task.status, toStatus) {
			return fmt.Errorf("startup recovery: task %s transition %s -> %s: %w",
				task.id, task.status, toStatus, store.ErrRecoveryIntegrity)
		}
		query := `UPDATE tasks SET status = ?, assigned_session_id = NULL, assigned_worker_id = NULL,
			     assigned_at = NULL, verified_by = NULL, verified_at = NULL,
			     active_lease_id = NULL, lease_expires_at = NULL,
			     version = version + 1, updated_at = datetime('now')
			 WHERE project_id = ? AND id = ? AND status = ? AND version = ?`
		args := []any{toStatus, task.projectID, task.id, task.status, task.version}
		if task.leaseID.Valid {
			query += ` AND active_lease_id = ?`
			args = append(args, task.leaseID.String)
		} else {
			query += ` AND active_lease_id IS NULL`
		}
		if task.verifiedBy.Valid {
			query += ` AND verified_by = ?`
			args = append(args, task.verifiedBy.String)
		} else {
			query += ` AND verified_by IS NULL`
		}
		result, err := tx.ExecContext(ctx,
			query, args...,
		)
		if err != nil {
			return fmt.Errorf("startup recovery: reconcile task %s: %w", task.id, err)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return fmt.Errorf("startup recovery: task %s changed during recovery: %w", task.id, store.ErrConcurrentConflict)
		}
		causationID := fmt.Sprintf("startup-recovery:task:%s:%d", task.id, task.version)
		if task.leaseID.Valid {
			causationID = task.leaseID.String
		}
		if err := appendStateHistory(ctx, tx, task.projectID, "task", task.id,
			task.status, toStatus, task.version, task.version+1,
			"system", reason, causationID); err != nil {
			return err
		}
		if err := transitionWorktreesForTask(ctx, tx, task.projectID, task.id,
			worktreeStatus, "system", reason, causationID); err != nil {
			return fmt.Errorf("startup recovery: reconcile workspace %s: %w", task.id, err)
		}
		if toStatus == model.TaskStatusQueued {
			queueProjects[task.projectID] = struct{}{}
		}
	}

	workerRows, err := tx.QueryContext(ctx, `SELECT project_id, session_id, id, status, version, current_task_id
		FROM agent_workers WHERE status <> 'lost' OR current_task_id IS NOT NULL
		ORDER BY project_id, session_id, id`)
	if err != nil {
		return fmt.Errorf("startup recovery: list workers: %w", err)
	}
	var workers []workerStateSnapshot
	for workerRows.Next() {
		var worker workerStateSnapshot
		if err := workerRows.Scan(&worker.projectID, &worker.sessionID, &worker.id,
			&worker.status, &worker.version, &worker.currentTaskID); err != nil {
			_ = workerRows.Close()
			return fmt.Errorf("startup recovery: scan worker: %w", err)
		}
		workers = append(workers, worker)
	}
	if err := workerRows.Err(); err != nil {
		_ = workerRows.Close()
		return fmt.Errorf("startup recovery: worker rows: %w", err)
	}
	if err := workerRows.Close(); err != nil {
		return fmt.Errorf("startup recovery: close worker rows: %w", err)
	}
	for _, worker := range workers {
		causationID := fmt.Sprintf("startup-recovery:worker:%s:%s:%d", worker.sessionID, worker.id, worker.version)
		if err := markWorkerLost(ctx, tx, worker, "system",
			"restart invalidated process-local worker authority", causationID); err != nil {
			return fmt.Errorf("startup recovery: worker %s lost: %w", worker.id, err)
		}
	}
	for projectID := range queueProjects {
		if err := bumpQueueVersionCAS(ctx, tx, projectID); err != nil {
			return fmt.Errorf("startup recovery: queue version %s: %w", projectID, err)
		}
	}

	var invalid int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks AS t
		LEFT JOIN task_leases AS l
		  ON l.id = t.active_lease_id AND l.project_id = t.project_id AND l.status = 'active'
		WHERE t.active_lease_id IS NOT NULL AND l.id IS NULL`).Scan(&invalid); err != nil {
		return fmt.Errorf("startup recovery: lease invariant query: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("startup recovery: %d task(s) reference invalid active leases: %w", invalid, store.ErrRecoveryIntegrity)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE status IN ('leased','executing') AND active_lease_id IS NULL`,
	).Scan(&invalid); err != nil {
		return fmt.Errorf("startup recovery: execution invariant query: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("startup recovery: %d active task(s) lack a lease: %w", invalid, store.ErrRecoveryIntegrity)
	}

	fkRows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("startup recovery: foreign key check: %w", err)
	}
	if fkRows.Next() {
		var table, parent string
		var rowID int64
		var fkID int
		if err := fkRows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			_ = fkRows.Close()
			return fmt.Errorf("startup recovery: foreign key result: %w", err)
		}
		_ = fkRows.Close()
		return fmt.Errorf("startup recovery: foreign key violation table=%s rowid=%d parent=%s fk=%d: %w",
			table, rowID, parent, fkID, store.ErrRecoveryIntegrity)
	}
	if err := fkRows.Close(); err != nil {
		return fmt.Errorf("startup recovery: close foreign key rows: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runtime_state(key, value, updated_at) VALUES ('startup_recovery', 'passed', datetime('now'))
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`); err != nil {
		return fmt.Errorf("startup recovery: runtime state: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(bound_project, action, result, detail, created_at)
		 VALUES ('system', 'runtime.recovery', 'ALLOWED', ?, datetime('now'))`,
		fmt.Sprintf(`{"interrupted_tasks":%d}`, len(interrupted)),
	); err != nil {
		return fmt.Errorf("startup recovery: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("startup recovery: commit: %w", err)
	}
	return nil
}
