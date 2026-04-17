package service

import (
	"context"
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
	return &SessionService{
		sessionStore:  sessionStore,
		workerStore:   workerStore,
		taskStore:     taskStore,
		worktreeStore: worktreeStore,
		auditStore:    auditStore,
		eventEmitter:  eventEmitter,
	}
}

// RegisterSession validates and persists a new agent session.
// It validates that the role is one of the 5 allowed roles, applies defaults
// for client_type ("other"), capacity (1), and status ("online"), then delegates
// to the store layer.
func (s *SessionService) RegisterSession(ctx context.Context, projectID string, sess *model.AgentSession) error {
	if !validRoles[sess.Role] {
		return fmt.Errorf("register session: %w: invalid role %q, must be one of backend/frontend/devops/verifier/coordinator", store.ErrInvalidParameter, sess.Role)
	}

	if sess.ClientType == "" {
		sess.ClientType = "other"
	}
	if sess.Capacity == 0 {
		sess.Capacity = 1
	}
	if sess.Status == "" {
		sess.Status = model.SessionStatusOnline
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
	if !validRoles[sess.Role] {
		return false, fmt.Errorf("ensure session: %w: invalid role %q", store.ErrInvalidParameter, sess.Role)
	}
	if sess.ClientType == "" {
		sess.ClientType = "other"
	}
	if sess.Capacity == 0 {
		sess.Capacity = 1
	}
	if sess.Status == "" {
		sess.Status = model.SessionStatusOnline
	}

	created, err := s.sessionStore.CreateIfNotExists(ctx, projectID, sess)
	if err != nil {
		return false, fmt.Errorf("ensure session: %w", err)
	}
	if created {
		safeEmit(s.eventEmitter, "session.online", projectID, map[string]string{"session_id": sess.ID})
		s.logAudit(ctx, projectID, "session.register", "ALLOWED", &sess.ID, nil)
	}
	return created, nil
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
	if err := s.sessionStore.UpdateHeartbeat(ctx, projectID, id); err != nil {
		return fmt.Errorf("update heartbeat: %w", err)
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

	if err := s.sessionStore.UpdateStatus(ctx, projectID, id, model.SessionStatusOffline); err != nil {
		return fmt.Errorf("disconnect session set offline: %w", err)
	}
	safeEmit(s.eventEmitter, "session.offline", projectID, map[string]string{"session_id": id})
	s.logAudit(ctx, projectID, "session.disconnect", "ALLOWED", &id, nil)

	if err := s.cleanupSessionWorkers(ctx, sess); err != nil {
		return fmt.Errorf("disconnect session cleanup workers: %w", err)
	}
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

// CleanupStaleSession handles a single stale session:
//  1. Sets session status to "offline"
//  2. Finds all workers for this session
//  3. For each worker with a current_task_id:
//     - "in_progress" -> "pending", clear assignments, mark worktree "stale"
//     - "verifying" (if this is the verifier session) -> "submitted", clear worker task
//     - "blocked" or "merge_conflicted" -> keep status, clear assignments
//     - "ready_to_merge" and "done" -> not affected
//  4. Deletes all workers for this session
func (s *SessionService) CleanupStaleSession(ctx context.Context, session *model.AgentSession) error {
	// Step 1: Mark session offline.
	if err := s.sessionStore.UpdateStatus(ctx, session.ProjectID, session.ID, model.SessionStatusOffline); err != nil {
		return fmt.Errorf("cleanup stale session set offline: %w", err)
	}

	// Step 2: Find all workers for this session.
	if err := s.cleanupSessionWorkers(ctx, session); err != nil {
		return fmt.Errorf("cleanup stale session workers: %w", err)
	}
	safeEmit(s.eventEmitter, "session.offline", session.ProjectID, map[string]string{"session_id": session.ID})
	return nil
}

// cleanupSessionWorkers handles the shared worker-cleanup logic used by both
// DisconnectSession and CleanupStaleSession.
func (s *SessionService) cleanupSessionWorkers(ctx context.Context, session *model.AgentSession) error {
	workers, err := s.workerStore.ListBySession(ctx, session.ProjectID, session.ID)
	if err != nil {
		return fmt.Errorf("list workers for session %s: %w", session.ID, err)
	}

	for _, worker := range workers {
		if worker.CurrentTaskID != nil && *worker.CurrentTaskID != "" {
			taskID := *worker.CurrentTaskID

			task, err := s.taskStore.GetByID(ctx, session.ProjectID, taskID)
			if err != nil {
				// Task may have been deleted; skip and continue with other workers.
				continue
			}

			switch task.Status {
			case model.TaskStatusInProgress:
				// Reset task to pending so another worker can claim it.
				// Use conditional update to prevent overwriting concurrent state changes
				if err := s.taskStore.UpdateStatusFrom(ctx, session.ProjectID, taskID, model.TaskStatusInProgress, model.TaskStatusPending); err != nil {
					slog.Warn("cleanupSessionWorkers: task status changed concurrently", "task_id", taskID)
					continue
				}
				// Clear assigned_session_id/assigned_worker_id by updating the task.
				task.AssignedSessionID = nil
				task.AssignedWorkerID = nil
				if err := s.taskStore.Update(ctx, session.ProjectID, task); err != nil {
					return fmt.Errorf("clear task %s assignments: %w", taskID, err)
				}
				// Mark associated worktree as stale.
				if wt, wtErr := s.worktreeStore.GetByTaskID(ctx, session.ProjectID, taskID); wtErr == nil {
					_ = s.worktreeStore.UpdateStatus(ctx, session.ProjectID, wt.ID, model.WorktreeStatusStale)
				}

			case model.TaskStatusVerifying:
				// Only the verifier session's timeout should revert to submitted.
				if task.VerifiedBy != nil && *task.VerifiedBy == session.ID {
					if err := s.taskStore.UpdateStatus(ctx, session.ProjectID, taskID, model.TaskStatusSubmitted); err != nil {
						return fmt.Errorf("revert task %s from verifying to submitted: %w", taskID, err)
					}
					if err := s.workerStore.UpdateCurrentTask(ctx, session.ProjectID, session.ID, worker.ID, ""); err != nil {
						return fmt.Errorf("clear worker %s current task: %w", worker.ID, err)
					}
				}

			case model.TaskStatusBlocked, model.TaskStatusMergeConflicted:
				// Keep the status but clear assignments so the task is not
				// permanently locked to a dead session.
				task.AssignedSessionID = nil
				task.AssignedWorkerID = nil
				if err := s.taskStore.Update(ctx, session.ProjectID, task); err != nil {
					return fmt.Errorf("clear task %s assignments for %s state: %w", taskID, task.Status, err)
				}

			case model.TaskStatusReadyToMerge, model.TaskStatusDone:
				// Not affected — these are terminal or near-terminal states.

			case model.TaskStatusSubmitted:
				// Submitted tasks are waiting for verification; the executor is no
				// longer needed so just clear the worker's current_task_id.
				// No status change needed.

			default:
				// No action for other states (pending, cancelled, etc.).
			}
		}

		// Step 4: Delete the worker.
		if err := s.workerStore.Delete(ctx, session.ProjectID, session.ID, worker.ID); err != nil {
			return fmt.Errorf("delete worker %s for session %s: %w", worker.ID, session.ID, err)
		}
	}

	return nil
}

// RegisterWorker creates a new worker within an existing session.
// It validates that the session exists and enforces the capacity limit (max 5 workers).
func (s *SessionService) RegisterWorker(ctx context.Context, projectID, sessionID string, worker *model.AgentWorker) error {
	// Verify session exists and get capacity.
	sess, err := s.sessionStore.GetByID(ctx, projectID, sessionID)
	if err != nil {
		return fmt.Errorf("register worker: %w", err)
	}

	// Enforce capacity limit.
	maxCapacity := sess.Capacity
	if maxCapacity <= 0 {
		maxCapacity = 1
	}
	if maxCapacity > 5 {
		maxCapacity = 5
	}

	count, err := s.workerStore.CountBySession(ctx, projectID, sessionID)
	if err != nil {
		return fmt.Errorf("register worker count: %w", err)
	}
	if count >= maxCapacity {
		return fmt.Errorf("register worker: %w: session %s has %d workers, capacity %d",
			store.ErrSessionCapacityFull, sessionID, count, maxCapacity)
	}

	worker.SessionID = sessionID
	worker.ProjectID = projectID
	if worker.Status == "" {
		worker.Status = "idle"
	}
	worker.LastActive = time.Now().UTC().Format("2006-01-02T15:04:05Z")

	if err := s.workerStore.Create(ctx, projectID, sessionID, worker); err != nil {
		return fmt.Errorf("register worker: %w", err)
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

// ReleaseWorker releases a single worker from its current task assignment.
// Unlike DisconnectSession which kills the entire session and all its workers,
// this only clears the specified worker's current_task and optionally deletes it.
func (s *SessionService) ReleaseWorker(ctx context.Context, projectID, sessionID, workerID string) error {
	// Verify the worker exists
	worker, err := s.workerStore.GetByID(ctx, projectID, sessionID, workerID)
	if err != nil {
		return fmt.Errorf("release worker: %w", err)
	}

	// Clear the worker's current task assignment
	if worker.CurrentTaskID != nil && *worker.CurrentTaskID != "" {
		if err := s.workerStore.UpdateCurrentTask(ctx, projectID, sessionID, workerID, ""); err != nil {
			return fmt.Errorf("release worker clear task: %w", err)
		}
	}

	// Delete the worker
	if err := s.workerStore.Delete(ctx, projectID, sessionID, workerID); err != nil {
		return fmt.Errorf("release worker delete: %w", err)
	}

	return nil
}

// StartStaleSessionScanner launches a background goroutine that periodically
// scans for stale sessions and cleans them up. It runs until ctx is cancelled.
func (s *SessionService) StartStaleSessionScanner(ctx context.Context, interval time.Duration, timeoutSec int) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stale, err := s.FindStaleSessions(ctx, timeoutSec)
				if err != nil {
					slog.Error("stale session scan error", "error", err)
					continue
				}
				for _, sess := range stale {
					if err := s.CleanupStaleSession(ctx, sess); err != nil {
						slog.Error("stale session cleanup error", "session_id", sess.ID, "error", err)
					}
				}
			}
		}
	}()
}

// logAudit writes a security audit entry for a session-level operation.
func (s *SessionService) logAudit(ctx context.Context, boundProject, action, result string, sessionID, taskID *string) {
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

// ForceRelease forcefully releases a session — marks it offline, cleans up
// all workers and releases their tasks. This is an admin operation.
func (s *SessionService) ForceRelease(ctx context.Context, projectID, sessionID string) error {
	sess, err := s.sessionStore.GetByID(ctx, projectID, sessionID)
	if err != nil {
		return fmt.Errorf("force release: %w", err)
	}

	// Reuse the stale session cleanup logic.
	if err := s.CleanupStaleSession(ctx, sess); err != nil {
		return fmt.Errorf("force release cleanup: %w", err)
	}

	s.logAudit(ctx, projectID, "session.force_release", "ALLOWED", &sessionID, nil)
	return nil
}
