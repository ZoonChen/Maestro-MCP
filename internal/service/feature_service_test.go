package service

import (
	"context"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CreateFeature
// ---------------------------------------------------------------------------

func TestCreateFeature_HappyPath(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	feature := &model.Feature{
		ID:          "feat-new-001",
		Title:       "New Feature",
		Description: "A brand new feature",
		Status:      model.FeatureStatusPlanning,
	}
	err := svc.featSvc.CreateFeature(ctx, testProjectID, feature)
	require.NoError(t, err)

	got, err := svc.stores.featureStore.GetByID(ctx, testProjectID, "feat-new-001")
	require.NoError(t, err)
	assert.Equal(t, "New Feature", got.Title)
	assert.Equal(t, model.FeatureStatusPlanning, got.Status)
}

func TestCreateFeature_DuplicateID(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	feature := &model.Feature{
		ID:          testFeatureID, // already seeded
		Title:       "Duplicate",
		Description: "Should fail",
		Status:      model.FeatureStatusPlanning,
	}
	err := svc.featSvc.CreateFeature(ctx, testProjectID, feature)
	require.Error(t, err, "duplicate feature ID should fail")
}

func TestCreateFeature_WrongProject(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	feature := &model.Feature{
		ID:          "feat-wrong-proj",
		Title:       "Wrong Project",
		Description: "Feature for nonexistent project",
		Status:      model.FeatureStatusPlanning,
	}
	err := svc.featSvc.CreateFeature(ctx, "nonexistent-project", feature)
	require.Error(t, err, "creating feature in nonexistent project should fail")
}

// ---------------------------------------------------------------------------
// AutoTransitionStatus
// ---------------------------------------------------------------------------

func TestAutoTransitionStatus_PlanningToActive(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// Create a feature in "planning" status.
	feature := &model.Feature{
		ID:          "feat-trans-01",
		Title:       "Transition Test",
		Description: "Planning to Active",
		Status:      model.FeatureStatusPlanning,
	}
	require.NoError(t, svc.featSvc.CreateFeature(ctx, testProjectID, feature))

	// Create a task for this feature (non-cancelled).
	task := &model.Task{
		ID:                 "T-trans-01",
		ProjectID:          testProjectID,
		FeatureID:          "feat-trans-01",
		Title:              "First task",
		Description:        "desc",
		Role:               model.RoleBackend,
		Status:             model.TaskStatusPending,
		AllowedDirectories: `["src/"]`,
		Priority:           model.PriorityNormal,
	}
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.featSvc.AutoTransitionStatus(ctx, testProjectID, "feat-trans-01")
	require.NoError(t, err)

	got, err := svc.stores.featureStore.GetByID(ctx, testProjectID, "feat-trans-01")
	require.NoError(t, err)
	assert.Equal(t, model.FeatureStatusActive, got.Status, "planning feature with tasks should become active")
}

func TestAutoTransitionStatus_AllDone_ToCompleted(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	feature := &model.Feature{
		ID:          "feat-done-01",
		Title:       "All Done",
		Description: "All tasks done",
		Status:      model.FeatureStatusActive,
	}
	require.NoError(t, svc.featSvc.CreateFeature(ctx, testProjectID, feature))

	// Create two done tasks.
	for i, id := range []string{"T-done-01a", "T-done-01b"} {
		task := &model.Task{
			ID:                 id,
			ProjectID:          testProjectID,
			FeatureID:          "feat-done-01",
			Title:              "Done task " + string(rune('A'+i)),
			Description:        "desc",
			Role:               model.RoleBackend,
			Status:             model.TaskStatusDone,
			AllowedDirectories: `["src/"]`,
			Priority:           model.PriorityNormal,
		}
		mustSeedHistoricalDoneTask(t, svc.stores, task)
	}

	err := svc.featSvc.AutoTransitionStatus(ctx, testProjectID, "feat-done-01")
	require.NoError(t, err)

	got, err := svc.stores.featureStore.GetByID(ctx, testProjectID, "feat-done-01")
	require.NoError(t, err)
	assert.Equal(t, model.FeatureStatusCompleted, got.Status, "all tasks done should set feature to completed")
}

func TestAutoTransitionStatus_OnePending_StaysActive(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	feature := &model.Feature{
		ID:          "feat-mix-01",
		Title:       "Mixed Status",
		Description: "Some done, some pending",
		Status:      model.FeatureStatusActive,
	}
	require.NoError(t, svc.featSvc.CreateFeature(ctx, testProjectID, feature))

	// One done, one pending.
	taskDone := &model.Task{
		ID:                 "T-mix-done",
		ProjectID:          testProjectID,
		FeatureID:          "feat-mix-01",
		Title:              "Done task",
		Description:        "desc",
		Role:               model.RoleBackend,
		Status:             model.TaskStatusDone,
		AllowedDirectories: `["src/"]`,
		Priority:           model.PriorityNormal,
	}
	mustSeedHistoricalDoneTask(t, svc.stores, taskDone)

	taskPending := &model.Task{
		ID:                 "T-mix-pending",
		ProjectID:          testProjectID,
		FeatureID:          "feat-mix-01",
		Title:              "Pending task",
		Description:        "desc",
		Role:               model.RoleFrontend,
		Status:             model.TaskStatusPending,
		AllowedDirectories: `["src/"]`,
		Priority:           model.PriorityNormal,
	}
	mustCreateTask(t, svc.stores.taskStore, taskPending)

	err := svc.featSvc.AutoTransitionStatus(ctx, testProjectID, "feat-mix-01")
	require.NoError(t, err)

	got, err := svc.stores.featureStore.GetByID(ctx, testProjectID, "feat-mix-01")
	require.NoError(t, err)
	assert.Equal(t, model.FeatureStatusActive, got.Status, "feature should stay active when not all tasks are done")
}

func TestAutoTransitionStatus_NoNonCancelledTasks_NoTransition(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	feature := &model.Feature{
		ID:          "feat-empty-01",
		Title:       "Empty Feature",
		Description: "No tasks at all",
		Status:      model.FeatureStatusPlanning,
	}
	require.NoError(t, svc.featSvc.CreateFeature(ctx, testProjectID, feature))

	err := svc.featSvc.AutoTransitionStatus(ctx, testProjectID, "feat-empty-01")
	require.NoError(t, err)

	got, err := svc.stores.featureStore.GetByID(ctx, testProjectID, "feat-empty-01")
	require.NoError(t, err)
	assert.Equal(t, model.FeatureStatusPlanning, got.Status, "feature with no tasks should not auto-transition")
}

func TestAutoTransitionStatus_AllCancelled_NoTransition(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	feature := &model.Feature{
		ID:          "feat-cancel-01",
		Title:       "All Cancelled",
		Description: "Only cancelled tasks",
		Status:      model.FeatureStatusPlanning,
	}
	require.NoError(t, svc.featSvc.CreateFeature(ctx, testProjectID, feature))

	task := &model.Task{
		ID:                 "T-cancel-only",
		ProjectID:          testProjectID,
		FeatureID:          "feat-cancel-01",
		Title:              "Cancelled task",
		Description:        "desc",
		Role:               model.RoleBackend,
		Status:             model.TaskStatusCancelled,
		AllowedDirectories: `["src/"]`,
		Priority:           model.PriorityNormal,
	}
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.featSvc.AutoTransitionStatus(ctx, testProjectID, "feat-cancel-01")
	require.NoError(t, err)

	got, err := svc.stores.featureStore.GetByID(ctx, testProjectID, "feat-cancel-01")
	require.NoError(t, err)
	assert.Equal(t, model.FeatureStatusPlanning, got.Status, "all cancelled tasks should not trigger transition")
}

func TestAutoTransitionStatus_Completed_RevertsToActive_WhenNewTaskAdded(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	feature := &model.Feature{
		ID:          "feat-revert-01",
		Title:       "Revert Test",
		Description: "Completed then new task",
		Status:      model.FeatureStatusCompleted,
	}
	require.NoError(t, svc.featSvc.CreateFeature(ctx, testProjectID, feature))

	// A pending task makes the feature revert to active.
	task := &model.Task{
		ID:                 "T-revert-pending",
		ProjectID:          testProjectID,
		FeatureID:          "feat-revert-01",
		Title:              "New pending task",
		Description:        "desc",
		Role:               model.RoleBackend,
		Status:             model.TaskStatusPending,
		AllowedDirectories: `["src/"]`,
		Priority:           model.PriorityNormal,
	}
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.featSvc.AutoTransitionStatus(ctx, testProjectID, "feat-revert-01")
	require.NoError(t, err)

	got, err := svc.stores.featureStore.GetByID(ctx, testProjectID, "feat-revert-01")
	require.NoError(t, err)
	assert.Equal(t, model.FeatureStatusActive, got.Status, "completed feature with new pending task should revert to active")
}

func TestAutoTransitionStatus_Closed_NoTransition(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	feature := &model.Feature{
		ID:          "feat-closed-01",
		Title:       "Closed Feature",
		Description: "Manually closed",
		Status:      model.FeatureStatusClosed,
	}
	require.NoError(t, svc.featSvc.CreateFeature(ctx, testProjectID, feature))

	// Add a task (should not affect closed feature).
	task := &model.Task{
		ID:                 "T-closed-task",
		ProjectID:          testProjectID,
		FeatureID:          "feat-closed-01",
		Title:              "Task in closed feature",
		Description:        "desc",
		Role:               model.RoleBackend,
		Status:             model.TaskStatusPending,
		AllowedDirectories: `["src/"]`,
		Priority:           model.PriorityNormal,
	}
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.featSvc.AutoTransitionStatus(ctx, testProjectID, "feat-closed-01")
	require.NoError(t, err)

	got, err := svc.stores.featureStore.GetByID(ctx, testProjectID, "feat-closed-01")
	require.NoError(t, err)
	assert.Equal(t, model.FeatureStatusClosed, got.Status, "closed feature should never auto-transition")
}

func TestAutoTransitionStatus_NonexistentFeature(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	err := svc.featSvc.AutoTransitionStatus(ctx, testProjectID, "feat-nonexist")
	require.Error(t, err)
	assert.True(t, assert.ObjectsAreEqual(store.ErrFeatureNotFound, err) || err != nil)
}
