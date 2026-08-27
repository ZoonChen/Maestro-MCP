package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetNextTaskMCPFailsClosedAndCompensatesMissingContext(t *testing.T) {
	services, database, projectID, taskID, sessionID, workspace := newMCPContextFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_next_task"
	req.Params.Arguments = map[string]any{
		"project_id": projectID,
		"role":       model.RoleBackend,
		"session_id": sessionID,
		"worker_id":  "context-worker",
	}

	result, transportErr := handleGetNextTask(context.Background(), req, services)
	require.NoError(t, transportErr)
	require.NotNil(t, result)
	require.True(t, result.IsError, "context omission must be an MCP tool error")
	content := mcpResultText(result)
	var payload MaestroError
	require.NoError(t, json.Unmarshal([]byte(content), &payload))
	assert.Equal(t, service.ContextErrorRequiredSourceMissing, payload.Code)
	assert.NotContains(t, content, "context fail-closed task", "a bare Task must never be returned")

	var (
		taskStatus      string
		activeLeaseID   sql.NullString
		assignedSession sql.NullString
		assignedWorker  sql.NullString
		blockerReason   sql.NullString
	)
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT status, active_lease_id,
		assigned_session_id, assigned_worker_id, blocker_reason
		FROM tasks WHERE project_id = ? AND id = ?`, projectID, taskID).Scan(
		&taskStatus, &activeLeaseID, &assignedSession, &assignedWorker, &blockerReason,
	))
	assert.Equal(t, model.TaskStatusNeedsHuman, taskStatus)
	assert.False(t, activeLeaseID.Valid)
	assert.False(t, assignedSession.Valid)
	assert.False(t, assignedWorker.Valid)
	require.True(t, blockerReason.Valid)
	assert.Contains(t, blockerReason.String, service.ContextErrorRequiredSourceMissing)

	var leaseStatus string
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT status FROM task_leases
		WHERE project_id = ? AND task_id = ?`, projectID, taskID).Scan(&leaseStatus))
	assert.Equal(t, model.LeaseStatusReleased, leaseStatus)
	var workerStatus string
	var currentTask sql.NullString
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT status, current_task_id
		FROM agent_workers WHERE project_id = ? AND id = ?`, projectID, "context-worker").Scan(
		&workerStatus, &currentTask,
	))
	assert.Equal(t, model.WorkerStatusIdle, workerStatus)
	assert.False(t, currentTask.Valid)
	var worktrees int
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM worktrees
		WHERE project_id = ? AND task_id = ?`, projectID, taskID).Scan(&worktrees))
	assert.Zero(t, worktrees, "exact cleanup must retire the rejected claim Worktree")
	_, err := os.Stat(filepath.Join(workspace, ".maestro", "worktrees", taskID))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestGetNextTaskMCPRejectsMissingCompensationDependenciesBeforeClaim(t *testing.T) {
	services, database, projectID, taskID, sessionID, _ := newMCPContextFixture(t)
	services.Worktree = nil
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project_id": projectID,
		"role":       model.RoleBackend,
		"session_id": sessionID,
		"worker_id":  "context-worker",
	}

	result, err := handleGetNextTask(context.Background(), req, services)
	require.NoError(t, err)
	require.True(t, result.IsError)
	var payload MaestroError
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &payload))
	assert.Equal(t, service.ContextErrorBuildFailed, payload.Code)
	var status string
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT status FROM tasks
		WHERE project_id = ? AND id = ?`, projectID, taskID).Scan(&status))
	assert.Equal(t, model.TaskStatusQueued, status)
	var activeLeases int
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM task_leases
		WHERE project_id = ? AND task_id = ? AND status = 'active'`, projectID, taskID).Scan(&activeLeases))
	assert.Zero(t, activeLeases)
}

func TestGetNextTaskMCPQuarantinesPreviouslyAssignedWorktree(t *testing.T) {
	services, database, projectID, taskID, sessionID, _ := newMCPContextFixture(t)
	claimed, err := services.Task.GetNextTask(
		context.Background(), projectID, sessionID, model.RoleBackend, "context-worker",
	)
	require.NoError(t, err)
	require.Equal(t, taskID, claimed.ID)
	var worktreePath string
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT worktree_path FROM worktrees
		WHERE project_id = ? AND task_id = ?`, projectID, taskID).Scan(&worktreePath))
	markerPath := filepath.Join(worktreePath, "src", "agent-change.txt")
	require.NoError(t, os.WriteFile(markerPath, []byte("preserve possible side effects\n"), 0o600))

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project_id": projectID,
		"role":       model.RoleBackend,
		"session_id": sessionID,
		"worker_id":  "context-worker",
	}
	result, err := handleGetNextTask(context.Background(), req, services)
	require.NoError(t, err)
	require.True(t, result.IsError)
	var payload MaestroError
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &payload))
	assert.Equal(t, service.ContextErrorRequiredSourceMissing, payload.Code)
	var taskStatus, worktreeStatus, leaseStatus, workerStatus string
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT status FROM tasks
		WHERE project_id = ? AND id = ?`, projectID, taskID).Scan(&taskStatus))
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT status FROM worktrees
		WHERE project_id = ? AND task_id = ?`, projectID, taskID).Scan(&worktreeStatus))
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT status FROM task_leases
		WHERE project_id = ? AND task_id = ?`, projectID, taskID).Scan(&leaseStatus))
	require.NoError(t, database.QueryRowContext(context.Background(), `SELECT status FROM agent_workers
		WHERE project_id = ? AND id = ?`, projectID, "context-worker").Scan(&workerStatus))
	assert.Equal(t, model.TaskStatusNeedsHuman, taskStatus)
	assert.Equal(t, model.WorktreeStatusQuarantined, worktreeStatus)
	assert.Equal(t, model.LeaseStatusReleased, leaseStatus)
	assert.Equal(t, model.WorkerStatusIdle, workerStatus)
	preserved, err := os.ReadFile(markerPath)
	require.NoError(t, err)
	assert.Equal(t, "preserve possible side effects\n", string(preserved))
}

