package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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

// ES256 (ECDSA P-256) verification and JWKS rotation coverage: the
// algorithm allowlist admits both frozen families; key rotation by
// unknown-kid refresh is exercised against a live mock issuer.

type es256IdP struct {
	server *httptest.Server
	key    *ecdsa.PrivateKey
	kid    string
	issuer string
}

func newES256IdP(t *testing.T) *es256IdP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	idp := &es256IdP{key: key, kid: "es-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, idp.issuer, idp.issuer+"/certs")
	})
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Read idp.key at request time so rotation tests can swap it.
		x, y := ecdhCoordinates(idp, t)
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"EC","crv":"P-256","use":"sig","kid":%q,"x":%q,"y":%q}]}`,
			idp.kid, x, y)
	})
	idp.server = httptest.NewServer(mux)
	idp.issuer = idp.server.URL
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *es256IdP) esToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "ES256", "kid": idp.kid}
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
	signature, err := ecdsa.SignASN1(rand.Reader, idp.key, digest[:])
	require.NoError(t, err)
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestTokenVerifierAcceptsES256(t *testing.T) {
	idp := newES256IdP(t)
	verifier, err := NewTokenVerifier(idp.issuer, "maestro", idp.server.Client())
	require.NoError(t, err)
	now := time.Now().UTC()
	claims := map[string]any{
		"iss": idp.issuer, "sub": "ec-user",
		"aud": []string{"maestro"},
		"exp": now.Add(900 * time.Second).Unix(), "nbf": now.Unix() - 10,
	}
	verified, err := verifier.Verify(idp.esToken(t, claims), now)
	require.NoError(t, err)
	assert.Equal(t, "ec-user", verified.Subject)

	// A foreign ES256 key never verifies.
	other := newES256IdP(t)
	foreign := other.esToken(t, claims)
	_, err = verifier.Verify(foreign, now)
	require.Error(t, err)
}

func TestTokenVerifierJWKSRotationByUnknownKid(t *testing.T) {
	idp := newES256IdP(t)
	verifier, err := NewTokenVerifier(idp.issuer, "maestro", idp.server.Client())
	require.NoError(t, err)
	now := time.Now().UTC()
	claims := map[string]any{
		"iss": idp.issuer, "sub": "rotate-user",
		"aud": []string{"maestro"},
		"exp": now.Add(900 * time.Second).Unix(),
	}

	// Prime the cache with the current key.
	_, err = verifier.Verify(idp.esToken(t, claims), now)
	require.NoError(t, err)

	// Rotate the issuer key under a NEW kid: the next verify hits an
	// unknown kid, refreshes the JWKS, and succeeds.
	rotated, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	idp.key = rotated
	idp.kid = "es-key-2"
	_, err = verifier.Verify(idp.esToken(t, claims), now)
	require.NoError(t, err, "an unknown kid must trigger a refresh and then verify")
}

func TestTokenVerifierRejectsMalformedWireShapes(t *testing.T) {
	idp := newES256IdP(t)
	verifier, err := NewTokenVerifier(idp.issuer, "maestro", idp.server.Client())
	require.NoError(t, err)
	now := time.Now().UTC()

	notJSON := base64.RawURLEncoding.EncodeToString([]byte("notjson"))
	for name, token := range map[string]string{
		"bad payload base64":   "eyJhbGciOiJFUzI1NiJ9.!!!.aaa",
		"bad signature base64": notJSON + "." + notJSON + ".!!!",
		"segments not json":    notJSON + "." + notJSON + ".AAA",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verifier.Verify(token, now)
			require.Error(t, err)
		})
	}
}

// ecdhCoordinates encodes the public point via the non-deprecated Bytes
// API and splits it into base64url x/y for the JWKS document.
func ecdhCoordinates(idp *es256IdP, t *testing.T) (string, string) {
	t.Helper()
	point, pointErr := idp.key.PublicKey.Bytes()
	if pointErr != nil {
		t.Fatalf("encode public point: %v", pointErr)
	}
	if len(point) != 65 || point[0] != 0x04 {
		t.Fatalf("unexpected public point length %d", len(point))
	}
	x := base64.RawURLEncoding.EncodeToString(point[1:33])
	y := base64.RawURLEncoding.EncodeToString(point[33:65])
	return x, y
}
