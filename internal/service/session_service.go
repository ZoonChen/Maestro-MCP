package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// validRoles contains the set of allowed agent session roles.
var validRoles = map[string]bool{
	model.RoleBackend:     true,
	model.RoleFrontend:    true,
	model.RoleDevops:      true,
	model.RoleVerifier:    true,
	model.RoleCoordinator: true,
}

// SessionService manages agent sessions, workers, and stale-session cleanup.
type SessionService struct {
	sessionStore  store.SessionStore
	workerStore   store.WorkerStore
	taskStore     store.TaskStore
	worktreeStore store.WorktreeStore
	auditStore    store.AuditLogStore
	eventEmitter  EventEmitter
	db            *sql.DB
}

// NewSessionService creates a new SessionService with the required store dependencies.
func NewSessionService(
	sessionStore store.SessionStore,
	workerStore store.WorkerStore,
	taskStore store.TaskStore,
	worktreeStore store.WorktreeStore,
	auditStore store.AuditLogStore,
	eventEmitter EventEmitter,
) *SessionService {
	result := &SessionService{
		sessionStore:  sessionStore,
		workerStore:   workerStore,
		taskStore:     taskStore,
		worktreeStore: worktreeStore,
		auditStore:    auditStore,
		eventEmitter:  eventEmitter,
	}
	if provider, ok := sessionStore.(interface{ Database() *sql.DB }); ok {
		result.db = provider.Database()
	}
	return result
}

// RegisterSession validates and persists a new agent session.
// It validates that the role is one of the 5 allowed roles, applies defaults
// for client_type ("other"), capacity (1), and status ("online"), then delegates
// to the store layer.
func (s *SessionService) RegisterSession(ctx context.Context, projectID string, sess *model.AgentSession) error {
	if err := normalizeSessionRegistration(projectID, sess); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	if !validRoles[sess.Role] {
		return fmt.Errorf("register session: %w: invalid role %q, must be one of backend/frontend/devops/verifier/coordinator", store.ErrInvalidParameter, sess.Role)
	}

	if err := s.sessionStore.Create(ctx, projectID, sess); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	safeEmit(s.eventEmitter, "session.online", projectID, map[string]string{"session_id": sess.ID})
	s.logAudit(ctx, projectID, "session.register", "ALLOWED", &sess.ID, nil)
	return nil
}

// EnsureSession creates the session if it does not already exist, using INSERT OR IGNORE
// to avoid UNIQUE constraint violations under concurrent access.
// Returns true if the session was created, false if it already existed.
func (s *SessionService) EnsureSession(ctx context.Context, projectID string, sess *model.AgentSession) (bool, error) {
	if err := normalizeSessionRegistration(projectID, sess); err != nil {
		return false, fmt.Errorf("ensure session: %w", err)
	}
	if !validRoles[sess.Role] {
		return false, fmt.Errorf("ensure session: %w: invalid role %q", store.ErrInvalidParameter, sess.Role)
	}

	created, err := s.sessionStore.CreateIfNotExists(ctx, projectID, sess)
	if err != nil {
		return false, fmt.Errorf("ensure session: %w", err)
	}
	if created {
		safeEmit(s.eventEmitter, "session.online", projectID, map[string]string{"session_id": sess.ID})
		s.logAudit(ctx, projectID, "session.register", "ALLOWED", &sess.ID, nil)
		return true, nil
	}

	// A logical Session ID is an idempotency scope, not an authorization
	// shortcut. Reusing it with a different immutable identity would otherwise
	// let a caller silently change role, client identity, or worker capacity.
	existing, err := s.sessionStore.GetByID(ctx, projectID, sess.ID)
	if err != nil {
		return false, fmt.Errorf("ensure session: verify idempotent registration: %w", err)
	}
	if existing.Role != sess.Role || existing.ClientType != sess.ClientType || existing.Capacity != sess.Capacity {
		detail := fmt.Sprintf(`{"existing_role":%q,"requested_role":%q,"existing_client":%q,"requested_client":%q,"existing_capacity":%d,"requested_capacity":%d}`,
			existing.Role, sess.Role, existing.ClientType, sess.ClientType, existing.Capacity, sess.Capacity)
		s.logAuditDetail(ctx, projectID, "session.ensure", "DENIED", &sess.ID, nil, detail)
		return false, fmt.Errorf("ensure session: logical id %q reused with different identity: %w", sess.ID, store.ErrIdempotencyConflict)
	}
	return false, nil
}

func normalizeSessionRegistration(projectID string, sess *model.AgentSession) error {
	if sess == nil || projectID == "" || sess.ID == "" {
		return fmt.Errorf("project and session id are required: %w", store.ErrInvalidParameter)
	}
	if sess.ProjectID != "" && sess.ProjectID != projectID {
		return fmt.Errorf("session project %q does not match authorized project %q: %w",
			sess.ProjectID, projectID, store.ErrProjectScopeViolation)
	}
	sess.ProjectID = projectID
	if sess.ClientType == "" {
		sess.ClientType = "other"
	}
	if sess.Capacity == 0 {
		sess.Capacity = 1
	}
	if sess.Capacity < 1 || sess.Capacity > 5 {
		return fmt.Errorf("capacity must be between 1 and 5: %w", store.ErrInvalidParameter)
	}
	if sess.Status == "" {
		sess.Status = model.SessionStatusOnline
	}
	if sess.Status != model.SessionStatusOnline {
		return fmt.Errorf("new session must be online: %w", store.ErrTaskStateInvalid)
	}
	return nil
}

