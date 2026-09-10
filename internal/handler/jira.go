package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// The W4.5 J3 read surface: anchors with both sides of the mirror
// (SoR values and the Jira-side snapshot) and the reconcile list.
// Read-only by design (task brief J3-5): anchor creation and
// divergence adjudication stay on their server-side surfaces this
// generation — the console states that boundary instead of hiding it.

// JiraStore is the store surface this handler consumes.
type JiraStore interface {
	ListJiraAnchors(ctx context.Context, projectID string) ([]store.JiraAnchorView, error)
	ListJiraReconcileItems(ctx context.Context, projectID string, resolved bool) ([]store.JiraReconcileItem, error)
}

// JiraHandler serves the Jira connector read endpoints.
type JiraHandler struct {
	store JiraStore
}

// NewJiraHandler builds the connector read handler.
func NewJiraHandler(store JiraStore) *JiraHandler {
	return &JiraHandler{store: store}
}

// jiraAnchorWire is one anchor on the wire: both sides of the mirror
// plus the degraded-state bookkeeping.
type jiraAnchorWire struct {
	WorkItemID     string `json:"work_item_id"`
	IssueKey       string `json:"issue_key"`
	JiraProjectKey string `json:"jira_project_key"`
	AnchorSource   string `json:"anchor_source"`
	// SoR side (Maestro is the work-graph SoR): title/status read
	// live from the work item; assignee/iteration held on the anchor.
	SoRTitle       string `json:"sor_title"`
	SoRStatus      string `json:"sor_status"`
	SoRStatusLabel string `json:"sor_status_label"`
	SoRAssignee    string `json:"sor_assignee"`
	SoRIteration   string `json:"sor_iteration_label"`
	// Jira side (read-only snapshot; status is display-only and can
	// never feed Maestro state).
	IssueTitle    string   `json:"issue_title"`
	IssueAssignee string   `json:"issue_assignee"`
	IssueLabels   []string `json:"issue_labels"`
	IssueStatus   string   `json:"issue_status"`
	SnapshotAt    string   `json:"snapshot_at"`
	LastMirrorAt  string   `json:"last_mirror_at"`
	LastMirrorOK  bool     `json:"last_mirror_ok"`
	LastError     string   `json:"last_error"`
}

func jiraAnchorToWire(view store.JiraAnchorView, statusLabel string) jiraAnchorWire {
	return jiraAnchorWire{
		WorkItemID: view.Anchor.WorkItemID, IssueKey: view.Anchor.IssueKey,
		JiraProjectKey: view.Anchor.JiraProjectKey, AnchorSource: view.Anchor.AnchorSource,
		SoRTitle: view.WorkItemTitle, SoRStatus: view.WorkItemStatus, SoRStatusLabel: statusLabel,
		SoRAssignee: view.Anchor.Assignee, SoRIteration: view.Anchor.IterationLabel,
		IssueTitle: view.Anchor.IssueTitle, IssueAssignee: view.Anchor.IssueAssignee,
		IssueLabels: view.Anchor.IssueLabels, IssueStatus: view.Anchor.IssueStatus,
		SnapshotAt: view.Anchor.SnapshotAt, LastMirrorAt: view.Anchor.LastMirrorAt,
		LastMirrorOK: view.Anchor.LastMirrorOK, LastError: view.Anchor.LastError,
	}
}

// ListJiraAnchors answers the project's anchors in issue-key order.
func (h *JiraHandler) ListJiraAnchors(c *gin.Context) {
	views, err := h.store.ListJiraAnchors(c.Request.Context(), c.Param("pid"))
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Jira anchors could not be listed")
		return
	}
	anchors := make([]jiraAnchorWire, 0, len(views))
	for _, view := range views {
		anchors = append(anchors, jiraAnchorToWire(view, "maestro:"+view.WorkItemStatus))
	}
	c.JSON(http.StatusOK, gin.H{"project_id": c.Param("pid"), "anchors": anchors})
}

// jiraReconcileItemWire is one divergence item on the wire.
type jiraReconcileItemWire struct {
	ID             string `json:"id"`
	IssueKey       string `json:"issue_key"`
	WorkItemID     string `json:"work_item_id"`
	Field          string `json:"field"`
	SoRValue       string `json:"sor_value"`
	MirrorValue    string `json:"mirror_value"`
	State          string `json:"state"`
	OpenCycles     int    `json:"open_cycles"`
	DetectedAt     string `json:"detected_at"`
	UpdatedAt      string `json:"updated_at"`
	ResolvedBy     string `json:"resolved_by,omitempty"`
	ResolvedAt     string `json:"resolved_at,omitempty"`
	Resolution     string `json:"resolution,omitempty"`
	ResolutionNote string `json:"resolution_note,omitempty"`
}

func jiraReconcileItemToWire(item store.JiraReconcileItem) jiraReconcileItemWire {
	return jiraReconcileItemWire{
		ID: item.ID, IssueKey: item.IssueKey, WorkItemID: item.WorkItemID,
		Field: item.Field, SoRValue: item.SoRValue, MirrorValue: item.MirrorValue,
		State: item.State, OpenCycles: item.OpenCycles,
		DetectedAt: item.DetectedAt, UpdatedAt: item.UpdatedAt,
		ResolvedBy: item.ResolvedBy, ResolvedAt: item.ResolvedAt,
		Resolution: item.Resolution, ResolutionNote: item.ResolutionNote,
	}
}

// ListJiraReconcileItems answers the project's divergence list — the
// live queue by default, the resolved history with ?state=resolved.
func (h *JiraHandler) ListJiraReconcileItems(c *gin.Context) {
	resolved := c.Query("state") == "resolved"
	items, err := h.store.ListJiraReconcileItems(c.Request.Context(), c.Param("pid"), resolved)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Jira reconcile items could not be listed")
		return
	}
	wire := make([]jiraReconcileItemWire, 0, len(items))
	for _, item := range items {
		wire = append(wire, jiraReconcileItemToWire(item))
	}
	c.JSON(http.StatusOK, gin.H{"project_id": c.Param("pid"), "items": wire})
}
