package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// RegisterVerifierTools registers tools used by verifier agents to review and merge tasks.
func RegisterVerifierTools(s *mcpserver.MCPServer, services *Services) {
	registerGetVerificationTask(s, services)
	registerSubmitVerification(s, services)
	registerMergeTask(s, services)
}

// registerGetVerificationTask adds the get_verification_task tool.
func registerGetVerificationTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("get_verification_task",
			mcp.WithDescription("Atomically claim the next submitted task for verification. Does not change assigned_session_id — that remains pointing to the original executor."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("verifier_session_id",
				mcp.Description("Verifier session ID (defaults to 'verifier-session')"),
			),
			mcp.WithString("verifier_worker_id",
				mcp.Description("Verifier worker ID (defaults to 'verifier-worker')"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			verifierSessionID := req.GetString("verifier_session_id", "verifier-session")
			verifierWorkerID := req.GetString("verifier_worker_id", "verifier-worker")

			task, err := services.Task.GetVerificationTask(ctx, projectID, verifierSessionID, verifierWorkerID)
			if err != nil {
				return errorResult(fmt.Errorf("no verification task available: %w", err)), nil
			}

			// Include validation history for context.
			history, histErr := services.Validation.GetValidationHistory(ctx, projectID, task.ID)
			if histErr != nil {
				history = nil // Graceful degradation.
			}

			result, _ := json.Marshal(map[string]interface{}{
				"task":               task,
				"validation_history": history,
			})
			return mcp.NewToolResultText(string(result)), nil
		},
	)
}

// registerSubmitVerification adds the submit_verification tool.
func registerSubmitVerification(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("submit_verification",
			mcp.WithDescription("Submit a verification verdict for a task. If passed, the task moves to ready_to_merge. If not passed, it returns to in_progress for the original executor."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("ID of the task being verified"),
			),
			mcp.WithBoolean("passed",
				mcp.Required(),
				mcp.Description("Whether the task passed verification"),
			),
			mcp.WithString("verifier_session_id",
				mcp.Description("Verifier session ID (defaults to 'verifier-session')"),
			),
			mcp.WithString("verifier_worker_id",
				mcp.Description("Verifier worker ID (defaults to 'verifier-worker')"),
			),
			mcp.WithString("notes",
				mcp.Description("Optional verification notes or feedback"),
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
			passed := req.GetBool("passed", false)
			verifierSessionID := req.GetString("verifier_session_id", "verifier-session")
			verifierWorkerID := req.GetString("verifier_worker_id", "verifier-worker")
			notes := req.GetString("notes", "")

			if err := services.Task.SubmitVerification(ctx, projectID, verifierSessionID, verifierWorkerID, taskID, passed, notes); err != nil {
				return errorResult(fmt.Errorf("verification failed: %w", err)), nil
			}

			newStatus := "in_progress"
			if passed {
				newStatus = "ready_to_merge"
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"task_id":"%s","passed":%v,"status":"%s"}`, taskID, passed, newStatus)), nil
		},
	)
}

// registerMergeTask adds the merge_task tool.
func registerMergeTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("merge_task",
			mcp.WithDescription("Merge a verified, ready_to_merge task. Performs a real git merge of the task branch into the main branch. Transitions the task to 'done' on success or 'merge_conflicted' on conflict."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("ID of the ready_to_merge task"),
			),
			mcp.WithString("session_id",
				mcp.Description("Session ID performing the merge (defaults to 'coordinator')"),
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

			// session_id defaults to "coordinator" for merge operations.
			sessionID := req.GetString("session_id", "coordinator")

			if err := services.Task.MergeTask(ctx, projectID, taskID, sessionID); err != nil {
				return errorResult(fmt.Errorf("merge failed: %w", err)), nil
			}

			// Fetch the task to report its actual post-merge status.
			task, getErr := services.Task.GetTask(ctx, projectID, taskID)
			resultStatus := "done"
			if getErr == nil {
				resultStatus = task.Status
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"task_id":"%s","status":"%s"}`, taskID, resultStatus)), nil
		},
	)
}
