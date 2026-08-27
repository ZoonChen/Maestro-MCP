package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// WorktreeService manages git worktree lifecycle and durable cleanup intents.
type WorktreeService struct {
	worktreeStore store.WorktreeStore
	projectStore  store.ProjectStore
	db            *sql.DB
}

// NewWorktreeService requires the trusted Project root and DB transaction
// boundary needed by safe GC. Cleanup fails closed if they are unavailable.
func NewWorktreeService(worktreeStore store.WorktreeStore, projectStore store.ProjectStore, db *sql.DB) *WorktreeService {
	return &WorktreeService{worktreeStore: worktreeStore, projectStore: projectStore, db: db}
}

func (s *WorktreeService) CreateWorktree(ctx context.Context, projectID string, w *model.Worktree) (int64, error) {
	id, err := s.worktreeStore.Create(ctx, projectID, w)
	if err != nil {
		return 0, fmt.Errorf("create worktree: %w", err)
	}
	return id, nil
}

func (s *WorktreeService) GetWorktreeByTask(ctx context.Context, projectID, taskID string) (*model.Worktree, error) {
	w, err := s.worktreeStore.GetByTaskID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("get worktree by task %s: %w", taskID, err)
	}
	return w, nil
}

func (s *WorktreeService) UpdateWorktreeStatus(ctx context.Context, projectID string, id int64, status string) error {
	if s == nil || s.db == nil || !model.IsWorktreeStatus(status) {
		return fmt.Errorf("update worktree status: invalid runtime or status: %w", store.ErrInvalidParameter)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("update worktree status: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var currentStatus string
	var version, generation int64
	if err := tx.QueryRowContext(ctx, `SELECT status, version, generation FROM worktrees
		WHERE project_id = ? AND id = ?`, projectID, id).Scan(&currentStatus, &version, &generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("update worktree status: %w", store.ErrWorktreeNotFound)
		}
		return fmt.Errorf("update worktree status: snapshot: %w", err)
	}
	if currentStatus == status {
		return nil
	}
	if !model.CanWorktreeTransition(currentStatus, status) {
		return fmt.Errorf("update worktree status: %s -> %s: %w", currentStatus, status, store.ErrTaskStateInvalid)
	}
	result, err := tx.ExecContext(ctx, `UPDATE worktrees SET status = ?, version = version + 1,
		updated_at = datetime('now') WHERE project_id = ? AND id = ? AND status = ?
		AND version = ? AND generation = ?`, status, projectID, id, currentStatus, version, generation)
	if err != nil {
		return fmt.Errorf("update worktree status: update: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("update worktree status: CAS: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	causationID := fmt.Sprintf("worktree.update:%d:v%d", id, version)
	if err := appendStateHistory(ctx, tx, projectID, "worktree", fmt.Sprint(id),
		currentStatus, status, version, version+1, "system", "worktree status updated", causationID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update worktree status: commit: %w", err)
	}
	return nil
}

func (s *WorktreeService) ListWorktreesByStatus(ctx context.Context, projectID, status string) ([]*model.Worktree, error) {
	worktrees, err := s.worktreeStore.ListByStatus(ctx, projectID, status)
	if err != nil {
		return nil, fmt.Errorf("list worktrees by status %s: %w", status, err)
	}
	return worktrees, nil
}

// DeleteWorktree is intentionally disabled. Deleting only the durable row can
// orphan a physical Git worktree and bypass the Lease/state/version checks in
// GCWorktrees and CleanupPendingWorktree.
func (s *WorktreeService) DeleteWorktree(_ context.Context, _ string, _ int64) error {
	return fmt.Errorf("delete worktree record directly: %w", store.ErrOperationDisabled)
}

// CleanupPendingWorktree executes the cleanup intent for one exact Task. It is
// used after a context-delivery rejection so an unrelated bad GC candidate in
// the same Project cannot prevent release of the newly claimed Worktree.
func (s *WorktreeService) CleanupPendingWorktree(ctx context.Context, projectID, taskID string) error {
	if s == nil || s.worktreeStore == nil || s.projectStore == nil || s.db == nil {
		return fmt.Errorf("cleanup pending worktree: runtime dependencies unavailable: %w", store.ErrRecoveryIntegrity)
	}
	if projectID == "" || !taskIDRe.MatchString(taskID) {
		return fmt.Errorf("cleanup pending worktree: invalid identity: %w", store.ErrInvalidParameter)
	}
	worktree, err := s.worktreeStore.GetByTaskID(ctx, projectID, taskID)
	if err != nil {
		return fmt.Errorf("cleanup pending worktree: candidate: %w", err)
	}
	if worktree.Status != model.WorktreeStatusCleanupPending {
		return fmt.Errorf("cleanup pending worktree: candidate is %s: %w", worktree.Status, store.ErrTaskStateInvalid)
	}
	project, err := s.projectStore.GetByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("cleanup pending worktree: project: %w", err)
	}
	workspace, err := canonicalExistingDir(project.WorkspacePath)
	if err != nil {
		return fmt.Errorf("cleanup pending worktree: trusted workspace invalid: %w", err)
	}
	if _, err := getBaseCommit(ctx, workspace); err != nil {
		return fmt.Errorf("cleanup pending worktree: trusted workspace is not a valid repository: %w", err)
	}
	if err := s.cleanupWorktree(ctx, projectID, workspace, worktree); err != nil {
		return fmt.Errorf("cleanup pending worktree: %w", err)
	}
	return nil
}

// GCWorktrees executes cleanup intents idempotently. It only removes a path
// when the DB candidate has no active Lease, the Task is in a safe state, the
// stored path is exactly the registered task path below the trusted Project
// workspace, and the version still matches when the result is committed.
// Quarantined workspaces are evidence and are intentionally never collected.
func (s *WorktreeService) GCWorktrees(ctx context.Context, projectID string) error {
	if s == nil || s.worktreeStore == nil || s.projectStore == nil || s.db == nil {
		return fmt.Errorf("gc worktrees: runtime dependencies unavailable: %w", store.ErrRecoveryIntegrity)
	}
	project, err := s.projectStore.GetByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("gc worktrees: project: %w", err)
	}
	workspace, err := canonicalExistingDir(project.WorkspacePath)
	if err != nil {
		return fmt.Errorf("gc worktrees: trusted workspace invalid: %w", err)
	}
	if _, err := getBaseCommit(ctx, workspace); err != nil {
		return fmt.Errorf("gc worktrees: trusted workspace is not a valid repository: %w", err)
	}

	var cleanupErrors []error
	// cleanup_pending is the explicit crash-safe intent. abandoned/merged are
	// legacy terminal records and go through the same proof before retirement.
	for _, status := range []string{
		model.WorktreeStatusCleanupPending,
		model.WorktreeStatusAbandoned,
		model.WorktreeStatusMerged,
	} {
		worktrees, listErr := s.worktreeStore.ListByStatus(ctx, projectID, status)
		if listErr != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("gc worktrees list %s: %w", status, listErr))
			continue
		}
		for _, wt := range worktrees {
			if cleanupErr := s.cleanupWorktree(ctx, projectID, workspace, wt); cleanupErr != nil {
				s.recordWorktreeGCDenied(ctx, projectID, wt)
				cleanupErrors = append(cleanupErrors, fmt.Errorf("gc worktree %d: %w", wt.ID, cleanupErr))
			}
		}
	}
	return errors.Join(cleanupErrors...)
}

