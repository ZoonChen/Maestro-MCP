package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OIDC access-token verification (ADR-003 / SEC-IDENTITY-RBAC section 7):
// signature algorithm allowlist (asymmetric only), exact issuer, audience
// membership, exp/nbf with at most 60 seconds of clock skew, JWKS fetched
// via OIDC discovery and refreshed on unknown kid (rotation). Every
// failure is a deny; there is no token passthrough and no fallback trust.

const (
	// maxClockSkew matches SEC-IDENTITY-RBAC section 7.
	maxClockSkew = 60 * time.Second
	// jwksCacheTTL bounds key trust; an unknown kid forces one refresh.
	jwksCacheTTL = 5 * time.Minute
)

// allowedSignatureAlgorithms is the closed algorithm set. Symmetric and
// "none" algorithms are structurally absent: the classic alg-confusion
// family cannot be selected, not merely rejected at runtime.
var allowedSignatureAlgorithms = map[string]bool{
	"RS256": true,
	"ES256": true,
}

// VerifiedClaims are the validated identity facts extracted from a token.
// Only these fields feed PrincipalContext construction.
type VerifiedClaims struct {
	Issuer   string
	Subject  string
	Audience []string
	Expiry   time.Time
	JTI      string
}

// TokenVerifier verifies OIDC JWT access tokens against one issuer.
type TokenVerifier struct {
	issuer       string
	audience     string
	httpClient   *http.Client
	discoveryURL string

	mu        sync.Mutex
	jwksURL   string
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time
}

// NewTokenVerifier builds a verifier for the configured issuer/audience.
func NewTokenVerifier(issuer, audience string, client *http.Client) (*TokenVerifier, error) {
	if !strings.HasPrefix(issuer, "https://") {
		// The local compose IdP may run plain HTTP; production is HTTPS-only
		// per the machine schema. Explicitly narrow the exception.
		if !strings.HasPrefix(issuer, "http://") {
			return nil, fmt.Errorf("identity: issuer must be an HTTP(S) URL: %q", issuer)
		}
	}
	if audience == "" {
		return nil, fmt.Errorf("identity: audience must not be empty")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TokenVerifier{
		issuer:       issuer,
		audience:     audience,
		httpClient:   client,
		discoveryURL: strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration",
	}, nil
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
}

type jwtClaims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  []string `json:"aud"`
	Expiry    int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
	IssuedAt  int64    `json:"iat"`
	JTI       string   `json:"jti"`
}

// Verify validates one access token end to end and returns its claims.
func (v *TokenVerifier) Verify(token string, now time.Time) (VerifiedClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return VerifiedClaims{}, fmt.Errorf("identity: malformed token")
	}
	var header jwtHeader
	if err := decodeSegment(parts[0], &header); err != nil {
		return VerifiedClaims{}, fmt.Errorf("identity: token header: %w", err)
	}
	if !allowedSignatureAlgorithms[header.Algorithm] {
		return VerifiedClaims{}, fmt.Errorf("identity: signature algorithm %q is not allowed", header.Algorithm)
	}

	signed := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return VerifiedClaims{}, fmt.Errorf("identity: token signature encoding: %w", err)
	}
	digest := sha256.Sum256([]byte(signed))

	key, err := v.key(header.KeyID, now)
	if err != nil {
		return VerifiedClaims{}, err
	}
	switch parsed := key.(type) {
	case *rsa.PublicKey:
		if header.Algorithm != "RS256" {
			return VerifiedClaims{}, fmt.Errorf("identity: key/algorithm mismatch")
		}
		if err := rsa.VerifyPKCS1v15(parsed, crypto.SHA256, digest[:], signature); err != nil {
			return VerifiedClaims{}, fmt.Errorf("identity: signature verification failed")
		}
	case *ecdsa.PublicKey:
		if header.Algorithm != "ES256" {
			return VerifiedClaims{}, fmt.Errorf("identity: key/algorithm mismatch")
		}
		if !ecdsa.VerifyASN1(parsed, digest[:], signature) {
			return VerifiedClaims{}, fmt.Errorf("identity: signature verification failed")
		}
	default:
		return VerifiedClaims{}, fmt.Errorf("identity: unsupported key type")
	}

	var claims jwtClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return VerifiedClaims{}, fmt.Errorf("identity: token claims: %w", err)
	}
	if claims.Issuer != v.issuer {
		return VerifiedClaims{}, fmt.Errorf("identity: issuer mismatch")
	}
	if !containsString(claims.Audience, v.audience) {
		return VerifiedClaims{}, fmt.Errorf("identity: audience mismatch")
	}
	if now.After(time.Unix(claims.Expiry, 0).Add(maxClockSkew)) {
		return VerifiedClaims{}, fmt.Errorf("identity: token expired")
	}
	if claims.NotBefore != 0 && now.Add(maxClockSkew).Before(time.Unix(claims.NotBefore, 0)) {
		return VerifiedClaims{}, fmt.Errorf("identity: token not yet valid")
	}
	if claims.Subject == "" {
		return VerifiedClaims{}, fmt.Errorf("identity: token has no subject")
	}
	return VerifiedClaims{
		Issuer:   claims.Issuer,
		Subject:  claims.Subject,
		Audience: claims.Audience,
		Expiry:   time.Unix(claims.Expiry, 0),
		JTI:      claims.JTI,
	}, nil
}

