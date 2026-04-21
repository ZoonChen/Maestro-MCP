// Package tools implements all MCP tool registrations for Maestro-MCP.
// Each registration function creates the tool definition and handler,
// then adds it to the MCP server via AddTool.
package tools

import (
	"github.com/ZoonChen/Maestro-MCP/internal/service"
)

// Services holds all service dependencies needed by MCP tool handlers.
type Services struct {
	Project    *service.ProjectService
	Feature    *service.FeatureService
	Task       *service.TaskService
	Session    *service.SessionService
	Worktree   *service.WorktreeService
	Validation *service.ValidationService
	Contract   *service.ContractService
	Context    *service.ContextService
}
