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
	return signed + "." + base64.RawURLEncoding.EncodeToString(rawSignES256(t, idp.key, signed))
}

// rawSignES256 produces the RFC 7515 section 3.4 wire form: the
// fixed-width concatenation R||S (32+32 octets), never ASN.1 DER.
func rawSignES256(t *testing.T, key *ecdsa.PrivateKey, signed string) []byte {
	t.Helper()
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	require.NoError(t, err)
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	s.FillBytes(raw[32:])
	return raw
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

// TestTokenVerifierRFC7515A3Vector locks the ES256 wire form against the
// published RFC 7515 Appendix A.3 example (JWS Using ECDSA P-256
// SHA-256). The vector token carries no sub and a foreign issuer, so a
// clean end-to-end Verify is impossible by design; the discriminating
// assertion is the FAILURE STAGE: the RFC raw R||S signature must pass
// the signature check and die later at "issuer mismatch", while a
// one-byte flip of the same signature dies at "signature verification
// failed" — together they pin the fixed-length raw encoding (a DER
// verifier inverts both outcomes).
func TestTokenVerifierRFC7515A3Vector(t *testing.T) {
	const rfcHeader = "eyJhbGciOiJFUzI1NiJ9"
	const rfcPayload = "eyJpc3MiOiJqb2UiLA0KICJleHAiOjEzMDA4MTkzODAsDQogImh0dHA6Ly9leGFtcGxlLmNvbS9pc19yb290Ijp0cnVlfQ"
	const rfcSignature = "DtEhU3ljbEg8L38VWAfUAqOyKAM6-Xx-F4GawxaepmXFCgfTjDxw5djxLa8ISlSApmWQxfKTUJqPP3-Kg6NU1Q"
	// RFC 7515 A.3 JWK public coordinates (x, y).
	const rfcX = "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU"
	const rfcY = "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"

	issuer := "" // bound after the server exists; handlers read it at request time
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, issuer, issuer+"/certs")
	})
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"EC","crv":"P-256","use":"sig","kid":"rfc-a3","x":%q,"y":%q}]}`, rfcX, rfcY)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	issuer = server.URL

	// The vector token itself claims iss="joe" — the discriminating
	// mismatch this test asserts at the claims stage, after the RFC
	// signature has already passed.
	verifier, err := NewTokenVerifier(issuer, "maestro", server.Client())
	require.NoError(t, err)
	now := time.Unix(1300819380-600, 0) // inside the vector token's validity window

	token := rfcHeader + "." + rfcPayload + "." + rfcSignature
	_, err = verifier.Verify(token, now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issuer mismatch",
		"the RFC raw R||S signature must verify; only the issuer may reject this token")

	// Flip the last octet of S: the same token must now die at the
	// signature stage.
	raw, decodeErr := base64.RawURLEncoding.DecodeString(rfcSignature)
	require.NoError(t, decodeErr)
	require.Len(t, raw, 64)
	raw[63] ^= 0x01
	tampered := rfcHeader + "." + rfcPayload + "." + base64.RawURLEncoding.EncodeToString(raw)
	_, err = verifier.Verify(tampered, now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signature verification failed")
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
