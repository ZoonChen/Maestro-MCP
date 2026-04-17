package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// RegisterWorkerTools registers tools used by worker agents to claim and submit tasks.
func RegisterWorkerTools(s *mcpserver.MCPServer, services *Services) {
	registerGetNextTask(s, services)
	registerSubmitTaskResult(s, services)
	registerReportBlocker(s, services)
	registerClaimBatch(s, services)
	registerReleaseWorker(s, services)
}

// registerGetNextTask adds the get_next_task tool.
func registerGetNextTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("get_next_task",
			mcp.WithDescription("Atomically claim the next available pending task matching the given role. Uses serializable transaction isolation with retry for concurrency safety."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("role",
				mcp.Required(),
				mcp.Description("Agent role to match: backend, frontend, devops, or verifier"),
				mcp.Enum("backend", "frontend", "devops", "verifier"),
			),
			mcp.WithString("session_id",
				mcp.Description("Agent session ID (defaults to 'default-session')"),
			),
			mcp.WithString("worker_id",
				mcp.Description("Worker ID within the session (defaults to 'default-worker')"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			role, err := req.RequireString("role")
			if err != nil {
				return errorResult(err), nil
			}
			sessionID := req.GetString("session_id", "default-session")
			workerID := req.GetString("worker_id", "default-worker")

			task, err := services.Task.GetNextTask(ctx, projectID, sessionID, role, workerID)
			if err != nil {
				return errorResult(fmt.Errorf("no task available: %w", err)), nil
			}

			// Build task context: include the task itself plus dependency summaries
			// and required API contracts.
			taskCtx, ctxErr := services.Context.GetTaskContext(ctx, projectID, task.ID)
			if ctxErr != nil {
				// Context enrichment failed; still return the bare task.
				data, marshalErr := json.Marshal(task)
				if marshalErr != nil {
					return nil, fmt.Errorf("marshal task: %w", marshalErr)
				}
				return mcp.NewToolResultText(string(data)), nil
			}

			data, err := json.Marshal(taskCtx)
			if err != nil {
				return nil, fmt.Errorf("marshal task context: %w", err)
			}
			return mcp.NewToolResultText(string(data)), nil
		},
	)
}

// registerSubmitTaskResult adds the submit_task_result tool.
func registerSubmitTaskResult(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("submit_task_result",
			mcp.WithDescription("Submit completed work for a task. The server performs zero-trust validation: it runs git diff and executes tests itself, never trusting agent-reported results."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("ID of the task being submitted"),
			),
			mcp.WithString("session_id",
				mcp.Description("Agent session ID (defaults to 'default-session')"),
			),
			mcp.WithString("worker_id",
				mcp.Description("Worker ID (defaults to 'default-worker')"),
			),
			mcp.WithString("summary",
				mcp.Description("Optional summary of the work completed"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			taskID, err := req.RequireString("task_id")
			if err != nil {
				return errorResult(err), nil
			}
			sessionID := req.GetString("session_id", "default-session")
			workerID := req.GetString("worker_id", "default-worker")
			summaryStr := req.GetString("summary", "")

			var summary *string
			if summaryStr != "" {
				summary = &summaryStr
			}

			if err := services.Validation.SubmitAndValidate(ctx, projectID, taskID, sessionID, workerID, summary); err != nil {
				return errorResult(fmt.Errorf("submission failed: %w", err)), nil
			}

			return mcp.NewToolResultText(fmt.Sprintf(`{"task_id":"%s","status":"submitted","validation":"pending"}`, taskID)), nil
		},
	)
}

// registerReportBlocker adds the report_blocker tool.
func registerReportBlocker(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("report_blocker",
			mcp.WithDescription("Report that a task is blocked. The task transitions from in_progress to blocked status."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("ID of the blocked task"),
			),
			mcp.WithString("session_id",
				mcp.Description("Agent session ID (defaults to 'default-session')"),
			),
			mcp.WithString("reason",
				mcp.Required(),
				mcp.Description("Description of what is blocking the task"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			taskID, err := req.RequireString("task_id")
			if err != nil {
				return errorResult(err), nil
			}
			sessionID := req.GetString("session_id", "default-session")
			reason, err := req.RequireString("reason")
			if err != nil {
				return errorResult(err), nil
			}

			if err := services.Task.ReportBlocker(ctx, projectID, taskID, sessionID, reason); err != nil {
				return errorResult(fmt.Errorf("failed to report blocker: %w", err)), nil
			}

			return mcp.NewToolResultText(fmt.Sprintf(`{"task_id":"%s","status":"blocked"}`, taskID)), nil
		},
	)
}

// registerClaimBatch adds the claim_batch tool.
func registerClaimBatch(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("claim_batch",
			mcp.WithDescription("Claim multiple tasks at once for batch processing. Returns lists of claimed tasks and any failures."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("role",
				mcp.Required(),
				mcp.Description("Agent role to match: backend, frontend, devops, or verifier"),
				mcp.Enum("backend", "frontend", "devops", "verifier"),
			),
			mcp.WithNumber("count",
				mcp.Required(),
				mcp.Description("Number of tasks to claim"),
			),
			mcp.WithString("session_id",
				mcp.Description("Agent session ID (defaults to 'default-session')"),
			),
			mcp.WithString("worker_id",
				mcp.Description("Worker ID (defaults to 'default-worker')"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			role, err := req.RequireString("role")
			if err != nil {
				return errorResult(err), nil
			}
			countFloat, err := req.RequireFloat("count")
			if err != nil {
				return errorResult(err), nil
			}
			count := int(countFloat)
			if count <= 0 || count > 20 {
				return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "count must be between 1 and 20"}), nil
			}
			sessionID := req.GetString("session_id", "default-session")
			workerID := req.GetString("worker_id", "default-worker")

			var claimed []string
			var failed []string

			for i := 0; i < count; i++ {
				task, taskErr := services.Task.GetNextTask(ctx, projectID, sessionID, role, workerID)
				if taskErr != nil {
					failed = append(failed, fmt.Sprintf("task %d: %v", i+1, taskErr))
					break // No more tasks available for this role; stop trying.
				}
				claimed = append(claimed, task.ID)
			}

			result, _ := json.Marshal(map[string]interface{}{
				"claimed": claimed,
				"failed":  failed,
			})
			return mcp.NewToolResultText(string(result)), nil
		},
	)
}

// registerReleaseWorker adds the release_worker tool.
func registerReleaseWorker(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("release_worker",
			mcp.WithDescription("Release a worker, clearing its current task assignment. Used when a worker is shutting down or being reassigned."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("session_id",
				mcp.Required(),
				mcp.Description("Session ID the worker belongs to"),
			),
			mcp.WithString("worker_id",
				mcp.Required(),
				mcp.Description("ID of the worker to release"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			sessionID, err := req.RequireString("session_id")
			if err != nil {
				return errorResult(err), nil
			}
			workerID, err := req.RequireString("worker_id")
			if err != nil {
				return errorResult(err), nil
			}

			// Clear the worker's current task assignment.
			if err := services.Session.ReleaseWorker(ctx, projectID, sessionID, workerID); err != nil {
				return errorResult(fmt.Errorf("failed to release worker: %w", err)), nil
			}

			return mcp.NewToolResultText(fmt.Sprintf(`{"worker_id":"%s","status":"released"}`, workerID)), nil
		},
	)
}
