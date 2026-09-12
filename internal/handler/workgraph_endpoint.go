package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ZoonChen/Maestro-MCP/internal/publicerror"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
)

// The J2c console surface (ADR-009 §4): the Work Graph views (containment
// tree, statuses, JoinPolicy), the asset-ledger view and the HITL seal —
// the one server-exclusive operation a human performs on the console.
// Agents reach the graph through the MCP tools; this tree is the human
// control plane under the frozen /api/v3 permissions.

// WorkGraphConsoleStore is the work-graph read + seal surface.
type WorkGraphConsoleStore interface {
	ListWorkPlans(ctx context.Context, projectID string) ([]*store.WorkPlan, error)
	GetWorkPlan(ctx context.Context, planID string) (*store.WorkPlan, error)
	ListWorkNodes(ctx context.Context, planID string) ([]*store.WorkNode, error)
	CurrentPlanRevision(ctx context.Context, planID string) (*store.PlanRevision, error)
	ListNodeRevisionSpecs(ctx context.Context, planID, revisionID string) ([]*store.WorkNodeRevision, error)
	ListDecompositionProposals(ctx context.Context, planID string) ([]*store.DecompositionProposalRecord, error)
	SealPlanRevision(ctx context.Context, planID string, expectedGraphVersion int64, actor string) (*store.PlanRevision, error)
}

// AssetConsoleStore is the ledger read surface.
type AssetConsoleStore interface {
	ListAssets(ctx context.Context, projectID string) ([]*store.Asset, error)
	// ListWaitingGateBindings is the W5-5 downstream waiting surface:
	// stale gate bindings enriched with the latest registered asset
	// version, so the console answers "which version does this gate
	// wait for" while dispatch is blocked.
	ListWaitingGateBindings(ctx context.Context, projectID string) ([]*store.WaitingGateBinding, error)
}

// WorkGraphHandler serves the J2c console tree.
type WorkGraphHandler struct {
	graph  WorkGraphConsoleStore
	assets AssetConsoleStore
}

// NewWorkGraphHandler wires the console surface (both stores nil keeps
// the handler unregistered — compose decides).
func NewWorkGraphHandler(graph WorkGraphConsoleStore, assets AssetConsoleStore) *WorkGraphHandler {
	return &WorkGraphHandler{graph: graph, assets: assets}
}

// Console wire shapes (snake_case; the store rows carry no JSON tags).
type consoleWorkPlan struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	HumanCode    string `json:"human_code"`
	RootNodeID   string `json:"root_node_id"`
	GraphVersion int64  `json:"graph_version"`
	Status       string `json:"status"`
}

func consolePlan(plan *store.WorkPlan) consoleWorkPlan {
	return consoleWorkPlan{ID: plan.ID, Title: plan.Title, HumanCode: plan.HumanCode,
		RootNodeID: plan.RootNodeID, GraphVersion: plan.GraphVersion, Status: plan.Status}
}

type consoleWorkNode struct {
	ID           string `json:"id"`
	ParentNodeID string `json:"parent_node_id"`
	NodeType     string `json:"node_type"`
	SlotKey      string `json:"slot_key"`
	HumanCode    string `json:"human_code"`
	Status       string `json:"status"`
	NodeVersion  int64  `json:"node_version"`
	Depth        int    `json:"depth"`
}

type consoleNodeSpec struct {
	NodeID           string          `json:"node_id"`
	Title            string          `json:"title,omitempty"`
	SuccessThreshold json.RawMessage `json:"success_threshold,omitempty"`
	BudgetUnits      int64           `json:"budget_units,omitempty"`
	Deadline         string          `json:"deadline,omitempty"`
	FailurePolicy    string          `json:"failure_policy,omitempty"`
	CancelPolicy     string          `json:"cancel_policy,omitempty"`
}

type consoleProposal struct {
	ID                   string          `json:"id"`
	Status               string          `json:"status"`
	ExpectedGraphVersion int64           `json:"expected_graph_version"`
	Violations           json.RawMessage `json:"violations"`
	AppliedNodeIDs       json.RawMessage `json:"applied_node_ids"`
	SubmittedBy          string          `json:"submitted_by"`
	DecidedAt            string          `json:"decided_at"`
	CreatedAt            string          `json:"created_at"`
}

func consoleProposalFrom(record *store.DecompositionProposalRecord) consoleProposal {
	violations := json.RawMessage("null")
	if len(record.Violations) > 0 {
		violations = json.RawMessage(record.Violations)
	}
	applied := json.RawMessage("null")
	if len(record.AppliedNodeIDs) > 0 {
		applied = json.RawMessage(record.AppliedNodeIDs)
	}
	return consoleProposal{ID: record.ID, Status: record.Status,
		ExpectedGraphVersion: record.ExpectedGraphVersion, Violations: violations,
		AppliedNodeIDs: applied, SubmittedBy: record.SubmittedBy,
		DecidedAt: record.DecidedAt, CreatedAt: record.CreatedAt}
}

