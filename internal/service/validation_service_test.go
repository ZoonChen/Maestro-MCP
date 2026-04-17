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
// resolveTestRequirements fallback chain
// ---------------------------------------------------------------------------

func TestResolveTestRequirements_TaskLevel(t *testing.T) {
	task := &model.Task{
		TestRequirements: json.RawMessage(`{"command":"go test ./...","coverage_format":"go-cover","coverage_path":"coverage.out","min_coverage":80.0}`),
	}

	reqs := resolveTestRequirements(task, nil)
	require.NotNil(t, reqs, "should resolve from task level")
	assert.Equal(t, "go test ./...", reqs.Command)
	assert.Equal(t, "go-cover", reqs.CoverageFormat)
	assert.Equal(t, "coverage.out", reqs.CoveragePath)
	assert.InDelta(t, 80.0, reqs.MinCoverage, 0.01)
}

func TestResolveTestRequirements_TaskOverridesProject(t *testing.T) {
	cmd := "npm test"
	task := &model.Task{
		TestRequirements: json.RawMessage(`{"command":"go test ./..."}`),
	}
	project := &model.Project{
		Config: json.RawMessage(`{"default_test_command":"npm test","default_coverage_format":"cobertura"}`),
	}

	reqs := resolveTestRequirements(task, project)
	require.NotNil(t, reqs)
	assert.Equal(t, "go test ./...", reqs.Command, "task level should override project level")
	_ = cmd
}

func TestResolveTestRequirements_ProjectFallback(t *testing.T) {
	task := &model.Task{
		TestRequirements: json.RawMessage(`{}`), // Empty object triggers fallback.
	}
	project := &model.Project{
		Config: json.RawMessage(`{"default_test_command":"make test","default_coverage_format":"cobertura","default_coverage_path":"coverage.xml","default_min_coverage":60.0}`),
	}

	reqs := resolveTestRequirements(task, project)
	require.NotNil(t, reqs, "should fall back to project config")
	assert.Equal(t, "make test", reqs.Command)
	assert.Equal(t, "cobertura", reqs.CoverageFormat)
	assert.Equal(t, "coverage.xml", reqs.CoveragePath)
	assert.InDelta(t, 60.0, reqs.MinCoverage, 0.01)
}

func TestResolveTestRequirements_NilWhenNoConfig(t *testing.T) {
	task := &model.Task{
		TestRequirements: nil,
	}
	project := &model.Project{
		Config: json.RawMessage(`{}`),
	}

	reqs := resolveTestRequirements(task, project)
	assert.Nil(t, reqs, "should return nil when no test config at any level")
}

func TestResolveTestRequirements_NilTask_NilProject(t *testing.T) {
	reqs := resolveTestRequirements(&model.Task{}, nil)
	assert.Nil(t, reqs, "should return nil when both task and project have no config")
}

func TestResolveTestRequirements_EmptyCommandFallsBack(t *testing.T) {
	task := &model.Task{
		TestRequirements: json.RawMessage(`{"command":""}`),
	}
	project := &model.Project{
		Config: json.RawMessage(`{"default_test_command":"pytest"}`),
	}

	reqs := resolveTestRequirements(task, project)
	require.NotNil(t, reqs)
	assert.Equal(t, "pytest", reqs.Command, "empty task command should fall back to project")
}

// ---------------------------------------------------------------------------
// filterEnv
// ---------------------------------------------------------------------------

func TestFilterEnv_WhitelistOnly(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"SECRET_TOKEN=abc123",
		"DATABASE_URL=postgres://...",
		"GOPATH=/go",
	}
	whitelist := []string{"PATH", "HOME", "GOPATH"}

	filtered := filterEnv(env, whitelist)
	assert.ElementsMatch(t, []string{"PATH=/usr/bin", "HOME=/home/user", "GOPATH=/go"}, filtered)
}

func TestFilterEnv_EmptyWhitelist(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/home/user"}
	filtered := filterEnv(env, []string{})
	assert.Empty(t, filtered, "empty whitelist should filter out everything")
}

func TestFilterEnv_EmptyEnv(t *testing.T) {
	filtered := filterEnv([]string{}, []string{"PATH"})
	assert.Empty(t, filtered)
}

func TestFilterEnv_MalformedEntry(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"NOEQUALSSIGN",
		"=VALUEWITHOUTKEY",
		"HOME=/home/user",
	}
	filtered := filterEnv(env, []string{"PATH", "HOME"})
	assert.ElementsMatch(t, []string{"PATH=/usr/bin", "HOME=/home/user"}, filtered)
}

// ---------------------------------------------------------------------------
// SubmitAndValidate status/ownership checks
// ---------------------------------------------------------------------------

func TestSubmitAndValidate_WrongStatus_Fails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// Create a task in "pending" status (not in_progress).
	task := newTestTask("T-sa-pending")
	mustCreateTask(t, svc.stores.taskStore, task)

	err := svc.validSvc.SubmitAndValidate(ctx, testProjectID, "T-sa-pending", "session-1", "worker-1", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskStateInvalid))
}

func TestSubmitAndValidate_WrongSession_Fails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	seedTestSession(t, svc.stores, "session-owner")
	sid := "session-owner"
	task := newTestTask("T-sa-wrong")
	task.Status = model.TaskStatusInProgress
	task.AssignedSessionID = &sid
	mustCreateTask(t, svc.stores.taskStore, task)

	// Try to submit from a different session.
	err := svc.validSvc.SubmitAndValidate(ctx, testProjectID, "T-sa-wrong", "session-impostor", "worker-1", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrTaskNotOwned))
}

func TestSubmitAndValidate_NonexistentTask(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	err := svc.validSvc.SubmitAndValidate(ctx, testProjectID, "T-NONEXIST", "session-1", "worker-1", nil)
	require.Error(t, err)
}
