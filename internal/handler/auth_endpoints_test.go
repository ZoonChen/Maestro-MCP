package handler

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated browser-login tests for the /auth protocol group (task brief
// E / UI-AUTH): a mock OIDC IdP with real discovery + JWKS + authorize
// + token endpoints, the real session store (migration 0015), the real
// StoreResolver, and the real frozen permission matrix. The mock IdP
// enforces PKCE S256 and client Basic authentication so the flow
// exercises the same server legs a production IdP would.

const (
	authTestDB    = "maestro_auth_test"
	consoleOrigin = "http://console.example"
	maestroHost   = "maestro.test"
	authSubject   = "browser-user"
)

type browserIdP struct {
	server       *httptest.Server
	key          *rsa.PrivateKey
	kid          string
	clientID     string
	clientSecret string

	lastChallenge string
	lastCallback  string
	lastState     string
	codeIssued    string
	codeVerifier  string
	tokenCalls    int
}

func newBrowserIdP(t *testing.T) *browserIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	idp := &browserIdP{key: key, kid: "browser-key-1", clientID: "maestro-test", clientSecret: "test-secret"}

	issuer := "" // bound after the server exists
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q,"authorization_endpoint":%q,"token_endpoint":%q}`,
			issuer, issuer+"/certs", issuer+"/authorize", issuer+"/token")
	})
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":"AQAB"}]}`,
			idp.kid, base64.RawURLEncoding.EncodeToString(key.N.Bytes()))
	})
	// The authorization endpoint plays the human consent screen: a real
	// IdP would authenticate the user here; the protocol surface it must
	// honor is the parameter contract (PKCE S256, state, callback).
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("response_type") != "code" ||
			query.Get("client_id") != idp.clientID ||
			query.Get("code_challenge_method") != "S256" ||
			query.Get("code_challenge") == "" || query.Get("state") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		callback := query.Get("redirect_uri")
		parsed, err := url.Parse(callback)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		idp.lastChallenge = query.Get("code_challenge")
		idp.lastCallback = callback
		idp.lastState = query.Get("state")
		idp.codeIssued = "code-" + fmt.Sprint(time.Now().UnixNano())
		redirect := parsed
		q := redirect.Query()
		q.Set("code", idp.codeIssued)
		q.Set("state", idp.lastState)
		redirect.RawQuery = q.Encode()
		//nolint:gosec // mock IdP: the redirect target is the maestro server's own callback URL, allowlist-validated upstream
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		idp.tokenCalls++
		username, password, ok := r.BasicAuth()
		if !ok || username != idp.clientID || password != idp.clientSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		form := r.PostForm
		digest := sha256.Sum256([]byte(form.Get("code_verifier")))
		if form.Get("grant_type") != "authorization_code" ||
			form.Get("code") != idp.codeIssued ||
			form.Get("redirect_uri") != idp.lastCallback ||
			base64.RawURLEncoding.EncodeToString(digest[:]) != idp.lastChallenge {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		idp.codeVerifier = form.Get("code_verifier")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":900}`,
			idp.accessToken(t, issuer))
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	issuer = idp.server.URL
	return idp
}

func (idp *browserIdP) accessToken(t *testing.T, issuer string) string {
	t.Helper()
	now := time.Now().UTC().Unix()
	header, headerErr := json.Marshal(map[string]any{"alg": "RS256", "kid": idp.kid})
	require.NoError(t, headerErr)
	claims, claimsErr := json.Marshal(map[string]any{
		"iss": issuer, "sub": authSubject, "aud": []string{"maestro"},
		"exp": now + 900, "nbf": now - 10, "iat": now, "jti": "browser-jti",
	})
	require.NoError(t, claimsErr)
	encode := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	signed := encode(header) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, digest[:])
	require.NoError(t, err)
	return signed + "." + encode(signature)
}

