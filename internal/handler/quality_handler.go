package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
)

// Quality REST surface of the frozen control-plane.yaml Quality tag
// (M2-QG-001): effective-policy read/strengthen, exact-SHA gate
// snapshots and immutable evidence reads, and the waiver lifecycle.
// Every route rides the SAME /api/v1 authorize(principal, action,
// resource) decision as the rest of the human surface — the routeAction
// map carries the frozen permissions (quality.read,
// project_policy.strengthen, waiver.request, waiver.approve,
// waiver.revoke).
//
// waiver.approve is granted by the frozen matrix only to functional
// approvers (security_owner / qa_owner with category-matching
// conditions). The identity layer does not model functional roles yet,
// so today the middleware denies every human principal on that route —
// the correct fail-closed decision, not a bug: once functional-role
// identity lands, the same mapping starts allowing it. The
// separation-of-duties inside the store (approver ≠ requester in the
// WHERE clause) is enforced regardless.

// QualityStore is the handler's persistence contract, satisfied by the
// PostgreSQL quality store.
type QualityStore interface {
	GetProjectPolicy(ctx context.Context, projectID string) (*evidence.Policy, int64, error)
	PutProjectPolicy(ctx context.Context, projectID string, policy *evidence.Policy, expectedRowVersion int64) (int64, error)
	ListGateSnapshots(ctx context.Context, projectID, workItemID string) ([]evidence.StoredSnapshot, error)
	ListEvidenceForWorkItem(ctx context.Context, projectID, workItemID string) ([]evidence.Record, error)
	GateSnapshotByID(ctx context.Context, projectID, gateID string) (*evidence.StoredSnapshot, bool, error)
	WaiverByID(ctx context.Context, projectID, waiverID string) (*evidence.Waiver, bool, error)
	WorkItemExists(ctx context.Context, projectID, workItemID string) (bool, error)
	CreateWaiver(ctx context.Context, waiver *evidence.Waiver, projectID, workItemID string) (string, error)
	ApproveWaiver(ctx context.Context, waiverID, approverID string) error
	RevokeWaiver(ctx context.Context, waiverID string) error
}

// QualityHandler serves the quality endpoints.
type QualityHandler struct {
	store   QualityStore
	company *evidence.Policy
}

// NewQualityHandler builds the handler with the embedded company
// baseline; the overlay store is the PostgreSQL quality surface.
func NewQualityHandler(qualityStore QualityStore) (*QualityHandler, error) {
	company, err := evidence.CompanyPolicy()
	if err != nil {
		return nil, err
	}
	return &QualityHandler{store: qualityStore, company: company}, nil
}

// GetQualityPolicy answers the effective inherited policy and its
// provenance (getEffectiveQualityPolicy). The ETag carries the overlay
// row version; without an overlay no version exists to match, so the
// response omits the header and the PUT create path uses If-None-Match.
func (h *QualityHandler) GetQualityPolicy(c *gin.Context) {
	projectID := c.Param("pid")
	overlay, rowVersion, err := h.store.GetProjectPolicy(c.Request.Context(), projectID)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "quality policy get failed", "error", err.Error())
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Effective policy could not be resolved")
		return
	}
	resolved, err := evidence.ResolveEffective(h.company, overlay)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "quality policy resolve failed", "error", err.Error())
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Effective policy could not be resolved")
		return
	}
	if overlay != nil {
		c.Header("ETag", quoteVersion(rowVersion))
	}
	c.JSON(http.StatusOK, gin.H{
		"project_id":       projectID,
		"effective_policy": resolved.Policy,
		"policy_digest":    resolved.PolicyDigest,
		"provenance":       resolved.Provenance,
	})
}