// ListWorkPlans serves GET /projects/:pid/work-graph.
func (h *WorkGraphHandler) ListWorkPlans(c *gin.Context) {
	plans, err := h.graph.ListWorkPlans(c.Request.Context(), c.Param("pid"))
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	out := make([]consoleWorkPlan, 0, len(plans))
	for _, plan := range plans {
		out = append(out, consolePlan(plan))
	}
	c.JSON(http.StatusOK, gin.H{"plans": out})
}

// GetWorkGraphPlan serves GET /projects/:pid/work-graph/plans/:planId:
// the full graph detail view (tree, statuses, current revision, typed
// specs with JoinPolicy, decided proposals).
func (h *WorkGraphHandler) GetWorkGraphPlan(c *gin.Context) {
	ctx := c.Request.Context()
	planID := c.Param("planId")
	plan, err := h.graph.GetWorkPlan(ctx, planID)
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	if plan.ProjectID != c.Param("pid") {
		staticErrorReply(c, http.StatusNotFound, "WORK_PLAN_NOT_FOUND", "Work plan not found")
		return
	}
	nodes, err := h.graph.ListWorkNodes(ctx, planID)
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	nodeOut := make([]consoleWorkNode, 0, len(nodes))
	for _, node := range nodes {
		nodeOut = append(nodeOut, consoleWorkNode{
			ID: node.ID, ParentNodeID: node.ParentNodeID, NodeType: node.NodeType,
			SlotKey: node.SlotKey, HumanCode: node.HumanCode, Status: node.Status,
			NodeVersion: node.NodeVersion, Depth: node.Depth,
		})
	}

	revisionView := gin.H{"available": false}
	specsOut := []consoleNodeSpec{}
	if current, revErr := h.graph.CurrentPlanRevision(ctx, planID); revErr == nil {
		revisionView = gin.H{
			"available":     true,
			"id":            current.ID,
			"revision_no":   current.RevisionNo,
			"status":        current.Status,
			"spec_digest":   current.SpecDigest,
			"sealed_at":     current.SealedAt,
			"node_manifest": json.RawMessage(orEmptyJSON(current.NodeManifest)),
		}
		specs, specErr := h.graph.ListNodeRevisionSpecs(ctx, planID, current.ID)
		if specErr != nil {
			publicErrorReply(c, specErr)
			return
		}
		for _, spec := range specs {
			view := consoleNodeSpec{NodeID: spec.NodeID}
			decoded := workgraph.NodeSpec{}
			if len(spec.Spec) > 0 && json.Unmarshal(spec.Spec, &decoded) == nil {
				view.Title = decoded.Title
				view.BudgetUnits = decoded.BudgetUnits
				view.Deadline = decoded.Deadline
				view.FailurePolicy = decoded.FailurePolicy
				view.CancelPolicy = decoded.CancelPolicy
				if decoded.SuccessThreshold != nil {
					if encoded, marshalErr := json.Marshal(decoded.SuccessThreshold); marshalErr == nil {
						view.SuccessThreshold = encoded
					}
				}
			}
			specsOut = append(specsOut, view)
		}
	}

	proposals, err := h.graph.ListDecompositionProposals(ctx, planID)
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	proposalsOut := make([]consoleProposal, 0, len(proposals))
	for _, record := range proposals {
		proposalsOut = append(proposalsOut, consoleProposalFrom(record))
	}

	c.JSON(http.StatusOK, gin.H{
		"plan":             consolePlan(plan),
		"nodes":            nodeOut,
		"current_revision": revisionView,
		"node_specs":       specsOut,
		"proposals":        proposalsOut,
	})
}

