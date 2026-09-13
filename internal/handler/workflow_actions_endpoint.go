package handler

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
)

// The W6-3 governance execution surface (S2C-A3): the four operations
// S2B's development slices drove through the store face — claim, execution
// completion, asset signoff and gate binding — become authenticated
// /api/v3 endpoints. The identity layer supplies the actor and the
// caller's ACTIVE functional roles (never a payload field); the frozen
// permission map (controlPlaneActions) gates each route; the store keeps
// every machine invariant. The S2B temporary driver retires on top of
// these endpoints.

// WorkflowActionsStore is the execution surface the endpoints consume.
type WorkflowActionsStore interface {
	RunnerBoundToProject(ctx context.Context, runnerID, projectID string) (bool, error)
	ProjectQueueVersion(ctx context.Context, projectID string) (int64, error)
	ClaimNextWorkItem(ctx context.Context, runnerID, connectionGeneration string, expectedQueueVersion int64, leaseTTL time.Duration) (*store.WorkItemClaim, error)
	CompleteExecution(ctx context.Context, executionID, runnerID, connectionGeneration, outcome string, commitSHA *string, summary string) error
}

// WorkflowAssetsStore is the asset-ledger action surface.
type WorkflowAssetsStore interface {
	GetAsset(ctx context.Context, assetID string, version int) (*store.Asset, error)
	ApproveAsset(ctx context.Context, assetID string, version int, actor string, approverRoles []string) (*store.Asset, error)
	BindAssetGate(ctx context.Context, projectID, workItemID, assetID string, assetVersion int, gateID string, actor string) (*store.AssetGateBinding, error)
}

// WorkflowActionsHandler serves the four governance execution routes.
type WorkflowActionsHandler struct {
	exec   WorkflowActionsStore
	assets WorkflowAssetsStore
}

// NewWorkflowActionsHandler wires the endpoints (nil stores leave the
// surface unexposed — callers mount nothing).
func NewWorkflowActionsHandler(exec WorkflowActionsStore, assets WorkflowAssetsStore) *WorkflowActionsHandler {
	return &WorkflowActionsHandler{exec: exec, assets: assets}
}

// workflowClaimLeaseTTL bounds the governance claim's lease.
const (
	workflowClaimLeaseDefault = 30 * time.Minute
	workflowClaimLeaseMax     = 24 * time.Hour
)

// Claim handles POST /api/v3/projects/:pid/work-items/claim: dispatch at
// most one queued work item to an approved, project-bound runner. The
// caller presents the runner identity and connection generation (the
// fencing contract); the queue token is read server-side unless the
// caller carries one from a prior claim.
func (h *WorkflowActionsHandler) Claim(c *gin.Context) {
	var body struct {
		RunnerID             string `json:"runner_id"`
		ConnectionGeneration string `json:"connection_generation"`
		QueueVersion         int64  `json:"queue_version"`
		QueueVersionPresent  bool   `json:"-"`
		LeaseTTLSeconds      int    `json:"lease_ttl_seconds"`
	}
	raw := struct {
		RunnerID             string `json:"runner_id"`
		ConnectionGeneration string `json:"connection_generation"`
		QueueVersion         *int64 `json:"queue_version"`
		LeaseTTLSeconds      int    `json:"lease_ttl_seconds"`
	}{}
	if err := c.ShouldBindJSON(&raw); err != nil {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Claim body does not match the contract")
		return
	}
	body.RunnerID, body.ConnectionGeneration = raw.RunnerID, raw.ConnectionGeneration
	body.LeaseTTLSeconds = raw.LeaseTTLSeconds
	if raw.QueueVersion != nil {
		body.QueueVersion, body.QueueVersionPresent = *raw.QueueVersion, true
	}
	if body.RunnerID == "" || body.ConnectionGeneration == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER",
			"runner_id and connection_generation are required")
		return
	}
	projectID := c.Param("pid")

	bound, err := h.exec.RunnerBoundToProject(c.Request.Context(), body.RunnerID, projectID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Runner binding could not be read")
		return
	}
	if !bound {
		staticErrorReply(c, http.StatusForbidden, "FORBIDDEN", "Runner is not bound to this project")
		return
	}

	queueVersion := body.QueueVersion
	if !body.QueueVersionPresent {
		queueVersion, err = h.exec.ProjectQueueVersion(c.Request.Context(), projectID)
		if err != nil {
			publicErrorReply(c, err)
			return
		}
	}
	leaseTTL := workflowClaimLeaseDefault
	if body.LeaseTTLSeconds > 0 {
		leaseTTL = time.Duration(body.LeaseTTLSeconds) * time.Second
		if leaseTTL > workflowClaimLeaseMax {
			leaseTTL = workflowClaimLeaseMax
		}
	}

	claim, err := h.exec.ClaimNextWorkItem(c.Request.Context(),
		body.RunnerID, body.ConnectionGeneration, queueVersion, leaseTTL)
	if err != nil {
		switch err {
		case store.ErrNoAvailableTask:
			c.JSON(http.StatusOK, gin.H{"available": false})
		case store.ErrConcurrentConflict:
			staticErrorReply(c, http.StatusConflict, "CONCURRENCY_CONFLICT",
				"Queue token is stale; re-observe and retry")
		case store.ErrRunnerNotFound, store.ErrRunnerNotBound, store.ErrRunnerStatusInvalid:
			staticErrorReply(c, http.StatusForbidden, "FORBIDDEN", "Runner is not eligible to claim")
		default:
			staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Lease dispatch failed")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"available":     true,
		"lease_id":      claim.LeaseID,
		"lease_version": claim.LeaseVersion,
		"execution_id":  claim.ExecutionID,
		"work_item_id":  claim.WorkItemID,
		"project_id":    claim.ProjectID,
		"queue_version": claim.QueueVersion,
		"expires_at":    time.Now().UTC().Add(leaseTTL).Format(time.RFC3339),
	})
}