func newMCPContextFixture(
	t *testing.T,
) (*Services, *sql.DB, string, string, string, string) {
	t.Helper()
	workspace := t.TempDir()
	runMCPContextGit(t, workspace, "init", "--quiet")
	runMCPContextGit(t, workspace, "config", "user.email", "maestro-test@example.invalid")
	runMCPContextGit(t, workspace, "config", "user.name", "Maestro Test")
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "src", "main.go"), []byte("package main\n"), 0o600))
	runMCPContextGit(t, workspace, "add", "--", "src/main.go")
	runMCPContextGit(t, workspace, "commit", "--quiet", "-m", "initial")

	databaseHandle, err := store.NewSQLiteDB(":memory:")
	require.NoError(t, err)
	require.NoError(t, databaseHandle.Init(context.Background()))
	t.Cleanup(func() { require.NoError(t, databaseHandle.Close()) })
	database := databaseHandle.DB()
	projectStore := store.NewSQLiteProjectStore(database)
	featureStore := store.NewSQLiteFeatureStore(database)
	taskStore := store.NewSQLiteTaskStore(database)
	resultStore := store.NewSQLiteTaskResultStore(database)
	validationStore := store.NewSQLiteValidationRunStore(database)
	sessionStore := store.NewSessionStore(database)
	workerStore := store.NewWorkerStore(database)
	worktreeStore := store.NewWorktreeStore(database)
	activityStore := store.NewActivityLogStore(database)
	auditStore := store.NewAuditLogStore(database)
	contractStore := store.NewContractStore(database)
	emitter := &mcpContextNoopEmitter{}
	taskService := service.NewTaskService(
		taskStore, resultStore, validationStore, sessionStore, workerStore,
		worktreeStore, activityStore, auditStore, projectStore, featureStore,
		database, emitter,
	)
	sessionService := service.NewSessionService(
		sessionStore, workerStore, taskStore, worktreeStore, auditStore, emitter,
	)
	worktreeService := service.NewWorktreeService(worktreeStore, projectStore, database)

	projectID := "project-context-mcp"
	featureID := "feature-context-mcp"
	taskID := "T-context-mcp"
	sessionID := "context-session"
	now := time.Now().UTC().Format(time.RFC3339)
	require.NoError(t, projectStore.Create(context.Background(), &model.Project{
		ID: projectID, Name: "MCP context project", WorkspacePath: workspace,
		Description: "context test", Status: model.ProjectStatusActive,
		Config: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, featureStore.Create(context.Background(), projectID, &model.Feature{
		ID: featureID, ProjectID: projectID, Title: "Context feature",
		Description: "context feature", ReferenceURLs: `[]`, Status: model.FeatureStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, taskService.CreateTask(context.Background(), projectID, &model.Task{
		ID: taskID, FeatureID: featureID, Title: "context fail-closed task",
		Description: "must not be returned without its contract", Role: model.RoleBackend,
		Status: model.TaskStatusQueued, AllowedDirectories: `["src/"]`,
		ForbiddenPatterns: json.RawMessage(`[]`),
		RequiredAPIs:      json.RawMessage(`[{"method":"GET","path":"/api/v1/required"}]`),
		Dependencies:      json.RawMessage(`[]`), TestRequirements: json.RawMessage(`{}`),
		Priority: model.PriorityNormal,
	}))
	require.NoError(t, sessionService.RegisterSession(context.Background(), projectID, &model.AgentSession{
		ID: sessionID, Role: model.RoleBackend, ClientType: "mcp-test", Capacity: 1,
	}))

	return &Services{
		Task:     taskService,
		Session:  sessionService,
		Worktree: worktreeService,
		Context:  service.NewContextService(taskStore, contractStore),
	}, database, projectID, taskID, sessionID, workspace
}

func mcpResultText(result *mcp.CallToolResult) string {
	var content strings.Builder
	for _, item := range result.Content {
		switch typed := item.(type) {
		case mcp.TextContent:
			content.WriteString(typed.Text)
		case *mcp.TextContent:
			content.WriteString(typed.Text)
		}
	}
	return content.String()
}

func runMCPContextGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), output)
}

type mcpContextNoopEmitter struct{}

func (*mcpContextNoopEmitter) EmitEvent(string, string, interface{}) {}
