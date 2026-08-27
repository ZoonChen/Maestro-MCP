package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestAuthMiddlewareFailsClosed(t *testing.T) {
	r := gin.New()
	r.Use(AuthMiddleware(""))
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/dashboard", func(c *gin.Context) { c.Status(http.StatusOK) })

	health := httptest.NewRecorder()
	r.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	assert.Equal(t, http.StatusOK, health.Code)

	dashboard := httptest.NewRecorder()
	r.ServeHTTP(dashboard, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	assert.Equal(t, http.StatusUnauthorized, dashboard.Code)
	assert.Contains(t, dashboard.Body.String(), "AUTH_NOT_CONFIGURED")
}

func TestAuthMiddlewareRejectsInvalidTokenWith401(t *testing.T) {
	r := gin.New()
	r.Use(AuthMiddleware("expected"))
	r.GET("/api", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Header().Get("WWW-Authenticate"), "invalid_token")
}

func TestCORSUsesExactAllowlistAndSameOrigin(t *testing.T) {
	r := gin.New()
	r.Use(CORS("https://allowed.example"))
	r.GET("/api", func(c *gin.Context) { c.Status(http.StatusOK) })

	tests := []struct {
		name   string
		origin string
		host   string
		want   int
	}{
		{name: "explicit", origin: "https://allowed.example", host: "maestro.example", want: http.StatusOK},
		{name: "same origin", origin: "http://maestro.example", host: "maestro.example", want: http.StatusOK},
		{name: "prefix attack", origin: "https://allowed.example.evil.test", host: "maestro.example", want: http.StatusForbidden},
		{name: "null origin", origin: "null", host: "maestro.example", want: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tt.host+"/api", nil)
			req.Host = tt.host
			req.Header.Set("Origin", tt.origin)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.Equal(t, tt.want, w.Code)
		})
	}
}

func TestRemoteWriteGuardDefaultsToReadOnly(t *testing.T) {
	r := gin.New()
	r.Use(RemoteWriteGuard(false))
	r.POST("/api/v1/projects", func(c *gin.Context) { c.Status(http.StatusCreated) })
	r.POST("/mcp", func(c *gin.Context) { c.Status(http.StatusOK) })

	apiWrite := httptest.NewRecorder()
	r.ServeHTTP(apiWrite, httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(`{}`)))
	assert.Equal(t, http.StatusForbidden, apiWrite.Code)
	assert.Contains(t, apiWrite.Body.String(), "REMOTE_WRITE_DISABLED")

	initialize := httptest.NewRecorder()
	r.ServeHTTP(initialize, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)))
	assert.Equal(t, http.StatusOK, initialize.Code)

	toolCall := httptest.NewRecorder()
	r.ServeHTTP(toolCall, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`)))
	assert.Equal(t, http.StatusForbidden, toolCall.Code)
}

func TestRemoteWriteGuardCanBeExplicitlyEnabled(t *testing.T) {
	r := gin.New()
	r.Use(RemoteWriteGuard(true))
	r.POST("/api", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api", nil))
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestDrainGuardStopsNewWritesAndAllowsHealthAndReadOnlyMCP(t *testing.T) {
	var draining atomic.Bool
	writeCalled := false
	r := gin.New()
	r.Use(DrainGuard(draining.Load))
	r.GET("/livez", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/write", func(c *gin.Context) {
		writeCalled = true
		c.Status(http.StatusNoContent)
	})
	r.POST("/mcp", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	draining.Store(true)

	health := httptest.NewRecorder()
	r.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/livez", nil))
	require.Equal(t, http.StatusOK, health.Code)

	write := httptest.NewRecorder()
	r.ServeHTTP(write, httptest.NewRequest(http.MethodPost, "/write", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusServiceUnavailable, write.Code)
	assert.False(t, writeCalled)
	assert.Contains(t, write.Body.String(), "RUNTIME_DRAINING")
	assert.Contains(t, write.Body.String(), "correlation_id")

	initialize := httptest.NewRecorder()
	r.ServeHTTP(initialize, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
	)))
	require.Equal(t, http.StatusNoContent, initialize.Code)

	toolCall := httptest.NewRecorder()
	r.ServeHTTP(toolCall, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`,
	)))
	require.Equal(t, http.StatusServiceUnavailable, toolCall.Code)
	assert.Contains(t, toolCall.Body.String(), "RUNTIME_DRAINING")
}

func TestSetupRouterDoesNotExposeLocalMergeRoute(t *testing.T) {
	r := SetupRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, "token")
	claimRoutes := map[string]string{
		"/api/v1/projects/:id/tasks/next":              "",
		"/api/v1/projects/:id/tasks/next-verification": "",
	}
	for _, route := range r.Routes() {
		assert.NotEqual(t, "/api/v1/projects/:id/tasks/:tid/merge", route.Path)
		if _, ok := claimRoutes[route.Path]; ok {
			claimRoutes[route.Path] = route.Method
		}
	}
	for path, method := range claimRoutes {
		assert.Equal(t, http.MethodPost, method, "claim endpoint must be a remote-write-gated mutation: %s", path)
	}
}