// ListAssets serves GET /projects/:pid/assets (the ledger view; the
// sensitivity/lifecycle filtering happens client-side on the frozen
// full listing — the pilot ledger is small by design). The response
// also carries the W5-5 waiting surface: every stale gate binding with
// the asset version it waits for, so a blocked dispatch is explainable
// from the console instead of surfacing only as ErrNoAvailableTask.
func (h *WorkGraphHandler) ListAssets(c *gin.Context) {
	assets, err := h.assets.ListAssets(c.Request.Context(), c.Param("pid"))
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	waiting, err := h.assets.ListWaitingGateBindings(c.Request.Context(), c.Param("pid"))
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	waitingOut := make([]gin.H, 0, len(waiting))
	for _, entry := range waiting {
		waitingOut = append(waitingOut, gin.H{
			"work_item_id":   entry.WorkItemID,
			"gate_id":        entry.GateID,
			"asset_id":       entry.AssetID,
			"bound_version":  entry.BoundVersion,
			"binding_status": entry.Status,
			"staled_at":      entry.StaledAt,
			"latest_version": entry.LatestVersion,
			"latest_status":  entry.LatestStatus,
		})
	}
	type assetRow struct {
		AssetID        string   `json:"asset_id"`
		Version        int      `json:"version"`
		AssetType      string   `json:"asset_type"`
		Title          string   `json:"title"`
		Status         string   `json:"status"`
		OwnerPrincipal string   `json:"owner_principal"`
		Sensitivity    string   `json:"sensitivity"`
		SupersedesRef  string   `json:"supersedes_ref"`
		SourceDigest   string   `json:"source_digest"`
		ContentRef     string   `json:"content_ref"`
		Reviewers      []string `json:"reviewers"`
		CreatedAt      string   `json:"created_at"`
		ReviewedAt     string   `json:"reviewed_at"`
		ApprovedAt     string   `json:"approved_at"`
		SupersededAt   string   `json:"superseded_at"`
	}
	out := make([]assetRow, 0, len(assets))
	for _, asset := range assets {
		out = append(out, assetRow{
			AssetID: asset.AssetID, Version: asset.Version, AssetType: asset.AssetType,
			Title: asset.Title, Status: asset.Status, OwnerPrincipal: asset.OwnerPrincipal,
			Sensitivity: asset.Sensitivity, SupersedesRef: asset.SupersedesRef,
			SourceDigest: asset.SourceDigest, ContentRef: asset.ContentRef,
			Reviewers: asset.Reviewers, CreatedAt: asset.CreatedAt,
			ReviewedAt: asset.ReviewedAt, ApprovedAt: asset.ApprovedAt, SupersededAt: asset.SupersededAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"assets": out, "waiting_gates": waitingOut})
}

// SealPlan serves POST /projects/:pid/work-graph/plans/:planId/seal —
// the HITL approval step (ADR-009 §2/§4): a human approves the plan
// semantics; the server freezes the draft revision behind the graph
// CAS. This operation is deliberately console-only: the MCP catalog
// never exposes it.
func (h *WorkGraphHandler) SealPlan(c *gin.Context) {
	ctx := c.Request.Context()
	planID := c.Param("planId")
	plan, err := h.graph.GetWorkPlan(ctx, planID)
	if err != nil {
		publicErrorReply(c, err)
		return
	}
	if plan.ProjectID != c.Param("pid") {
		staticErrorReply(c, http.StatusNotFound, "WORK_PLAN_NOT_FOUND", "Work plan not found")
		return
	}
	if c.GetHeader("Idempotency-Key") == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A valid Idempotency-Key is required")
		return
	}
	var body struct {
		ExpectedGraphVersion int64 `json:"expected_graph_version"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.ExpectedGraphVersion < 1 {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "expected_graph_version (>= 1) is required")
		return
	}
	principal := PrincipalFromContext(c)
	if principal == nil {
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}

	sealed, err := h.graph.SealPlanRevision(ctx, planID, body.ExpectedGraphVersion, principal.PrincipalID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRevisionSealed):
			// Idempotent replay surface: the sealed revision is the answer.
			if current, replayErr := h.graph.CurrentPlanRevision(ctx, planID); replayErr == nil && current.Status == "sealed" {
				c.JSON(http.StatusOK, gin.H{
					"revision": gin.H{
						"id": current.ID, "revision_no": current.RevisionNo, "status": current.Status,
						"spec_digest": current.SpecDigest, "sealed_at": current.SealedAt,
					},
					"replay": true,
				})
				return
			}
			publicErrorReply(c, err)
		default:
			publicErrorReply(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"revision": gin.H{
			"id": sealed.ID, "revision_no": sealed.RevisionNo, "status": sealed.Status,
			"spec_digest": sealed.SpecDigest, "sealed_at": sealed.SealedAt,
		},
		"replay": false,
	})
}

// publicErrorReply translates one store error through the fail-closed
// allowlist (control-plane replies carry stable codes only).
func publicErrorReply(c *gin.Context, err error) {
	public := publicerror.Classify(err)
	status := public.HTTPStatus
	if status < 400 {
		status = http.StatusInternalServerError
	}
	c.JSON(status, gin.H{
		"error":          public.Message,
		"error_code":     public.Code,
		"correlation_id": public.CorrelationID,
	})
}

func orEmptyJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}
