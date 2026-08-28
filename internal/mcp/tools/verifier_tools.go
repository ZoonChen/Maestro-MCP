package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	mcp "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// RegisterVerifierTools registers the v3 verification surface. Final merge
// remains human-only in GitLab and is deliberately absent here.
func RegisterVerifierTools(s *mcpserver.MCPServer, services *Services) {
	registerGetVerificationTask(s, services)
	registerSubmitVerification(s, services)
}

// registerGetVerificationTask adds the get_verification_task tool in its
// frozen v3 shape: idempotency key + queue CAS token; the verifier identity
// is the bound session (its registered role must be verifier) and the
// assigned executor recorded on the work item is never reassigned.
func registerGetVerificationTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("get_verification_task",
			mcp.WithDescription("Atomically lease the next eligible verification task for an independent verifier."),
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
			idempotencyKey, err := req.RequireString("idempotency_key")
			if err != nil {
				return errorResult(err), nil
			}
			if !claimIdempotencyKeyPattern.MatchString(idempotencyKey) {
				return errorResult(fmt.Errorf("idempotency_key: %w", store.ErrInvalidParameter)), nil
			}
			queueVersion, err := requireIntegerArg(req, "queue_version")
			if err != nil || queueVersion < 0 {
				return errorResult(fmt.Errorf("queue_version: %w", store.ErrInvalidParameter)), nil //nolint:nilerr // parameter validation failed, not the transport
			}
			projectID, sessionID, workerID, err := services.Binding.scope()
			if err != nil {
				return errorResult(err), nil
			}
			if err := queueTokenCheck(ctx, services, projectID, queueVersion); err != nil {
				return errorResult(err), nil
			}
			// The bound session's REGISTERED role is the eligibility
			// authority; the service transaction re-checks it.
			if err := verifySessionRole(ctx, services, projectID, sessionID, model.RoleVerifier); err != nil {
				return errorResult(err), nil
			}

			task, err := services.Task.GetVerificationTask(ctx, projectID, sessionID, workerID)
			if err != nil {
				return errorResult(fmt.Errorf("no verification task available: %w", err)), nil
			}
			history, histErr := services.Validation.GetValidationHistory(ctx, projectID, task.ID)
			if histErr != nil {
				return errorResult(fmt.Errorf("verification evidence unavailable: %w", histErr)), nil
			}

			leaseID, leaseVersion, leaseEpoch, err := services.Task.ActiveLeaseSnapshot(ctx, projectID, task.ID)
			if err != nil {
				return errorResult(fmt.Errorf("verification lease unavailable: %w", err)), nil
			}
			payload, err := json.Marshal(map[string]any{
				"work_item_id":       task.ID,
				"lease_id":           leaseID,
				"lease_version":      leaseVersion,
				"lease_epoch":        leaseEpoch,
				"queue_version":      queueVersion,
				"submitted_by":       task.AssignedSessionID,
				"validation_history": history,
			})
			if err != nil {
				return nil, fmt.Errorf("marshal verification claim: %w", err)
			}
			return mcp.NewToolResultText(string(payload)), nil
		},
	)
}

// registerSubmitVerification adds the submit_verification tool in its
// frozen v3 shape: the verdict is bound to the verification lease; an
// approval requires the server's own immutable evidence (zero trust).
func registerSubmitVerification(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("submit_verification",
			mcp.WithDescription("Submit a verification verdict for a leased validation. An approval requires the latest exact immutable evidence; a rejection fails the work item for explicit recovery."),
			mcp.WithString("work_item_id", mcp.Required(), mcp.Description("Work item under verification")),
			mcp.WithString("verification_lease_id", mcp.Required(), mcp.Description("Active verification Lease UUID")),
			mcp.WithNumber("lease_version", mcp.Required(), mcp.Description("Verification lease CAS version")),
			mcp.WithString("decision", mcp.Required(), mcp.Description("approved, rejected or needs_human"),
				mcp.Enum("approved", "rejected", "needs_human")),
			mcp.WithString("summary", mcp.Required(), mcp.Description("Verifier summary")),
			mcp.WithArray("evidence_refs", mcp.Required(), mcp.Description("1-100 evidence references")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			workItemID, err := req.RequireString("work_item_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseID, err := req.RequireString("verification_lease_id")
			if err != nil {
				return errorResult(err), nil
			}
			leaseVersion, err := requireLeaseVersion(req, "lease_version")
			if err != nil {
				return errorResult(err), nil
			}
			decision, err := req.RequireString("decision")
			if err != nil {
				return errorResult(err), nil
			}
			summary, err := req.RequireString("summary")
			if err != nil {
				return errorResult(err), nil
			}
			if summary == "" {
				return errorResult(fmt.Errorf("summary: %w", store.ErrInvalidParameter)), nil
			}
			if _, err := requireStringSlice(req, "evidence_refs", 1, 100); err != nil {
				return errorResult(err), nil
			}
			_, projectID, sessionID, workerID, err := requireLeaseContext(req, services)
			if err != nil {
				return errorResult(err), nil
			}

			if err := verifyLeaseForBoundSession(ctx, services, projectID, workItemID, leaseID, leaseVersion, sessionID); err != nil {
				return errorResult(fmt.Errorf("verdict rejected: %w", err)), nil
			}

			var passed bool
			var resulting string
			switch decision {
			case "approved":
				passed = true
				resulting = model.TaskStatusReadyForHumanMerge
			case "rejected":
				resulting = model.TaskStatusFailed
			case "needs_human":
				resulting = model.TaskStatusNeedsHuman
			default:
				return errorResult(fmt.Errorf("decision: %w", store.ErrInvalidParameter)), nil
			}
			physicalSession, _ := services.Session.GetSession(ctx, projectID, sessionID)
			if err := services.Task.SubmitVerification(ctx, projectID, physicalSession.ID, workerID, workItemID, passed, summary); err != nil {
				return errorResult(fmt.Errorf("verification verdict failed: %w", err)), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"work_item_id":%q,"decision":%q,"status":%q}`,
				workItemID, decision, resulting)), nil
		},
	)
}
