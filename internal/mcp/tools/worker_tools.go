package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
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

// claimIdempotencyKeyPattern mirrors the frozen tools.schema.json rule:
// 16-128 characters from the printable key alphabet.
var claimIdempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)

// requireIntegerArg reads a strict integer argument. JSON-RPC decoding
// yields float64, direct in-process calls often carry int64/int; anything
// else (or a fractional value) is an invalid parameter.
func requireIntegerArg(req mcp.CallToolRequest, name string) (int64, error) {
	raw, ok := req.GetArguments()[name]
	if !ok {
		return 0, fmt.Errorf("argument %q is required", name)
	}
	switch value := raw.(type) {
	case float64:
		if value != math.Trunc(value) {
			return 0, fmt.Errorf("argument %q must be an integer", name)
		}
		return int64(value), nil
	case int64:
		return value, nil
	case int:
		return int64(value), nil
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("argument %q must be an integer", name)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("argument %q must be an integer", name)
	}
}

// claimOutcome is the frozen claim response contract (tools.schema.json
// output_schema): lease identity and fencing fields plus the precise
// worktree path the caller must operate in.
type claimOutcome struct {
	WorkItemID   string `json:"work_item_id"`
	LeaseID      string `json:"lease_id"`
	LeaseVersion int64  `json:"lease_version"`
	LeaseEpoch   int64  `json:"lease_epoch"`
	QueueVersion int64  `json:"queue_version"`
	WorktreePath string `json:"worktree_path"`
}

