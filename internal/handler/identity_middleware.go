package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/publicerror"
	"github.com/gin-gonic/gin"
)

// OIDC identity middleware for the M1 identity layer (M1-AUTH-001):
// bearer-token authentication against the frozen token verifier, then the
// unified authorize(principal, action, resource) decision on every /api/v1
// route. Response semantics follow SEC-IDENTITY-RBAC section 5 and the
// frozen resource_hiding block: 401 unauthenticated, 404 for resources
// outside every membership (never 403), 403 authenticated-but-forbidden.

const principalContextKey = "maestro.principal"

// routeAction maps each /api/v1 route template to its frozen permission.
// Routes absent from the map have no mapped permission and therefore deny
// (default deny: an unmapped route is an authorization bug, not a bypass).
var routeAction = map[string]map[string]string{
	"/api/v1/projects": {
		http.MethodGet:  "project.read",
		http.MethodPost: "project.create",
	},
	"/api/v1/overview":     {http.MethodGet: "project.read"},
	"/api/v1/metrics":      {http.MethodGet: "project.read"},
	"/api/v1/projects/:id": {http.MethodGet: "project.read", http.MethodPatch: "project.update"},
	"/api/v1/projects/:id/features": {
		http.MethodGet:  "work_item.read",
		http.MethodPost: "work_item.create",
	},
	"/api/v1/projects/:id/features/:fid": {
		http.MethodGet:   "work_item.read",
		http.MethodPatch: "work_item.create",
	},
	"/api/v1/projects/:id/tasks": {
		http.MethodGet:  "work_item.read",
		http.MethodPost: "work_item.create",
	},
	"/api/v1/projects/:id/tasks/next":                        {http.MethodPost: "work_item.claim"},
	"/api/v1/projects/:id/tasks/next-verification":           {http.MethodPost: "verification.claim"},
	"/api/v1/projects/:id/tasks/:tid":                        {http.MethodGet: "work_item.read", http.MethodPatch: "work_item.create"},
	"/api/v1/projects/:id/tasks/:tid/claim":                  {http.MethodPost: "work_item.claim"},
	"/api/v1/projects/:id/tasks/:tid/heartbeat":              {http.MethodPost: "work_item.heartbeat"},
	"/api/v1/projects/:id/tasks/:tid/submit":                 {http.MethodPost: "work_item.submit"},
	"/api/v1/projects/:id/tasks/:tid/block":                  {http.MethodPost: "work_item.block"},
	"/api/v1/projects/:id/tasks/:tid/resolve":                {http.MethodPost: "work_item.create"},
	"/api/v1/projects/:id/tasks/:tid/verify":                 {http.MethodPost: "verification.submit"},
	"/api/v1/projects/:id/tasks/:tid/resolve-merge-conflict": {http.MethodPost: "work_item.create"},
	"/api/v1/projects/:id/tasks/:tid/cancel":                 {http.MethodPost: "work_item.cancel"},
	"/api/v1/projects/:id/tasks/:tid/validation":             {http.MethodGet: "quality.read"},
	"/api/v1/projects/:id/tasks/:tid/result":                 {http.MethodGet: "work_item.read"},
	"/api/v1/projects/:id/tasks/:tid/diff":                   {http.MethodGet: "work_item.read"},
	"/api/v1/projects/:id/tasks/:tid/force-rollback":         {http.MethodPost: "work_item.retry"},
	"/api/v1/projects/:id/sessions":                          {http.MethodGet: "project.read", http.MethodPost: "work_item.claim"},
	"/api/v1/projects/:id/sessions/:sid":                     {http.MethodGet: "project.read"},
	"/api/v1/projects/:id/sessions/:sid/heartbeat":           {http.MethodPut: "work_item.heartbeat"},
	"/api/v1/projects/:id/sessions/:sid/disconnect":          {http.MethodDelete: "work_item.heartbeat"},
	"/api/v1/projects/:id/sessions/:sid/force-release":       {http.MethodPost: "project.member.manage"},
	"/api/v1/projects/:id/sessions/:sid/workers":             {http.MethodGet: "work_item.read", http.MethodPost: "work_item.claim"},
	"/api/v1/projects/:id/sessions/:sid/workers/:wid":        {http.MethodDelete: "work_item.heartbeat"},
	"/api/v1/projects/:id/board":                             {http.MethodGet: "work_item.read"},
	"/api/v1/projects/:id/board/activity":                    {http.MethodGet: "work_item.read"},
	"/api/v1/projects/:id/worktrees/gc":                      {http.MethodPost: "project.update"},
	"/api/v1/projects/:id/ws":                                {http.MethodGet: "work_item.read"},
	"/api/v1/projects/:id/archive":                           {http.MethodPost: "project.update"},
	"/api/v1/projects/:id/restore":                           {http.MethodPost: "project.update"},
}

// OIDCMiddleware holds the identity wiring for one transport.
type OIDCMiddleware struct {
	policy   *identity.Policy
	verifier *identity.TokenVerifier
	resolver identity.PrincipalResolver
	nowFunc  func() time.Time
}

