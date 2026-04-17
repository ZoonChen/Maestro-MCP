// Package mcp sets up the Maestro MCP server, registering all tools,
// resources, and prompts that AI agents interact with.
package mcp

import (
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ZoonChen/Maestro-MCP/internal/mcp/tools"
)

// ServerVersion is the MCP protocol version, set via ldflags at build time.
var ServerVersion = "0.1.0"

// NewMaestroMCPServer creates the MCP server and registers all tools,
// resources, and prompts.
func NewMaestroMCPServer(svc *tools.Services) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("maestro-mcp", ServerVersion,
		mcpserver.WithToolCapabilities(true),
	)

	// Register tools
	tools.RegisterProjectTools(s, svc)
	tools.RegisterCoordinatorTools(s, svc)
	tools.RegisterWorkerTools(s, svc)
	tools.RegisterVerifierTools(s, svc)

	// Register resources
	RegisterResources(s, svc)

	// Register prompts
	RegisterPrompts(s, svc)

	return s
}
