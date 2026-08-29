package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Device tokens authenticate enrolled Runners to the v3 Runner API
// (runner.yaml securitySchemes: bearer). The token is an HMAC-SHA256
// signed statement of runner identity and expiry; possession proves
// enrollment, revocation is enforced by checking the registry status on
// every request — the token itself is never long-lived trust.

const deviceTokenTTL = 24 * time.Hour

// DeviceTokenMinter mints and verifies device tokens with one server
// secret. The secret comes from trusted server configuration.
type DeviceTokenMinter struct {
	secret []byte
	now    func() time.Time
}

// NewDeviceTokenMinter binds a secret; empty secrets are rejected so a
// misconfigured server cannot mint unverifiable trust.
func NewDeviceTokenMinter(secret string, now func() time.Time) (*DeviceTokenMinter, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("identity: device token secret must be at least 32 bytes")
	}
	if now == nil {
		now = time.Now
	}
	return &DeviceTokenMinter{secret: []byte(secret), now: now}, nil
}

type deviceTokenClaims struct {
	RunnerID string `json:"runner_id"`
	IssuedAt int64  `json:"iat"`
	Expiry   int64  `json:"exp"`
}

// Mint issues a token for one runner.
func (m *DeviceTokenMinter) Mint(runnerID string) (string, time.Time, error) {
	if runnerID == "" {
		return "", time.Time{}, fmt.Errorf("identity: runner id is required")
	}
	now := m.now().UTC()
	claims := deviceTokenClaims{
		RunnerID: runnerID,
		IssuedAt: now.Unix(),
		Expiry:   now.Add(deviceTokenTTL).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + m.signature(encoded), now.Add(deviceTokenTTL), nil
}

// Verify checks signature and expiry, returning the runner id.
func (m *DeviceTokenMinter) Verify(token string) (string, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("identity: malformed device token")
	}
	expected := m.signature(parts[0])
	if !hmac.Equal([]byte(expected), []byte(parts[1])) {
		return "", fmt.Errorf("identity: device token signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("identity: device token payload: %w", err)
	}
	var claims deviceTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("identity: device token claims: %w", err)
	}
	if m.now().UTC().After(time.Unix(claims.Expiry, 0)) {
		return "", fmt.Errorf("identity: device token expired")
	}
	if claims.RunnerID == "" {
		return "", fmt.Errorf("identity: device token has no runner id")
	}
	return claims.RunnerID, nil
}

func (m *DeviceTokenMinter) signature(encodedPayload string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(encodedPayload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// NewEnrollmentCode mints a one-time, high-entropy enrollment code and
// its SHA-256 hash for registry storage; only the hash is persisted.
func NewEnrollmentCode() (code, codeHash string, err error) {
	raw := make([]byte, 24)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("identity: enrollment code entropy: %w", err)
	}
	code = base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(code))
	return code, "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// HashEnrollmentCode derives the storage hash for a presented code.
func HashEnrollmentCode(code string) string {
	digest := sha256.Sum256([]byte(code))
	return "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:])
}
