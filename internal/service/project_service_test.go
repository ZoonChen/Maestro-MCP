package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CreateProject
// ---------------------------------------------------------------------------

func TestCreateProject_HappyPath(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	project := &model.Project{
		ID:            "proj-new-001",
		Name:          "New Project",
		WorkspacePath: "/tmp/new-workspace",
		Description:   "A new project",
		Status:        model.ProjectStatusActive,
		Config:        json.RawMessage(`{}`),
	}
	err := svc.projSvc.CreateProject(ctx, project)
	require.NoError(t, err)

	got, err := svc.projSvc.GetProject(ctx, "proj-new-001")
	require.NoError(t, err)
	assert.Equal(t, "New Project", got.Name)
	assert.Equal(t, "/tmp/new-workspace", got.WorkspacePath)
}

func TestCreateProject_DuplicateID(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	project := &model.Project{
		ID:            testProjectID, // already seeded
		Name:          "Duplicate",
		WorkspacePath: "/tmp/dup-workspace",
		Description:   "Should fail",
		Status:        model.ProjectStatusActive,
		Config:        json.RawMessage(`{}`),
	}
	err := svc.projSvc.CreateProject(ctx, project)
	require.Error(t, err, "duplicate project ID should fail")
}

// ---------------------------------------------------------------------------
// ArchiveProject / RestoreProject
// ---------------------------------------------------------------------------

func TestArchiveProject_HappyPath(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	err := svc.projSvc.ArchiveProject(ctx, testProjectID)
	require.NoError(t, err)

	got, err := svc.projSvc.GetProject(ctx, testProjectID)
	require.NoError(t, err)
	assert.Equal(t, model.ProjectStatusArchived, got.Status)
}

func TestArchiveProject_NonexistentProject(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	err := svc.projSvc.ArchiveProject(ctx, "proj-nonexist")
	require.Error(t, err)
}

func TestArchiveProject_AlreadyArchived(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// First archive.
	require.NoError(t, svc.projSvc.ArchiveProject(ctx, testProjectID))

	// Second archive should fail (store's WHERE status != 'archived' returns 0 rows).
	err := svc.projSvc.ArchiveProject(ctx, testProjectID)
	require.Error(t, err)
}

func TestRestoreProject_HappyPath(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// Archive first.
	require.NoError(t, svc.projSvc.ArchiveProject(ctx, testProjectID))

	// Then restore.
	err := svc.projSvc.RestoreProject(ctx, testProjectID)
	require.NoError(t, err)

	got, err := svc.projSvc.GetProject(ctx, testProjectID)
	require.NoError(t, err)
	assert.Equal(t, model.ProjectStatusActive, got.Status)
}

func TestRestoreProject_NotArchived(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// Project is active; restoring an active project should fail.
	err := svc.projSvc.RestoreProject(ctx, testProjectID)
	require.Error(t, err)
}

func TestRestoreProject_NonexistentProject(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	err := svc.projSvc.RestoreProject(ctx, "proj-nonexist")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// ListProjects
// ---------------------------------------------------------------------------

func TestListProjects_ExcludeArchived(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// Archive the seed project.
	require.NoError(t, svc.projSvc.ArchiveProject(ctx, testProjectID))

	// Create a new active project.
	newProj := &model.Project{
		ID:            "proj-active-002",
		Name:          "Active Project",
		WorkspacePath: "/tmp/active-workspace-002",
		Description:   "Still active",
		Status:        model.ProjectStatusActive,
		Config:        json.RawMessage(`{}`),
	}
	require.NoError(t, svc.projSvc.CreateProject(ctx, newProj))

	// List without archived should return only the active project.
	projects, err := svc.projSvc.ListProjects(ctx, false)
	require.NoError(t, err)
	assert.Len(t, projects, 1)
	assert.Equal(t, "proj-active-002", projects[0].ID)
}

func TestListProjects_IncludeArchived(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// Archive the seed project.
	require.NoError(t, svc.projSvc.ArchiveProject(ctx, testProjectID))

	// List with archived should return both.
	projects, err := svc.projSvc.ListProjects(ctx, true)
	require.NoError(t, err)
	assert.Len(t, projects, 1, "only one project exists (seeded, now archived)")
	assert.Equal(t, model.ProjectStatusArchived, projects[0].Status)
}

// ---------------------------------------------------------------------------
// FindByPath
// ---------------------------------------------------------------------------

func TestFindByPath_ExactMatch(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	projects, err := svc.projSvc.FindByPath(ctx, "/tmp/test-workspace")
	require.NoError(t, err)
	assert.Len(t, projects, 1)
	assert.Equal(t, testProjectID, projects[0].ID)
}

func TestFindByPath_NoMatch(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	projects, err := svc.projSvc.FindByPath(ctx, "/nonexistent/path")
	require.NoError(t, err)
	assert.Empty(t, projects)
}

func TestFindByPath_ArchivedProjectExcluded(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// Archive the project.
	require.NoError(t, svc.projSvc.ArchiveProject(ctx, testProjectID))

	// FindByPath on the archived project's path should return empty (store excludes archived).
	projects, err := svc.projSvc.FindByPath(ctx, "/tmp/test-workspace")
	require.NoError(t, err)
	assert.Empty(t, projects, "archived projects should not appear in FindByPath")
}

// ---------------------------------------------------------------------------
// BindProject
// ---------------------------------------------------------------------------

func TestBindProject_ByID(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	p, err := svc.projSvc.BindProject(ctx, testProjectID, false)
	require.NoError(t, err)
	assert.Equal(t, testProjectID, p.ID)
}

func TestBindProject_ByPath(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	p, err := svc.projSvc.BindProject(ctx, "/tmp/test-workspace", true)
	require.NoError(t, err)
	assert.Equal(t, testProjectID, p.ID)
}

func TestBindProject_ByPath_NotFound(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	_, err := svc.projSvc.BindProject(ctx, "/nonexistent/path", true)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrProjectNotBound))
}

func TestBindProject_ByID_Archived(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	require.NoError(t, svc.projSvc.ArchiveProject(ctx, testProjectID))

	_, err := svc.projSvc.BindProject(ctx, testProjectID, false)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrProjectArchived))
}

func TestBindProject_ByID_Nonexistent(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	_, err := svc.projSvc.BindProject(ctx, "proj-nonexist", false)
	require.Error(t, err)
}
