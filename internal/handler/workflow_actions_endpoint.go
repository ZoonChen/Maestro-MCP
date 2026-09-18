package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
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
	ClaimTargetedWorkItem(ctx context.Context, runnerID, connectionGeneration, targetWorkItemID string, expectedQueueVersion int64, leaseTTL time.Duration) (*store.WorkItemClaim, error)
	CompleteExecution(ctx context.Context, executionID, runnerID, connectionGeneration, outcome string, commitSHA *string, summary string) error
	ReleaseExecution(ctx context.Context, executionID, runnerID, connectionGeneration, reason, actor string) error
}

// WorkflowValidationStore is the W7-3 (F15) validation-run reporting
// surface the boundary gate reads from.
type WorkflowValidationStore interface {
	ReportValidationRun(ctx context.Context, projectID, workItemID, idempotencyKey string, run store.ReportedValidationRun) (store.StoredValidationRun, bool, error)
}

// WorkflowAssetsStore is the asset-ledger action surface.
type WorkflowAssetsStore interface {
	GetAsset(ctx context.Context, assetID string, version int) (*store.Asset, error)
	ApproveAsset(ctx context.Context, assetID string, version int, actor string, approverRoles []string) (*store.Asset, error)
	BindAssetGate(ctx context.Context, projectID, workItemID, assetID string, assetVersion int, gateID string, actor string) (*store.AssetGateBinding, error)
}

// WorkflowActionsHandler serves the governance execution routes.
type WorkflowActionsHandler struct {
	exec       WorkflowActionsStore
	assets     WorkflowAssetsStore
	validation WorkflowValidationStore
}

// NewWorkflowActionsHandler wires the endpoints (nil stores leave the
// surface unexposed — callers mount nothing).
func NewWorkflowActionsHandler(exec WorkflowActionsStore, assets WorkflowAssetsStore, validation WorkflowValidationStore) *WorkflowActionsHandler {
	return &WorkflowActionsHandler{exec: exec, assets: assets, validation: validation}
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
// caller carries one from a prior claim. The optional work_item_id is
// the W7-1 (F29) targeting guard: when present and the dispatch head is
// a different item, the claim refuses (409 CLAIM_TARGET_MISMATCH)
// instead of silently mis-dispatching.
func (h *WorkflowActionsHandler) Claim(c *gin.Context) {
	var body struct {
		RunnerID             string `json:"runner_id"`
		ConnectionGeneration string `json:"connection_generation"`
		QueueVersion         int64  `json:"queue_version"`
		QueueVersionPresent  bool   `json:"-"`
		LeaseTTLSeconds      int    `json:"lease_ttl_seconds"`
		TargetWorkItemID     string `json:"work_item_id"`
	}
	raw := struct {
		RunnerID             string `json:"runner_id"`
		ConnectionGeneration string `json:"connection_generation"`
		QueueVersion         *int64 `json:"queue_version"`
		LeaseTTLSeconds      int    `json:"lease_ttl_seconds"`
		TargetWorkItemID     string `json:"work_item_id"`
	}{}
	if err := c.ShouldBindJSON(&raw); err != nil {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Claim body does not match the contract")
		return
	}
	body.RunnerID, body.ConnectionGeneration = raw.RunnerID, raw.ConnectionGeneration
	body.LeaseTTLSeconds = raw.LeaseTTLSeconds
	body.TargetWorkItemID = raw.TargetWorkItemID
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

	var claim *store.WorkItemClaim
	if body.TargetWorkItemID != "" {
		claim, err = h.exec.ClaimTargetedWorkItem(c.Request.Context(),
			body.RunnerID, body.ConnectionGeneration, body.TargetWorkItemID, queueVersion, leaseTTL)
	} else {
		claim, err = h.exec.ClaimNextWorkItem(c.Request.Context(),
			body.RunnerID, body.ConnectionGeneration, queueVersion, leaseTTL)
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNoAvailableTask):
			c.JSON(http.StatusOK, gin.H{"available": false})
		case errors.Is(err, store.ErrConcurrentConflict):
			staticErrorReply(c, http.StatusConflict, "CONCURRENCY_CONFLICT",
				"Queue token is stale; re-observe and retry")
		case errors.Is(err, store.ErrClaimTargetMismatch):
			staticErrorReply(c, http.StatusConflict, "CLAIM_TARGET_MISMATCH",
				"The queue head is a different work item; refusing to claim another item (no silent mis-dispatch)")
		case errors.Is(err, store.ErrRunnerNotFound), errors.Is(err, store.ErrRunnerNotBound), errors.Is(err, store.ErrRunnerStatusInvalid):
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
// the gate verdict (W6-1) owns everything after that. The W7-2 (F32)
// server-side fence: a completed SHA must be a full 40-hex Git object
// name and must agree with the branch head the platform already
// projected for the work item (COMMIT_SHA_MISMATCH otherwise).
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
	if body.CommitSHA != nil && *body.CommitSHA != "" && !workflowCommitSHAPattern.MatchString(*body.CommitSHA) {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER",
			"commit_sha must be a full 40-character hex Git object name")
		return
	}
	err := h.exec.CompleteExecution(c.Request.Context(), c.Param("execId"),
		body.RunnerID, body.ConnectionGeneration, body.Outcome, body.CommitSHA, body.Summary)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrLeaseNotFound):
			staticErrorReply(c, http.StatusGone, "LEASE_EXPIRED", "The execution or its lease no longer exists")
		case errors.Is(err, store.ErrRunnerGenerationStale):
			staticErrorReply(c, http.StatusConflict, "LEASE_VERSION_MISMATCH",
				"The lease was fenced by a newer generation")
		case errors.Is(err, store.ErrInvalidParameter):
			staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Unknown outcome")
		case errors.Is(err, store.ErrCommitSHAMismatch):
			staticErrorReply(c, http.StatusBadRequest, "COMMIT_SHA_MISMATCH",
				"commit_sha does not match the branch head the platform projected for this work item")
		default:
			publicErrorReply(c, err)
		}
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"execution_id": c.Param("execId"), "outcome": body.Outcome})
}