// registerGetNextTask adds the get_next_task tool in its frozen v3 shape:
// the only inputs are the mandatory idempotency key and queue-version CAS
// token (capabilities are advisory); project scope and session identity
// come from the server-side TransportBinding and can never be self-reported
// by the caller.
func registerGetNextTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("get_next_task",
			mcp.WithDescription("Atomically lease the next eligible execution task from the authorized queue."),
			mcp.WithString("idempotency_key",
				mcp.Required(),
				mcp.Description("16-128 character replay key scoped to principal/project/queue"),
			),
			mcp.WithNumber("queue_version",
				mcp.Required(),
				mcp.Description("Queue CAS token observed by this client; stale tokens conflict"),
			),
			mcp.WithString("capabilities",
				mcp.Description("Caller capability labels used for eligibility filtering"),
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
	idempotencyKey, err := req.RequireString("idempotency_key")
	if err != nil {
		return errorResult(err), nil
	}
	if !claimIdempotencyKeyPattern.MatchString(idempotencyKey) {
		return errorResult(fmt.Errorf("idempotency_key: %w", store.ErrInvalidParameter)), nil
	}
	queueVersion, err := requireIntegerArg(req, "queue_version")
	if err != nil || queueVersion < 0 {
		return errorResult(fmt.Errorf("queue_version: %w", store.ErrInvalidParameter)), nil //nolint:nilerr // the parameter, not the transport, failed validation
	}
	if queueVersion < 0 {
		return errorResult(fmt.Errorf("queue_version: %w", store.ErrInvalidParameter)), nil
	}
	projectID, sessionID, workerID, err := services.Binding.scope()
	if err != nil {
		return errorResult(err), nil
	}

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

	// The session's REGISTERED role is the eligibility authority; the claim
	// transaction enforces role/status/capacity and the queue CAS token.
	session, err := services.Session.GetSession(ctx, projectID, sessionID)
	if err != nil {
		return errorResult(fmt.Errorf("bound session is not registered: %w", err)), nil
	}

	task, claimCreated, err := services.Task.GetNextTaskWithVersion(
		ctx, projectID, sessionID, session.Role, workerID, idempotencyKey, queueVersion,
	)
	if err != nil {
		return errorResult(fmt.Errorf("no task available: %w", err)), nil
	}

	// A claim is usable only when all required dependency and API sources can
	// be assembled. Any failure revokes the already-committed authority before
	// an MCP error is returned; a bare Task is never a success fallback.
	if _, contextErr := services.Context.GetTaskContext(ctx, projectID, task.ID); contextErr != nil {
		return rejectClaimAfterContextFailure(
			ctx, services, task, sessionID, workerID, claimCreated, normalizeContextBuildError(contextErr),
		), nil
	}

	// Response assembly never rewrites context-source error codes: snapshot
	// failures get their own assembly error after compensation.
	leaseID, leaseVersion, leaseEpoch, err := services.Task.ActiveLeaseSnapshot(ctx, projectID, task.ID)
	if err != nil {
		return rejectClaimAfterContextFailure(
			ctx, services, task, sessionID, workerID, claimCreated,
			service.NewContextBuildError(service.ContextErrorBuildFailed, "claim lease snapshot unavailable", err),
		), nil
	}
	worktree, err := services.Worktree.GetWorktreeByTask(ctx, projectID, task.ID)
	if err != nil {
		return rejectClaimAfterContextFailure(
			ctx, services, task, sessionID, workerID, claimCreated,
			service.NewContextBuildError(service.ContextErrorBuildFailed, "claim worktree unavailable", err),
		), nil
	}

	outcome, err := json.Marshal(claimOutcome{
		WorkItemID:   task.ID,
		LeaseID:      leaseID,
		LeaseVersion: leaseVersion,
		LeaseEpoch:   leaseEpoch,
		QueueVersion: queueVersion,
		WorktreePath: worktree.WorktreePath,
	})
	if err != nil {
		return rejectClaimAfterContextFailure(
			ctx, services, task, sessionID, workerID, claimCreated,
			service.NewContextBuildError(service.ContextErrorBuildFailed, "claim response could not be encoded", err),
		), nil
	}
	return mcp.NewToolResultText(string(outcome)), nil
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

// registerHeartbeatTask adds the heartbeat_task tool in its frozen v3
// shape: work_item/lease identity plus the replay key; session and project
// scope come from the server-side TransportBinding.
func registerHeartbeatTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("heartbeat_task",
			mcp.WithDescription("Renew an active execution lease without changing its ownership."),
			mcp.WithString("work_item_id",
				mcp.Required(),
				mcp.Description("Work item owned by the lease"),
			),
			mcp.WithString("lease_id",
				mcp.Required(),
				mcp.Description("Active Lease UUID"),
			),
			mcp.WithNumber("lease_version",
				mcp.Required(),
				mcp.Description("Current Lease CAS version"),
			),
			mcp.WithString("idempotency_key",
				mcp.Required(),
				mcp.Description("16-128 character replay key"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			taskID, err := req.RequireString("work_item_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseID, err := req.RequireString("lease_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseVersion, err := requireIntegerArg(req, "lease_version")
			if err != nil || leaseVersion < 0 {
				return errorResult(fmt.Errorf("lease_version: %w", store.ErrInvalidParameter)), nil //nolint:nilerr // the parameter, not the transport, failed validation
			}
			idempotencyKey, err := req.RequireString("idempotency_key")
			if err != nil {
				return errorResult(err), nil
			}
			projectID, sessionID, workerID, err := services.Binding.scope()
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
				"work_item_id": taskID, "lease_id": lease.ID,
				"lease_version": lease.Version, "expires_at": lease.ExpiresAt,
			})
			if err != nil {
				return nil, fmt.Errorf("marshal heartbeat result: %w", err)
			}
			return mcp.NewToolResultText(string(payload)), nil
		},
	)
}

// registerSubmitTaskResult adds the submit_task_result tool in its frozen
// v3 shape: the submission is lease-bound (work item + lease id + CAS
// version) with mandatory evidence references; identity comes from the
// TransportBinding and the server performs zero-trust validation, never
// trusting the reported commit_sha or evidence content.
func registerSubmitTaskResult(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("submit_task_result",
			mcp.WithDescription("Submit completed work for a leased work item. The server performs zero-trust validation: it runs git diff and executes tests itself; agent-reported commit_sha and evidence are recorded, never trusted."),
			mcp.WithString("work_item_id", mcp.Required(), mcp.Description("Leased work item")),
			mcp.WithString("lease_id", mcp.Required(), mcp.Description("Active Lease UUID")),
			mcp.WithNumber("lease_version", mcp.Required(), mcp.Description("Lease CAS version")),
			mcp.WithString("commit_sha", mcp.Required(), mcp.Description("Reported commit SHA (40-64 hex); server recomputes and does not trust it")),
			mcp.WithArray("evidence_refs", mcp.Required(), mcp.Description("1-100 evidence references")),
			mcp.WithString("summary", mcp.Description("Optional summary of the completed work")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			workItemID, err := req.RequireString("work_item_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseID, err := req.RequireString("lease_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseVersion, err := requireLeaseVersion(req, "lease_version")
			if err != nil {
				return errorResult(err), nil
			}
			commitSHA, err := req.RequireString("commit_sha")
			if err != nil {
				return errorResult(err), nil
			}
			if !commitSHAPattern.MatchString(commitSHA) {
				return errorResult(fmt.Errorf("commit_sha: %w", store.ErrInvalidParameter)), nil
			}
			if _, err := requireStringSlice(req, "evidence_refs", 1, 100); err != nil {
				return errorResult(err), nil
			}
			summaryStr := req.GetString("summary", "")
			_, projectID, sessionID, workerID, err := requireLeaseContext(req, services)
			if err != nil {
				return errorResult(err), nil
			}

			if err := services.Task.VerifyLeaseAuthority(ctx, projectID, workItemID, leaseID, leaseVersion, sessionID); err != nil {
				return errorResult(fmt.Errorf("submission rejected: %w", err)), nil
			}

			var summary *string
			if summaryStr != "" {
				summary = &summaryStr
			}
			if err := services.Validation.SubmitAndValidate(ctx, projectID, workItemID, sessionID, workerID, summary); err != nil {
				return errorResult(fmt.Errorf("submission failed: %w", err)), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"work_item_id":%q,"status":"validating","validation":"in_progress"}`, workItemID)), nil
		},
	)
}

// commitSHAPattern accepts 40 (git-1) or 64 (git-2/sha256) hex characters.
var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// registerReportBlocker adds the report_blocker tool in its frozen v3
// shape: the blocker is lease-bound; identity comes from the binding.
func registerReportBlocker(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("report_blocker",
			mcp.WithDescription("Record an evidenced blocker against the currently leased work item."),
			mcp.WithString("work_item_id", mcp.Required(), mcp.Description("Leased work item")),
			mcp.WithString("lease_id", mcp.Required(), mcp.Description("Active Lease UUID")),
			mcp.WithNumber("lease_version", mcp.Required(), mcp.Description("Lease CAS version")),
			mcp.WithString("reason", mcp.Required(), mcp.Description("Why the work item is blocked")),
			mcp.WithArray("evidence_refs", mcp.Description("Optional evidence references (max 100)")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			workItemID, err := req.RequireString("work_item_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseID, err := req.RequireString("lease_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseVersion, err := requireLeaseVersion(req, "lease_version")
			if err != nil {
				return errorResult(err), nil
			}
			reason, err := req.RequireString("reason")
			if err != nil {
				return errorResult(err), nil
			}
			if reason == "" {
				return errorResult(fmt.Errorf("reason: %w", store.ErrInvalidParameter)), nil
			}
			if _, err := requireStringSlice(req, "evidence_refs", 0, 100); err != nil {
				return errorResult(err), nil
			}
			_, projectID, sessionID, _, err := requireLeaseContext(req, services)
			if err != nil {
				return errorResult(err), nil
			}

			if err := services.Task.VerifyLeaseAuthority(ctx, projectID, workItemID, leaseID, leaseVersion, sessionID); err != nil {
				return errorResult(fmt.Errorf("blocker rejected: %w", err)), nil
			}
			if err := services.Task.ReportBlocker(ctx, projectID, workItemID, sessionID, reason); err != nil {
				return errorResult(fmt.Errorf("failed to report blocker: %w", err)), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"work_item_id":%q,"status":"blocked"}`, workItemID)), nil
		},
	)
}
