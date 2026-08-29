package identity

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The verifier is tested against a local mock IdP that serves real OIDC
// discovery and JWKS documents and signs genuine RS256 tokens — no
// shortcut fixtures. Every negative path must fail closed.

type mockIdP struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	audience string
	now      time.Time
}

func newMockIdP(t *testing.T) *mockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	idp := &mockIdP{key: key, kid: "test-key-1", audience: "maestro", now: time.Now().UTC()}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`,
			idp.issuer, idp.issuer+"/protocol/openid-connect/certs")
	})
	mux.HandleFunc("/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		modulus := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":%q,"n":%q,"e":"AQAB"}]}`,
			idp.kid, modulus)
	})
	idp.server = httptest.NewServer(mux)
	idp.issuer = idp.server.URL
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *mockIdP) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "kid": idp.kid}
	if override, ok := claims["_header"]; ok {
		for key, value := range override.(map[string]any) {
			header[key] = value
		}
		delete(claims, "_header")
	}
	encode := func(value any) string {
		payload, err := json.Marshal(value)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(payload)
	}
	signed := encode(header) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, digest[:])
	require.NoError(t, err)
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (idp *mockIdP) validClaims() map[string]any {
	now := idp.now.Unix()
	return map[string]any{
		"iss": idp.issuer, "sub": "user-1",
		"aud": []string{idp.audience},
		"exp": now + 900, "nbf": now - 10, "iat": now, "jti": "jti-1",
	}
}

func (idp *mockIdP) verifier(t *testing.T) *TokenVerifier {
	t.Helper()
	verifier, err := NewTokenVerifier(idp.issuer, idp.audience, idp.server.Client())
	require.NoError(t, err)
	return verifier
}

func TestTokenVerifierAcceptsValidRS256(t *testing.T) {
	idp := newMockIdP(t)
	verifier := idp.verifier(t)

	claims, err := verifier.Verify(idp.token(t, idp.validClaims()), idp.now)
	require.NoError(t, err)
	assert.Equal(t, "user-1", claims.Subject)
	assert.Equal(t, idp.issuer, claims.Issuer)
	assert.Equal(t, "jti-1", claims.JTI)
}

func TestTokenVerifierRejectsEveryForgedToken(t *testing.T) {
	idp := newMockIdP(t)
	verifier := idp.verifier(t)

	cases := map[string]func(map[string]any){
		"wrong issuer":   func(c map[string]any) { c["iss"] = "https://evil.example" },
		"wrong audience": func(c map[string]any) { c["aud"] = []string{"other-api"} },
		"expired":        func(c map[string]any) { c["exp"] = idp.now.Unix() - 3600 },
		"future nbf":     func(c map[string]any) { c["nbf"] = idp.now.Unix() + 3600 },
		"no subject":     func(c map[string]any) { c["sub"] = "" },
		"alg none":       func(c map[string]any) { c["_header"] = map[string]any{"alg": "none"} },
		"alg hs256":      func(c map[string]any) { c["_header"] = map[string]any{"alg": "HS256"} },
		"unknown kid":    func(c map[string]any) { c["_header"] = map[string]any{"kid": "not-a-kid"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := idp.validClaims()
			mutate(claims)
			_, err := verifier.Verify(idp.token(t, claims), idp.now)
			require.Error(t, err, "forged token %q must be rejected", name)
		})
	}

	// A token signed by a DIFFERENT key entirely.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	foreger := &mockIdP{key: other, kid: "test-key-1", issuer: idp.issuer, audience: idp.audience, now: idp.now, server: idp.server}
	_, err = verifier.Verify(foreger.token(t, foreger.validClaims()), idp.now)
	require.Error(t, err, "a foreign signature must never verify")

	// Garbage shapes.
	for _, garbage := range []string{"", "not-a-jwt", "a.b", "a.b.c.d"} {
		_, err = verifier.Verify(garbage, idp.now)
		require.Error(t, err)
	}
}

func TestTokenVerifierClockSkewBound(t *testing.T) {
	idp := newMockIdP(t)
	verifier := idp.verifier(t)

	// 30 seconds past expiry is inside the 60s skew window.
	claims := idp.validClaims()
	claims["exp"] = idp.now.Unix() - 30
	_, err := verifier.Verify(idp.token(t, claims), idp.now)
	assert.NoError(t, err, "bounded skew is allowed")

	// 120 seconds past expiry is beyond it.
	claims["exp"] = idp.now.Unix() - 120
	_, err = verifier.Verify(idp.token(t, claims), idp.now)
	assert.Error(t, err)
}

func TestTokenVerifierDiscoveryDriftFailsClosed(t *testing.T) {
	idp := newMockIdP(t)
	// Point the verifier at an issuer whose discovery document claims a
	// DIFFERENT issuer: identity confusion must fail closed.
	verifier, err := NewTokenVerifier(idp.issuer, idp.audience, idp.server.Client())
	require.NoError(t, err)
	spoofed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"https://someone-else","jwks_uri":"` + idp.issuer + `/protocol/openid-connect/certs"}`))
	}))
	defer spoofed.Close()
	verifier.discoveryURL = spoofed.URL + "/.well-known/openid-configuration"

	_, err = verifier.Verify(idp.token(t, idp.validClaims()), idp.now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}

func TestNewTokenVerifierValidatesConfiguration(t *testing.T) {
	_, err := NewTokenVerifier("notaurl", "maestro", nil)
	assert.Error(t, err)

	_, err = NewTokenVerifier("https://idp.example", "", nil)
	assert.Error(t, err)
}
