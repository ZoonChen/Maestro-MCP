package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Middleware-level cookie-session tests without the CORS layer, so the
// CSRF boundary of authenticateSession itself is what answers (the
// full-router variant is covered by the PG-gated browser flow, where
// either layer may reject first).

type fakeSessionStore struct {
	sessions map[string]*store.AuthSession // token string -> row
}

func (f *fakeSessionStore) CreateLoginRequest(_ context.Context, _ store.AuthLoginRequest) error {
	return nil
}

func (f *fakeSessionStore) ConsumeLoginRequest(_ context.Context, _ []byte) (*store.AuthLoginRequest, bool, error) {
	return nil, false, nil
}

func (f *fakeSessionStore) CreateSession(_ context.Context, session store.AuthSession, _ string) error {
	f.sessions[string(session.TokenHash)] = &session
	return nil
}

func (f *fakeSessionStore) SessionByTokenHash(_ context.Context, tokenHash []byte) (*store.AuthSession, bool, error) {
	session, ok := f.sessions[string(tokenHash)]
	if !ok {
		return nil, false, nil
	}
	return session, true, nil
}

func (f *fakeSessionStore) TouchSession(_ context.Context, _ []byte) error { return nil }

func (f *fakeSessionStore) RevokeSessionByTokenHash(_ context.Context, tokenHash []byte, _, _ string) (bool, error) {
	if session, ok := f.sessions[string(tokenHash)]; ok && session.RevokedAt == nil {
		now := time.Now()
		session.RevokedAt = &now
		return true, nil
	}
	return false, nil
}

// sessionCookie builds the incoming credential cookie; transport
// attributes are meaningless on an incoming request cookie.
func sessionCookie(value string) *http.Cookie {
	return &http.Cookie{Name: identity.SessionCookieName, Value: value} //nolint:gosec // incoming test credential; attributes only apply to Set-Cookie
}

type fakeByIDResolver struct{}

func (fakeByIDResolver) ResolveByID(_ context.Context, userID string) (*model.PrincipalContext, error) {
	if userID != "user-1" {
		return nil, assert.AnError
	}
	return &model.PrincipalContext{
		PrincipalID:        userID,
		Type:               model.PrincipalTypeHuman,
		ProjectMemberships: map[string]string{"project-a": "developer"},
	}, nil
}

func newSessionMiddlewareRouter(t *testing.T, sessions *fakeSessionStore) *gin.Engine {
	t.Helper()
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	verifier, err := identity.NewTokenVerifier("https://idp.example", "maestro", nil)
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, &identity.StaticResolver{}).
		WithBrowserSessions(sessions, fakeByIDResolver{}, []string{"http://console.example"})

	router := gin.New()
	router.Use(mw.Authenticate)
	router.GET("/api/v1/projects", func(c *gin.Context) { c.Status(http.StatusOK) })
	return router
}

func seedFakeSession(t *testing.T, sessions *fakeSessionStore) (token string, row *store.AuthSession) {
	t.Helper()
	token = "fake-session-token"
	tokenHash := identity.HashOpaqueToken(token)
	row = &store.AuthSession{
		ID:        "018f7500-0000-7000-8000-000000000301",
		UserID:    "user-1",
		TokenHash: tokenHash,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	sessions.sessions[string(tokenHash)] = row
	return token, row
}

func TestSessionCookieAuthenticationBoundary(t *testing.T) {
	sessions := &fakeSessionStore{sessions: map[string]*store.AuthSession{}}
	router := newSessionMiddlewareRouter(t, sessions)

	t.Run("valid cookie authenticates", func(t *testing.T) {
		token, _ := seedFakeSession(t, sessions)
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		request.AddCookie(sessionCookie(token))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusOK, response.Code)
	})

	t.Run("cross-origin cookie request is 403 at the session boundary", func(t *testing.T) {
		token, _ := seedFakeSession(t, sessions)
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		request.AddCookie(sessionCookie(token))
		request.Header.Set("Origin", "http://evil.example")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusForbidden, response.Code)
		assert.Contains(t, response.Body.String(), "CSRF_ORIGIN_BLOCKED")
	})

	t.Run("allowed origin passes the boundary", func(t *testing.T) {
		token, _ := seedFakeSession(t, sessions)
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		request.AddCookie(sessionCookie(token))
		request.Header.Set("Origin", "http://console.example")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusOK, response.Code)
	})

	t.Run("revoked session is 401", func(t *testing.T) {
		token, row := seedFakeSession(t, sessions)
		now := time.Now()
		row.RevokedAt = &now
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		request.AddCookie(sessionCookie(token))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
	})

	t.Run("expired session is 401", func(t *testing.T) {
		token, row := seedFakeSession(t, sessions)
		row.ExpiresAt = time.Now().Add(-time.Minute)
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		request.AddCookie(sessionCookie(token))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
	})

	t.Run("unknown cookie value is 401", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		request.AddCookie(sessionCookie("no-such-session"))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
	})

	t.Run("console shell stays anonymous", func(t *testing.T) {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
		assert.Equal(t, http.StatusNotFound, response.Code, "no shell route on this bare engine, but never 401")
	})
}
