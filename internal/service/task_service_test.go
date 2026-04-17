package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CreateTask
// ---------------------------------------------------------------------------

func TestCreateTask_HappyPath(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-00001")
	err := svc.taskSvc.CreateTask(ctx, testProjectID, task)
	require.NoError(t, err)

	// Verify the task was persisted.
	got, err := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-00001")
	require.NoError(t, err)
	assert.Equal(t, "T-00001", got.ID)
	assert.Equal(t, "Test Task T-00001", got.Title)
	assert.Equal(t, model.RoleBackend, got.Role)
	assert.Equal(t, model.TaskStatusPending, got.Status)
	assert.Equal(t, model.PriorityNormal, got.Priority)
	assert.Equal(t, testFeatureID, got.FeatureID)
}

func TestCreateTask_SetsDefaults(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := &model.Task{
		ID:                 "T-defaults",
		FeatureID:          testFeatureID,
		Title:              "Defaults Test",
		Description:        "Desc",
		Role:               model.RoleFrontend,
		AllowedDirectories: `["src/"]`,
	}
	err := svc.taskSvc.CreateTask(ctx, testProjectID, task)
	require.NoError(t, err)

	got, err := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-defaults")
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusPending, got.Status, "status should default to pending")
	assert.Equal(t, model.PriorityNormal, got.Priority, "priority should default to normal")
	assert.NotEmpty(t, got.CreatedAt, "created_at should be set")
	assert.NotEmpty(t, got.UpdatedAt, "updated_at should be set")
}

func TestCreateTask_EmptyTitle(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-notitle")
	task.Title = ""
	err := svc.taskSvc.CreateTask(ctx, testProjectID, task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidParameter))
}

func TestCreateTask_EmptyDescription(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-nodesc")
	task.Description = ""
	err := svc.taskSvc.CreateTask(ctx, testProjectID, task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidParameter))
}

func TestCreateTask_InvalidRole(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-badrole")
	task.Role = "wizard"
	err := svc.taskSvc.CreateTask(ctx, testProjectID, task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidParameter))
}

func TestCreateTask_MissingFeatureID(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-nofeat")
	task.FeatureID = ""
	err := svc.taskSvc.CreateTask(ctx, testProjectID, task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidParameter))
}

func TestCreateTask_MissingAllowedDirectories(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-nodirs")
	task.AllowedDirectories = ""
	err := svc.taskSvc.CreateTask(ctx, testProjectID, task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidParameter))
}

func TestCreateTask_FeatureNotFound(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-badfeat")
	task.FeatureID = "feat-nonexistent"
	err := svc.taskSvc.CreateTask(ctx, testProjectID, task)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrFeatureNotFound))
}

func TestCreateTask_TriggersFeatureStatusChange(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// Feature starts as "active". After creating a task, the callback should fire.
	callbackFired := false
	svc.taskSvc.OnFeatureStatusChange = func(_ context.Context, pid, fid string) {
		callbackFired = true
		assert.Equal(t, testProjectID, pid)
		assert.Equal(t, testFeatureID, fid)
	}

	task := newTestTask("T-callback")
	require.NoError(t, svc.taskSvc.CreateTask(ctx, testProjectID, task))
	assert.True(t, callbackFired, "OnFeatureStatusChange callback should fire")
}

// ---------------------------------------------------------------------------
// CancelTask
// ---------------------------------------------------------------------------

func TestCancelTask_FromPending(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-cp")
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.CancelTask(ctx, testProjectID, "T-cp", "session-1", "not needed")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-cp")
	assert.Equal(t, model.TaskStatusCancelled, got.Status)
	assert.NotNil(t, got.CancelReason)
	assert.Equal(t, "not needed", *got.CancelReason)
}

func TestCancelTask_FromInProgress(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	seedTestSession(t, svc.stores, "session-1")
	task := newTestTask("T-cip")
	task.Status = model.TaskStatusInProgress
	sid := "session-1"
	wid := "worker-1"
	task.AssignedSessionID = &sid
	task.AssignedWorkerID = &wid
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.CancelTask(ctx, testProjectID, "T-cip", "session-1", "stuck")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-cip")
	assert.Equal(t, model.TaskStatusCancelled, got.Status)
}

func TestCancelTask_FromBlocked(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-cb")
	task.Status = model.TaskStatusBlocked
	reason := "waiting for API"
	task.BlockerReason = &reason
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.CancelTask(ctx, testProjectID, "T-cb", "session-1", "no longer needed")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-cb")
	assert.Equal(t, model.TaskStatusCancelled, got.Status)
}

func TestCancelTask_FromDone_Fails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-cd")
	task.Status = model.TaskStatusDone
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.CancelTask(ctx, testProjectID, "T-cd", "session-1", "oops")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskStateInvalid))
}

func TestCancelTask_FromCancelled_Fails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-cc")
	task.Status = model.TaskStatusCancelled
	reason := "already cancelled"
	task.CancelReason = &reason
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.CancelTask(ctx, testProjectID, "T-cc", "session-1", "double cancel")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskStateInvalid))
}

func TestCancelTask_NonexistentTask(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	err := svc.taskSvc.CancelTask(ctx, testProjectID, "T-NONEXIST", "session-1", "gone")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskNotFound))
}

