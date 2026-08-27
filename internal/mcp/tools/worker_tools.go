package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// RegisterWorkerTools registers tools used by worker agents to claim and submit tasks.
func RegisterWorkerTools(s *mcpserver.MCPServer, services *Services) {
	registerGetNextTask(s, services)
	registerHeartbeatTask(s, services)
	registerSubmitTaskResult(s, services)
	registerReportBlocker(s, services)
}

// registerGetNextTask adds the get_next_task tool.
func registerGetNextTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("get_next_task",
			mcp.WithDescription("Atomically claim the next available queued task matching the given role. The durable Lease moves it through leased to executing."),
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
			return handleGetNextTask(ctx, req, services)
		},
	)
}

func handleGetNextTask(
	ctx context.Context,
	req mcp.CallToolRequest,
	services *Services,
) (*mcp.CallToolResult, error) {
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

	// The claim lifecycle cannot be made safe if any required compensation
	// dependency is absent. Reject before claiming instead of risking a Lease
	// that no caller can use or release.
	if services == nil || services.Task == nil || services.Context == nil || services.Worktree == nil {
		return errorResult(service.NewContextBuildError(
			service.ContextErrorBuildFailed,
			"context claim services are unavailable",
			nil,
		)), nil
	}

	task, claimCreated, err := services.Task.GetNextTaskForContext(ctx, projectID, sessionID, role, workerID)
	if err != nil {
		return errorResult(fmt.Errorf("no task available: %w", err)), nil
	}

	// A claim is usable only when all required dependency and API sources can be
	// assembled. Any failure revokes the already-committed authority before an
	// MCP error is returned; a bare Task is never a success fallback.
	taskCtx, contextErr := services.Context.GetTaskContext(ctx, projectID, task.ID)
	if contextErr != nil {
		return rejectClaimAfterContextFailure(
			ctx, services, task, sessionID, workerID, claimCreated, normalizeContextBuildError(contextErr),
		), nil
	}

	data, err := json.Marshal(taskCtx)
	if err != nil {
		contextErr = service.NewContextBuildError(
			service.ContextErrorBuildFailed,
			"task context could not be encoded",
			err,
		)
		return rejectClaimAfterContextFailure(ctx, services, task, sessionID, workerID, claimCreated, contextErr), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func normalizeContextBuildError(err error) error {
	var contextErr *service.ContextBuildError
	if errors.As(err, &contextErr) {
		return contextErr
	}
	return service.NewContextBuildError(
		service.ContextErrorBuildFailed,
		"task context construction failed",
		err,
	)
}

func rejectClaimAfterContextFailure(
	ctx context.Context,
	services *Services,
	task *model.Task,
	sessionID, workerID string,
	discardUndeliveredWorktree bool,
	contextErr error,
) *mcp.CallToolResult {
	contextCode := service.ContextBuildErrorCode(contextErr)
	if err := services.Task.CompensateContextFailure(
		ctx, task, sessionID, workerID, "", contextCode, discardUndeliveredWorktree,
	); err != nil {
		return errorResult(service.NewContextBuildError(
			service.ContextErrorCompensationFailed,
			"context failed and execution authority could not be fully revoked",
			errors.Join(contextErr, err),
		))
	}

	if !discardUndeliveredWorktree {
		return errorResult(contextErr)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	if err := services.Worktree.CleanupPendingWorktree(cleanupCtx, task.ProjectID, task.ID); err != nil {
		return errorResult(service.NewContextBuildError(
			service.ContextErrorCleanupPending,
			"context failed; execution authority was revoked and workspace cleanup remains pending",
			errors.Join(contextErr, err),
		))
	}
	return errorResult(contextErr)
}

// registerHeartbeatTask renews only the caller's current durable Task Lease.
// The M0 stdio compatibility transport still carries local scope explicitly;
// M1 derives these fields from the authenticated Runner connection.
func registerHeartbeatTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("heartbeat_task",
			mcp.WithDescription("Renew an active Task Lease using its epoch-bound lease ID and version. Expired, stale, replay-mismatched, or non-owner heartbeats fail closed."),
			mcp.WithString("project_id", mcp.Required(), mcp.Description("M0 local project ID")),
			mcp.WithString("task_id", mcp.Required(), mcp.Description("Task ID owned by the Lease")),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Owning local Session ID")),
			mcp.WithString("worker_id", mcp.Required(), mcp.Description("Owning Worker ID")),
			mcp.WithString("lease_id", mcp.Required(), mcp.Description("Active Lease UUID")),
			mcp.WithNumber("lease_version", mcp.Required(), mcp.Description("Current Lease CAS version")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
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
			sessionID, err := req.RequireString("session_id")
			if err != nil {
				return errorResult(err), nil
			}
			workerID, err := req.RequireString("worker_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseID, err := req.RequireString("lease_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseVersionFloat, err := req.RequireFloat("lease_version")
			if err != nil {
				return errorResult(err), nil
			}
			leaseVersion := int64(leaseVersionFloat)
			if leaseVersionFloat != float64(leaseVersion) || leaseVersion < 0 {
				return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "lease_version must be a non-negative integer"}), nil
			}
			idempotencyKey, err := req.RequireString("idempotency_key")
			if err != nil {
				return errorResult(err), nil
			}

			lease, err := services.Task.HeartbeatTask(
				ctx, projectID, taskID, sessionID, workerID,
				leaseID, leaseVersion, idempotencyKey,
			)
			if err != nil {
				return errorResult(fmt.Errorf("heartbeat rejected: %w", err)), nil
			}
			payload, err := json.Marshal(map[string]any{
				"task_id": taskID, "lease_id": lease.ID,
				"lease_version": lease.Version, "expires_at": lease.ExpiresAt,
			})
			if err != nil {
				return nil, fmt.Errorf("marshal heartbeat result: %w", err)
			}
			return mcp.NewToolResultText(string(payload)), nil
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

			return mcp.NewToolResultText(fmt.Sprintf(`{"task_id":"%s","status":"validating","validation":"passed"}`, taskID)), nil
		},
	)
}

// registerReportBlocker adds the report_blocker tool.
func registerReportBlocker(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("report_blocker",
			mcp.WithDescription("Report that a task is blocked. The task transitions from executing to blocked."),
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
