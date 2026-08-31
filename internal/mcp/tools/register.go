// Package tools implements all MCP tool registrations for Maestro-MCP.
// Each registration function creates the tool definition and handler,
// then adds it to the MCP server via AddTool.
package tools

import (
	"context"
	"fmt"

	mcp "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ZoonChen/Maestro-MCP/internal/service"
)

// Services holds all service dependencies needed by MCP tool handlers.
type Services struct {
	// Binding is the trusted server-side scope for this transport. Tools
	// that need project scope resolve it from here, never from payloads.
	Binding *TransportBinding
	// Guard enforces the frozen tool permissions through the same policy
	// as REST when the identity layer is mounted; nil keeps the M0
	// delegated-context mode (the binding is then the authorization).
	Guard *ToolGuard

	Project    *service.ProjectService
	Feature    *service.FeatureService
	Task       *service.TaskService
	Session    *service.SessionService
	Worktree   *service.WorktreeService
	Validation *service.ValidationService
	Contract   *service.ContractService
	Context    *service.ContextService
}

// guardTool wraps one tool handler with the unified policy decision
// point: the session's REGISTERED role is the authority, and the frozen
// catalog's required_permission is enforced through the same
// Policy.Authorize the REST surface uses. Unregistered sessions and
// unknown tools fail closed before any handler logic runs.
func (s *Services) guardTool(name string, handler mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if s == nil || s.Guard == nil {
			return handler(ctx, req)
		}
		projectID, sessionID, _, err := s.Binding.scope()
		if err != nil {
			return errorResult(err), nil
		}
		session, sessionErr := s.Session.GetSession(ctx, projectID, sessionID)
		if sessionErr != nil {
			return errorResult(fmt.Errorf("bound session is not registered: %w", sessionErr)), nil
		}
		principal := s.Guard.Principal(s.Binding, session.Role)
		decision, authErr := s.Guard.Authorize(ctx, name, principal, projectID)
		if authErr != nil {
			// Unknown tool names deny: the catalog is the only surface.
			return maestroToolError(MaestroError{Code: "FORBIDDEN", Message: "Tool is not in the frozen catalog"}), nil //nolint:nilerr // catalog violations deny rather than error open
		}
		if !decision.Allow {
			return Deny(decision), nil
		}
		return handler(ctx, req)
	}
}
