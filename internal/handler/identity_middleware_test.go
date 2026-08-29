package handler

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The 401/403/404 authorization matrix for the OIDC identity middleware,
// driven end to end through the real router with a local mock IdP issuing
// genuinely signed RS256 tokens (the token layer itself is covered by the
// identity package's verifier tests).

type handlerTestIdP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newHandlerTestIdP(t *testing.T) *handlerTestIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	idp := &handlerTestIdP{key: key, kid: "handler-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`,
			idp.server.URL, idp.server.URL+"/certs")
	})
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":"AQAB"}]}`,
			idp.kid, base64.RawURLEncoding.EncodeToString(key.N.Bytes()))
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *handlerTestIdP) signedToken(t *testing.T, subject string) string {
	t.Helper()
	now := time.Now().UTC().Unix()
	header, headerErr := json.Marshal(map[string]any{"alg": "RS256", "kid": idp.kid})
	require.NoError(t, headerErr)
	claims, claimsErr := json.Marshal(map[string]any{
		"iss": idp.server.URL, "sub": subject,
		"aud": []string{"maestro"},
		"exp": now + 900, "nbf": now - 10, "iat": now, "jti": "handler-jti",
	})
	require.NoError(t, claimsErr)
	encode := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	signed := encode(header) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, digest[:])
	require.NoError(t, err)
	return signed + "." + encode(signature)
}

func newOIDCTestRouter(t *testing.T, memberships map[string]map[string]string) (*gin.Engine, *handlerTestIdP) {
	t.Helper()
	idp := newHandlerTestIdP(t)
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mount := NewOIDCMiddleware(policy, verifier, &identity.StaticResolver{Memberships: memberships}).IdentityMount()

	router := SetupRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, "unused-legacy-token",
		RouterOptions{Identity: mount, RemoteWrite: true})
	return router, idp
}

func TestOIDCMiddlewareMatrix(t *testing.T) {
	router, idp := newOIDCTestRouter(t, map[string]map[string]string{
		"user-developer": {"project-a": "developer"},
		"user-admin":     {"project-a": "project_admin"},
		"user-outsider":  {"project-b": "developer"},
	})

	tests := []struct {
		name       string
		subject    string // empty means no Authorization header
		rawToken   string
		method     string
		target     string
		wantStatus int
		// passedAuthz marks cases where authorization SUCCEEDS and the
		// assertion is only that no 401/403/404 came from the identity
		// layer (the router carries nil handlers, so downstream replies
		// are not meaningful here).
		passedAuthz bool
	}{
		{"no token is 401", "", "", http.MethodGet, "/api/v1/projects", http.StatusUnauthorized, false},
		{"garbage token is 401", "", "garbage", http.MethodGet, "/api/v1/projects", http.StatusUnauthorized, false},
		{"unknown subject is 401", "user-unknown", "", http.MethodGet, "/api/v1/projects", http.StatusUnauthorized, false},
		{"member lists projects passes authz", "user-developer", "", http.MethodGet, "/api/v1/projects", 0, true},
		{"developer cannot archive 403", "user-developer", "", http.MethodPost, "/api/v1/projects/project-a/archive", http.StatusForbidden, false},
		{"non-member project hides as 404", "user-outsider", "", http.MethodGet, "/api/v1/projects/project-a/tasks", http.StatusNotFound, false},
		{"member reads tasks passes authz", "user-developer", "", http.MethodGet, "/api/v1/projects/project-a/tasks", 0, true},
		{"admin archive passes authz", "user-admin", "", http.MethodPost, "/api/v1/projects/project-a/archive", 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, nil)
			switch {
			case test.rawToken != "":
				request.Header.Set("Authorization", "Bearer "+test.rawToken)
			case test.subject != "":
				request.Header.Set("Authorization", "Bearer "+idp.signedToken(t, test.subject))
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if test.passedAuthz {
				assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
					response.Code, "identity layer must not reject: body: %s", response.Body.String())
				return
			}
			assert.Equal(t, test.wantStatus, response.Code, "body: %s", response.Body.String())
		})
	}
}

func TestOIDCMiddlewareDoesNotLeakReasons(t *testing.T) {
	router, idp := newOIDCTestRouter(t, map[string]map[string]string{
		"user-developer": {"project-a": "developer"},
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project-a/archive", nil)
	request.Header.Set("Authorization", "Bearer "+idp.signedToken(t, "user-developer"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code)
	assert.NotContains(t, response.Body.String(), "project_admin")
	assert.NotContains(t, response.Body.String(), "policy")
}

func TestOIDCMiddlewareHealthStaysAnonymous(t *testing.T) {
	router, _ := newOIDCTestRouter(t, nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	assert.Equal(t, http.StatusOK, response.Code)
}