type worktreeGCSnapshot struct {
	status          string
	version         int64
	taskStatus      string
	activeLeaseID   sql.NullString
	activeLeaseRows int
}

func (s *WorktreeService) cleanupWorktree(ctx context.Context, projectID, workspace string, wt *model.Worktree) error {
	if wt == nil || wt.ProjectID != projectID || wt.ID <= 0 || !taskIDRe.MatchString(wt.TaskID) {
		return fmt.Errorf("candidate identity invalid: %w", store.ErrRecoveryIntegrity)
	}
	snapshot, err := s.readWorktreeGCSnapshot(ctx, projectID, wt.ID)
	if err != nil {
		return err
	}
	if snapshot.status != wt.Status || snapshot.version != wt.Version {
		return store.ErrConcurrentConflict
	}
	if snapshot.activeLeaseID.Valid || snapshot.activeLeaseRows != 0 {
		return fmt.Errorf("candidate has active lease: %w", store.ErrConcurrentConflict)
	}
	if !worktreeGCStateAllowed(snapshot.status, snapshot.taskStatus) {
		return fmt.Errorf("task %s is not safe for %s cleanup: %w", snapshot.taskStatus, snapshot.status, store.ErrTaskStateInvalid)
	}

	expectedBranch := "task/" + wt.TaskID
	if wt.BranchName != expectedBranch {
		return fmt.Errorf("branch identity mismatch: %w", store.ErrRecoveryIntegrity)
	}
	expectedPath := filepath.Join(workspace, ".maestro", "worktrees", wt.TaskID)
	storedAbs, err := filepath.Abs(wt.WorktreePath)
	if err != nil || strings.ContainsRune(wt.WorktreePath, '\x00') || filepath.Clean(storedAbs) != expectedPath {
		return fmt.Errorf("path identity mismatch: %w", store.ErrRecoveryIntegrity)
	}

	pathExists := true
	canonicalPath, err := canonicalExistingDir(wt.WorktreePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect worktree path: %w", err)
		}
		pathExists = false
	} else if canonicalPath != expectedPath {
		return fmt.Errorf("canonical path identity mismatch: %w", store.ErrRecoveryIntegrity)
	}
	registered, err := registeredGitWorktree(ctx, workspace, expectedPath)
	if err != nil {
		return err
	}
	if pathExists && !registered {
		return fmt.Errorf("directory is not a registered worktree: %w", store.ErrRecoveryIntegrity)
	}
	if !pathExists && registered {
		return fmt.Errorf("registered worktree path is missing; reconciliation required: %w", store.ErrRecoveryIntegrity)
	}
	if pathExists {
		if err := removeWorktree(ctx, workspace, expectedPath); err != nil {
			return err
		}
	}
	if err := deleteBranchIfExists(ctx, workspace, expectedBranch); err != nil {
		return err
	}

	// Re-read authority after filesystem work. A concurrent claim observes the
	// cleanup_pending record and compensates; the transaction refuses to retire
	// a record if a new active Lease appeared in the meantime.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("commit cleanup begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var current worktreeGCSnapshot
	if err := tx.QueryRowContext(ctx, `SELECT w.status, w.version, t.status, t.active_lease_id,
		(SELECT COUNT(*) FROM task_leases AS l
		 WHERE l.project_id = w.project_id AND l.task_id = w.task_id AND l.status = 'active')
		FROM worktrees AS w JOIN tasks AS t
		  ON t.project_id = w.project_id AND t.id = w.task_id
		WHERE w.project_id = ? AND w.id = ?`, projectID, wt.ID).Scan(
		&current.status, &current.version, &current.taskStatus,
		&current.activeLeaseID, &current.activeLeaseRows,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // A prior successful idempotent attempt already retired it.
		}
		return fmt.Errorf("commit cleanup snapshot: %w", err)
	}
	if current.status != snapshot.status || current.version != snapshot.version ||
		current.activeLeaseID.Valid || current.activeLeaseRows != 0 ||
		!worktreeGCStateAllowed(current.status, current.taskStatus) {
		return store.ErrConcurrentConflict
	}

	finalStatus := current.status
	finalVersion := current.version
	if current.status == model.WorktreeStatusCleanupPending {
		result, err := tx.ExecContext(ctx, `UPDATE worktrees
			SET status = 'abandoned', version = version + 1, updated_at = datetime('now')
			WHERE project_id = ? AND id = ? AND status = 'cleanup_pending' AND version = ?`,
			projectID, wt.ID, current.version)
		if err != nil {
			return fmt.Errorf("commit cleanup transition: %w", err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("commit cleanup transition CAS: %w", errors.Join(store.ErrConcurrentConflict, err))
		}
		finalStatus = model.WorktreeStatusAbandoned
		finalVersion++
		if err := appendStateHistory(ctx, tx, projectID, "worktree", fmt.Sprint(wt.ID),
			model.WorktreeStatusCleanupPending, finalStatus, current.version, finalVersion,
			"system", "physical workspace cleanup verified", fmt.Sprintf("gc:%d:%d", wt.ID, current.version)); err != nil {
			return err
		}
	}
	detail, err := json.Marshal(map[string]any{
		"worktree_id": wt.ID, "task_id": wt.TaskID, "generation": wt.Generation,
		"from_status": current.status, "final_status": finalStatus, "version": finalVersion,
	})
	if err != nil {
		return fmt.Errorf("commit cleanup audit encoding: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log
		(bound_project, target_project, target_task, action, result, detail, created_at)
		VALUES (?, ?, ?, 'worktree.gc', 'ALLOWED', ?, datetime('now'))`,
		projectID, projectID, wt.TaskID, string(detail)); err != nil {
		return fmt.Errorf("commit cleanup audit: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM worktrees
		WHERE project_id = ? AND id = ? AND status = ? AND version = ?`,
		projectID, wt.ID, finalStatus, finalVersion)
	if err != nil {
		return fmt.Errorf("commit cleanup retire record: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("commit cleanup retire CAS: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cleanup: %w", err)
	}
	return nil
}

func (s *WorktreeService) readWorktreeGCSnapshot(ctx context.Context, projectID string, id int64) (worktreeGCSnapshot, error) {
	var snapshot worktreeGCSnapshot
	err := s.db.QueryRowContext(ctx, `SELECT w.status, w.version, t.status, t.active_lease_id,
		(SELECT COUNT(*) FROM task_leases AS l
		 WHERE l.project_id = w.project_id AND l.task_id = w.task_id AND l.status = 'active')
		FROM worktrees AS w JOIN tasks AS t
		  ON t.project_id = w.project_id AND t.id = w.task_id
		WHERE w.project_id = ? AND w.id = ?`, projectID, id).Scan(
		&snapshot.status, &snapshot.version, &snapshot.taskStatus,
		&snapshot.activeLeaseID, &snapshot.activeLeaseRows)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, store.ErrWorktreeNotFound
	}
	if err != nil {
		return snapshot, fmt.Errorf("read cleanup candidate: %w", err)
	}
	return snapshot, nil
}

func worktreeGCStateAllowed(worktreeStatus, taskStatus string) bool {
	switch worktreeStatus {
	case model.WorktreeStatusCleanupPending, model.WorktreeStatusAbandoned:
		switch taskStatus {
		case model.TaskStatusQueued, model.TaskStatusCancelled, model.TaskStatusFailed,
			model.TaskStatusNeedsHuman, model.TaskStatusDone:
			return true
		}
	case model.WorktreeStatusMerged:
		return taskStatus == model.TaskStatusDone
	}
	return false
}

func registeredGitWorktree(ctx context.Context, workspace, expectedPath string) (bool, error) {
	out, err := runGitOutput(ctx, workspace, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, fmt.Errorf("list registered worktrees: %w", err)
	}
	for _, field := range strings.Split(string(out), "\x00") {
		if !strings.HasPrefix(field, "worktree ") {
			continue
		}
		listedAbs, err := filepath.Abs(strings.TrimPrefix(field, "worktree "))
		if err != nil {
			return false, fmt.Errorf("registered worktree path invalid: %w", err)
		}
		if filepath.Clean(listedAbs) == expectedPath {
			return true, nil
		}
	}
	return false, nil
}

func deleteBranchIfExists(ctx context.Context, workspace, branchName string) error {
	if !strings.HasPrefix(branchName, "task/") || !taskIDRe.MatchString(strings.TrimPrefix(branchName, "task/")) {
		return fmt.Errorf("delete branch: unsafe branch identity: %w", store.ErrRecoveryIntegrity)
	}
	ref := "refs/heads/" + branchName
	out, err := runGitOutput(ctx, workspace, "for-each-ref", "--format=%(refname)", ref)
	if err != nil {
		return fmt.Errorf("inspect cleanup branch: %w", err)
	}
	listed := strings.TrimSpace(string(out))
	if listed == "" {
		return nil
	}
	if listed != ref {
		return fmt.Errorf("cleanup branch identity mismatch: %w", store.ErrRecoveryIntegrity)
	}
	return deleteBranch(ctx, workspace, branchName)
}

func (s *WorktreeService) recordWorktreeGCDenied(ctx context.Context, projectID string, wt *model.Worktree) {
	if s == nil || s.db == nil || wt == nil {
		return
	}
	detail, err := json.Marshal(map[string]any{
		"worktree_id": wt.ID, "task_id": wt.TaskID, "generation": wt.Generation,
		"error_code": "WORKSPACE_GC_FAILED",
	})
	if err != nil {
		return
	}
	_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO audit_log
		(bound_project, target_project, target_task, action, result, detail, created_at)
		VALUES (?, ?, ?, 'worktree.gc', 'DENIED', ?, datetime('now'))`,
		projectID, projectID, wt.TaskID, string(detail))
}
