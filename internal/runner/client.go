package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Client is the outbound-only Control Plane client. Every request carries
// the device bearer token; failures return typed errors so the daemon can
// distinguish retryable transport trouble from terminal refusals
// (revoked, expired, stale generation) without guessing.
type Client struct {
	baseURL     string
	accessToken string
	httpClient  *http.Client
}

// NewClient binds the client to a Control Plane base URL. The token can
// be set per credential with SetToken after enrollment.
func NewClient(baseURL string, httpClient *http.Client) (*Client, error) {
	if !strings.HasPrefix(baseURL, "https://") && !strings.HasPrefix(baseURL, "http://") {
		return nil, fmt.Errorf("runner: control plane URL must be HTTP(S): %q", baseURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 45 * time.Second}
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), httpClient: httpClient}, nil
}

// SetToken installs the device bearer token.
func (c *Client) SetToken(token string) { c.accessToken = token }

// ProtocolError is a non-2xx reply from the Control Plane.
type ProtocolError struct {
	StatusCode int
	Code       string
	Message    string
	Retryable  bool
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("runner: control plane reply %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// Terminal reports whether the error can never succeed on retry
// (revocation, expiry, fencing, bad request).
func (e *ProtocolError) Terminal() bool {
	switch e.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusConflict, http.StatusGone:
		return true
	}
	return false
}

// Enroll exchanges the one-time code for a pending credential.
func (c *Client) Enroll(ctx context.Context, request EnrollmentRequest, idempotencyKey string) (*Credential, error) {
	var credential Credential
	if err := c.post(ctx, "/api/v3/runners/enroll", "", idempotencyKey, request, http.StatusCreated, &credential); err != nil {
		return nil, err
	}
	return &credential, nil
}

// ClaimLease long-polls for the next lease. A 200 with available=false is
// the explicit no-work reply, not an error.
func (c *Client) ClaimLease(ctx context.Context, request ClaimRequest, idempotencyKey string) (*Lease, *NoWork, error) {
	body, err := c.raw(ctx, "/api/v3/runner-leases/claim", idempotencyKey, request)
	if err != nil {
		return nil, nil, err
	}
	var probe struct {
		Available *bool `json:"available"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, nil, fmt.Errorf("runner: malformed claim reply: %w", err)
	}
	if probe.Available != nil && !*probe.Available {
		var noWork NoWork
		if err := json.Unmarshal(body, &noWork); err != nil {
			return nil, nil, fmt.Errorf("runner: malformed no-work reply: %w", err)
		}
		return nil, &noWork, nil
	}
	var lease Lease
	if err := json.Unmarshal(body, &lease); err != nil {
		return nil, nil, fmt.Errorf("runner: malformed lease reply: %w", err)
	}
	return &lease, nil, nil
}

// HeartbeatLease renews the lease deadline.
func (c *Client) HeartbeatLease(ctx context.Context, leaseID string, request HeartbeatRequest, idempotencyKey string) error {
	return c.post(ctx, "/api/v3/runner-leases/"+leaseID+"/heartbeat", "", idempotencyKey, request, http.StatusOK, nil)
}

// UploadDiagnosticEvidence records bounded local evidence (202 accepted).
func (c *Client) UploadDiagnosticEvidence(ctx context.Context, executionID string, evidence DiagnosticEvidence, idempotencyKey string) error {
	return c.post(ctx, "/api/v3/executions/"+executionID+"/evidence", "", idempotencyKey, evidence, http.StatusAccepted, nil)
}

// CompleteExecution reports the terminal outcome (202 accepted).
func (c *Client) CompleteExecution(ctx context.Context, executionID string, completion ExecutionCompletion, idempotencyKey string) error {
	return c.post(ctx, "/api/v3/executions/"+executionID+"/complete", "", idempotencyKey, completion, http.StatusAccepted, nil)
}

func (c *Client) post(ctx context.Context, path, _ string, idempotencyKey string, payload any, _ int, out any) error {
	body, err := c.raw(ctx, path, idempotencyKey, payload)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("runner: malformed reply from %s: %w", path, err)
	}
	return nil
}

func (c *Client) raw(ctx context.Context, path, idempotencyKey string, payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("runner: encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if len(idempotencyKey) >= 16 && len(idempotencyKey) <= 128 {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if c.accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("runner: transport: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("runner: read reply: %w", err)
	}
	if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusOK && len(body) == 0 && payload == nil {
		return body, nil
	}
	if response.StatusCode >= 400 {
		var envelope struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		}
		_ = json.Unmarshal(body, &envelope)
		if envelope.Code == "" {
			envelope.Code = fmt.Sprintf("HTTP_%d", response.StatusCode)
		}
		return nil, &ProtocolError{
			StatusCode: response.StatusCode,
			Code:       envelope.Code,
			Message:    envelope.Message,
			Retryable:  envelope.Retryable,
		}
	}
	return body, nil
}

// Backoff computes bounded exponential backoff with jitter for retryable
// transport failures (ADR-001 section 5). The first retry waits at most
// the base; every doubling is capped.
func Backoff(attempt int, base, cap time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if cap <= 0 {
		cap = 30 * time.Second
	}
	wait := base
	for i := 1; i < attempt && wait < cap; i++ {
		wait *= 2
	}
	if wait > cap {
		wait = cap
	}
	// Jitter draws from the crypto source: backoff timing must not be
	// predictable enough to synchronize retries.
	jitterRange := big.NewInt(int64(wait)/4 + 1)
	jitterDraw, jitterErr := rand.Int(rand.Reader, jitterRange)
	if jitterErr != nil {
		return wait
	}
	return wait + time.Duration(jitterDraw.Int64())
}
