package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/publicerror"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
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

	// Browser-session surface (task brief E): when armed, requests with
	// no bearer credential may authenticate with the BFF session
	// cookie under the strict Origin boundary (CSRF). The principal is
	// re-resolved per request so revocations propagate immediately.
	sessions        store.AuthSessionsStore
	sessionUsers    identity.PrincipalByIDResolver
	originAllowlist map[string]struct{}
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

// WithBrowserSessions arms the cookie authentication path (bearer
// absent → session cookie) and returns the same middleware.
func (m *OIDCMiddleware) WithBrowserSessions(sessions store.AuthSessionsStore, sessionUsers identity.PrincipalByIDResolver, allowedOrigins []string) *OIDCMiddleware {
	m.sessions = sessions
	m.sessionUsers = sessionUsers
	m.originAllowlist = buildOriginAllowlist(allowedOrigins)
	return m
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

// isAnonymousShellPath matches the console SPA document and its static
// bundle: the shell is the login page's own host surface and carries no
// data; every data API still authenticates.
func isAnonymousShellPath(path string) bool {
	return path == "/" || path == "/dashboard" || strings.HasPrefix(path, "/dashboard/assets/")
}

// Authenticate validates the request credential and resolves the
// server-side principal. Invalid, expired or unknown identities are 401
// — fail closed. The credential is the bearer token when an
// Authorization header is present; otherwise, when the browser-session
// surface is armed, the BFF session cookie (strict Origin boundary).
func (m *OIDCMiddleware) Authenticate(c *gin.Context) {
	// Liveness/readiness probes stay anonymous (M0 contract); every other
	// route requires an authenticated principal. The /api/v3 Runner group
	// carries its own scheme per runner.yaml (one public enroll route,
	// device tokens elsewhere) and self-gates in RegisterRunnerV3. The
	// /webhooks/gitlab receiver authenticates with the instance's shared
	// token per control-plane.yaml and self-gates in the ingestor.
	// The console shell (SPA document + static assets) is deliberately
	// anonymous: it is the login page's own host, carries no data, and
	// every data call still authenticates (BFF pattern).
	if isAnonymousHealthPath(c.Request.URL.Path) ||
		isAnonymousShellPath(c.Request.URL.Path) ||
		strings.HasPrefix(c.Request.URL.Path, "/api/v3/") ||
		strings.HasPrefix(c.Request.URL.Path, "/webhooks/") {
		c.Next()
		return
	}
	if !m.authenticateCredential(c) {
		return
	}
	c.Next()
}

// authenticateCredential resolves the request credential: bearer when
// the Authorization header is present, otherwise the session cookie
// when armed, otherwise 401.
func (m *OIDCMiddleware) authenticateCredential(c *gin.Context) bool {
	if c.GetHeader("Authorization") != "" {
		return m.authenticateBearer(c)
	}
	if m.sessions != nil {
		return m.authenticateSession(c)
	}
	c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
	c.Abort()
	staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
	return false
}

// authenticateSession validates the BFF cookie. The CSRF boundary is
// structural: a cookie-authenticated request whose Origin header is
// present must match the allowlist EXACTLY — a cross-origin cookie
// request is 403, and an unknown, revoked or expired session is 401.
func (m *OIDCMiddleware) authenticateSession(c *gin.Context) bool {
	token, err := c.Cookie(identity.SessionCookieName)
	if err != nil || token == "" {
		c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return false
	}
	if origin := c.GetHeader("Origin"); origin != "" {
		normalized, ok := normalizeOrigin(origin)
		if !ok || !requestOriginAllowed(c.Request, normalized, m.originAllowlist) {
			c.Abort()
			staticErrorReply(c, http.StatusForbidden, "CSRF_ORIGIN_BLOCKED", "Cross-origin session requests are not permitted")
			return false
		}
	}
	session, found, err := m.sessions.SessionByTokenHash(c.Request.Context(), identity.HashOpaqueToken(token))
	if err != nil || !found {
		c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return false
	}
	if session.RevokedAt != nil || m.nowFunc().After(session.ExpiresAt) {
		c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return false
	}
	principal, err := m.sessionUsers.ResolveByID(c.Request.Context(), session.UserID)
	if err != nil {
		c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return false
	}
	_ = m.sessions.TouchSession(c.Request.Context(), session.TokenHash)
	c.Set(principalContextKey, principal)
	return true
}

// authenticateBearer verifies the bearer token and resolves the
// principal, aborting with 401 on failure. It is shared by the global
// middleware and self-gating v3 admin routes.
func (m *OIDCMiddleware) authenticateBearer(c *gin.Context) bool {
	parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return false
	}
	principal, ok := m.principalFromToken(c, strings.TrimSpace(parts[1]))
	if !ok {
		c.Header("WWW-Authenticate", `Bearer realm="maestro", error="invalid_token"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "Authentication token is invalid or expired")
		return false
	}
	c.Set(principalContextKey, principal)
	return true
}

// principalFromToken verifies one bearer token through the frozen chain
// and resolves the principal WITHOUT writing a transport reply — shared
// by the middleware and the /auth/session identity echo.
func (m *OIDCMiddleware) principalFromToken(c *gin.Context, token string) (*model.PrincipalContext, bool) {
	claims, err := m.verifier.Verify(token, m.nowFunc())
	if err != nil {
		return nil, false
	}
	principal, err := m.resolver.Resolve(c.Request.Context(), claims.Issuer, claims.Subject)
	if err != nil {
		return nil, false
	}
	return principal, true
}

// BearerProbe exposes the frozen bearer verification as a non-aborting
// lookup for the /auth session probe (identity echo for API-token
// callers; no session is created or consumed).
func (m *OIDCMiddleware) BearerProbe() func(*gin.Context) (*model.PrincipalContext, bool) {
	return func(c *gin.Context) (*model.PrincipalContext, bool) {
		parts := strings.SplitN(c.GetHeader("Authorization"), " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			return nil, false
		}
		return m.principalFromToken(c, strings.TrimSpace(parts[1]))
	}
}

// Authorize enforces the unified decision on every /api/v1 route. Denials
// with "no membership" hide the resource (404); other denials are 403;
// missing authentication surfaces as 401.
func (m *OIDCMiddleware) Authorize(c *gin.Context) {
	m.authorizeRoute(c, routeAction)
}

// authorizeRoute is the shared decision body for the v1 and control-
// plane (/api/v3) trees: same policy, same deny semantics, different
// frozen route-permission maps.
func (m *OIDCMiddleware) authorizeRoute(c *gin.Context, actions map[string]map[string]string) {
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
	routeActions, mapped := actions[route]
	action, known := routeActions[c.Request.Method]
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