// PutQualityPolicy replaces the project overlay, which may only
// strengthen the company baseline (putProjectQualityPolicy). Optimistic
// concurrency: If-None-Match:* creates, If-Match:"N" replaces; same
// semver with different content is a conflict.
func (h *QualityHandler) PutQualityPolicy(c *gin.Context) {
	projectID := c.Param("pid")
	if c.GetHeader("Idempotency-Key") == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A valid Idempotency-Key is required")
		return
	}
	raw, err := c.GetRawData()
	if err != nil {
		staticErrorReply(c, http.StatusBadRequest, "BODY_UNREADABLE", "Policy body could not be read")
		return
	}
	policy, err := evidence.ParsePolicy(raw)
	if err != nil {
		staticErrorReply(c, http.StatusBadRequest, "POLICY_INVALID", "Policy body does not match the frozen schema")
		return
	}
	if policy.Scope != "project" {
		staticErrorReply(c, http.StatusBadRequest, "POLICY_INVALID", "Project policy must use scope=project")
		return
	}

	// Resolve BEFORE persisting: weakening answers 422 and never touches
	// the stored row.
	resolved, err := evidence.ResolveEffective(h.company, policy)
	if err != nil {
		var weakened *evidence.ErrPolicyWeakened
		if errors.As(err, &weakened) {
			staticErrorReply(c, http.StatusUnprocessableEntity, "POLICY_WEAKENED", weakened.Reason)
			return
		}
		staticErrorReply(c, http.StatusBadRequest, "POLICY_INVALID", "Policy could not be resolved against the company baseline")
		return
	}

	created := false
	var expected int64
	if noneMatch := c.GetHeader("If-None-Match"); strings.TrimSpace(noneMatch) == "*" {
		created = true
	} else if match, ok := parseIfMatch(c.GetHeader("If-Match")); ok {
		expected = match
	} else {
		staticErrorReply(c, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED", "If-Match or If-None-Match:* is required")
		return
	}

	current, _, err := h.store.GetProjectPolicy(c.Request.Context(), projectID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Policy could not be read")
		return
	}
	if current != nil && !created && current.Version == policy.Version {
		// Same semver with different content is rejected outright
		// (QUAL-QUALITY-POLICY section 8); identical content replays.
		if currentDigest, digestErr := current.Digest(); digestErr == nil {
			if newDigest, newErr := policy.Digest(); newErr == nil && currentDigest != newDigest {
				staticErrorReply(c, http.StatusConflict, "POLICY_VERSION_CONFLICT", "Policy version already exists with different content")
				return
			}
		}
	}
	if !created && current == nil {
		staticErrorReply(c, http.StatusPreconditionFailed, "PRECONDITION_FAILED", "No project policy exists to replace")
		return
	}

	rowVersion, err := h.store.PutProjectPolicy(c.Request.Context(), projectID, policy, expected)
	if errors.Is(err, store.ErrQualityPolicyConflict) {
		staticErrorReply(c, http.StatusPreconditionFailed, "PRECONDITION_FAILED", "Policy row version mismatch")
		return
	}
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Policy could not be stored")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.Header("ETag", quoteVersion(rowVersion))
	c.JSON(status, gin.H{
		"project_id":       projectID,
		"effective_policy": resolved.Policy,
		"policy_digest":    resolved.PolicyDigest,
		"provenance":       resolved.Provenance,
	})
}

// ListWorkItemGates lists the stored gate snapshots, each row bound to
// its own exact SHA tuple and policy version (listWorkItemGates).
func (h *QualityHandler) ListWorkItemGates(c *gin.Context) {
	projectID, workItemID := c.Param("pid"), c.Param("wid")
	exists, err := h.store.WorkItemExists(c.Request.Context(), projectID, workItemID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gates could not be listed")
		return
	}
	if !exists {
		staticErrorReply(c, http.StatusNotFound, "WORK_ITEM_NOT_FOUND", "Work item is unknown in this project")
		return
	}
	snapshots, err := h.store.ListGateSnapshots(c.Request.Context(), projectID, workItemID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Gates could not be listed")
		return
	}
	gates := make([]gin.H, 0, len(snapshots))
	for _, snapshot := range snapshots {
		gates = append(gates, gin.H{
			"id":             snapshot.GateID,
			"project_id":     projectID,
			"work_item_id":   snapshot.WorkItemID,
			"check":          snapshot.Check,
			"required":       true,
			"state":          snapshot.Status,
			"source_sha":     snapshot.SourceSHA,
			"target_sha":     snapshot.TargetSHA,
			"policy_version": snapshot.PolicyVersion,
			"version":        snapshot.Version,
		})
	}
	c.JSON(http.StatusOK, gates)
}