// workflowCommitSHAPattern is the F32 shape fence: Git object names are
// exactly 40 lowercase-or-uppercase hex characters (SHA-1). Truncated
// SHAs (the 8-char form Git prints for humans) are refused.
var workflowCommitSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// Release handles POST /api/v3/projects/:pid/executions/:execId/release:
// the W7-1 (F29) give-back face. The lease-owning connection returns a
// mis-claimed or abandoned execution before any terminal outcome; the
// work item re-enters the queue at the head of its priority band and
// the queue CAS token advances, so the next claim observes it first.
func (h *WorkflowActionsHandler) Release(c *gin.Context) {
	var body struct {
		RunnerID             string `json:"runner_id"`
		ConnectionGeneration string `json:"connection_generation"`
		Reason               string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.RunnerID == "" ||
		body.ConnectionGeneration == "" || len(body.Reason) < 8 || len(body.Reason) > 2000 {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER",
			"runner_id, connection_generation and a reason of 8-2000 characters are required")
		return
	}
	principal := PrincipalFromContext(c)
	if principal == nil {
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}
	actor := "user:" + principal.PrincipalID
	err := h.exec.ReleaseExecution(c.Request.Context(), c.Param("execId"),
		body.RunnerID, body.ConnectionGeneration, body.Reason, actor)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrLeaseNotFound):
			staticErrorReply(c, http.StatusGone, "LEASE_EXPIRED", "The execution or its lease no longer exists")
		case errors.Is(err, store.ErrRunnerGenerationStale):
			staticErrorReply(c, http.StatusConflict, "LEASE_VERSION_MISMATCH",
				"The lease was fenced by a newer generation")
		case errors.Is(err, store.ErrConcurrentConflict):
			staticErrorReply(c, http.StatusConflict, "CONCURRENCY_CONFLICT",
				"The work item left executing concurrently; re-observe its state")
		default:
			publicErrorReply(c, err)
		}
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"execution_id": c.Param("execId"), "outcome": "released"})
}

// ReportValidationRun handles POST /api/v3/projects/:pid/work-items/:wid/
// validation-runs — the W7-3 (F15) reporting face for the boundary
// gate's missing writer. The Idempotency-Key header is required: the
// same key replays the same row (200); a new key mints the next
// attempt. This replaces the per-slice psql backfill the S2B sessions
// performed since S2B3.
func (h *WorkflowActionsHandler) ReportValidationRun(c *gin.Context) {
	if h.validation == nil {
		staticErrorReply(c, http.StatusServiceUnavailable, "CONNECTOR_NOT_CONFIGURED",
			"Validation-run reporting is not configured")
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > 200 {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER",
			"An Idempotency-Key header of 1-200 characters is required")
		return
	}
	var body struct {
		ProfileRef   string          `json:"profile_ref"`
		BaseCommit   string          `json:"base_commit"`
		SourceCommit string          `json:"source_commit"`
		ChangedFiles json.RawMessage `json:"changed_files"`
		DurationMS   int64           `json:"duration_ms"`
		BoundaryOK   bool            `json:"boundary_ok"`
		TestOK       bool            `json:"test_ok"`
		CoverageOK   bool            `json:"coverage_ok"`
		Result       string          `json:"result"`
		Producer     string          `json:"producer"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.ProfileRef == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER",
			"profile_ref is required (the boundary gate reads it)")
		return
	}
	if body.DurationMS < 0 {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "duration_ms must be >= 0")
		return
	}
	if len(body.ChangedFiles) > 0 && !json.Valid(body.ChangedFiles) {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "changed_files must be a JSON array")
		return
	}
	stored, created, err := h.validation.ReportValidationRun(c.Request.Context(),
		c.Param("pid"), c.Param("wid"), idempotencyKey, store.ReportedValidationRun{
			ProfileRef:   body.ProfileRef,
			BaseCommit:   body.BaseCommit,
			SourceCommit: body.SourceCommit,
			ChangedFiles: body.ChangedFiles,
			DurationMS:   body.DurationMS,
			BoundaryOK:   body.BoundaryOK,
			TestOK:       body.TestOK,
			CoverageOK:   body.CoverageOK,
			Result:       body.Result,
			Producer:     body.Producer,
		})
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			staticErrorReply(c, http.StatusNotFound, "NOT_FOUND", "Work item not found")
			return
		}
		publicErrorReply(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, gin.H{
		"id":       stored.ID,
		"attempt":  stored.Attempt,
		"created":  created,
		"replayed": !created,
	})
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