type authFixture struct {
	router  *gin.Engine
	db      *sql.DB
	pg      *store.PostgresStore
	idp     *browserIdP
	project string
	team    string
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, authTestDB))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, authTestDB))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, authTestDB))
		_ = admin.Close()
	})

	db, err := store.OpenPostgres(context.Background(), authDatabaseDSN(t))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	project := "018f7500-0000-7000-8000-000000000101"
	team := "018f7500-0000-7000-8000-000000000100"
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO teams (id, name) VALUES ($1, 'auth e2e team')`, team)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'auth-e2e', 'Auth E2E', 'active')`, project, team)
	require.NoError(t, err)

	idp := newBrowserIdP(t)
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	oidcClient, err := identity.NewOIDCClient(idp.server.URL, idp.clientID, idp.clientSecret, idp.server.Client())
	require.NoError(t, err)

	storeResolver := identity.NewStoreResolver(pg.Identities())
	mw := NewOIDCMiddleware(policy, verifier, storeResolver).
		WithBrowserSessions(pg.AuthSessions(), storeResolver, []string{consoleOrigin})
	endpoints := NewAuthEndpoints(oidcClient, verifier, storeResolver, storeResolver,
		pg.AuthSessions(), []string{consoleOrigin}).WithBearerProbe(mw.BearerProbe())

	quality, err := NewQualityHandler(pg.Quality())
	require.NoError(t, err)
	router := gin.New()
	router.Use(mw.Authenticate)
	mount := mw.IdentityMount()
	mount.RegisterRoutes = endpoints.RegisterRoutes
	router = SetupRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, "unused",
		RouterOptions{Identity: mount, AllowedOrigins: []string{consoleOrigin}})
	RegisterControlPlane(router, ControlPlaneOptions{
		Identity: mw, Quality: quality,
		GitLab:      NewGitLabHandler(pg.Instances()),
		DeadLetters: NewDeadLetterHandler(pg.Webhooks()),
		Scope:       pg.Instances(),
	})
	return &authFixture{router: router, db: db, pg: pg, idp: idp, project: project, team: team}
}

// login drives the full protocol dance and returns the session cookie
// value plus the callback URL (for replay tests).
func (f *authFixture) login(t *testing.T, clientState string) (cookie, callbackURL string) {
	t.Helper()
	authorize := httptest.NewRequest(http.MethodGet,
		"/auth/authorize?redirect_uri="+url.QueryEscape(consoleOrigin+"/dashboard")+"&state="+url.QueryEscape(clientState), nil)
	authorize.Host = maestroHost
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, authorize)
	require.Equal(t, http.StatusFound, response.Code, response.Body.String())
	idpRedirect := response.Header().Get("Location")
	require.NotEmpty(t, idpRedirect)

	// The IdP consent screen (real HTTP round trip to the mock IdP; the
	// browser would follow this redirect, the test captures it).
	noRedirect := f.idp.server.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	idpResponse, err := noRedirect.Get(idpRedirect)
	require.NoError(t, err)
	defer idpResponse.Body.Close()
	require.Equal(t, http.StatusFound, idpResponse.StatusCode)
	callbackURL = idpResponse.Header.Get("Location")
	require.NotEmpty(t, callbackURL)
	require.Contains(t, callbackURL, "/auth/callback?")

	callback := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	callbackResponse := httptest.NewRecorder()
	f.router.ServeHTTP(callbackResponse, callback)
	require.Equal(t, http.StatusFound, callbackResponse.Code, callbackResponse.Body.String())
	assert.Equal(t, consoleOrigin+"/dashboard", callbackResponse.Header().Get("Location"))

	setCookie := callbackResponse.Header().Get("Set-Cookie")
	require.Contains(t, setCookie, identity.SessionCookieName+"=")
	require.Contains(t, setCookie, "HttpOnly")
	require.Contains(t, setCookie, "Secure")
	require.Contains(t, setCookie, "SameSite=Lax")
	cookie = strings.Split(strings.Split(setCookie, ";")[0], "=")[1]
	require.NotEmpty(t, cookie)
	return cookie, callbackURL
}