func TestSetupRouterAccessLogOmitsQueryString(t *testing.T) {
	var accessLog bytes.Buffer
	r := SetupRouter(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, "token",
		RouterOptions{LogWriter: &accessLog},
	)
	queryCanary := "m0-query-secret-canary"
	request := httptest.NewRequest(
		http.MethodGet,
		"/health?access_token="+queryCanary+"&credential=also-sensitive",
		nil,
	)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, accessLog.String(), "/health")
	assert.NotContains(t, accessLog.String(), queryCanary)
	assert.NotContains(t, accessLog.String(), "access_token")
	assert.NotContains(t, accessLog.String(), "credential")

	pathCanary := "m0-access-path-secret-canary"
	unmatched := httptest.NewRequest(
		http.MethodGet,
		"/unknown/"+pathCanary+"?access_token="+queryCanary,
		nil,
	)
	unmatched.Header.Set("Authorization", "Bearer token")
	unmatchedResponse := httptest.NewRecorder()
	r.ServeHTTP(unmatchedResponse, unmatched)
	require.Equal(t, http.StatusNotFound, unmatchedResponse.Code)
	assert.Contains(t, accessLog.String(), `route="<unmatched>"`)
	assert.NotContains(t, accessLog.String(), pathCanary)
	assert.NotContains(t, unmatchedResponse.Body.String(), pathCanary)
	assert.NotContains(t, unmatchedResponse.Body.String(), queryCanary)
	assert.Contains(t, unmatchedResponse.Body.String(), "ROUTE_NOT_FOUND")
	assert.Contains(t, unmatchedResponse.Body.String(), "correlation_id")
}

func TestSetupRouterDoesNotTrustForwardedClientIP(t *testing.T) {
	var accessLog bytes.Buffer
	r := SetupRouter(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, "token",
		RouterOptions{LogWriter: &accessLog},
	)
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.RemoteAddr = "192.0.2.10:43210"
	request.Header.Set("X-Forwarded-For", "203.0.113.77")
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, accessLog.String(), "192.0.2.10")
	assert.NotContains(t, accessLog.String(), "203.0.113.77")
}

func TestForwardedForCannotBypassRateLimit(t *testing.T) {
	r := SetupRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, "token")
	for index := range 101 {
		request := httptest.NewRequest(http.MethodGet, "/health", nil)
		request.RemoteAddr = "192.0.2.20:43210"
		request.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", index+1))
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		if index < 100 {
			require.Equal(t, http.StatusOK, response.Code, "request %d", index+1)
			continue
		}
		require.Equal(t, http.StatusTooManyRequests, response.Code)
		assert.Contains(t, response.Body.String(), "RATE_LIMIT_EXCEEDED")
	}
}

func TestRecoveryLogOmitsRequestAndPanicSecrets(t *testing.T) {
	var recoveryLog bytes.Buffer
	r := gin.New()
	r.Use(safeRecoveryWithWriter(&recoveryLog))
	r.GET("/panic/:untrusted", func(*gin.Context) {
		panic("m0-panic-secret-canary")
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/panic/m0-path-secret-canary?access_token=m0-query-secret-canary",
		nil,
	)
	request.Header.Set("Cookie", "session=m0-cookie-secret-canary")
	request.Header.Set("X-API-Key", "m0-header-secret-canary")
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)

	require.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Contains(t, recoveryLog.String(), `route="/panic/:untrusted"`)
	assert.Contains(t, recoveryLog.String(), "correlation_id")
	assert.Contains(t, response.Body.String(), `"error_code":"INTERNAL_ERROR"`)
	assert.Contains(t, response.Body.String(), `"correlation_id":"`)
	for _, secret := range []string{
		"m0-panic-secret-canary",
		"m0-path-secret-canary",
		"m0-query-secret-canary",
		"m0-cookie-secret-canary",
		"m0-header-secret-canary",
	} {
		assert.NotContains(t, recoveryLog.String(), secret)
		assert.NotContains(t, response.Body.String(), secret)
	}
}

func TestRESTErrorBoundaryNeverExposesInternalOrValidationCause(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "internal dependency",
			err:        fmt.Errorf("database m0-rest-internal-canary: %w", store.ErrRecoveryIntegrity),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL_ERROR",
		},
		{
			name:       "wrapped client error",
			err:        fmt.Errorf("field m0-rest-client-canary: %w", store.ErrInvalidParameter),
			wantStatus: http.StatusBadRequest,
			wantCode:   "INVALID_PARAMETER",
		},
		{
			name: "validation cause",
			err: &service.ValidationError{
				Code: "VALIDATION_INPUT_INVALID", Message: "m0-rest-validation-message-canary",
				Cause: errors.New("m0-rest-validation-cause-canary"),
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "VALIDATION_INPUT_INVALID",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := gin.New()
			r.GET("/failure", func(c *gin.Context) { errorReply(c, test.err) })
			response := httptest.NewRecorder()
			r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/failure", nil))
			require.Equal(t, test.wantStatus, response.Code)
			assert.Contains(t, response.Body.String(), `"error_code":"`+test.wantCode+`"`)
			assert.Contains(t, response.Body.String(), `"correlation_id":"`)
			assert.NotContains(t, response.Body.String(), "canary")
		})
	}
}

func TestProjectGuardNeverExposesStoreErrorOrProjectIdentifier(t *testing.T) {
	database, err := store.NewSQLiteDB(":memory:")
	require.NoError(t, err)
	require.NoError(t, database.Init(context.Background()))
	projectService := service.NewProjectService(store.NewSQLiteProjectStore(database.DB()), nil)
	require.NoError(t, database.Close())

	r := gin.New()
	r.GET("/projects/:id", ProjectGuard(projectService), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	canary := "m0-projectguard-secret-canary"
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/projects/"+canary, nil))

	require.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Contains(t, response.Body.String(), `"error_code":"INTERNAL_ERROR"`)
	assert.Contains(t, response.Body.String(), `"correlation_id":"`)
	assert.NotContains(t, response.Body.String(), canary)
	assert.NotContains(t, response.Body.String(), "database is closed")
}