// NewOIDCMiddleware builds the bearer-authentication and authorize
// middleware pair from the frozen policy, token verifier and principal
// resolver.
func NewOIDCMiddleware(policy *identity.Policy, verifier *identity.TokenVerifier, resolver identity.PrincipalResolver) *OIDCMiddleware {
	return &OIDCMiddleware{
		policy:   policy,
		verifier: verifier,
		resolver: resolver,
		nowFunc:  time.Now,
	}
}

// IdentityMount wires the middleware pair into the router contract.
func (m *OIDCMiddleware) IdentityMount() *IdentityMount {
	return &IdentityMount{
		Authenticate: m.Authenticate,
		Authorize:    m.Authorize,
	}
}

// PrincipalFromContext extracts the authenticated principal, if any.
func PrincipalFromContext(c *gin.Context) *model.PrincipalContext {
	if c == nil {
		return nil
	}
	principal, _ := c.Get(principalContextKey)
	typed, _ := principal.(*model.PrincipalContext)
	return typed
}

// Authenticate validates the bearer token and resolves the server-side
// principal. Invalid, expired or unknown identities are 401 — fail closed.
func (m *OIDCMiddleware) Authenticate(c *gin.Context) {
	// Liveness/readiness probes stay anonymous (M0 contract); every other
	// route requires an authenticated principal. The /api/v3 Runner group
	// carries its own scheme per runner.yaml (one public enroll route,
	// device tokens elsewhere) and self-gates in RegisterRunnerV3. The
	// /webhooks/gitlab receiver authenticates with the instance's shared
	// token per control-plane.yaml and self-gates in the ingestor.
	if isAnonymousHealthPath(c.Request.URL.Path) ||
		strings.HasPrefix(c.Request.URL.Path, "/api/v3/") ||
		strings.HasPrefix(c.Request.URL.Path, "/webhooks/") {
		c.Next()
		return
	}
	if !m.authenticateBearer(c) {
		return
	}
	c.Next()
}

// authenticateBearer verifies the bearer token and resolves the
// principal, aborting with 401 on failure. It is shared by the global
// middleware and self-gating v3 admin routes.
func (m *OIDCMiddleware) authenticateBearer(c *gin.Context) bool {
	header := c.GetHeader("Authorization")
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return false
	}
	claims, err := m.verifier.Verify(strings.TrimSpace(parts[1]), m.nowFunc())
	if err != nil {
		c.Header("WWW-Authenticate", `Bearer realm="maestro", error="invalid_token"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "Authentication token is invalid or expired")
		return false
	}
	principal, err := m.resolver.Resolve(c.Request.Context(), claims.Issuer, claims.Subject)
	if err != nil {
		c.Header("WWW-Authenticate", `Bearer realm="maestro", error="invalid_token"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "Authentication token is invalid or expired")
		return false
	}
	c.Set(principalContextKey, principal)
	return true
}

// Authorize enforces the unified decision on every /api/v1 route. Denials
// with "no membership" hide the resource (404); other denials are 403;
// missing authentication surfaces as 401.
func (m *OIDCMiddleware) Authorize(c *gin.Context) {
	principal := PrincipalFromContext(c)
	if principal == nil {
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}
	route := c.FullPath()
	if route == "" {
		route = "<unmatched>"
	}
	actions, mapped := routeAction[route]
	action, known := actions[c.Request.Method]
	if !mapped || !known {
		c.Abort()
		staticErrorReply(c, http.StatusForbidden, "FORBIDDEN", "No permission is mapped for this route")
		return
	}

	projectID := c.Param("id")
	resource := model.Resource{Type: "project", ProjectID: projectID}
	if strings.Contains(route, ":tid") || strings.Contains(route, "board") || strings.Contains(route, "tasks") {
		resource.Type = "work_item"
	}
	if strings.Contains(route, "sessions") {
		resource.Type = "session"
	}

	// Scopeless routes (global project list, overview, metrics) authorize
	// against ANY membership: the handler then filters per project. No
	// membership anywhere denies without hiding (there is no single
	// resource to hide).
	if projectID == "" {
		for scope := range principal.ProjectMemberships {
			scoped := resource
			scoped.ProjectID = scope
			if decision := m.policy.Authorize(c.Request.Context(), principal, action, scoped); decision.Allow {
				c.Next()
				return
			}
		}
		c.Abort()
		staticErrorReply(c, http.StatusForbidden, "FORBIDDEN", "Action is not permitted for this principal")
		return
	}

	decision := m.policy.Authorize(c.Request.Context(), principal, action, resource)
	if decision.Allow {
		c.Next()
		return
	}
	reason := ""
	if len(decision.Reasons) > 0 {
		reason = decision.Reasons[0]
	}
	if strings.HasPrefix(reason, "no membership") {
		// Resource hiding: an unauthorized project is indistinguishable
		// from a nonexistent one (SEC-IDENTITY-RBAC TC-RBAC-005).
		c.Abort()
		public := publicerror.Classify(nil)
		c.JSON(http.StatusNotFound, gin.H{
			"error":          "ROUTE_NOT_FOUND",
			"error_code":     "ROUTE_NOT_FOUND",
			"correlation_id": public.CorrelationID,
		})
		return
	}
	c.Abort()
	staticErrorReply(c, http.StatusForbidden, "FORBIDDEN", "Action is not permitted for this principal")
}