// ListWorkItemEvidence lists the immutable evidence records
// (listWorkItemEvidence), each carrying its authority and SHA tuple.
func (h *QualityHandler) ListWorkItemEvidence(c *gin.Context) {
	projectID, workItemID := c.Param("pid"), c.Param("wid")
	exists, err := h.store.WorkItemExists(c.Request.Context(), projectID, workItemID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Evidence could not be listed")
		return
	}
	if !exists {
		staticErrorReply(c, http.StatusNotFound, "WORK_ITEM_NOT_FOUND", "Work item is unknown in this project")
		return
	}
	records, err := h.store.ListEvidenceForWorkItem(c.Request.Context(), projectID, workItemID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Evidence could not be listed")
		return
	}
	for index := range records {
		records[index].SchemaVersion = "3.0"
	}
	c.JSON(http.StatusOK, records)
}

type waiverRequestBody struct {
	SourceSHA       string `json:"source_sha"`
	MergeRequestIID int64  `json:"merge_request_iid"`
	Check           string `json:"check"`
	Reason          string `json:"reason"`
	ExpiresAt       string `json:"expires_at"`
}

// RequestGateWaiver creates a time-limited waiver request bound to one
// gate snapshot (requestGateWaiver): the If-Match must carry the gate's
// current version, and the body's SHA and check must match the gate row
// — a waiver may never bind a drifted tuple.
func (h *QualityHandler) RequestGateWaiver(c *gin.Context) {
	projectID, gateID := c.Param("pid"), c.Param("gid")
	if c.GetHeader("Idempotency-Key") == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A valid Idempotency-Key is required")
		return
	}
	snapshot, found, err := h.store.GateSnapshotByID(c.Request.Context(), projectID, gateID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Waiver target could not be resolved")
		return
	}
	if !found {
		staticErrorReply(c, http.StatusNotFound, "GATE_NOT_FOUND", "Gate is unknown in this project")
		return
	}
	match, ok := parseIfMatch(c.GetHeader("If-Match"))
	if !ok {
		staticErrorReply(c, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED", "If-Match with the gate version is required")
		return
	}
	if match != snapshot.Version {
		staticErrorReply(c, http.StatusPreconditionFailed, "PRECONDITION_FAILED", "Gate version mismatch")
		return
	}

	var body waiverRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Waiver body does not match the contract")
		return
	}
	if body.SourceSHA != snapshot.SourceSHA || body.Check != snapshot.Check {
		staticErrorReply(c, http.StatusUnprocessableEntity, "WAIVER_TUPLE_MISMATCH", "Waiver must bind the gate's exact SHA and check")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "expires_at must be RFC3339")
		return
	}

	principal := PrincipalFromContext(c)
	if principal == nil {
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}
	resolved, err := h.effectivePolicy(c, projectID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Waiver could not be validated")
		return
	}
	waiver, err := evidence.NewWaiver(resolved, evidence.WaiverRequestInput{
		GateID:          snapshot.GateID,
		Check:           snapshot.Check,
		SourceSHA:       snapshot.SourceSHA,
		MergeRequestIID: body.MergeRequestIID,
		Requester:       principal.PrincipalID,
		Reason:          body.Reason,
		ExpiresAt:       expiresAt,
	}, time.Now())
	if err != nil {
		staticErrorReply(c, http.StatusUnprocessableEntity, "WAIVER_INVALID", err.Error())
		return
	}

	waiverID, err := h.store.CreateWaiver(c.Request.Context(), waiver, projectID, snapshot.WorkItemID)
	if errors.Is(err, store.ErrWaiverConflict) {
		staticErrorReply(c, http.StatusConflict, "WAIVER_EXISTS", "A waiver already exists for this gate and SHA")
		return
	}
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Waiver could not be recorded")
		return
	}
	waiver.ID = waiverID
	c.JSON(http.StatusCreated, waiverReply(waiver, snapshot.Check))
}

// ApproveGateWaiver records the independent approval
// (approveGateWaiver). Permission is waiver.approve — functional
// approvers only per the frozen matrix.
func (h *QualityHandler) ApproveGateWaiver(c *gin.Context) {
	h.transitionWaiver(c, func(waiver *evidence.Waiver, principalID, _ string) error {
		return h.store.ApproveWaiver(c.Request.Context(), waiver.ID, principalID)
	})
}

