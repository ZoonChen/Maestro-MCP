package tools

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// Cross-surface consistency: the SAME Policy.Authorize that governs REST
// decides MCP tool calls. Role insufficiency denies with the policy's
// reason; sufficient roles pass; unknown tools deny; a nil policy keeps
// the M0 delegated-context behavior.

func TestToolGuardDeniesInsufficientRole(t *testing.T) {
	services, _, projectID, _, _, _ := newMCPContextFixture(t)
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	guard, err := NewToolGuard(policy)
	require.NoError(t, err)
	services.Guard = guard

	// The fixture session registers role=backend; the delegated context
	// maps it to the developer RBAC role, and create_work_item is a
	// coordinator-class action: a developer is denied by the SAME matrix
	// REST applies.
	principal := guard.Principal(services.Binding, model.RoleBackend)
	decision, err := guard.Authorize(context.Background(), "create_work_item", principal, projectID)
	require.NoError(t, err)
	assert.False(t, decision.Allow, "backend must not create work items")

	// The full guarded handler path denies identically.
	req := mcp.CallToolRequest{}
	req.Params.Name = "create_work_item"
	req.Params.Arguments = map[string]any{
		"client_work_item_key": "policy-test-key-00001",
		"title":                "denied", "description": "denied",
		"kind": "bugfix", "priority": "normal",
		"repository_id": "018f1f4d-8f50-7b65-b4d1-43f8a49870d2",
		"target_branch": "main", "expected_absent": true,
		"idempotency_key": "policy-denied-0000000001",
	}
	// The guard wraps handlers at registration; call through the wrapper
	// so the decision point is on the path exactly as the server runs it.
	wrapped := services.guardTool("create_work_item",
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleCreateWorkItem(ctx, req, services)
		})
	result, transportErr := wrapped(context.Background(), req)
	require.NoError(t, transportErr)
	require.True(t, result.IsError, "body: %s", mcpResultText(result))
	assert.Contains(t, mcpResultText(result), "FORBIDDEN")
}

func TestToolGuardAllowsSufficientRoles(t *testing.T) {
	services, _, projectID, _, _, _ := newMCPContextFixture(t)
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	guard, err := NewToolGuard(policy)
	require.NoError(t, err)

	principal := guard.Principal(services.Binding, model.RoleBackend)
	decision, err := guard.Authorize(context.Background(), "get_next_task", principal, projectID)
	require.NoError(t, err)
	assert.True(t, decision.Allow, "backend must claim: %v", decision.Reasons)

	// A verifier session (task role) maps to the verifier RBAC role, which
	// holds work_item.read.
	principal = guard.Principal(services.Binding, "verifier")
	decision, err = guard.Authorize(context.Background(), "list_work_items", principal, projectID)
	require.NoError(t, err)
	assert.True(t, decision.Allow, "verifier read: %v", decision.Reasons)

	// Cross-project isolation: the same role in another project denies.
	decision, err = guard.Authorize(context.Background(), "list_work_items", principal, "project-other")
	require.NoError(t, err)
	assert.False(t, decision.Allow)
}

func TestToolGuardUnknownToolDenies(t *testing.T) {
	guard, err := NewToolGuard(nil)
	require.NoError(t, err)

	_, err = guard.Authorize(context.Background(), "merge_task", guard.Principal(&TransportBinding{ProjectID: "p", SessionID: "s", WorkerID: "w"}, model.RoleBackend), "p")
	require.Error(t, err, "an unlisted tool name must never authorize")
}

func TestToolGuardNilPolicyKeepsDelegatedContext(t *testing.T) {
	guard, err := NewToolGuard(nil)
	require.NoError(t, err)

	decision, err := guard.Authorize(context.Background(), "get_next_task", nil, "any")
	require.NoError(t, err)
	assert.True(t, decision.Allow, "nil policy keeps the M0 delegated-context mode")
}

func TestToolGuardCatalogMatchesFrozenSchema(t *testing.T) {
	guard, err := NewToolGuard(nil)
	require.NoError(t, err)
	require.Len(t, guard.permissions, 14, "the frozen catalog carries exactly fourteen tools")
	for _, tool := range []string{"get_next_task", "submit_verification", "create_work_item", "get_gitlab_status"} {
		require.Contains(t, guard.permissions, tool)
	}
}