// ---------------------------------------------------------------------------
// ReportBlocker
// ---------------------------------------------------------------------------

func TestReportBlocker_FromInProgress(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	seedTestSession(t, svc.stores, "session-1")
	task := newTestTask("T-bip")
	task.Status = model.TaskStatusInProgress
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ReportBlocker(ctx, testProjectID, "T-bip", "session-1", "blocked by external API")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-bip")
	assert.Equal(t, model.TaskStatusBlocked, got.Status)
	assert.NotNil(t, got.BlockerReason)
	assert.Equal(t, "blocked by external API", *got.BlockerReason)
}

func TestReportBlocker_FromPending_Fails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-bp")
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ReportBlocker(ctx, testProjectID, "T-bp", "session-1", "reason")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskStateInvalid))
}

func TestReportBlocker_FromDone_Fails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-bd")
	task.Status = model.TaskStatusDone
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ReportBlocker(ctx, testProjectID, "T-bd", "session-1", "reason")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskStateInvalid))
}

// ---------------------------------------------------------------------------
// ResolveBlocker
// ---------------------------------------------------------------------------

func TestResolveBlocker_ReassignTrue_ToInProgress(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	seedTestSession(t, svc.stores, "session-1")
	sid := "session-1"
	wid := "worker-1"

	task := newTestTask("T-rbt")
	task.Status = model.TaskStatusBlocked
	reason := "was blocked"
	task.BlockerReason = &reason
	task.AssignedSessionID = &sid
	task.AssignedWorkerID = &wid
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ResolveBlocker(ctx, testProjectID, "T-rbt", true, "resolved by restart")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-rbt")
	assert.Equal(t, model.TaskStatusInProgress, got.Status)
	assert.Nil(t, got.BlockerReason, "blocker_reason should be cleared")
	// Session and worker should remain assigned.
	assert.NotNil(t, got.AssignedSessionID)
	assert.Equal(t, "session-1", *got.AssignedSessionID)
}

func TestResolveBlocker_ReassignFalse_ToPending(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	seedTestSession(t, svc.stores, "session-1")
	sid := "session-1"
	wid := "worker-1"

	task := newTestTask("T-rbf")
	task.Status = model.TaskStatusBlocked
	reason := "was blocked"
	task.BlockerReason = &reason
	task.AssignedSessionID = &sid
	task.AssignedWorkerID = &wid
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ResolveBlocker(ctx, testProjectID, "T-rbf", false, "resolved, re-queue")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-rbf")
	assert.Equal(t, model.TaskStatusPending, got.Status)
	assert.Nil(t, got.AssignedSessionID, "session should be cleared when reassign=false")
	assert.Nil(t, got.AssignedWorkerID, "worker should be cleared when reassign=false")
}

func TestResolveBlocker_NotBlocked_Fails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-rbn")
	task.Status = model.TaskStatusInProgress
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ResolveBlocker(ctx, testProjectID, "T-rbn", true, "fix")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskStateInvalid))
}

// ---------------------------------------------------------------------------
// ForceRollback
// ---------------------------------------------------------------------------

func TestForceRollback_FromInProgress(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	seedTestSession(t, svc.stores, "session-1")
	sid := "session-1"
	task := newTestTask("T-rip")
	task.Status = model.TaskStatusInProgress
	task.AssignedSessionID = &sid
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ForceRollback(ctx, testProjectID, "T-rip", "admin-session")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-rip")
	assert.Equal(t, model.TaskStatusPending, got.Status)
	assert.Nil(t, got.AssignedSessionID)
}

func TestForceRollback_FromSubmitted(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-rsub")
	task.Status = model.TaskStatusSubmitted
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ForceRollback(ctx, testProjectID, "T-rsub", "admin-session")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-rsub")
	assert.Equal(t, model.TaskStatusPending, got.Status)
}

func TestForceRollback_FromVerifying(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-rvfy")
	task.Status = model.TaskStatusVerifying
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ForceRollback(ctx, testProjectID, "T-rvfy", "admin-session")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-rvfy")
	assert.Equal(t, model.TaskStatusPending, got.Status)
}

func TestForceRollback_FromBlocked(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-rblk")
	task.Status = model.TaskStatusBlocked
	reason := "stuck"
	task.BlockerReason = &reason
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ForceRollback(ctx, testProjectID, "T-rblk", "admin-session")
	require.NoError(t, err)

	got, _ := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-rblk")
	assert.Equal(t, model.TaskStatusPending, got.Status)
}

func TestForceRollback_FromDone_Fails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-rdone")
	task.Status = model.TaskStatusDone
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ForceRollback(ctx, testProjectID, "T-rdone", "admin-session")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskStateInvalid))
}

func TestForceRollback_FromCancelled_Fails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	task := newTestTask("T-rcan")
	task.Status = model.TaskStatusCancelled
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.taskSvc.ForceRollback(ctx, testProjectID, "T-rcan", "admin-session")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskStateInvalid))
}

func TestForceRollback_NonexistentTask(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	err := svc.taskSvc.ForceRollback(ctx, testProjectID, "T-NONEXIST", "admin-session")
	require.Error(t, err)
}
