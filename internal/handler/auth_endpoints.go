package handler

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The /auth protocol group (task brief E, M4-UI-001 UI-AUTH): the BFF
// half of the browser login. The server owns the OIDC Authorization
// Code + PKCE exchange; the browser only ever holds principal metadata
// and a one-shot return-state value, because the session credential is
// an HttpOnly cookie bound to a server-side opaque session
// (SEC-IDENTITY-RBAC section 2/7).
//
// Wire contract frozen by the console client (web/src/auth): GET
// /auth/session answers 401 unauthenticated / 200 {principal, roles,
// project_scope}; GET /auth/authorize?redirect_uri&state 302s into the
// IdP; POST /auth/logout is cookie-bound. Callback failures bounce
// back to the console with auth_error + the client's return-state.

// AuthEndpoints serves the protocol routes. It is mounted by the router
// through IdentityMount.RegisterRoutes; the group deliberately sits
// before the engine-wide authentication so login and callback stay
// anonymous protocol entries.
type AuthEndpoints struct {
	client          *identity.OIDCClient
	verifier        *identity.TokenVerifier
	resolver        identity.PrincipalResolver
	sessionUsers    identity.PrincipalByIDResolver
	sessions        store.AuthSessionsStore
	originAllowlist map[string]struct{}
	bearerProbe     func(*gin.Context) (*model.PrincipalContext, bool)
	nowFunc         func() time.Time
}

// NewAuthEndpoints wires the protocol handlers. The client secret is
// required: without it the code exchange cannot run and the endpoints
// must not mount (honest degradation, never a fake login).
func NewAuthEndpoints(
	client *identity.OIDCClient,
	verifier *identity.TokenVerifier,
	resolver identity.PrincipalResolver,
	sessionUsers identity.PrincipalByIDResolver,
	sessions store.AuthSessionsStore,
	allowedOrigins []string,
) *AuthEndpoints {
	return &AuthEndpoints{
		client:          client,
		verifier:        verifier,
		resolver:        resolver,
		sessionUsers:    sessionUsers,
		sessions:        sessions,
		originAllowlist: buildOriginAllowlist(allowedOrigins),
		nowFunc:         time.Now,
	}
}

// WithBearerProbe arms the identity echo for callers presenting a valid
// bearer credential instead of the cookie (API-token browsers, e2e
// harnesses): the probe answers with that principal. No session is
// created or consumed by this path.
func (e *AuthEndpoints) WithBearerProbe(probe func(*gin.Context) (*model.PrincipalContext, bool)) *AuthEndpoints {
	e.bearerProbe = probe
	return e
}

// RegisterRoutes attaches the protocol routes to the /auth group.
func (e *AuthEndpoints) RegisterRoutes(group *gin.RouterGroup) {
	group.GET("/authorize", e.Authorize)
	group.GET("/callback", e.Callback)
	group.GET("/session", e.Session)
	group.POST("/logout", e.Logout)
}

// Authorize starts the authorization-code flow: validate the console's
// return target against the exact Origin allowlist, persist a one-shot
// handshake row (state hash + PKCE verifier), and 302 into the IdP.
func (e *AuthEndpoints) Authorize(c *gin.Context) {
	redirectURI := c.Query("redirect_uri")
	parsed, err := url.Parse(redirectURI)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "redirect_uri must be an absolute HTTP(S) URL")
		return
	}
	// The Open Redirect boundary: the return target's origin must match
	// the allowlist EXACTLY (no prefixes, no wildcards — the security
	// authority's rule), and it may never point back into the protocol
	// group itself.
	if !originAllowed(parsed.Scheme+"://"+parsed.Host, e.originAllowlist) || strings.HasPrefix(parsed.Path, "/auth") {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "redirect_uri origin is not allowed")
		return
	}
	clientState := c.Query("state")
	if len(clientState) > 200 {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "state is too long")
		return
	}

	stateToken, stateHash, err := identity.NewOpaqueToken()
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Login could not be started")
		return
	}
	codeVerifier, codeChallenge, err := identity.NewCodeVerifier()
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Login could not be started")
		return
	}
	callbackURI := externalBaseURL(c) + "/auth/callback"
	request := store.AuthLoginRequest{
		ID:             uuid.NewString(),
		StateTokenHash: stateHash,
		ClientState:    clientState,
		RedirectURI:    redirectURI,
		CallbackURI:    callbackURI,
		CodeVerifier:   codeVerifier,
		ExpiresAt:      e.nowFunc().Add(identity.LoginRequestTTL),
	}
	if err := e.sessions.CreateLoginRequest(c.Request.Context(), request); err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Login could not be started")
		return
	}
	target, err := e.client.AuthorizeURL(callbackURI, stateToken, codeChallenge)
	if err != nil {
		staticErrorReply(c, http.StatusServiceUnavailable, "IDP_UNAVAILABLE", "The identity provider could not be reached")
		return
	}
	c.Redirect(http.StatusFound, target)
}

