package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OIDC authorization-code client for the /auth BFF endpoints (task brief
// E, SEC-IDENTITY-RBAC section 2: Browser = OIDC Authorization Code +
// PKCE, BFF secure cookie). This client owns the server-to-IdP legs
// only: building the authorize redirect and exchanging the code with
// the PKCE verifier. The exchanged access token is verified by the SAME
// frozen TokenVerifier chain as bearer requests — no second trust path.

const (
	// LoginRequestTTL bounds one authorization-code handshake.
	LoginRequestTTL = 10 * time.Minute
	// SessionTTL is the absolute BFF session lifetime. The security
	// authority freezes cookie attributes but not the BFF session
	// duration; 8h is the registered task-brief-E decision (migration
	// README 0015 deviation note).
	SessionTTL = 8 * time.Hour
	// SessionCookieName is the BFF session cookie: HttpOnly + Secure +
	// SameSite=Lax, set by the /auth endpoints.
	SessionCookieName = "maestro_session"
)

// OIDCClient performs discovery once and exchanges authorization codes.
type OIDCClient struct {
	issuer       string
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu                    sync.Mutex
	authorizationEndpoint string
	tokenEndpoint         string
	fetchedAt             time.Time
}

// NewOIDCClient builds the client for one configured IdP application.
func NewOIDCClient(issuer, clientID, clientSecret string, client *http.Client) (*OIDCClient, error) {
	if !strings.HasPrefix(issuer, "https://") && !strings.HasPrefix(issuer, "http://") {
		return nil, fmt.Errorf("identity: oidc issuer must be an HTTP(S) URL: %q", issuer)
	}
	if clientID == "" {
		return nil, fmt.Errorf("identity: oidc client_id must not be empty")
	}
	if clientSecret == "" {
		return nil, fmt.Errorf("identity: oidc client_secret must not be empty")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &OIDCClient{issuer: issuer, clientID: clientID, clientSecret: clientSecret, httpClient: client}, nil
}

type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// endpoints resolves (and caches) the authorization and token endpoints
// from OIDC discovery. The discovery failure mode is fail-closed: no
// endpoints, no login.
func (c *OIDCClient) endpoints() (authorizationEndpoint, tokenEndpoint string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authorizationEndpoint != "" && time.Since(c.fetchedAt) < jwksCacheTTL {
		return c.authorizationEndpoint, c.tokenEndpoint, nil
	}
	document, err := c.fetchJSON(strings.TrimSuffix(c.issuer, "/") + "/.well-known/openid-configuration")
	if err != nil {
		return "", "", fmt.Errorf("identity: oidc discovery: %w", err)
	}
	var discovered oidcDiscovery
	if err := json.Unmarshal(document, &discovered); err != nil {
		return "", "", fmt.Errorf("identity: oidc discovery document: %w", err)
	}
	if discovered.Issuer != c.issuer {
		return "", "", fmt.Errorf("identity: oidc discovery issuer %q does not match configured %q", discovered.Issuer, c.issuer)
	}
	if discovered.AuthorizationEndpoint == "" || discovered.TokenEndpoint == "" {
		return "", "", fmt.Errorf("identity: oidc discovery document lacks authorization/token endpoints")
	}
	if !strings.HasPrefix(discovered.AuthorizationEndpoint, "https://") || !strings.HasPrefix(discovered.TokenEndpoint, "https://") {
		// Plain HTTP endpoints are tolerated only for the local compose
		// IdP (http issuer), matching the verifier's issuer policy.
		if strings.HasPrefix(c.issuer, "https://") {
			return "", "", fmt.Errorf("identity: oidc endpoints must be HTTPS for an HTTPS issuer")
		}
	}
	c.authorizationEndpoint = discovered.AuthorizationEndpoint
	c.tokenEndpoint = discovered.TokenEndpoint
	c.fetchedAt = time.Now()
	return c.authorizationEndpoint, c.tokenEndpoint, nil
}

// AuthorizeURL builds the browser redirect into the IdP authorization
// endpoint with PKCE S256 and the server-held opaque state.
func (c *OIDCClient) AuthorizeURL(callbackURI, stateToken, codeChallenge string) (string, error) {
	authorizationEndpoint, _, err := c.endpoints()
	if err != nil {
		return "", err
	}
	query := url.Values{}
	query.Set("response_type", "code")
	query.Set("client_id", c.clientID)
	query.Set("redirect_uri", callbackURI)
	query.Set("scope", "openid profile")
	query.Set("state", stateToken)
	query.Set("code_challenge", codeChallenge)
	query.Set("code_challenge_method", "S256")
	return authorizationEndpoint + "?" + query.Encode(), nil
}

// ExchangeCode trades the authorization code for the access token
// (RFC 6749 section 4.1.3 with RFC 7636 PKCE). The returned token is
// NOT trusted here: the caller verifies it through the frozen verifier.
func (c *OIDCClient) ExchangeCode(code, callbackURI, codeVerifier string) (string, error) {
	_, tokenEndpoint, err := c.endpoints()
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", callbackURI)
	form.Set("code_verifier", codeVerifier)
	form.Set("client_id", c.clientID)

	request, err := http.NewRequest(http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("identity: token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// RFC 6749 section 2.3.1: the client credentials travel as HTTP
	// Basic, never in the URL.
	request.SetBasicAuth(url.QueryEscape(c.clientID), url.QueryEscape(c.clientSecret))

	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("identity: token endpoint: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("identity: token response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("identity: token endpoint: status %d", response.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("identity: token response body: %w", err)
	}
	if token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		return "", fmt.Errorf("identity: token response carries no bearer access token")
	}
	return token.AccessToken, nil
}

// fetchJSON is the same bounded GET the verifier uses for discovery and
// JWKS documents.
func (c *OIDCClient) fetchJSON(url string) ([]byte, error) {
	response, err := c.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// --- opaque token material (session cookie + login state) ---

// NewOpaqueToken returns a fresh 32-byte random token in base64url and
// the sha256 hash to persist. Only the hash is ever stored
// (SEC-IDENTITY-RBAC section 9).
func NewOpaqueToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("identity: opaque token entropy: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, HashOpaqueToken(token), nil
}

// HashOpaqueToken is the single persistence hash for cookie/state
// tokens: sha256 of the exact string the client presents.
func HashOpaqueToken(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

// NewCodeVerifier returns a PKCE code_verifier (RFC 7636 section 4.1)
// and its S256 code_challenge.
func NewCodeVerifier() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("identity: pkce verifier entropy: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	challenge = base64.RawURLEncoding.EncodeToString(HashOpaqueToken(verifier))
	return verifier, challenge, nil
}