// idpBearerToken mints a valid RS256 access token from the mock IdP.
func idpBearerToken(t *testing.T, idp *browserIdP) string {
	t.Helper()
	return idp.accessToken(t, idp.server.URL)
}

func requestWithCookie(method, target, cookie string, origin string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.AddCookie(&http.Cookie{Name: identity.SessionCookieName, Value: cookie}) //nolint:gosec // incoming test credential; attributes only apply to Set-Cookie
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	return request
}

func (f *authFixture) subjectUserID(t *testing.T) string {
	t.Helper()
	var userID string
	err := f.db.QueryRowContext(context.Background(),
		`SELECT id FROM users WHERE subject = $1`, authSubject).Scan(&userID)
	require.NoError(t, err)
	return userID
}

// authDatabaseDSN points the shared compose DSN at this fixture's
// scratch database.
func authDatabaseDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	index := strings.LastIndex(dsn, "/")
	if index < 0 {
		t.Fatal("MAESTRO_TEST_POSTGRES_DSN has no database path")
	}
	return dsn[:index+1] + authTestDB
}

func TestAuthBrowserLoginFlow(t *testing.T) {
	f := newAuthFixture(t)

	t.Run("authorize rejects return targets outside the allowlist", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet,
			"/auth/authorize?redirect_uri="+url.QueryEscape("http://evil.example/dashboard"), nil)
		response := httptest.NewRecorder()
		f.router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusBadRequest, response.Code)
	})

	t.Run("full login establishes a session cookie and console session", func(t *testing.T) {
		cookie, _ := f.login(t, "return-state-1")

		// Before any membership exists the session resolves but
		// authorizes nothing: the v3 surface answers 403/404, the
		// session probe still shows the principal.
		session := httptest.NewRecorder()
		f.router.ServeHTTP(session, requestWithCookie(http.MethodGet, "/auth/session", cookie, ""))
		require.Equal(t, http.StatusOK, session.Code)
		assert.Contains(t, session.Body.String(), `"principal"`)
		assert.Contains(t, session.Body.String(), `"roles":[]`)

		// Membership granted after login is visible on the NEXT request:
		// the principal is re-resolved per request, never cached in the
		// session row.
		_, err := f.db.ExecContext(context.Background(),
			`INSERT INTO memberships (team_id, user_id, role) VALUES ($1, $2, 'developer')`,
			f.team, f.subjectUserID(t))
		require.NoError(t, err)

		session2 := httptest.NewRecorder()
		f.router.ServeHTTP(session2, requestWithCookie(http.MethodGet, "/auth/session", cookie, ""))
		require.Equal(t, http.StatusOK, session2.Code)
		assert.Contains(t, session2.Body.String(), `"developer"`)
		assert.Contains(t, session2.Body.String(), f.project)

		// The cookie authenticates the human /api/v3 surface with a REAL
		// handler (developer holds quality.read).
		policy := httptest.NewRecorder()
		f.router.ServeHTTP(policy, requestWithCookie(http.MethodGet,
			"/api/v3/projects/"+f.project+"/quality-policy", cookie, ""))
		require.Equal(t, http.StatusOK, policy.Code, policy.Body.String())

		// Creation is audited atomically with the session row.
		var created int
		err = f.db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM audit_events WHERE action = 'auth.session.created'`).Scan(&created)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, created, 1)
	})

	t.Run("PKCE verifier traveled through the exchange", func(t *testing.T) {
		before := f.idp.tokenCalls
		f.login(t, "return-state-2")
		assert.NotEmpty(t, f.idp.codeVerifier, "the token exchange must carry the PKCE verifier")
		assert.Equal(t, before+1, f.idp.tokenCalls)
		assert.NotEqual(t, f.idp.codeVerifier, f.idp.lastChallenge)
	})

	t.Run("replayed callback state fails closed", func(t *testing.T) {
		_, callbackURL := f.login(t, "return-state-3")
		replay := httptest.NewRequest(http.MethodGet, callbackURL, nil)
		replayResponse := httptest.NewRecorder()
		f.router.ServeHTTP(replayResponse, replay)
		require.Equal(t, http.StatusFound, replayResponse.Code)
		location := replayResponse.Header().Get("Location")
		assert.Contains(t, location, "auth_error=state_invalid")
	})

	t.Run("expired session is 401", func(t *testing.T) {
		cookie, _ := f.login(t, "return-state-4")
		_, err := f.db.ExecContext(context.Background(),
			`UPDATE auth_sessions SET created_at = now() - interval '9 hours', expires_at = now() - interval '1 minute'`)
		require.NoError(t, err)
		session := httptest.NewRecorder()
		f.router.ServeHTTP(session, requestWithCookie(http.MethodGet, "/auth/session", cookie, ""))
		assert.Equal(t, http.StatusUnauthorized, session.Code)
	})

	t.Run("cross-origin cookie request is 403 (CSRF boundary)", func(t *testing.T) {
		cookie, _ := f.login(t, "return-state-5")
		blocked := httptest.NewRecorder()
		f.router.ServeHTTP(blocked, requestWithCookie(http.MethodGet,
			"/api/v3/projects/"+f.project+"/quality-policy", cookie, "http://evil.example"))
		// Either layer may answer: the engine CORS allowlist or the
		// session CSRF boundary (see TestSessionCSRFBoundaryBlocksFirst
		// for the middleware-level assertion).
		assert.Equal(t, http.StatusForbidden, blocked.Code)
	})

	t.Run("logout revokes server-side and clears the cookie", func(t *testing.T) {
		cookie, _ := f.login(t, "return-state-6")

		crossOrigin := httptest.NewRecorder()
		f.router.ServeHTTP(crossOrigin, requestWithCookie(http.MethodPost, "/auth/logout", cookie, "http://evil.example"))
		assert.Equal(t, http.StatusForbidden, crossOrigin.Code)

		logout := httptest.NewRecorder()
		f.router.ServeHTTP(logout, requestWithCookie(http.MethodPost, "/auth/logout", cookie, consoleOrigin))
		require.Equal(t, http.StatusNoContent, logout.Code)

		session := httptest.NewRecorder()
		f.router.ServeHTTP(session, requestWithCookie(http.MethodGet, "/auth/session", cookie, ""))
		assert.Equal(t, http.StatusUnauthorized, session.Code)

		var revoked int
		err := f.db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM audit_events WHERE action = 'auth.session.revoked'`).Scan(&revoked)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, revoked, 1)
	})

	t.Run("bearer credential echoes its principal on the session probe", func(t *testing.T) {
		// The API-token identity echo: a valid bearer header answers the
		// probe without any cookie (no session is created).
		request := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
		request.Header.Set("Authorization", "Bearer "+idpBearerToken(t, f.idp))
		response := httptest.NewRecorder()
		f.router.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), f.subjectUserID(t))

		garbage := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
		garbage.Header.Set("Authorization", "Bearer not-a-token")
		garbageResponse := httptest.NewRecorder()
		f.router.ServeHTTP(garbageResponse, garbage)
		assert.Equal(t, http.StatusUnauthorized, garbageResponse.Code)
	})

	t.Run("anonymous console shell loads; anonymous data calls stay 401", func(t *testing.T) {
		shell := httptest.NewRecorder()
		f.router.ServeHTTP(shell, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
		// The embedded shell may or may not exist in this worktree build;
		// the identity middleware must not be what answers.
		assert.NotEqual(t, http.StatusUnauthorized, shell.Code)

		data := httptest.NewRecorder()
		f.router.ServeHTTP(data, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
		assert.Equal(t, http.StatusUnauthorized, data.Code)
	})
}