// Callback finishes the flow: burn the one-shot state, exchange the
// code with the PKCE verifier, verify the access token through the SAME
// frozen chain as bearer requests, resolve the server-side principal,
// and establish the opaque session + HttpOnly cookie. Every failure
// bounces back to the console with auth_error + the client's
// return-state — never a bare error page mid-redirect.
func (e *AuthEndpoints) Callback(c *gin.Context) {
	failToClient := func(redirectURI, reason, clientState string) {
		c.Redirect(http.StatusFound, withAuthError(redirectURI, reason, clientState))
	}
	// An unresolvable state (unknown, expired, replayed) leaves no
	// trustworthy return target: bounce same-origin to the shell.
	stateToken := c.Query("state")
	if stateToken == "" {
		c.Redirect(http.StatusFound, dashboardAuthError("state_invalid"))
		return
	}
	request, found, err := e.sessions.ConsumeLoginRequest(c.Request.Context(), identity.HashOpaqueToken(stateToken))
	if err != nil || !found {
		c.Redirect(http.StatusFound, dashboardAuthError("state_invalid"))
		return
	}

	if idpError := c.Query("error"); idpError != "" {
		failToClient(request.RedirectURI, idpError, request.ClientState)
		return
	}
	code := c.Query("code")
	if code == "" {
		failToClient(request.RedirectURI, "code_missing", request.ClientState)
		return
	}
	accessToken, err := e.client.ExchangeCode(code, request.CallbackURI, request.CodeVerifier)
	if err != nil {
		failToClient(request.RedirectURI, "exchange_failed", request.ClientState)
		return
	}
	claims, err := e.verifier.Verify(accessToken, e.nowFunc())
	if err != nil {
		failToClient(request.RedirectURI, "token_invalid", request.ClientState)
		return
	}
	principal, err := e.resolver.Resolve(c.Request.Context(), claims.Issuer, claims.Subject)
	if err != nil {
		failToClient(request.RedirectURI, "identity_unresolvable", request.ClientState)
		return
	}

	token, tokenHash, err := identity.NewOpaqueToken()
	if err != nil {
		failToClient(request.RedirectURI, "session_unavailable", request.ClientState)
		return
	}
	expiresAt := e.nowFunc().Add(identity.SessionTTL)
	session := store.AuthSession{
		ID:        uuid.NewString(),
		UserID:    principal.PrincipalID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	}
	if err := e.sessions.CreateSession(c.Request.Context(), session, request.ID); err != nil {
		failToClient(request.RedirectURI, "session_unavailable", request.ClientState)
		return
	}
	setSessionCookie(c, token, expiresAt)
	c.Redirect(http.StatusFound, request.RedirectURI)
}

// Session answers the console's session probe. Unauthenticated (no
// cookie, unknown, revoked or expired session) is 401; the client turns
// that into the login gate.
func (e *AuthEndpoints) Session(c *gin.Context) {
	principal, ok := e.sessionPrincipal(c)
	if !ok && e.bearerProbe != nil && c.GetHeader("Authorization") != "" {
		principal, ok = e.bearerProbe(c)
	}
	if !ok {
		c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}
	roles := make([]string, 0, len(principal.ProjectMemberships))
	scope := make([]string, 0, len(principal.ProjectMemberships))
	for projectID, role := range principal.ProjectMemberships {
		roles = append(roles, role)
		scope = append(scope, projectID)
	}
	sort.Strings(roles)
	sort.Strings(scope)
	c.JSON(http.StatusOK, gin.H{
		"principal":     principal.PrincipalID,
		"roles":         roles,
		"project_scope": scope,
	})
}

