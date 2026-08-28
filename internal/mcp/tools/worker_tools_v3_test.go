package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedRequiredContract inserts the API contract the fixture task requires
// so the context service can build a complete task context.
func seedRequiredContract(t *testing.T, database *sql.DB, projectID string) {
	t.Helper()
	_, err := database.ExecContext(context.Background(),
		`INSERT INTO api_contracts (project_id, method, path, request_schema, response_schema, description, source_file, parsed_at)
		 VALUES (?, 'GET', '/api/v1/required', NULL, NULL, NULL, 'fixture', datetime('now'))`, projectID)
	require.NoError(t, err)
}

// v3-contract tests for the claim tool: stale queue tokens conflict, an
// unbound transport fails closed, and a successful claim returns the frozen
// claimOutcome including the precise worktree path.

func TestGetNextTaskV3StaleQueueVersionConflicts(t *testing.T) {
	services, _, projectID, _, _, _ := newMCPContextFixture(t)
	current, err := services.Task.CurrentQueueVersion(context.Background(), projectID)
	require.NoError(t, err)

	req := mcp.CallToolRequest{}
	req.Params.Name = "get_next_task"
	req.Params.Arguments = map[string]any{
		"idempotency_key": "mcp-v3-stale-queue-key-0001",
		"queue_version":   current + 1,
	}
	result, transportErr := handleGetNextTask(context.Background(), req, services)
	require.NoError(t, transportErr)
	require.True(t, result.IsError)
	var payload MaestroError
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &payload))
	assert.Equal(t, "CONCURRENT_CONFLICT", payload.Code)
}

func TestGetNextTaskV3UnboundTransportFailsClosed(t *testing.T) {
	services, _, _, _, _, _ := newMCPContextFixture(t)
	services.Binding = nil

	req := mcp.CallToolRequest{}
	req.Params.Name = "get_next_task"
	req.Params.Arguments = map[string]any{
		"idempotency_key": "mcp-v3-unbound-key-00000001",
		"queue_version":   0,
	}
	result, transportErr := handleGetNextTask(context.Background(), req, services)
	require.NoError(t, transportErr)
	require.True(t, result.IsError, "an unbound transport must never fall back to a default project")
	var payload MaestroError
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &payload))
	assert.Equal(t, "INTERNAL_ERROR", payload.Code)
}

func TestGetNextTaskV3SelfReportedScopeIsRejected(t *testing.T) {
	services, database, projectID, _, _, _ := newMCPContextFixture(t)
	seedRequiredContract(t, database, projectID)
	// Even if a legacy client smuggles the v2 self-report fields, the
	// handler only reads idempotency_key and queue_version; unknown fields
	// never influence scope resolution.
	req := mcp.CallToolRequest{}
	req.Params.Name = "get_next_task"
	current, err := services.Task.CurrentQueueVersion(context.Background(), projectID)
	require.NoError(t, err)
	req.Params.Arguments = map[string]any{
		"idempotency_key": "mcp-v3-legacy-key-0000000001",
		"queue_version":   current,
		"project_id":      "some-other-project",
		"session_id":      "attacker-session",
		"worker_id":       "attacker-worker",
		"role":            "verifier",
	}
	result, transportErr := handleGetNextTask(context.Background(), req, services)
	require.NoError(t, transportErr)
	// The claim either succeeds for the BOUND project or conflicts on the
	// queue token; it can never succeed against some-other-project.
	require.False(t, result.IsError, "content: %s", mcpResultText(result))
	var outcome claimOutcome
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &outcome))
	assert.Equal(t, services.Binding.ProjectID, projectID,
		"smuggled scope fields must never redirect the claim")
}

func TestGetNextTaskV3SuccessReturnsFrozenOutcome(t *testing.T) {
	services, database, projectID, taskID, _, _ := newMCPContextFixture(t)
	seedRequiredContract(t, database, projectID)
	current, err := services.Task.CurrentQueueVersion(context.Background(), projectID)
	require.NoError(t, err)

	req := mcp.CallToolRequest{}
	req.Params.Name = "get_next_task"
	req.Params.Arguments = map[string]any{
		"idempotency_key": "mcp-v3-success-key-000000001",
		"queue_version":   current,
	}
	result, transportErr := handleGetNextTask(context.Background(), req, services)
	require.NoError(t, transportErr)
	require.False(t, result.IsError, "content: %s", mcpResultText(result))

	var outcome claimOutcome
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &outcome))
	assert.Equal(t, taskID, outcome.WorkItemID)
	assert.NotEmpty(t, outcome.LeaseID)
	assert.GreaterOrEqual(t, outcome.LeaseVersion, int64(1))
	assert.GreaterOrEqual(t, outcome.LeaseEpoch, int64(1))
	assert.Equal(t, current, outcome.QueueVersion)

	var worktreePath string
	require.NoError(t, database.QueryRowContext(context.Background(),
		`SELECT worktree_path FROM worktrees WHERE project_id = ? AND task_id = ?`,
		projectID, taskID).Scan(&worktreePath))
	assert.Equal(t, worktreePath, outcome.WorktreePath,
		"the claim response must carry the precise server-allocated worktree path")

	// Idempotent replay with the same key and token returns the same lease
	// without creating a second one.
	replay := mcp.CallToolRequest{}
	replay.Params.Name = "get_next_task"
	replay.Params.Arguments = map[string]any{
		"idempotency_key": "mcp-v3-success-key-000000001",
		"queue_version":   current,
	}
	replayResult, replayErr := handleGetNextTask(context.Background(), replay, services)
	require.NoError(t, replayErr)
	require.False(t, replayResult.IsError, "content: %s", mcpResultText(replayResult))
	var replayOutcome claimOutcome
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(replayResult)), &replayOutcome))
	assert.Equal(t, outcome.LeaseID, replayOutcome.LeaseID)
}
