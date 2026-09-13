package handler

import (
	"context"
	"net/http"

	"github.com/ZoonChen/Maestro-MCP/internal/publicerror"
	"github.com/gin-gonic/gin"
)

// The frozen control-plane.yaml tree is served under its declared
// server base path /api/v3 (contract path alignment: the quality
// endpoints landed on /api/v1 and the webhook receiver at the root in
// their first slices — this group is the correction). The v3 Runner
// group carries its own device-token scheme and the webhook receiver
// its shared-token scheme, which is why the engine-wide OIDC
// Authenticate exempts the whole /api/v3 prefix; every HUMAN surface
// mounted here therefore re-authenticates through the same verifier
// and authorizes through the same frozen policy — one authorize for
// every surface.

// controlPlaneActions maps the /api/v3 route templates to their frozen
// permissions. Reads without an explicit x-maestro-permission map to
// the narrowest frozen read grant that covers them.
var controlPlaneActions = map[string]map[string]string{
	"/api/v3/gitlab/instances": {
		http.MethodGet:  "gitlab_instance.configure", // platform-admin listing
		http.MethodPost: "gitlab_instance.configure",
	},
	"/api/v3/projects/:pid/gitlab-mapping": {
		http.MethodGet: "project.read",
		http.MethodPut: "project.repository.manage",
	},
	"/api/v3/projects/:pid/gitlab/merge-requests/:iid": {
		http.MethodGet: "project.read",
	},
	"/api/v3/projects/:pid/gitlab/merge-requests/:iid/reconcile": {
		http.MethodPost: "gitlab.reconcile",
	},
	"/api/v3/projects/:pid/audit-export": {
		http.MethodGet: "audit.export",
	},
	"/api/v3/projects/:pid/audit-export/verify": {
		http.MethodPost: "audit.export",
	},
	"/api/v3/projects/:pid/slo-snapshot": {
		http.MethodGet: "project.read",
	},
	"/api/v3/projects/:pid/pilot-flags": {
		http.MethodGet: "pilot.read",
	},
	"/api/v3/projects/:pid/jira-anchors": {
		http.MethodGet: "project.read",
	},
	"/api/v3/projects/:pid/jira-reconcile-items": {
		http.MethodGet: "project.read",
	},
	"/api/v3/projects/:pid/work-graph": {
		http.MethodGet: "workgraph.read",
	},
	"/api/v3/projects/:pid/work-graph/plans/:planId": {
		http.MethodGet: "workgraph.read",
	},
	"/api/v3/projects/:pid/work-graph/plans/:planId/seal": {
		// ADR-009 §2: the human approval of plan semantics. The frozen
		// workgraph.seal grant carries the technical_lead functional plane
		// (J4, DEC-2 terminal state — the project_policy.strengthen
		// interim mapping is retired).
		http.MethodPost: "workgraph.seal",
	},
	"/api/v3/projects/:pid/assets": {
		http.MethodGet: "asset.read",
	},
	"/api/v3/projects/:pid/pilot-flags/:flag": {
		http.MethodPut: "pilot.write",
	},
	"/api/v3/projects/:pid/quality-policy": {
		http.MethodGet: "quality.read",
		http.MethodPut: "project_policy.strengthen",
	},
	"/api/v3/projects/:pid/work-items/:wid/gates": {
		http.MethodGet: "quality.read",
	},
	"/api/v3/projects/:pid/work-items/:wid/evidence": {
		http.MethodGet: "quality.read",
	},
	"/api/v3/projects/:pid/work-items/:wid/waivers": {
		http.MethodGet: "quality.read",
	},
	"/api/v3/webhooks/dead-letters/:inbox_id/replay": {
		// W5-6 / OPS-1 terminal state: the replay is an OPERATIONS
		// action on the frozen webhook.deadletter.replay string held by
		// the operations_owner functional plane — no longer the MR
		// reconciliation string project_admin happens to carry.
		http.MethodPost: "webhook.deadletter.replay",
	},
	"/api/v3/projects/:pid/gates/:gid/waivers": {
		http.MethodPost: "waiver.request",
	},
	"/api/v3/projects/:pid/waivers/:wid/approve": {
		http.MethodPost: "waiver.approve",
	},
	"/api/v3/projects/:pid/waivers/:wid/revoke": {
		http.MethodPost: "waiver.revoke",
	},
}

// ControlPlaneOptions wires the human /api/v3 control-plane group.
type ControlPlaneOptions struct {
	Identity      *OIDCMiddleware
	Quality       *QualityHandler
	GitLab        *GitLabHandler
	Observability *ObservabilityHandler
	SLO           *SLOSnapshotHandler
	DeadLetters   *DeadLetterHandler
	Pilot         *PilotHandler
	Jira          *JiraHandler
	WorkGraph     *WorkGraphHandler
	Scope         ScopeGuard
}

// ScopeGuard hides unknown project scopes (the v3 replacement for the
// v1 ProjectGuard: resource hiding stays indistinguishable from
// absence).
type ScopeGuard interface {
	ProjectExists(ctx context.Context, projectID string) (bool, error)
}