// RevokeGateWaiver cancels a not-yet-terminal waiver (revokeGateWaiver).
func (h *QualityHandler) RevokeGateWaiver(c *gin.Context) {
	h.transitionWaiver(c, func(_ *evidence.Waiver, _, waiverID string) error {
		return h.store.RevokeWaiver(c.Request.Context(), waiverID)
	})
}

func (h *QualityHandler) transitionWaiver(c *gin.Context, apply func(*evidence.Waiver, string, string) error) {
	projectID, waiverID := c.Param("pid"), c.Param("wid")
	if c.GetHeader("Idempotency-Key") == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A valid Idempotency-Key is required")
		return
	}
	waiver, found, err := h.store.WaiverByID(c.Request.Context(), projectID, waiverID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Waiver could not be read")
		return
	}
	if !found {
		staticErrorReply(c, http.StatusNotFound, "WAIVER_NOT_FOUND", "Waiver is unknown in this project")
		return
	}
	match, ok := parseIfMatch(c.GetHeader("If-Match"))
	if !ok {
		staticErrorReply(c, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED", "If-Match with the waiver version is required")
		return
	}
	if match != waiver.Version {
		staticErrorReply(c, http.StatusPreconditionFailed, "PRECONDITION_FAILED", "Waiver version mismatch")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Reason) < 8 || len(body.Reason) > 2000 {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A reason of 8-2000 characters is required")
		return
	}
	principal := PrincipalFromContext(c)
	if principal == nil {
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}

	if err := apply(waiver, principal.PrincipalID, waiverID); err != nil {
		switch {
		case errors.Is(err, store.ErrWaiverSelfApprove):
			staticErrorReply(c, http.StatusForbidden, "SEPARATION_OF_DUTIES", "The approver must differ from the requester")
		case errors.Is(err, store.ErrWaiverConflict):
			staticErrorReply(c, http.StatusConflict, "WAIVER_STATE_CONFLICT", "Waiver state changed concurrently")
		case errors.Is(err, store.ErrWaiverAbsent):
			staticErrorReply(c, http.StatusNotFound, "WAIVER_NOT_FOUND", "Waiver is unknown in this project")
		default:
			staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Waiver transition failed")
		}
		return
	}
	updated, _, err := h.store.WaiverByID(c.Request.Context(), projectID, waiverID)
	if err != nil || updated == nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Waiver could not be re-read")
		return
	}
	check := h.checkForGate(c, projectID, updated.GateID)
	c.JSON(http.StatusOK, waiverReply(updated, check))
}

func (h *QualityHandler) effectivePolicy(c *gin.Context, projectID string) (*evidence.EffectivePolicy, error) {
	overlay, _, err := h.store.GetProjectPolicy(c.Request.Context(), projectID)
	if err != nil {
		return nil, err
	}
	return evidence.ResolveEffective(h.company, overlay)
}

func (h *QualityHandler) checkForGate(c *gin.Context, projectID, gateID string) string {
	snapshot, found, err := h.store.GateSnapshotByID(c.Request.Context(), projectID, gateID)
	if err != nil || !found {
		return ""
	}
	return snapshot.Check
}

func waiverReply(waiver *evidence.Waiver, check string) gin.H {
	approver := any(nil)
	if waiver.Approver != "" {
		approver = waiver.Approver
	}
	return gin.H{
		"id":                waiver.ID,
		"gate_id":           waiver.GateID,
		"status":            waiver.State,
		"requester_id":      waiver.Requester,
		"approver_id":       approver,
		"source_sha":        waiver.SourceSHA,
		"merge_request_iid": waiver.MergeRequestIID,
		"check":             check,
		"expires_at":        waiver.ExpiresAt.UTC().Format(time.RFC3339),
		"version":           waiver.Version,
	}
}

// parseIfMatch accepts the strong ETag form "N" used by the quality
// surface; weak or malformed values fail the precondition.
func parseIfMatch(header string) (int64, bool) {
	value := strings.TrimSpace(header)
	value = strings.Trim(value, `"`)
	if value == "" {
		return 0, false
	}
	version, err := strconv.ParseInt(value, 10, 64)
	if err != nil || version < 1 {
		return 0, false
	}
	return version, true
}

func quoteVersion(version int64) string {
	return `"` + strconv.FormatInt(version, 10) + `"`
}
