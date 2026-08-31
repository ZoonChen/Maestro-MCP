package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WebSocket-surface authorization consistency (M1 exit gate: REST, MCP,
// WebSocket and background decide through the same authorize). The WS
// upgrade lives under /api/v1, so the OIDC middleware's route map and
// the frozen resource-hiding semantics govern it identically — these
// tests prove it end to end through the real router.

func TestWebSocketSurfaceAuthorizationMatrix(t *testing.T) {
	router, idp := newOIDCTestRouter(t, map[string]map[string]string{
		"user-developer": {"project-a": "developer"},
		"user-outsider":  {"project-b": "developer"},
	})

	tests := []struct {
		name    string
		subject string
		target  string
		want    int
		// Upgrade attempts that pass authorization fail the handshake
		// (426/400 without WS support in httptest) — never 401/403/404.
		expectHandshakeFailure bool
	}{
		{"no token is 401", "", "/api/v1/projects/project-a/ws", http.StatusUnauthorized, false},
		{"cross-project hides as 404", "user-outsider", "/api/v1/projects/project-a/ws", http.StatusNotFound, false},
		{"member passes authorization to the handshake", "user-developer", "/api/v1/projects/project-a/ws", 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "websocket")
			request.Header.Set("Sec-WebSocket-Version", "13")
			request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			if test.subject != "" {
				request.Header.Set("Authorization", "Bearer "+idp.signedToken(t, test.subject))
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if test.expectHandshakeFailure {
				assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
					response.Code, "identity layer must not reject a member's upgrade: code=%d body=%s",
					response.Code, response.Body.String())
				return
			}
			assert.Equal(t, test.want, response.Code, "body: %s", response.Body.String())
		})
	}
}

func TestWebSocketSurfaceSharesTheMCPDecision(t *testing.T) {
	// Both surfaces resolve work_item.read for the same principal through
	// the same frozen policy instance: an outsider denied on REST list is
	// denied on the WS upgrade in the SAME project with the SAME code.
	router, idp := newOIDCTestRouter(t, map[string]map[string]string{
		"user-outsider": {"project-b": "developer"},
	})

	rest := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-a/tasks", nil)
	rest.Header.Set("Authorization", "Bearer "+idp.signedToken(t, "user-outsider"))
	restResponse := httptest.NewRecorder()
	router.ServeHTTP(restResponse, rest)
	require.Equal(t, http.StatusNotFound, restResponse.Code)

	wsRequest := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-a/ws", nil)
	wsRequest.Header.Set("Connection", "Upgrade")
	wsRequest.Header.Set("Upgrade", "websocket")
	wsRequest.Header.Set("Sec-WebSocket-Version", "13")
	wsRequest.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	wsRequest.Header.Set("Authorization", "Bearer "+idp.signedToken(t, "user-outsider"))
	wsResponse := httptest.NewRecorder()
	router.ServeHTTP(wsResponse, wsRequest)
	assert.Equal(t, http.StatusNotFound, wsResponse.Code,
		"the WS surface must hide the same invisible project identically")
}