func (v *TokenVerifier) key(kid string, now time.Time) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[kid]; ok && now.Sub(v.fetchedAt) < jwksCacheTTL {
		return key, nil
	}
	// Refresh once (unknown kid or stale cache) before failing.
	if err := v.refreshKeys(); err != nil {
		if key, ok := v.keys[kid]; ok {
			return key, nil
		}
		return nil, fmt.Errorf("identity: jwks: %w", err)
	}
	key, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("identity: no jwks key for kid %q", kid)
	}
	return key, nil
}

func (v *TokenVerifier) refreshKeys() error {
	if v.jwksURL == "" {
		document, err := v.fetchJSON(v.discoveryURL)
		if err != nil {
			return fmt.Errorf("discovery: %w", err)
		}
		var discovered struct {
			JWKSURL string `json:"jwks_uri"`
			Issuer  string `json:"issuer"`
		}
		if err := json.Unmarshal(document, &discovered); err != nil {
			return fmt.Errorf("discovery document: %w", err)
		}
		if discovered.Issuer != v.issuer {
			return fmt.Errorf("discovery issuer %q does not match configured %q", discovered.Issuer, v.issuer)
		}
		if discovered.JWKSURL == "" {
			return fmt.Errorf("discovery document has no jwks_uri")
		}
		v.jwksURL = discovered.JWKSURL
	}
	document, err := v.fetchJSON(v.jwksURL)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	var keySet struct {
		Keys []jwksKey `json:"keys"`
	}
	if err := json.Unmarshal(document, &keySet); err != nil {
		return fmt.Errorf("jwks document: %w", err)
	}
	keys := make(map[string]crypto.PublicKey, len(keySet.Keys))
	for _, entry := range keySet.Keys {
		key, keyErr := entry.publicKey()
		if keyErr != nil {
			continue // unknown key types are skipped, never trusted
		}
		if entry.KeyID == "" {
			continue
		}
		keys[entry.KeyID] = key
	}
	if len(keys) == 0 {
		return fmt.Errorf("jwks document carries no usable keys")
	}
	v.keys = keys
	v.fetchedAt = time.Now()
	return nil
}

func (v *TokenVerifier) fetchJSON(url string) ([]byte, error) {
	response, err := v.httpClient.Get(url)
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

type jwksKey struct {
	KeyID       string `json:"kid"`
	KeyType     string `json:"kty"`
	Usage       string `json:"use"`
	Algorithm   string `json:"alg"`
	RSAModulus  string `json:"n"`
	RSAExponent string `json:"e"`
	ECCurve     string `json:"crv"`
	ECX         string `json:"x"`
	ECY         string `json:"y"`
}

func (k jwksKey) publicKey() (crypto.PublicKey, error) {
	switch k.KeyType {
	case "RSA":
		modulus, err := base64URLBigInt(k.RSAModulus)
		if err != nil {
			return nil, err
		}
		exponent, err := base64URLInt(k.RSAExponent)
		if err != nil {
			return nil, err
		}
		return &rsa.PublicKey{N: modulus, E: exponent}, nil
	case "EC":
		if k.ECCurve != "P-256" {
			return nil, fmt.Errorf("unsupported curve %q", k.ECCurve)
		}
		x, err := base64URLBigInt(k.ECX)
		if err != nil {
			return nil, err
		}
		y, err := base64URLBigInt(k.ECY)
		if err != nil {
			return nil, err
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.KeyType)
	}
}

func base64URLBigInt(encoded string) (*big.Int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(raw), nil
}

func base64URLInt(encoded string) (int, error) {
	value, err := base64URLBigInt(encoded)
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

func decodeSegment(segment string, target any) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
