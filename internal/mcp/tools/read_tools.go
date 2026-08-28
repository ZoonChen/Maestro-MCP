package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	mcp "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// RegisterReadTools registers the five read-only v3 tools. Project scope
// always resolves from the TransportBinding; no read accepts a scope field.

// registerListWorkItems adds the list_work_items tool (frozen v3 shape).
func registerListWorkItems(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("list_work_items",
			mcp.WithDescription("List work items in the authorized project, optionally filtered by canonical states."),
			mcp.WithString("states", mcp.Description("Comma-separated canonical state filter")),
			mcp.WithString("cursor", mcp.Description("Opaque pagination cursor from a previous response")),
			mcp.WithNumber("limit", mcp.Description("Page size (1-100, default 50)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, _, _, err := services.Binding.scope()
			if err != nil {
				return errorResult(err), nil
			}
			filter := store.TaskFilter{}
			if states := req.GetString("states", ""); states != "" {
				for _, state := range strings.Split(states, ",") {
					if !canonicalWorkItemStates[state] {
						return errorResult(fmt.Errorf("states: %w", store.ErrInvalidParameter)), nil
					}
				}
				filter.Status = states
			}
			limit := 50
			if raw, ok := req.GetArguments()["limit"]; ok {
				parsed, castErr := requireIntegerArg(req, "limit")
				if castErr != nil || parsed < 1 || parsed > 100 {
					return errorResult(fmt.Errorf("limit: %w", store.ErrInvalidParameter)), nil //nolint:nilerr // parameter validation failed, not the transport
				}
				limit = int(parsed)
				_ = raw
			}
			offset := 0
			if cursor := req.GetString("cursor", ""); cursor != "" {
				parsed, parseErr := strconv.Atoi(cursor)
				if parseErr != nil || parsed < 0 {
					return errorResult(fmt.Errorf("cursor: %w", store.ErrInvalidParameter)), nil
				}
				offset = parsed
			}

			items, err := services.Task.ListTasks(ctx, projectID, filter)
			if err != nil {
				return errorResult(fmt.Errorf("list work items: %w", err)), nil
			}
			if offset > len(items) {
				offset = len(items)
			}
			items = items[offset:]
			next := ""
			if len(items) > limit {
				items = items[:limit]
				next = strconv.Itoa(offset + limit)
			}
			payload, err := json.Marshal(map[string]any{
				"work_items":  items,
				"next_cursor": next,
			})
			if err != nil {
				return nil, fmt.Errorf("marshal work item list: %w", err)
			}
			return mcp.NewToolResultText(string(payload)), nil
		},
	)
}

// registerGetWorkItem adds the get_work_item tool (frozen v3 shape).
func registerGetWorkItem(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("get_work_item",
			mcp.WithDescription("Read one work item from the authorized project."),
			mcp.WithString("work_item_id", mcp.Required(), mcp.Description("Work item identifier")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			workItemID, err := req.RequireString("work_item_id")
			if err != nil {
				return errorResult(err), nil
			}
			projectID, _, _, err := services.Binding.scope()
			if err != nil {
				return errorResult(err), nil
			}
			task, err := services.Task.GetTask(ctx, projectID, workItemID)
			if err != nil {
				return errorResult(fmt.Errorf("work item: %w", err)), nil
			}
			payload, err := json.Marshal(task)
			if err != nil {
				return nil, fmt.Errorf("marshal work item: %w", err)
			}
			return mcp.NewToolResultText(string(payload)), nil
		},
	)
}

// registerGetTaskContext adds the get_task_context tool (frozen v3 shape).
func registerGetTaskContext(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("get_task_context",
			mcp.WithDescription("Assemble the full execution context for a work item: dependencies, contracts and requirements."),
			mcp.WithString("work_item_id", mcp.Required(), mcp.Description("Work item identifier")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			workItemID, err := req.RequireString("work_item_id")
			if err != nil {
				return errorResult(err), nil
			}
			projectID, _, _, err := services.Binding.scope()
			if err != nil {
				return errorResult(err), nil
			}
			taskContext, err := services.Context.GetTaskContext(ctx, projectID, workItemID)
			if err != nil {
				return errorResult(normalizeContextBuildError(err)), nil
			}
			payload, err := json.Marshal(taskContext)
			if err != nil {
				return nil, fmt.Errorf("marshal task context: %w", err)
			}
			return mcp.NewToolResultText(string(payload)), nil
		},
	)
}

// registerGetQualityStatus adds the get_quality_status tool (frozen v3
// shape): the latest validation evidence for the work item, always labeled
// with its authority (diagnostic until M2 CI ingestion).
func registerGetQualityStatus(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("get_quality_status",
			mcp.WithDescription("Read the latest validation evidence and quality gates for a work item."),
			mcp.WithString("work_item_id", mcp.Required(), mcp.Description("Work item identifier")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			workItemID, err := req.RequireString("work_item_id")
			if err != nil {
				return errorResult(err), nil
			}
			projectID, _, _, err := services.Binding.scope()
			if err != nil {
				return errorResult(err), nil
			}
			history, err := services.Validation.GetValidationHistory(ctx, projectID, workItemID)
			if err != nil {
				return errorResult(fmt.Errorf("quality status: %w", err)), nil
			}
			latest := map[string]any{"attempts": len(history)}
			authority := model.EvidenceAuthorityDiagnostic
			if len(history) > 0 {
				latest["attempt"] = history[len(history)-1].Attempt
				latest["result"] = history[len(history)-1].Result
				latest["coverage"] = history[len(history)-1].Coverage
				authority = history[len(history)-1].Authority
			}
			payload, err := json.Marshal(map[string]any{
				"work_item_id": workItemID,
				"latest":       latest,
				"authority":    authority,
			})
			if err != nil {
				return nil, fmt.Errorf("marshal quality status: %w", err)
			}
			return mcp.NewToolResultText(string(payload)), nil
		},
	)
}

// registerGetGitlabStatus adds the get_gitlab_status tool (frozen v3
// shape). GitLab integration lands in M2; until then the tool answers with
// an explicit not-integrated status instead of fabricated data or an error.
func registerGetGitlabStatus(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("get_gitlab_status",
			mcp.WithDescription("Read the GitLab integration status (MR, pipeline) for a work item."),
			mcp.WithString("work_item_id", mcp.Required(), mcp.Description("Work item identifier")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			workItemID, err := req.RequireString("work_item_id")
			if err != nil {
				return errorResult(err), nil
			}
			if _, _, _, err := services.Binding.scope(); err != nil {
				return errorResult(err), nil
			}
			payload, err := json.Marshal(map[string]any{
				"work_item_id": workItemID,
				"integrated":   false,
				"reason":       "gitlab integration is not configured until M2",
			})
			if err != nil {
				return nil, fmt.Errorf("marshal gitlab status: %w", err)
			}
			return mcp.NewToolResultText(string(payload)), nil
		},
	)
}

// canonicalWorkItemStates is the frozen WorkItemState vocabulary.
var canonicalWorkItemStates = map[string]bool{
	"draft": true, "queued": true, "leased": true, "executing": true,
	"validating": true, "ready_for_human_merge": true, "done": true,
	"blocked": true, "cancelling": true, "cancelled": true, "failed": true,
	"needs_human": true,
}

// RegisterReadTools registers the read-only v3 tool set on the server.
func RegisterReadTools(s *mcpserver.MCPServer, services *Services) {
	registerListWorkItems(s, services)
	registerGetWorkItem(s, services)
	registerGetTaskContext(s, services)
	registerGetQualityStatus(s, services)
	registerGetGitlabStatus(s, services)
}
