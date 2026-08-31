package tools

import (
	"context"
	"fmt"

	mcpspec "github.com/ZoonChen/Maestro-MCP/docs/specs/mcp"
	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

// Policy guard for MCP tool calls (M1 exit gate: REST, MCP Tool, MCP
// Resource, WebSocket and background share ONE authorization decision).
//
// Composition modes:
//   - Identity layer mounted (OIDC on): every tool call resolves the
//     principal from the SERVER-SIDE binding (the registered session's
//     role is the authority) and enforces the frozen
//     required_permission through the same Policy.Authorize REST uses.
//     Denials carry the policy's reason, never payload echoes.
//   - Local delegated context (M0 baseline, no identity layer): the
//     TransportBinding remains the authorization — the host-injected
//     single-user/single-project context of SEC-IDENTITY-RBAC section 2.

// ToolGuard enforces the frozen catalog permissions for one transport.
type ToolGuard struct {
	policy      *identity.Policy
	permissions map[string]string
}

// NewToolGuard builds the guard from the frozen embedded catalog. A nil
// policy keeps the M0 delegated-context mode (no enforcement).
func NewToolGuard(policy *identity.Policy) (*ToolGuard, error) {
	permissions, err := mcpspec.ToolPermissions()
	if err != nil {
		return nil, err
	}
	return &ToolGuard{policy: policy, permissions: permissions}, nil
}

// delegatedContextRoles maps the session's TASK-ROLE vocabulary
// (backend/frontend/devops/verifier/coordinator — work eligibility) onto
// the frozen RBAC roles for the stdio delegated context: a locally
// delegated worker acts as a developer, a verifier session as a
// verifier, a coordinator session as a coordinator. The OIDC mode
// derives memberships from the identity registry instead and never uses
// this mapping.
var delegatedContextRoles = map[string]string{
	"backend": "developer", "frontend": "developer", "devops": "developer",
	"verifier": "verifier", "coordinator": "coordinator",
}

// Principal derives the server-side principal for the bound session:
// the REGISTERED session role is the authority; payload fields never
// participate.
func (g *ToolGuard) Principal(b *TransportBinding, sessionRole string) *model.PrincipalContext {
	if b == nil {
		return nil
	}
	rbacRole := delegatedContextRoles[sessionRole]
	if rbacRole == "" {
		// Unknown task roles authorize nothing (fail closed); the empty
		// membership is denied by the policy's default deny.
		rbacRole = "__unknown__"
	}
	return &model.PrincipalContext{
		PrincipalID: "session:" + b.SessionID,
		Type:        model.PrincipalTypeHuman,
		ProjectMemberships: map[string]string{
			b.ProjectID: rbacRole,
		},
	}
}

// Authorize enforces the tool's frozen permission. Unknown tool names
// deny — an unlisted tool is a catalog bug, never a bypass.
func (g *ToolGuard) Authorize(ctx context.Context, toolName string, principal *model.PrincipalContext, projectID string) (model.Decision, error) {
	permission, known := g.permissions[toolName]
	if !known {
		return model.Decision{}, fmt.Errorf("tool %q is not in the frozen catalog", toolName)
	}
	if g.policy == nil {
		// Delegated-context mode: the binding authorized the transport.
		return model.Decision{Allow: true, Reasons: []string{"server-side delegated context"}}, nil
	}
	return g.policy.Authorize(ctx, principal, permission, model.Resource{Type: "work_item", ProjectID: projectID}), nil
}

// Deny converts a denied decision into the structured MCP error reply.
func Deny(decision model.Decision) *mcp.CallToolResult {
	reason := "action is not permitted for this principal"
	if len(decision.Reasons) > 0 {
		reason = decision.Reasons[0]
	}
	return maestroToolError(MaestroError{
		Code:    "FORBIDDEN",
		Message: reason,
	})
}
