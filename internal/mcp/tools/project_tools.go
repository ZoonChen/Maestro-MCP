package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// RegisterProjectTools registers the register_project and list_projects tools.
func RegisterProjectTools(s *mcpserver.MCPServer, services *Services) {
	// register_project
	s.AddTool(
		mcp.NewTool("register_project",
			mcp.WithDescription("Register a new project in Maestro. The project's git workspace path is required for worktree-based isolation."),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("Human-readable project name"),
			),
			mcp.WithString("workspace_path",
				mcp.Required(),
				mcp.Description("Absolute path to the project's git workspace directory"),
			),
			mcp.WithString("description",
				mcp.Description("Optional project description"),
			),
			mcp.WithString("config",
				mcp.Description("Optional JSON object with ProjectConfig fields (default_command_profile_id/version/digest, coverage defaults, merge_target_branch, etc.). Arbitrary command strings are forbidden."),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := req.RequireString("name")
			if err != nil {
				return errorResult(err), nil
			}
			workspacePath, err := req.RequireString("workspace_path")
			if err != nil {
				return errorResult(err), nil
			}
			description := req.GetString("description", "")
			configStr := req.GetString("config", "")

			var configJSON json.RawMessage
			if configStr != "" {
				if !json.Valid([]byte(configStr)) {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "config must be a valid JSON string"}), nil
				}
				configJSON = json.RawMessage(configStr)
			} else {
				configJSON = json.RawMessage("{}")
			}

			project := &model.Project{
				ID:            "P-" + uuid.New().String()[:8],
				Name:          name,
				WorkspacePath: workspacePath,
				Description:   description,
				Status:        model.ProjectStatusActive,
				Config:        configJSON,
			}

			if err := services.Project.CreateProject(ctx, project); err != nil {
				return errorResult(fmt.Errorf("failed to create project: %w", err)), nil
			}

			result, _ := json.Marshal(map[string]string{
				"project_id": project.ID,
				"status":     project.Status,
			})
			return mcp.NewToolResultText(string(result)), nil
		},
	)

	// list_projects
	s.AddTool(
		mcp.NewTool("list_projects",
			mcp.WithDescription("List all registered projects. By default, archived projects are excluded."),
			mcp.WithBoolean("include_archived",
				mcp.Description("Whether to include archived projects (default: false)"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			includeArchived := req.GetBool("include_archived", false)

			projects, err := services.Project.ListProjects(ctx, includeArchived)
			if err != nil {
				return errorResult(fmt.Errorf("failed to list projects: %w", err)), nil
			}

			data, err := json.Marshal(projects)
			if err != nil {
				return errorResult(fmt.Errorf("marshal projects: %w", err)), nil
			}
			return mcp.NewToolResultText(string(data)), nil
		},
	)
}
