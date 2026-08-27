package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorktreeGCCompletesCleanupPendingWithHistoryAndAudit(t *testing.T) {
	svc, gc, task, wt := newWorktreeGCFixture(t, model.TaskStatusQueued, model.WorktreeStatusCleanupPending)
	ctx := context.Background()

	require.NoError(t, gc.GCWorktrees(ctx, testProjectID))
	_, err := os.Stat(wt.WorktreePath)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
	require.ErrorIs(t, err, store.ErrWorktreeNotFound)

	var history, allowed int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND aggregate_type = 'worktree' AND aggregate_id = ?
		  AND from_status = 'cleanup_pending' AND to_status = 'abandoned'`,
		testProjectID, wtIDString(wt.ID)).Scan(&history))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log
		WHERE bound_project = ? AND target_task = ? AND action = 'worktree.gc' AND result = 'ALLOWED'`,
		testProjectID, task.ID).Scan(&allowed))
	assert.Equal(t, 1, history)
	assert.Equal(t, 1, allowed)

	// A repeated background scan observes no intent and has no second effect.
	require.NoError(t, gc.GCWorktrees(ctx, testProjectID))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log
		WHERE bound_project = ? AND target_task = ? AND action = 'worktree.gc' AND result = 'ALLOWED'`,
		testProjectID, task.ID).Scan(&allowed))
	assert.Equal(t, 1, allowed)
}

func TestWorktreeGCReconcilesPhysicalCleanupCommittedBeforeDatabase(t *testing.T) {
	svc, gc, task, wt := newWorktreeGCFixture(t, model.TaskStatusQueued, model.WorktreeStatusCleanupPending)
	ctx := context.Background()
	project, err := svc.stores.projectStore.GetByID(ctx, testProjectID)
	require.NoError(t, err)
	require.NoError(t, removeWorktree(ctx, project.WorkspacePath, wt.WorktreePath))
	require.NoError(t, deleteBranch(ctx, project.WorkspacePath, wt.BranchName))

	require.NoError(t, gc.GCWorktrees(ctx, testProjectID))
	_, err = svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
	require.ErrorIs(t, err, store.ErrWorktreeNotFound)
}

func TestWorktreeGCRejectsActiveLeaseAndPathEscape(t *testing.T) {
	t.Run("active lease", func(t *testing.T) {
		svc := setupTestEnv(t)
		ctx := context.Background()
		workspace, base := createTestGitRepository(t)
		_, err := svc.stores.db.ExecContext(ctx, `UPDATE projects SET workspace_path = ? WHERE id = ?`,
			workspace, testProjectID)
		require.NoError(t, err)
		seedTestSession(t, svc.stores, "gc-owner")
		task := newTestTask("T-gc-active")
		seedTaskWithActiveLease(t, svc.stores, task, "gc-owner", "gc-worker")
		path, err := createWorktree(ctx, workspace, task.ID)
		require.NoError(t, err)
		wt := createGCWorktreeRecord(t, svc, task.ID, path, base, model.WorktreeStatusCleanupPending)
		gc := NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db)

		err = gc.GCWorktrees(ctx, testProjectID)
		require.Error(t, err)
		assert.ErrorIs(t, err, store.ErrConcurrentConflict)
		_, statErr := os.Stat(path)
		require.NoError(t, statErr)
		preserved, getErr := svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
		require.NoError(t, getErr)
		assert.Equal(t, wt.ID, preserved.ID)
		assert.Equal(t, model.WorktreeStatusCleanupPending, preserved.Status)
		assertWorktreeGCDenied(t, svc, task.ID)
	})

	t.Run("path outside trusted root", func(t *testing.T) {
		svc := setupTestEnv(t)
		ctx := context.Background()
		workspace, base := createTestGitRepository(t)
		_, err := svc.stores.db.ExecContext(ctx, `UPDATE projects SET workspace_path = ? WHERE id = ?`,
			workspace, testProjectID)
		require.NoError(t, err)
		task := newTestTask("T-gc-escape")
		mustCreateTask(t, svc.stores.taskStore, task)
		outside := t.TempDir()
		createGCWorktreeRecord(t, svc, task.ID, outside, base, model.WorktreeStatusCleanupPending)
		gc := NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db)

		err = gc.GCWorktrees(ctx, testProjectID)
		require.Error(t, err)
		assert.ErrorIs(t, err, store.ErrRecoveryIntegrity)
		_, statErr := os.Stat(outside)
		require.NoError(t, statErr, "GC must never remove a directory outside the reserved root")
		preserved, getErr := svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
		require.NoError(t, getErr)
		assert.Equal(t, model.WorktreeStatusCleanupPending, preserved.Status)
		assertWorktreeGCDenied(t, svc, task.ID)
	})
}

func TestWorktreeGCPreservesQuarantineEvidence(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	workspace, base := createTestGitRepository(t)
	_, err := svc.stores.db.ExecContext(ctx, `UPDATE projects SET workspace_path = ? WHERE id = ?`,
		workspace, testProjectID)
	require.NoError(t, err)
	task := newTestTask("T-gc-quarantine")
	mustCreateTask(t, svc.stores.taskStore, task)
	path, err := createWorktree(ctx, workspace, task.ID)
	require.NoError(t, err)
	createGCWorktreeRecord(t, svc, task.ID, path, base, model.WorktreeStatusQuarantined)
	gc := NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db)

	require.NoError(t, gc.GCWorktrees(ctx, testProjectID))
	_, err = os.Stat(path)
	require.NoError(t, err)
	preserved, err := svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.WorktreeStatusQuarantined, preserved.Status)
}

func TestWorktreeGCRejectsMissingRuntimeAndInvalidTrustedWorkspace(t *testing.T) {
	ctx := context.Background()
	require.ErrorIs(t, NewWorktreeService(nil, nil, nil).GCWorktrees(ctx, testProjectID), store.ErrRecoveryIntegrity)

	svc := setupTestEnv(t)
	gc := NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db)
	require.ErrorIs(t, gc.GCWorktrees(ctx, "missing-project"), store.ErrProjectNotFound)
	_, err := svc.stores.db.ExecContext(ctx, `UPDATE projects SET workspace_path = ? WHERE id = ?`,
		"/definitely/missing/maestro-workspace", testProjectID)
	require.NoError(t, err)
	require.Error(t, gc.GCWorktrees(ctx, testProjectID))

	notGit := t.TempDir()
	_, err = svc.stores.db.ExecContext(ctx, `UPDATE projects SET workspace_path = ? WHERE id = ?`, notGit, testProjectID)
	require.NoError(t, err)
	require.Error(t, gc.GCWorktrees(ctx, testProjectID))
}

func TestWorktreeGCRejectsUnsafeTaskBranchAndUnregisteredDirectory(t *testing.T) {
	t.Run("task state", func(t *testing.T) {
		_, gc, _, wt := newWorktreeGCFixture(t, model.TaskStatusValidating, model.WorktreeStatusCleanupPending)
		err := gc.GCWorktrees(context.Background(), testProjectID)
		require.ErrorIs(t, err, store.ErrTaskStateInvalid)
		_, statErr := os.Stat(wt.WorktreePath)
		require.NoError(t, statErr)
	})

	t.Run("branch identity", func(t *testing.T) {
		svc, gc, _, wt := newWorktreeGCFixture(t, model.TaskStatusQueued, model.WorktreeStatusCleanupPending)
		_, err := svc.stores.db.Exec(`UPDATE worktrees SET branch_name = 'task/forged'
			WHERE project_id = ? AND id = ?`, testProjectID, wt.ID)
		require.NoError(t, err)
		wt.BranchName = "task/forged"
		err = gc.GCWorktrees(context.Background(), testProjectID)
		require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
		_, statErr := os.Stat(wt.WorktreePath)
		require.NoError(t, statErr)
	})

	t.Run("unregistered directory", func(t *testing.T) {
		svc := setupTestEnv(t)
		ctx := context.Background()
		workspace, base := createTestGitRepository(t)
		_, err := svc.stores.db.ExecContext(ctx, `UPDATE projects SET workspace_path = ? WHERE id = ?`, workspace, testProjectID)
		require.NoError(t, err)
		task := newTestTask("T-gc-unregistered")
		mustCreateTask(t, svc.stores.taskStore, task)
		path := filepath.Join(workspace, ".maestro", "worktrees", task.ID)
		require.NoError(t, os.MkdirAll(path, 0o700))
		createGCWorktreeRecord(t, svc, task.ID, path, base, model.WorktreeStatusCleanupPending)
		gc := NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db)
		err = gc.GCWorktrees(ctx, testProjectID)
		require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
		_, statErr := os.Stat(path)
		require.NoError(t, statErr)
	})
}

func TestWorktreeGCCrashPointsRemainRetryable(t *testing.T) {
	tests := []struct {
		name, trigger string
	}{
		{
			name: "transition",
			trigger: `CREATE TRIGGER test_gc_failure BEFORE UPDATE OF status ON worktrees
				WHEN NEW.status = 'abandoned' BEGIN SELECT RAISE(ABORT, 'GC_TRANSITION_FAILURE'); END`,
		},
		{
			name: "state history",
			trigger: `CREATE TRIGGER test_gc_failure BEFORE INSERT ON state_history
				WHEN NEW.aggregate_type = 'worktree'
				BEGIN SELECT RAISE(ABORT, 'GC_HISTORY_FAILURE'); END`,
		},
		{
			name: "audit",
			trigger: `CREATE TRIGGER test_gc_failure BEFORE INSERT ON audit_log
				WHEN NEW.action = 'worktree.gc' AND NEW.result = 'ALLOWED'
				BEGIN SELECT RAISE(ABORT, 'GC_AUDIT_FAILURE'); END`,
		},
		{
			name: "record retirement",
			trigger: `CREATE TRIGGER test_gc_failure BEFORE DELETE ON worktrees
				BEGIN SELECT RAISE(ABORT, 'GC_DELETE_FAILURE'); END`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, gc, task, wt := newWorktreeGCFixture(t, model.TaskStatusQueued, model.WorktreeStatusCleanupPending)
			_, err := svc.stores.db.Exec(tc.trigger)
			require.NoError(t, err)
			err = gc.GCWorktrees(context.Background(), testProjectID)
			require.Error(t, err)
			preserved, getErr := svc.stores.worktreeStore.GetByTaskID(context.Background(), testProjectID, task.ID)
			require.NoError(t, getErr)
			assert.Equal(t, wt.ID, preserved.ID)
			assert.Equal(t, model.WorktreeStatusCleanupPending, preserved.Status,
				"the DB result transaction must roll back even after physical cleanup")

			_, err = svc.stores.db.Exec(`DROP TRIGGER test_gc_failure`)
			require.NoError(t, err)
			require.NoError(t, gc.GCWorktrees(context.Background(), testProjectID),
				"missing physical path/branch after a crash must reconcile idempotently")
			_, err = svc.stores.worktreeStore.GetByTaskID(context.Background(), testProjectID, task.ID)
			require.ErrorIs(t, err, store.ErrWorktreeNotFound)
		})
	}
}

func TestCleanupWorktreeRejectsInvalidMissingAndStaleCandidates(t *testing.T) {
	svc, gc, _, wt := newWorktreeGCFixture(t, model.TaskStatusQueued, model.WorktreeStatusCleanupPending)
	project, err := svc.stores.projectStore.GetByID(context.Background(), testProjectID)
	require.NoError(t, err)
	workspace, err := canonicalExistingDir(project.WorkspacePath)
	require.NoError(t, err)

	invalid := *wt
	invalid.ID = 0
	require.ErrorIs(t, gc.cleanupWorktree(context.Background(), testProjectID, workspace, &invalid), store.ErrRecoveryIntegrity)
	missing := *wt
	missing.ID = wt.ID + 10_000
	require.ErrorIs(t, gc.cleanupWorktree(context.Background(), testProjectID, workspace, &missing), store.ErrWorktreeNotFound)
	stale := *wt
	stale.Version++
	require.ErrorIs(t, gc.cleanupWorktree(context.Background(), testProjectID, workspace, &stale), store.ErrConcurrentConflict)
}

func TestWorktreeGCRejectsSymlinkAtExactRegisteredPath(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	workspace, base := createTestGitRepository(t)
	_, err := svc.stores.db.ExecContext(ctx, `UPDATE projects SET workspace_path = ? WHERE id = ?`, workspace, testProjectID)
	require.NoError(t, err)
	task := newTestTask("T-gc-symlink")
	mustCreateTask(t, svc.stores.taskStore, task)
	canonicalWorkspace, err := canonicalExistingDir(workspace)
	require.NoError(t, err)
	expected := filepath.Join(canonicalWorkspace, ".maestro", "worktrees", task.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(expected), 0o700))
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, expected))
	createGCWorktreeRecord(t, svc, task.ID, expected, base, model.WorktreeStatusCleanupPending)
	gc := NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db)

	err = gc.GCWorktrees(ctx, testProjectID)
	require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
	_, err = os.Stat(outside)
	require.NoError(t, err)
}

func TestWorktreeGCRejectsNonDirectoryAtExactPath(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	workspace, base := createTestGitRepository(t)
	_, err := svc.stores.db.ExecContext(ctx, `UPDATE projects SET workspace_path = ? WHERE id = ?`, workspace, testProjectID)
	require.NoError(t, err)
	task := newTestTask("T-gc-file")
	mustCreateTask(t, svc.stores.taskStore, task)
	canonicalWorkspace, err := canonicalExistingDir(workspace)
	require.NoError(t, err)
	expected := filepath.Join(canonicalWorkspace, ".maestro", "worktrees", task.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(expected), 0o700))
	require.NoError(t, os.WriteFile(expected, []byte("do not delete"), 0o600))
	createGCWorktreeRecord(t, svc, task.ID, expected, base, model.WorktreeStatusCleanupPending)
	gc := NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db)

	err = gc.GCWorktrees(ctx, testProjectID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inspect worktree path")
	content, err := os.ReadFile(expected)
	require.NoError(t, err)
	assert.Equal(t, "do not delete", string(content))
}

func TestWorktreeGCCleansLegacyTerminalRecordsThroughSameSafetyProof(t *testing.T) {
	for _, tc := range []struct {
		name, taskStatus, worktreeStatus string
	}{
		{name: "abandoned", taskStatus: model.TaskStatusCancelled, worktreeStatus: model.WorktreeStatusAbandoned},
		{name: "merged", taskStatus: model.TaskStatusDone, worktreeStatus: model.WorktreeStatusMerged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, gc, task, _ := newWorktreeGCFixture(t, tc.taskStatus, tc.worktreeStatus)
			require.NoError(t, gc.GCWorktrees(context.Background(), testProjectID))
			_, err := svc.stores.worktreeStore.GetByTaskID(context.Background(), testProjectID, task.ID)
			require.ErrorIs(t, err, store.ErrWorktreeNotFound)
		})
	}
}

func TestWorktreeServiceCRUDDelegatesScopedStoreRules(t *testing.T) {
	svc := setupTestEnv(t)
	service := NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db)
	task := newTestTask("T-worktree-crud")
	mustCreateTask(t, svc.stores.taskStore, task)
	now := time.Now().UTC().Format(time.RFC3339)
	wt := &model.Worktree{
		TaskID: task.ID, ProjectID: testProjectID, WorktreePath: "/tmp/worktree-crud",
		BranchName: "task/" + task.ID, BaseCommit: strings.Repeat("a", 40),
		Status: model.WorktreeStatusAllocated, Generation: 1, CreatedAt: now, UpdatedAt: now,
	}
	id, err := service.CreateWorktree(context.Background(), testProjectID, wt)
	require.NoError(t, err)
	wt.ID = id
	read, err := service.GetWorktreeByTask(context.Background(), testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, id, read.ID)
	require.NoError(t, service.UpdateWorktreeStatus(context.Background(), testProjectID, id, model.WorktreeStatusActive))
	listed, err := service.ListWorktreesByStatus(context.Background(), testProjectID, model.WorktreeStatusActive)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.ErrorIs(t, service.DeleteWorktree(context.Background(), testProjectID, id), store.ErrOperationDisabled)
	read, err = service.GetWorktreeByTask(context.Background(), testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, id, read.ID, "disabled direct deletion must preserve the durable cleanup intent")
}

func newWorktreeGCFixture(t *testing.T, taskStatus, worktreeStatus string) (*testServices, *WorktreeService, *model.Task, *model.Worktree) {
	t.Helper()
	svc := setupTestEnv(t)
	ctx := context.Background()
	workspace, base := createTestGitRepository(t)
	_, err := svc.stores.db.ExecContext(ctx, `UPDATE projects SET workspace_path = ? WHERE id = ?`,
		workspace, testProjectID)
	require.NoError(t, err)
	task := newTestTask("T-gc-safe")
	task.Status = taskStatus
	if taskStatus == model.TaskStatusDone {
		mustSeedHistoricalDoneTask(t, svc.stores, task)
	} else {
		mustCreateTask(t, svc.stores.taskStore, task)
	}
	path, err := createWorktree(ctx, workspace, task.ID)
	require.NoError(t, err)
	wt := createGCWorktreeRecord(t, svc, task.ID, path, base, worktreeStatus)
	return svc, NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db), task, wt
}

func createGCWorktreeRecord(t *testing.T, svc *testServices, taskID, path, base, status string) *model.Worktree {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	wt := &model.Worktree{
		TaskID: taskID, ProjectID: testProjectID, WorktreePath: path,
		BranchName: "task/" + taskID, BaseCommit: base, Status: status,
		Generation: 1, Version: 0, CreatedAt: now, UpdatedAt: now,
	}
	id, err := svc.stores.worktreeStore.Create(context.Background(), testProjectID, wt)
	require.NoError(t, err)
	wt.ID = id
	return wt
}

func assertWorktreeGCDenied(t *testing.T, svc *testServices, taskID string) {
	t.Helper()
	var denied int
	require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM audit_log
		WHERE bound_project = ? AND target_task = ? AND action = 'worktree.gc' AND result = 'DENIED'`,
		testProjectID, taskID).Scan(&denied))
	assert.Equal(t, 1, denied)
}

func wtIDString(id int64) string {
	return fmt.Sprint(id)
}