// RegisterControlPlane mounts the human control-plane tree. The group
// authenticates every request through the same bearer verifier the v1
// tree uses, then applies the frozen permission per route template.
func RegisterControlPlane(r *gin.Engine, options ControlPlaneOptions) {
	if options.Identity == nil {
		return // no identity layer: the surface stays unexposed
	}
	group := r.Group("/api/v3")
	group.Use(
		requireControlPlanePrincipal(options.Identity),
		authorizeControlPlane(options.Identity),
		hideUnknownControlPlaneScope(options.Scope),
	)

	if options.Quality != nil {
		group.GET("/projects/:pid/quality-policy", options.Quality.GetQualityPolicy)
		group.PUT("/projects/:pid/quality-policy", options.Quality.PutQualityPolicy)
		group.GET("/projects/:pid/work-items/:wid/gates", options.Quality.ListWorkItemGates)
		group.GET("/projects/:pid/work-items/:wid/evidence", options.Quality.ListWorkItemEvidence)
		group.GET("/projects/:pid/work-items/:wid/waivers", options.Quality.ListWorkItemWaivers)
		group.POST("/projects/:pid/gates/:gid/waivers", options.Quality.RequestGateWaiver)
		group.POST("/projects/:pid/waivers/:wid/approve", options.Quality.ApproveGateWaiver)
		group.POST("/projects/:pid/waivers/:wid/revoke", options.Quality.RevokeGateWaiver)
	}
	if options.DeadLetters != nil {
		group.POST("/webhooks/dead-letters/:inbox_id/replay", options.DeadLetters.ReplayDeadLetter)
	}
	if options.GitLab != nil {
		group.GET("/gitlab/instances", options.GitLab.ListInstances)
		group.POST("/gitlab/instances", options.GitLab.CreateInstance)
		group.GET("/projects/:pid/gitlab-mapping", options.GitLab.GetMapping)
		group.PUT("/projects/:pid/gitlab-mapping", options.GitLab.PutMapping)
		group.GET("/projects/:pid/gitlab/merge-requests/:iid", options.GitLab.GetMergeRequest)
		group.POST("/projects/:pid/gitlab/merge-requests/:iid/reconcile", options.GitLab.ReconcileMergeRequest)
	}
	if options.Observability != nil {
		group.GET("/projects/:pid/audit-export", options.Observability.ExportAuditChain)
		group.POST("/projects/:pid/audit-export/verify", options.Observability.VerifyAuditChain)
	}
	if options.SLO != nil {
		group.GET("/projects/:pid/slo-snapshot", options.SLO.GetSLOSnapshot)
	}
	if options.Pilot != nil {
		group.GET("/projects/:pid/pilot-flags", options.Pilot.ListPilotFlags)
		group.PUT("/projects/:pid/pilot-flags/:flag", options.Pilot.PutPilotFlag)
	}
	if options.Jira != nil {
		group.GET("/projects/:pid/jira-anchors", options.Jira.ListJiraAnchors)
		group.GET("/projects/:pid/jira-reconcile-items", options.Jira.ListJiraReconcileItems)
	}
	if options.WorkGraph != nil {
		group.GET("/projects/:pid/work-graph", options.WorkGraph.ListWorkPlans)
		group.GET("/projects/:pid/work-graph/plans/:planId", options.WorkGraph.GetWorkGraphPlan)
		group.POST("/projects/:pid/work-graph/plans/:planId/seal", options.WorkGraph.SealPlan)
		group.GET("/projects/:pid/assets", options.WorkGraph.ListAssets)
	}
}

// requireControlPlanePrincipal authenticates the credential the same
// way the v1 tree does (the engine-wide Authenticate deliberately
// exempts /api/v3 for the self-gated machine surfaces): bearer when
// present, otherwise the BFF session cookie for console browsers.
func requireControlPlanePrincipal(m *OIDCMiddleware) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !m.authenticateCredential(c) {
			return
		}
		c.Next()
	}
}

// authorizeControlPlane enforces the frozen permission for the matched
// route template with the same deny semantics as the v1 decision point.
func authorizeControlPlane(m *OIDCMiddleware) gin.HandlerFunc {
	return func(c *gin.Context) {
		m.authorizeRoute(c, controlPlaneActions)
	}
}

// hideUnknownControlPlaneScope answers 404 for project-scoped routes
// whose project does not exist (no ProjectGuard on the v3 tree).
func hideUnknownControlPlaneScope(guard ScopeGuard) gin.HandlerFunc {
	return func(c *gin.Context) {
		if guard == nil {
			c.Next()
			return
		}
		projectID := c.Param("pid")
		if projectID == "" {
			c.Next()
			return
		}
		exists, err := guard.ProjectExists(c.Request.Context(), projectID)
		if err != nil {
			c.Abort()
			staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Scope could not be resolved")
			return
		}
		if !exists {
			c.Abort()
			public := publicerror.Classify(nil)
			c.JSON(http.StatusNotFound, gin.H{
				"error":          "ROUTE_NOT_FOUND",
				"error_code":     "ROUTE_NOT_FOUND",
				"correlation_id": public.CorrelationID,
			})
			return
		}
		c.Next()
	}
}