// Complete handles POST /api/v3/projects/:pid/executions/:execId/complete:
// record the terminal outcome (completed requires the commit SHA) and
// release the lease. outcome=completed moves the work item to validating;
// the gate verdict (W6-1) owns everything after that.
func (h *WorkflowActionsHandler) Complete(c *gin.Context) {
	var body struct {
		RunnerID             string  `json:"runner_id"`
		ConnectionGeneration string  `json:"connection_generation"`
		Outcome              string  `json:"outcome"`
		CommitSHA            *string `json:"commit_sha"`
		Summary              string  `json:"summary"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.RunnerID == "" ||
		body.ConnectionGeneration == "" || body.Outcome == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER",
			"runner_id, connection_generation and outcome are required")
		return
	}
	if body.Outcome == "completed" && (body.CommitSHA == nil || *body.CommitSHA == "") {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER",
			"A completed execution must carry its commit_sha")
		return
	}
	err := h.exec.CompleteExecution(c.Request.Context(), c.Param("execId"),
		body.RunnerID, body.ConnectionGeneration, body.Outcome, body.CommitSHA, body.Summary)
	if err != nil {
		switch err {
		case store.ErrLeaseNotFound:
			staticErrorReply(c, http.StatusGone, "LEASE_EXPIRED", "The execution or its lease no longer exists")
		case store.ErrRunnerGenerationStale:
			staticErrorReply(c, http.StatusConflict, "LEASE_VERSION_MISMATCH",
				"The lease was fenced by a newer generation")
		case store.ErrInvalidParameter:
			staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Unknown outcome")
		default:
			publicErrorReply(c, err)
		}
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"execution_id": c.Param("execId"), "outcome": body.Outcome})
}

// ApproveAsset handles POST /api/v3/projects/:pid/assets/:assetId/versions/:version/approve:
// the W5-2 multi-sign signoff. The caller's ACTIVE functional roles ride
// from the server-side principal (the same source the MCP tool uses);
// the route's project must own the asset.
func (h *WorkflowActionsHandler) ApproveAsset(c *gin.Context) {
	version, parseErr := strconv.Atoi(c.Param("version"))
	if parseErr != nil || version < 1 {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "version must be a positive integer")
		return
	}
	assetID := c.Param("assetId")
	projectID := c.Param("pid")

	current, err := h.assets.GetAsset(c.Request.Context(), assetID, version)
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	if current.ProjectID != projectID {
		// Resource hiding: another project's asset is indistinguishable
		// from absence.
		staticErrorReply(c, http.StatusNotFound, "ASSET_NOT_FOUND", "Asset not found")
		return
	}

	principal := PrincipalFromContext(c)
	if principal == nil {
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}
	asset, err := h.assets.ApproveAsset(c.Request.Context(), assetID, version,
		"user:"+principal.PrincipalID, principal.FunctionalRoles)
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"asset_id": asset.AssetID, "version": asset.Version, "status": asset.Status,
		"required_approver_roles": asset.RequiredApproverRoles,
	})
}

// BindGate handles POST /api/v3/projects/:pid/work-items/:wid/gate-bindings:
// pin a work item's gate to an approved ledger version (WGM-INV-015's
// consumption contract). The actor is the authenticated principal.
func (h *WorkflowActionsHandler) BindGate(c *gin.Context) {
	var body struct {
		AssetID      string `json:"asset_id"`
		AssetVersion int    `json:"asset_version"`
		GateID       string `json:"gate_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.AssetID == "" ||
		body.AssetVersion < 1 || body.GateID == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER",
			"asset_id, asset_version (>= 1) and gate_id are required")
		return
	}
	principal := PrincipalFromContext(c)
	if principal == nil {
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}
	binding, err := h.assets.BindAssetGate(c.Request.Context(), c.Param("pid"), c.Param("wid"),
		body.AssetID, body.AssetVersion, body.GateID, "user:"+principal.PrincipalID)
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"binding_id":   binding.ID,
		"work_item_id": binding.WorkItemID,
		"asset":        binding.AssetID + "@" + strconv.Itoa(binding.BoundVersion),
		"gate":         binding.GateID,
		"status":       binding.Status,
	})
}
