package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityMountNilKeepsSharedTokenFailClosed(t *testing.T) {
	r := SetupRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, "token")
	for _, route := range r.Routes() {
		assert.NotContains(t, route.Path, "/auth", "no identity routes may exist without a mounted identity layer")
	}

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	require.Equal(t, http.StatusUnauthorized, response.Code)
	assert.Contains(t, response.Body.String(), "AUTH_REQUIRED")
}

func TestIdentityMountAuthenticateReplacesSharedToken(t *testing.T) {
	authenticated := false
	mount := &IdentityMount{
		Authenticate: func(c *gin.Context) {
			authenticated = true
			c.Set("principal", "user-1")
			c.Next()
		},
	}
	r := SetupRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, "legacy-shared-token",
		RouterOptions{Identity: mount})

	response := httptest.NewRecorder()
	// No Authorization header: the legacy shared-token middleware would have
	// rejected this, the mounted identity layer owns the decision instead.
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusOK, response.Code)
	assert.True(t, authenticated, "mounted Authenticate must run for every request")
}

func TestIdentityMountAuthorizeGuardsAPIV1(t *testing.T) {
	authorizedPaths := []string{}
	mount := &IdentityMount{
		Authenticate: func(c *gin.Context) { c.Next() },
		// A deny-all policy decision point: it must intercept the request
		// before any business handler runs (the nil handler would otherwise
		// fail the request with a 500 panic recovery).
		Authorize: func(c *gin.Context) {
			authorizedPaths = append(authorizedPaths, c.FullPath())
			c.AbortWithStatus(http.StatusForbidden)
		},
	}
	r := SetupRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, "unused",
		RouterOptions{Identity: mount})

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Len(t, authorizedPaths, 1)
	assert.Equal(t, "/api/v1/projects", authorizedPaths[0])

	// Health routes stay outside the authorize(principal, action, resource)
	// decision point.
	response = httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusOK, response.Code)
	assert.Len(t, authorizedPaths, 1, "authorize must not run for anonymous health paths")
}

func TestIdentityMountRegistersAuthRoutes(t *testing.T) {
	mount := &IdentityMount{
		RegisterRoutes: func(group *gin.RouterGroup) {
			group.GET("/login", func(c *gin.Context) { c.String(http.StatusOK, "login-start") })
			group.POST("/callback", func(c *gin.Context) { c.Status(http.StatusOK) })
		},
	}
	r := SetupRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, "token",
		RouterOptions{Identity: mount})

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	require.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), "login-start")
}
