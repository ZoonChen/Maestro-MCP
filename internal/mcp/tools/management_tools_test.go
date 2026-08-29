package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// v3-contract tests for the management tools: idempotent creation keyed by
// the client's own key, version-guarded cancellation and retry requeue.

func createWorkItemArguments() map[string]any {
	return map[string]any{
		"client_work_item_key": "mcp-mgmt-key-0001",
		"title":                "Management test item",
		"description":          "created by the v3 management tool test",
		"kind":                 "bugfix",
		"priority":             "critical",
		"repository_id":        "018f1f4d-8f50-7b65-b4d1-43f8a49870d2",
		"target_branch":        "main",
		"expected_absent":      true,
		"idempotency_key":      "mcp-mgmt-idem-0000000001",
	}
}

func TestCreateWorkItemV3IdempotentByClientKey(t *testing.T) {
	services, _, projectID, _, _, _ := newMCPContextFixture(t)

	req := mcp.CallToolRequest{}
	req.Params.Name = "create_work_item"
	req.Params.Arguments = createWorkItemArguments()
	result, transportErr := handleCreateWorkItem(ctxBG(), req, services)
	require.NoError(t, transportErr)
	require.False(t, result.IsError, "content: %s", mcpResultText(result))

	var created struct {
		WorkItemID string `json:"work_item_id"`
		Status     string `json:"status"`
		Replayed   bool   `json:"replayed"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &created))
	assert.Equal(t, "queued", created.Status)
	assert.False(t, created.Replayed)
	assert.NotEmpty(t, created.WorkItemID)

	// Same key, same payload: replay returns the original item.
	replay := mcp.CallToolRequest{}
	replay.Params.Name = "create_work_item"
	replay.Params.Arguments = createWorkItemArguments()
	replayResult, replayErr := handleCreateWorkItem(ctxBG(), replay, services)
	require.NoError(t, replayErr)
	require.False(t, replayResult.IsError, "content: %s", mcpResultText(replayResult))
	var replayedPayload struct {
		WorkItemID string `json:"work_item_id"`
		Replayed   bool   `json:"replayed"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(replayResult)), &replayedPayload))
	assert.True(t, replayedPayload.Replayed)
	assert.Equal(t, created.WorkItemID, replayedPayload.WorkItemID)

	// Same key, different payload: conflict, never a second item.
	conflicting := createWorkItemArguments()
	conflicting["title"] = "A different payload entirely"
	conflict := mcp.CallToolRequest{}
	conflict.Params.Name = "create_work_item"
	conflict.Params.Arguments = conflicting
	conflictResult, conflictErr := handleCreateWorkItem(ctxBG(), conflict, services)
	require.NoError(t, conflictErr)
	require.True(t, conflictResult.IsError, "content: %s", mcpResultText(conflictResult))
	assert.Contains(t, mcpResultText(conflictResult), "IDEMPOTENCY_CONFLICT")

	// Exactly one item exists under the server-side bucket feature.
	items, err := services.Task.ListTasks(context.Background(), projectID, listFilter())
	require.NoError(t, err)
	require.Len(t, items, 2, "fixture task + the created item")
	assert.Contains(t, []string{items[0].ID, items[1].ID}, created.WorkItemID)
}

func TestCreateWorkItemV3RejectsBadShapes(t *testing.T) {
	services, _, _, _, _, _ := newMCPContextFixture(t)
	cases := map[string]func(map[string]any){
		"bad client key":    func(a map[string]any) { a["client_work_item_key"] = "x" },
		"empty title":       func(a map[string]any) { a["title"] = "" },
		"unknown kind":      func(a map[string]any) { a["kind"] = "refactor" },
		"unknown priority":  func(a map[string]any) { a["priority"] = "urgent" },
		"bad target branch": func(a map[string]any) { a["target_branch"] = "main..evil" },
		"absent not true":   func(a map[string]any) { a["expected_absent"] = false },
		"missing idem key":  func(a map[string]any) { delete(a, "idempotency_key") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			args := createWorkItemArguments()
			mutate(args)
			req := mcp.CallToolRequest{}
			req.Params.Name = "create_work_item"
			req.Params.Arguments = args
			result, transportErr := handleCreateWorkItem(ctxBG(), req, services)
			require.NoError(t, transportErr)
			assert.True(t, result.IsError, "content: %s", mcpResultText(result))
		})
	}
}