// GetSession retrieves a session by ID within a project.
func (s *SessionService) GetSession(ctx context.Context, projectID, id string) (*model.AgentSession, error) {
	sess, err := s.sessionStore.GetByID(ctx, projectID, id)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return sess, nil
}

// ListSessions returns all sessions for a project.
func (s *SessionService) ListSessions(ctx context.Context, projectID string) ([]*model.AgentSession, error) {
	sessions, err := s.sessionStore.List(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return sessions, nil
}

// UpdateHeartbeat refreshes the heartbeat timestamp for a session.
func (s *SessionService) UpdateHeartbeat(ctx context.Context, projectID, id string) error {
	if s.db == nil {
		return fmt.Errorf("update heartbeat: transactional database unavailable: %w", store.ErrRecoveryIntegrity)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("update heartbeat: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sessionKey, status string
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT id, status, version FROM agent_sessions
		WHERE project_id = ? AND COALESCE(external_id, id) = ?`, projectID, id,
	).Scan(&sessionKey, &status, &version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrSessionNotFound
		}
		return fmt.Errorf("update heartbeat: session snapshot: %w", err)
	}
	if status != model.SessionStatusOnline {
		return fmt.Errorf("update heartbeat: session is %s: %w", status, store.ErrOperationDisabled)
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_sessions
		SET last_heartbeat = datetime('now'), version = version + 1
		WHERE project_id = ? AND id = ? AND status = ? AND version = ?`,
		projectID, sessionKey, status, version)
	if err != nil {
		return fmt.Errorf("update heartbeat: CAS: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("update heartbeat: session changed: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	causationID := fmt.Sprintf("session-heartbeat:%s:%d", id, version)
	if err := appendStateHistory(ctx, tx, projectID, "session", sessionKey,
		status, status, version, version+1, id, "session heartbeat accepted", causationID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update heartbeat: commit: %w", err)
	}
	return nil
}

// DisconnectSession marks a session as offline and handles cleanup of workers
// and in-progress tasks (delegates to CleanupStaleSession logic).
func (s *SessionService) DisconnectSession(ctx context.Context, projectID, id string) error {
	sess, err := s.sessionStore.GetByID(ctx, projectID, id)
	if err != nil {
		return fmt.Errorf("disconnect session: %w", err)
	}

	changed, err := s.cleanupSessionWorkers(ctx, sess, sessionCleanupRequest{
		actor:       id,
		reason:      "session disconnected explicitly",
		causationID: fmt.Sprintf("session-disconnect:%s:%d", id, sess.Version),
		auditAction: "session.disconnect.cleanup",
	})
	if err != nil {
		return fmt.Errorf("disconnect session cleanup workers: %w", err)
	}
	if changed {
		safeEmit(s.eventEmitter, "session.offline", projectID, map[string]string{"session_id": id})
	}
	s.logAudit(ctx, projectID, "session.disconnect", "ALLOWED", &id, nil)
	return nil
}

// FindStaleSessions finds sessions that have not sent a heartbeat within the
// given timeout. This is a cross-project operation called by a background goroutine.
func (s *SessionService) FindStaleSessions(ctx context.Context, timeoutSec int) ([]*model.AgentSession, error) {
	sessions, err := s.sessionStore.FindStale(ctx, timeoutSec)
	if err != nil {
		return nil, fmt.Errorf("find stale sessions: %w", err)
	}
	return sessions, nil
}

// CleanupStaleSession atomically marks the session and its workers offline/lost.
// An unexpired lease is preserved until its deadline. Interrupted execution is
// quarantined; a leased-only task is safe to re-queue because execution never
// began. The exact observed Session version and cutoff guard stale cleanup.
func (s *SessionService) CleanupStaleSession(ctx context.Context, session *model.AgentSession) error {
	return s.cleanupStaleSessionAt(ctx, session, time.Now().UTC())
}

// cleanupStaleSessionAt binds cleanup to the exact stale snapshot returned by
// FindStaleSessions and to the scanner's cutoff. A heartbeat that wins the gap
// between scan and cleanup advances the Session version, so this operation
// becomes a safe no-op rather than disconnecting a recovered Session.
func (s *SessionService) cleanupStaleSessionAt(ctx context.Context, session *model.AgentSession, cutoff time.Time) error {
	if session == nil {
		return fmt.Errorf("cleanup stale session: session is required: %w", store.ErrInvalidParameter)
	}
	expectedVersion := session.Version
	changed, err := s.cleanupSessionWorkers(ctx, session, sessionCleanupRequest{
		actor:           "system",
		reason:          "session heartbeat exceeded stale cutoff",
		causationID:     fmt.Sprintf("session-stale:%s:%d", session.ID, session.Version),
		auditAction:     "session.stale_cleanup",
		expectedVersion: &expectedVersion,
		heartbeatCutoff: cutoff.UTC().Format("2006-01-02 15:04:05"),
	})
	if err != nil {
		return fmt.Errorf("cleanup stale session workers: %w", err)
	}
	if changed {
		safeEmit(s.eventEmitter, "session.offline", session.ProjectID, map[string]string{"session_id": session.ID})
	}
	return nil
}

type sessionCleanupRequest struct {
	actor           string
	reason          string
	causationID     string
	auditAction     string
	expectedVersion *int64
	heartbeatCutoff string
}

// cleanupSessionWorkers handles the shared worker-cleanup logic used by both
// DisconnectSession and CleanupStaleSession.
func (s *SessionService) cleanupSessionWorkers(ctx context.Context, session *model.AgentSession, request sessionCleanupRequest) (bool, error) {
	if s.db == nil {
		return false, fmt.Errorf("cleanup session workers: transactional database unavailable: %w", store.ErrRecoveryIntegrity)
	}
	if session == nil || session.ProjectID == "" || session.ID == "" || request.actor == "" ||
		request.reason == "" || request.causationID == "" || request.auditAction == "" {
		return false, fmt.Errorf("cleanup session workers: incomplete cleanup authority: %w", store.ErrInvalidParameter)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, fmt.Errorf("cleanup session workers: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sessionKey, currentStatus, lastHeartbeat string
	var sessionVersion int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id, status, version, last_heartbeat FROM agent_sessions
		 WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		session.ProjectID, session.ID,
	).Scan(&sessionKey, &currentStatus, &sessionVersion, &lastHeartbeat); err != nil {
		if err == sql.ErrNoRows {
			return false, store.ErrSessionNotFound
		}
		return false, fmt.Errorf("cleanup session workers: resolve session: %w", err)
	}
	if request.expectedVersion != nil && (currentStatus != model.SessionStatusOnline ||
		sessionVersion != *request.expectedVersion || request.heartbeatCutoff == "") {
		return false, nil
	}

	changed := false
	if currentStatus == model.SessionStatusOnline {
		query := `UPDATE agent_sessions SET status = 'offline', version = version + 1
			WHERE project_id = ? AND id = ? AND status = 'online' AND version = ?`
		args := []any{session.ProjectID, sessionKey, sessionVersion}
		if request.expectedVersion != nil {
			query += ` AND last_heartbeat = ? AND julianday(last_heartbeat) <= julianday(?)`
			args = append(args, lastHeartbeat, request.heartbeatCutoff)
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return false, fmt.Errorf("cleanup session workers: mark offline: %w", err)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			if request.expectedVersion != nil {
				return false, nil
			}
			return false, store.ErrConcurrentConflict
		}
		if err := appendStateHistory(ctx, tx, session.ProjectID, "session", sessionKey,
			model.SessionStatusOnline, model.SessionStatusOffline, sessionVersion, sessionVersion+1,
			request.actor, request.reason, request.causationID); err != nil {
			return false, err
		}
		changed = true
	}

	type interruptedTask struct {
		id, status                         string
		version                            int64
		leaseID, leaseStatus, leaseExpires sql.NullString
		leaseVersion, leaseEpoch           sql.NullInt64
		leaseValid                         int
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id, t.status, t.version, t.active_lease_id, l.status,
		       l.version, l.epoch, l.expires_at,
		       CASE WHEN l.status = 'active' AND julianday(l.expires_at) > julianday('now') THEN 1 ELSE 0 END
		FROM tasks AS t
		LEFT JOIN task_leases AS l
		  ON l.id = t.active_lease_id AND l.project_id = t.project_id
		WHERE t.project_id = ? AND (
		      (t.status IN ('leased','executing','cancelling') AND t.assigned_session_id = ?)
		   OR (t.status = 'validating' AND t.verified_by = ?)
		)`,
		session.ProjectID, sessionKey, sessionKey,
	)
	if err != nil {
		return false, fmt.Errorf("cleanup session workers: list assigned tasks: %w", err)
	}
	var tasks []interruptedTask
	for rows.Next() {
		var item interruptedTask
		if err := rows.Scan(&item.id, &item.status, &item.version, &item.leaseID, &item.leaseStatus,
			&item.leaseVersion, &item.leaseEpoch, &item.leaseExpires, &item.leaseValid); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("cleanup session workers: scan task: %w", err)
		}
		tasks = append(tasks, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("cleanup session workers: task rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("cleanup session workers: close tasks: %w", err)
	}

	queueChanged := false
	for _, task := range tasks {
		validLease := task.leaseID.Valid && task.leaseStatus.String == model.LeaseStatusActive && task.leaseValid == 1
		if validLease {
			continue
		}
		if task.leaseID.Valid && task.leaseStatus.String == model.LeaseStatusActive {
			if !task.leaseVersion.Valid || !task.leaseEpoch.Valid || !task.leaseExpires.Valid {
				return false, fmt.Errorf("cleanup session workers: incomplete lease snapshot for %s: %w", task.id, store.ErrRecoveryIntegrity)
			}
			result, err := tx.ExecContext(ctx,
				`UPDATE task_leases SET status = 'expired', version = version + 1, updated_at = datetime('now')
				 WHERE id = ? AND project_id = ? AND task_id = ? AND status = 'active'
				   AND version = ? AND epoch = ? AND expires_at = ?`,
				task.leaseID.String, session.ProjectID, task.id, task.leaseVersion.Int64,
				task.leaseEpoch.Int64, task.leaseExpires.String,
			)
			if err != nil {
				return false, fmt.Errorf("cleanup session workers: expire lease: %w", err)
			}
			if n, _ := result.RowsAffected(); n != 1 {
				return false, store.ErrConcurrentConflict
			}
			if err := appendStateHistory(ctx, tx, session.ProjectID, "lease", task.leaseID.String,
				model.LeaseStatusActive, model.LeaseStatusExpired, task.leaseVersion.Int64, task.leaseVersion.Int64+1,
				request.actor, "session unavailable after lease deadline", request.causationID); err != nil {
				return false, err
			}
		}

		var targetStatus, worktreeStatus, transitionReason string
		switch task.status {
		case model.TaskStatusLeased:
			targetStatus = model.TaskStatusQueued
			worktreeStatus = model.WorktreeStatusCleanupPending
			transitionReason = "session unavailable before execution"
			queueChanged = true
		case model.TaskStatusExecuting, model.TaskStatusValidating:
			targetStatus = model.TaskStatusNeedsHuman
			worktreeStatus = model.WorktreeStatusQuarantined
			transitionReason = "session became unavailable without a valid lease"
		case model.TaskStatusCancelling:
			targetStatus = model.TaskStatusCancelled
			worktreeStatus = model.WorktreeStatusCleanupPending
			transitionReason = "session unavailable after cancellation request"
		default:
			return false, fmt.Errorf("cleanup session workers: task %s has unsupported state %s: %w",
				task.id, task.status, store.ErrRecoveryIntegrity)
		}
		query := `UPDATE tasks SET status = ?,
			     assigned_session_id = CASE WHEN assigned_session_id = ? THEN NULL ELSE assigned_session_id END,
			     assigned_worker_id = CASE WHEN assigned_session_id = ? THEN NULL ELSE assigned_worker_id END,
			     verified_by = CASE WHEN verified_by = ? THEN NULL ELSE verified_by END,
			     active_lease_id = NULL, lease_expires_at = NULL, version = version + 1,
			     updated_at = datetime('now')
			 WHERE project_id = ? AND id = ? AND status = ? AND version = ?`
		args := []any{targetStatus, sessionKey, sessionKey, sessionKey, session.ProjectID, task.id, task.status, task.version}
		if task.leaseID.Valid {
			query += ` AND active_lease_id = ?`
			args = append(args, task.leaseID.String)
		} else {
			query += ` AND active_lease_id IS NULL`
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return false, fmt.Errorf("cleanup session workers: reconcile task %s: %w", task.id, err)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return false, store.ErrConcurrentConflict
		}
		causationID := request.causationID
		if task.leaseID.Valid {
			causationID = task.leaseID.String
		}
		if err := appendStateHistory(ctx, tx, session.ProjectID, "task", task.id,
			task.status, targetStatus, task.version, task.version+1,
			request.actor, transitionReason, causationID); err != nil {
			return false, err
		}
		if err := transitionWorktreesForTask(ctx, tx, session.ProjectID, task.id, worktreeStatus,
			request.actor, transitionReason, causationID); err != nil {
			return false, fmt.Errorf("cleanup session workers: reconcile worktree: %w", err)
		}
		changed = true
	}

	workerRows, err := tx.QueryContext(ctx, `SELECT id, status, version, current_task_id
		FROM agent_workers WHERE project_id = ? AND session_id = ?
		  AND (status <> 'lost' OR current_task_id IS NOT NULL)
		ORDER BY id`, session.ProjectID, sessionKey)
	if err != nil {
		return false, fmt.Errorf("cleanup session workers: list workers: %w", err)
	}
	var workers []workerStateSnapshot
	for workerRows.Next() {
		var worker workerStateSnapshot
		if err := workerRows.Scan(&worker.id, &worker.status, &worker.version, &worker.currentTaskID); err != nil {
			_ = workerRows.Close()
			return false, fmt.Errorf("cleanup session workers: scan worker: %w", err)
		}
		worker.projectID = session.ProjectID
		worker.sessionID = sessionKey
		workers = append(workers, worker)
	}
	if err := workerRows.Err(); err != nil {
		_ = workerRows.Close()
		return false, fmt.Errorf("cleanup session workers: worker rows: %w", err)
	}
	if err := workerRows.Close(); err != nil {
		return false, fmt.Errorf("cleanup session workers: close workers: %w", err)
	}
	for _, worker := range workers {
		if err := markWorkerLost(ctx, tx, worker, request.actor, request.reason, request.causationID); err != nil {
			return false, fmt.Errorf("cleanup session workers: mark worker lost: %w", err)
		}
		changed = true
	}
	if queueChanged {
		if err := bumpQueueVersionCAS(ctx, tx, session.ProjectID); err != nil {
			return false, fmt.Errorf("cleanup session workers: queue version: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log(session_id, bound_project, target_project, action, result, detail, created_at)
		 VALUES (?, ?, ?, ?, 'ALLOWED', ?, datetime('now'))`,
		session.ID, session.ProjectID, session.ProjectID, request.auditAction,
		fmt.Sprintf(`{"causation_id":%q,"resources_changed":%t}`, request.causationID, changed),
	); err != nil {
		return false, fmt.Errorf("cleanup session workers: audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("cleanup session workers: commit: %w", err)
	}
	return changed, nil
}

type workerStateSnapshot struct {
	projectID, sessionID, id, status string
	version                          int64
	currentTaskID                    sql.NullString
}

func markWorkerLost(ctx context.Context, tx *sql.Tx, worker workerStateSnapshot, actor, reason, causationID string) error {
	if actor == "" || reason == "" || causationID == "" ||
		!model.CanWorkerTransition(worker.status, model.WorkerStatusLost) {
		return fmt.Errorf("worker %s transition %s -> lost is not authorized: %w",
			worker.id, worker.status, store.ErrRecoveryIntegrity)
	}
	query := `UPDATE agent_workers SET current_task_id = NULL, status = 'lost',
		version = version + 1, last_active = datetime('now')
		WHERE project_id = ? AND session_id = ? AND id = ? AND status = ? AND version = ?`
	args := []any{worker.projectID, worker.sessionID, worker.id, worker.status, worker.version}
	if worker.currentTaskID.Valid {
		query += ` AND current_task_id = ?`
		args = append(args, worker.currentTaskID.String)
	} else {
		query += ` AND current_task_id IS NULL`
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return errors.Join(store.ErrConcurrentConflict, err)
	}
	return appendStateHistory(ctx, tx, worker.projectID, "worker", worker.sessionID+"/"+worker.id,
		worker.status, model.WorkerStatusLost, worker.version, worker.version+1,
		actor, reason, causationID)
}

type worktreeStateSnapshot struct {
	id, generation, version int64
	status                  string
}

func transitionWorktreesForTask(
	ctx context.Context,
	tx *sql.Tx,
	projectID, taskID, targetStatus, actor, reason, causationID string,
) error {
	if projectID == "" || taskID == "" || actor == "" || reason == "" || causationID == "" ||
		!model.IsWorktreeStatus(targetStatus) {
		return fmt.Errorf("worktree transition authority is incomplete: %w", store.ErrRecoveryIntegrity)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, status, generation, version FROM worktrees
		WHERE project_id = ? AND task_id = ?
		  AND status IN ('allocated','active','sealed','submitted')
		ORDER BY generation, id`, projectID, taskID)
	if err != nil {
		return err
	}
	var worktrees []worktreeStateSnapshot
	for rows.Next() {
		var worktree worktreeStateSnapshot
		if err := rows.Scan(&worktree.id, &worktree.status, &worktree.generation, &worktree.version); err != nil {
			_ = rows.Close()
			return err
		}
		worktrees = append(worktrees, worktree)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, worktree := range worktrees {
		if !model.CanWorktreeTransition(worktree.status, targetStatus) {
			return fmt.Errorf("worktree %d generation %d transition %s -> %s: %w",
				worktree.id, worktree.generation, worktree.status, targetStatus, store.ErrRecoveryIntegrity)
		}
		result, err := tx.ExecContext(ctx, `UPDATE worktrees
			SET status = ?, version = version + 1, updated_at = datetime('now')
			WHERE id = ? AND project_id = ? AND task_id = ? AND status = ?
			  AND generation = ? AND version = ?`,
			targetStatus, worktree.id, projectID, taskID, worktree.status,
			worktree.generation, worktree.version)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return errors.Join(store.ErrConcurrentConflict, err)
		}
		if err := appendStateHistory(ctx, tx, projectID, "worktree", fmt.Sprint(worktree.id),
			worktree.status, targetStatus, worktree.version, worktree.version+1,
			actor, reason, causationID); err != nil {
			return err
		}
	}
	return nil
}

func bumpQueueVersionCAS(ctx context.Context, tx *sql.Tx, projectID string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO project_queue_versions(project_id, version) VALUES (?, 0)`, projectID,
	); err != nil {
		return err
	}
	var version int64
	if err := tx.QueryRowContext(ctx,
		`SELECT version FROM project_queue_versions WHERE project_id = ?`, projectID,
	).Scan(&version); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE project_queue_versions SET version = version + 1 WHERE project_id = ? AND version = ?`,
		projectID, version)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return errors.Join(store.ErrConcurrentConflict, err)
	}
	return nil
}

// RecoverExpiredLeases reconciles durable leases after their deadline even if
// the owning session is already offline (and therefore no longer appears in
// the stale-session query). It is safe to call repeatedly.
func (s *SessionService) RecoverExpiredLeases(ctx context.Context) error {
	if s.db == nil {
		return fmt.Errorf("recover expired leases: transactional database unavailable: %w", store.ErrRecoveryIntegrity)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("recover expired leases: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	type expiredLease struct {
		id, projectID, taskID, sessionKey, workerID, taskStatus string
		taskVersion, leaseVersion, leaseEpoch                   int64
		leaseExpires                                            string
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT l.id, l.project_id, l.task_id, l.session_id, l.worker_id,
		       t.status, t.version, l.version, l.epoch, l.expires_at
		FROM task_leases AS l
		JOIN tasks AS t ON t.project_id = l.project_id AND t.id = l.task_id
		WHERE l.status = 'active' AND julianday(l.expires_at) <= julianday('now')
		  AND t.active_lease_id = l.id
		ORDER BY l.project_id, l.task_id`)
	if err != nil {
		return fmt.Errorf("recover expired leases: list: %w", err)
	}
	var expired []expiredLease
	for rows.Next() {
		var lease expiredLease
		if err := rows.Scan(&lease.id, &lease.projectID, &lease.taskID, &lease.sessionKey,
			&lease.workerID, &lease.taskStatus, &lease.taskVersion, &lease.leaseVersion,
			&lease.leaseEpoch, &lease.leaseExpires); err != nil {
			_ = rows.Close()
			return fmt.Errorf("recover expired leases: scan: %w", err)
		}
		expired = append(expired, lease)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("recover expired leases: rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("recover expired leases: close rows: %w", err)
	}

	var orphaned int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM task_leases AS l
		JOIN tasks AS t ON t.project_id = l.project_id AND t.id = l.task_id
		WHERE l.status = 'active' AND julianday(l.expires_at) <= julianday('now')
		  AND (t.active_lease_id IS NULL OR t.active_lease_id <> l.id)`,
	).Scan(&orphaned); err != nil {
		return fmt.Errorf("recover expired leases: orphan check: %w", err)
	}
	if orphaned != 0 {
		return fmt.Errorf("recover expired leases: %d orphaned active leases: %w", orphaned, store.ErrRecoveryIntegrity)
	}

	queueProjects := make(map[string]struct{})
	for _, lease := range expired {
		var toStatus string
		worktreeStatus := model.WorktreeStatusQuarantined
		reason := "execution lease expired; side effects require reconciliation"
		switch lease.taskStatus {
		case model.TaskStatusLeased:
			toStatus = model.TaskStatusQueued
			worktreeStatus = model.WorktreeStatusCleanupPending
			reason = "lease expired before execution"
		case model.TaskStatusExecuting, model.TaskStatusValidating:
			toStatus = model.TaskStatusNeedsHuman
		case model.TaskStatusCancelling:
			toStatus = model.TaskStatusCancelled
			worktreeStatus = model.WorktreeStatusCleanupPending
			reason = "cancellation lease expired"
		default:
			return fmt.Errorf("recover expired leases: task %s is %s with active lease: %w",
				lease.taskID, lease.taskStatus, store.ErrRecoveryIntegrity)
		}
		if !model.CanTaskTransition(lease.taskStatus, toStatus) {
			return fmt.Errorf("recover expired leases: task %s transition %s -> %s: %w",
				lease.taskID, lease.taskStatus, toStatus, store.ErrRecoveryIntegrity)
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE task_leases SET status = 'expired', version = version + 1, updated_at = datetime('now')
			 WHERE id = ? AND project_id = ? AND task_id = ? AND session_id = ? AND worker_id = ?
			   AND status = 'active' AND version = ? AND epoch = ? AND expires_at = ?
			   AND julianday(expires_at) <= julianday('now')`,
			lease.id, lease.projectID, lease.taskID, lease.sessionKey, lease.workerID,
			lease.leaseVersion, lease.leaseEpoch, lease.leaseExpires,
		)
		if err != nil {
			return fmt.Errorf("recover expired leases: expire %s: %w", lease.id, err)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return store.ErrConcurrentConflict
		}
		if err := appendStateHistory(ctx, tx, lease.projectID, "lease", lease.id,
			model.LeaseStatusActive, model.LeaseStatusExpired, lease.leaseVersion, lease.leaseVersion+1,
			"system", reason, lease.id); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?, assigned_session_id = NULL, assigned_worker_id = NULL,
			     assigned_at = NULL, verified_by = NULL, verified_at = NULL,
			     active_lease_id = NULL, lease_expires_at = NULL, version = version + 1,
			     updated_at = datetime('now')
			 WHERE project_id = ? AND id = ? AND status = ? AND version = ? AND active_lease_id = ?`,
			toStatus, lease.projectID, lease.taskID, lease.taskStatus, lease.taskVersion, lease.id,
		)
		if err != nil {
			return fmt.Errorf("recover expired leases: task %s: %w", lease.taskID, err)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return store.ErrConcurrentConflict
		}
		if err := appendStateHistory(ctx, tx, lease.projectID, "task", lease.taskID,
			lease.taskStatus, toStatus, lease.taskVersion, lease.taskVersion+1,
			"system", reason, lease.id); err != nil {
			return err
		}
		var worker workerStateSnapshot
		worker.projectID, worker.sessionID, worker.id = lease.projectID, lease.sessionKey, lease.workerID
		if err := tx.QueryRowContext(ctx, `SELECT status, version, current_task_id FROM agent_workers
			WHERE project_id = ? AND session_id = ? AND id = ?`,
			lease.projectID, lease.sessionKey, lease.workerID,
		).Scan(&worker.status, &worker.version, &worker.currentTaskID); err != nil {
			return fmt.Errorf("recover expired leases: worker snapshot %s: %w", lease.workerID, err)
		}
		if worker.status != model.WorkerStatusLost || worker.currentTaskID.Valid {
			if err := markWorkerLost(ctx, tx, worker, "system", reason, lease.id); err != nil {
				return fmt.Errorf("recover expired leases: worker %s: %w", lease.workerID, err)
			}
		}
		if err := transitionWorktreesForTask(ctx, tx, lease.projectID, lease.taskID,
			worktreeStatus, "system", reason, lease.id); err != nil {
			return fmt.Errorf("recover expired leases: workspace %s: %w", lease.taskID, err)
		}
		if toStatus == model.TaskStatusQueued {
			queueProjects[lease.projectID] = struct{}{}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO audit_log(bound_project, target_project, target_task, action, result, detail, created_at)
			 VALUES (?, ?, ?, 'lease.expire', 'ALLOWED', ?, datetime('now'))`,
			lease.projectID, lease.projectID, lease.taskID,
			fmt.Sprintf(`{"lease_id":%q,"to_status":%q}`, lease.id, toStatus),
		); err != nil {
			return fmt.Errorf("recover expired leases: audit: %w", err)
		}
	}
	for projectID := range queueProjects {
		if err := bumpQueueVersionCAS(ctx, tx, projectID); err != nil {
			return fmt.Errorf("recover expired leases: queue version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recover expired leases: commit: %w", err)
	}
	return nil
}

// RegisterWorker creates a new worker within an existing session.
// It validates that the session exists and enforces the capacity limit (max 5 workers).
func (s *SessionService) RegisterWorker(ctx context.Context, projectID, sessionID string, worker *model.AgentWorker) error {
	if s.db == nil {
		return fmt.Errorf("register worker: transactional database unavailable: %w", store.ErrRecoveryIntegrity)
	}
	if worker == nil || worker.ID == "" {
		return fmt.Errorf("register worker: worker id is required: %w", store.ErrInvalidParameter)
	}
	if worker.Status == "" {
		worker.Status = model.WorkerStatusIdle
	}
	if worker.Status != model.WorkerStatusIdle {
		return fmt.Errorf("register worker: new worker must be idle: %w", store.ErrTaskStateInvalid)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("register worker: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sessionKey, status string
	var maxCapacity int
	if err := tx.QueryRowContext(ctx,
		`SELECT id, capacity, status FROM agent_sessions
		 WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		projectID, sessionID,
	).Scan(&sessionKey, &maxCapacity, &status); err != nil {
		if err == sql.ErrNoRows {
			return store.ErrSessionNotFound
		}
		return fmt.Errorf("register worker: session: %w", err)
	}
	if status != model.SessionStatusOnline {
		return fmt.Errorf("register worker: session is offline: %w", store.ErrOperationDisabled)
	}
	if maxCapacity <= 0 {
		maxCapacity = 1
	}
	if maxCapacity > 5 {
		maxCapacity = 5
	}
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_workers WHERE project_id = ? AND session_id = ? AND status <> 'lost'`,
		projectID, sessionKey,
	).Scan(&count); err != nil {
		return fmt.Errorf("register worker: count: %w", err)
	}
	if count >= maxCapacity {
		return fmt.Errorf("register worker: %w: session %s has %d active workers, capacity %d",
			store.ErrSessionCapacityFull, sessionID, count, maxCapacity)
	}
	worker.SessionID = sessionID
	worker.ProjectID = projectID
	worker.LastActive = time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_workers(id, session_id, project_id, current_task_id, status,
		 tasks_completed, version, last_active) VALUES (?, ?, ?, NULL, 'idle', 0, 0, ?)`,
		worker.ID, sessionKey, projectID, worker.LastActive,
	); err != nil {
		return fmt.Errorf("register worker: create: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("register worker: commit: %w", err)
	}
	return nil
}

// ListWorkers returns all workers for a given session.
func (s *SessionService) ListWorkers(ctx context.Context, projectID, sessionID string) ([]*model.AgentWorker, error) {
	workers, err := s.workerStore.ListBySession(ctx, projectID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	return workers, nil
}

// ReleaseWorker retires an idle worker identity. Historical Lease rows retain a
// foreign key to the worker, so worker identities are marked lost rather than
// deleted. Active work must be cancelled/recovered through the task workflow.
func (s *SessionService) ReleaseWorker(ctx context.Context, projectID, sessionID, workerID string) error {
	if s.db == nil {
		return fmt.Errorf("release worker: transactional database unavailable: %w", store.ErrRecoveryIntegrity)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("release worker: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sessionKey string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM agent_sessions
		WHERE project_id = ? AND COALESCE(external_id, id) = ?`, projectID, sessionID,
	).Scan(&sessionKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrSessionNotFound
		}
		return fmt.Errorf("release worker: session snapshot: %w", err)
	}
	var status string
	var version int64
	var currentTaskID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, version, current_task_id FROM agent_workers
		WHERE project_id = ? AND session_id = ? AND id = ?`, projectID, sessionKey, workerID,
	).Scan(&status, &version, &currentTaskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrWorkerNotFound
		}
		return fmt.Errorf("release worker: worker snapshot: %w", err)
	}
	if currentTaskID.Valid && currentTaskID.String != "" {
		return fmt.Errorf("release worker: active task %s requires cancellation or recovery: %w", currentTaskID.String, store.ErrOperationDisabled)
	}
	if status == model.WorkerStatusLost {
		return nil
	}
	if status != model.WorkerStatusIdle || !model.CanWorkerTransition(status, model.WorkerStatusLost) {
		return fmt.Errorf("release worker: worker is %s, expected idle: %w", status, store.ErrOperationDisabled)
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_workers
		SET status = 'lost', current_task_id = NULL, version = version + 1, last_active = datetime('now')
		WHERE project_id = ? AND session_id = ? AND id = ? AND status = ?
		  AND version = ? AND current_task_id IS NULL`,
		projectID, sessionKey, workerID, status, version)
	if err != nil {
		return fmt.Errorf("release worker: CAS: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("release worker: worker changed: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	causationID := fmt.Sprintf("worker-release:%s:%s:%d", sessionID, workerID, version)
	if err := appendStateHistory(ctx, tx, projectID, "worker", sessionKey+"/"+workerID,
		status, model.WorkerStatusLost, version, version+1,
		sessionID, "idle worker released", causationID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("release worker: commit: %w", err)
	}
	return nil
}

// StartStaleSessionScanner launches a background goroutine that periodically
// scans for stale sessions and cleans them up. It runs until ctx is cancelled.
func (s *SessionService) StartStaleSessionScanner(ctx context.Context, interval time.Duration, timeoutSec int) {
	go s.RunStaleSessionScanner(ctx, interval, timeoutSec)
}

// RunStaleSessionScanner is the synchronous form used by the process
// composition root so its lifecycle can be tracked and awaited during graceful
// shutdown. StartStaleSessionScanner remains for backwards compatibility.
func (s *SessionService) RunStaleSessionScanner(ctx context.Context, interval time.Duration, timeoutSec int) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RecoverExpiredLeases(ctx); err != nil {
				slog.Error("expired lease recovery error", "error", err)
			}
			stale, err := s.FindStaleSessions(ctx, timeoutSec)
			if err != nil {
				slog.Error("stale session scan error", "error", err)
				continue
			}
			cutoff := time.Now().UTC().Add(-time.Duration(timeoutSec) * time.Second)
			for _, sess := range stale {
				if err := s.cleanupStaleSessionAt(ctx, sess, cutoff); err != nil {
					slog.Error("stale session cleanup error", "session_id", sess.ID, "error", err)
				}
			}
		}
	}
}

// logAudit writes a security audit entry for a session-level operation.
func (s *SessionService) logAudit(ctx context.Context, boundProject, action, result string, sessionID, taskID *string) {
	s.logAuditDetail(ctx, boundProject, action, result, sessionID, taskID, "")
}

func (s *SessionService) logAuditDetail(ctx context.Context, boundProject, action, result string, sessionID, taskID *string, detail string) {
	entry := &model.AuditLog{
		SessionID:     sessionID,
		BoundProject:  boundProject,
		TargetProject: &boundProject,
		TargetTask:    taskID,
		Action:        action,
		Result:        result,
		CreatedAt:     time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if detail != "" {
		entry.Detail = &detail
	}
	if err := s.auditStore.Create(ctx, entry); err != nil {
		slog.Error("logAudit: failed to write audit entry", "error", err)
	}
}

// ForceRelease forcefully releases a session — marks it offline, cleans up
// all workers and releases their tasks. This is an admin operation.
func (s *SessionService) ForceRelease(ctx context.Context, projectID, sessionID string) error {
	sess, err := s.sessionStore.GetByID(ctx, projectID, sessionID)
	if err != nil {
		return fmt.Errorf("force release: %w", err)
	}

	changed, err := s.cleanupSessionWorkers(ctx, sess, sessionCleanupRequest{
		actor:       "system",
		reason:      "session force release requested",
		causationID: fmt.Sprintf("session-force-release:%s:%d", sessionID, sess.Version),
		auditAction: "session.force_release.cleanup",
	})
	if err != nil {
		return fmt.Errorf("force release cleanup: %w", err)
	}
	if changed {
		safeEmit(s.eventEmitter, "session.offline", projectID, map[string]string{"session_id": sessionID})
	}

	s.logAudit(ctx, projectID, "session.force_release", "ALLOWED", &sessionID, nil)
	return nil
}