// Logout revokes the session server-side (with its audit row) and
// clears the cookie. Idempotent: an absent or unknown cookie still
// answers 204 so the client can re-probe server truth.
func (e *AuthEndpoints) Logout(c *gin.Context) {
	if origin := c.GetHeader("Origin"); origin != "" && !originAllowed(origin, e.originAllowlist) {
		staticErrorReply(c, http.StatusForbidden, "CSRF_ORIGIN_BLOCKED", "Cross-origin session requests are not permitted")
		return
	}
	if token, err := c.Cookie(identity.SessionCookieName); err == nil && token != "" {
		// Actor for the audit row: the session being revoked itself.
		if session, found, lookupErr := e.sessions.SessionByTokenHash(c.Request.Context(), identity.HashOpaqueToken(token)); lookupErr == nil && found {
			_, _ = e.sessions.RevokeSessionByTokenHash(c.Request.Context(), session.TokenHash, session.UserID, uuid.NewString())
		}
	}
	setSessionCookie(c, "", time.Time{})
	c.Status(http.StatusNoContent)
}

// sessionPrincipal validates the cookie credential and rebuilds a FRESH
// principal: session validity (revocation, expiry) is re-checked per
// request and memberships re-resolved from the registry, so revocations
// propagate immediately.
func (e *AuthEndpoints) sessionPrincipal(c *gin.Context) (*model.PrincipalContext, bool) {
	token, err := c.Cookie(identity.SessionCookieName)
	if err != nil || token == "" {
		return nil, false
	}
	session, found, err := e.sessions.SessionByTokenHash(c.Request.Context(), identity.HashOpaqueToken(token))
	if err != nil || !found {
		return nil, false
	}
	now := e.nowFunc()
	if session.RevokedAt != nil || now.After(session.ExpiresAt) {
		return nil, false
	}
	resolved, err := e.sessionUsers.ResolveByID(c.Request.Context(), session.UserID)
	if err != nil {
		return nil, false
	}
	_ = e.sessions.TouchSession(c.Request.Context(), session.TokenHash)
	return resolved, true
}

// setSessionCookie writes the BFF cookie with the frozen attributes:
// HttpOnly, Secure, SameSite=Lax, path-scoped to the whole origin. An
// empty value clears it.
func setSessionCookie(c *gin.Context, token string, expiresAt time.Time) {
	maxAge := -1
	if token != "" {
		maxAge = int(time.Until(expiresAt).Seconds())
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(identity.SessionCookieName, token, maxAge, "/", "", true, true)
}

// externalBaseURL derives this deployment's externally visible base
// URL from the request itself (no trusted proxies are configured, so
// the Host header is the client's own truth).
func externalBaseURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}

// withAuthError bounces a callback failure back to the console with the
// auth_error code and the client's return-state (LoginGate only trusts
// the pair together). The redirect target was allowlist-validated when
// the handshake started; a malformed value here degrades to the shell.
func withAuthError(redirectURI, reason, clientState string) string {
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return dashboardAuthError(reason)
	}
	query := parsed.Query()
	query.Set("auth_error", reason)
	if clientState != "" {
		query.Set("state", clientState)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// dashboardAuthError is the same-origin fallback when no allowlisted
// return target is resolvable (unknown/replayed state).
func dashboardAuthError(reason string) string {
	return "/dashboard?auth_error=" + url.QueryEscape(reason)
}

// originAllowed is the exact-match Origin check (security authority
// section 7: no prefix or wildcard judgments).
func originAllowed(origin string, allowlist map[string]struct{}) bool {
	if normalized, ok := normalizeOrigin(origin); ok {
		if _, allowed := allowlist[normalized]; allowed {
			return true
		}
	}
	return false
}