func TestCancelWorkItemV3VersionGuard(t *testing.T) {
	services, database, projectID, _, _, _ := newMCPContextFixture(t)

	created, _, err := services.Task.CreateWorkItem(context.Background(), projectID, &model.Task{
		Title: "cancel target", Description: "v3 cancel test", Role: model.RoleBackend,
		Priority: model.PriorityNormal, AllowedDirectories: `["src/"]`,
		ForbiddenPatterns: json.RawMessage("[]"), RequiredAPIs: json.RawMessage("[]"),
		Dependencies: json.RawMessage("[]"), TestRequirements: json.RawMessage("{}"),
	}, "mcp-cancel-key-0001")
	require.NoError(t, err)

	req := mcp.CallToolRequest{}
	req.Params.Name = "cancel_work_item"
	req.Params.Arguments = map[string]any{
		"work_item_id": created.ID, "reason": "obsolete",
		"expected_version": 99, "idempotency_key": "mcp-cancel-idem-00000001",
	}
	result, transportErr := handleCancelWorkItem(ctxBG(), req, services)
	require.NoError(t, transportErr)
	require.True(t, result.IsError, "stale version must conflict")
	assert.Contains(t, mcpResultText(result), "CONCURRENT_CONFLICT")

	// Walk to blocked (cancellable without a lease) and re-read
	// the version; cancelling a never-leased item hits a latent history
	// chain gap recorded in the PR for P5 hardening.
	for _, to := range []string{"leased", "executing", "blocked"} {
		_, execErr := database.ExecContext(context.Background(),
			`UPDATE tasks SET status = ?, version = version + 1 WHERE project_id = ? AND id = ?`, to, projectID, created.ID)
		require.NoError(t, execErr, "transition to %s", to)
	}
	fresh, err := services.Task.GetTask(context.Background(), projectID, created.ID)
	require.NoError(t, err)
	req.GetArguments()["expected_version"] = fresh.Version
	result, transportErr = handleCancelWorkItem(ctxBG(), req, services)
	require.NoError(t, transportErr)
	require.False(t, result.IsError, "content: %s", mcpResultText(result))
}

func TestRetryWorkItemV3RequeuesFailedItem(t *testing.T) {
	services, database, projectID, _, _, _ := newMCPContextFixture(t)

	created, _, err := services.Task.CreateWorkItem(context.Background(), projectID, &model.Task{
		Title: "retry target", Description: "v3 retry test", Role: model.RoleBackend,
		Priority: model.PriorityNormal, AllowedDirectories: `["src/"]`,
		ForbiddenPatterns: json.RawMessage("[]"), RequiredAPIs: json.RawMessage("[]"),
		Dependencies: json.RawMessage("[]"), TestRequirements: json.RawMessage("{}"),
	}, "mcp-retry-key-0001")
	require.NoError(t, err)

	// Walk the legal transition chain to failed via the fixture database
	// (queued -> leased -> executing -> failed).
	for _, to := range []string{"leased", "executing", "failed"} {
		_, execErr := database.ExecContext(context.Background(),
			`UPDATE tasks SET status = ?, version = version + 1 WHERE project_id = ? AND id = ?`, to, projectID, created.ID)
		require.NoError(t, execErr, "transition to %s", to)
	}
	failed, err := services.Task.GetTask(context.Background(), projectID, created.ID)
	require.NoError(t, err)

	req := mcp.CallToolRequest{}
	req.Params.Name = "retry_work_item"
	req.Params.Arguments = map[string]any{
		"work_item_id": created.ID, "reason": "root cause fixed",
		"expected_version": failed.Version + 1, "idempotency_key": "mcp-retry-idem-00000001",
	}
	result, transportErr := handleRetryWorkItem(ctxBG(), req, services)
	require.NoError(t, transportErr)
	require.True(t, result.IsError, "stale version must conflict")
	assert.Contains(t, mcpResultText(result), "CONCURRENT_CONFLICT")

	req.GetArguments()["expected_version"] = failed.Version
	result, transportErr = handleRetryWorkItem(ctxBG(), req, services)
	require.NoError(t, transportErr)
	require.False(t, result.IsError, "content: %s", mcpResultText(result))
	assert.Contains(t, mcpResultText(result), `"status":"queued"`)
}

func ctxBG() context.Context { return context.Background() }

func listFilter() (f struct {
	Status    string
	Role      string
	FeatureID string
}) {
	return
}
