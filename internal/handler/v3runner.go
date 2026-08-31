package handler

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/publicerror"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
)

func hashDeviceKey(publicKey string) string {
	digest := sha256.Sum256([]byte(publicKey))
	return "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func marshalCapabilities(capabilities []string) json.RawMessage {
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		// []string cannot fail to marshal; keep the function total.
		return json.RawMessage("[]")
	}
	return encoded
}

// Control Plane side of the frozen runner.yaml (M1-RUN-001): enrollment
// with one-time project-bound codes, device-token authentication, and the
// admin approve/revoke lifecycle. Claim/heartbeat/execution endpoints
// land with the PostgreSQL work-item cutover — mounting them against the
// SQLite baseline would fake the protocol.

const (
	// enrollmentCodeTTL is the frozen ten-minute window.
	enrollmentCodeTTL = 10 * time.Minute

	deviceTokenHeader = "Authorization"
)

// RunnerV3Options carries the v3 Runner API wiring. Admin authenticates
// through the same OIDC middleware as every other human surface.
type RunnerV3Options struct {
	Registry *store.PostgresStore
	Tokens   *identity.DeviceTokenMinter
	Policy   *identity.Policy
	Admin    *OIDCMiddleware
	Now      func() time.Time
}

// RegisterRunnerV3 mounts the v3 Runner lifecycle endpoints. Enrollment
// is the only pre-credential route; everything else requires a verified
// device token whose runner is not revoked.
func RegisterRunnerV3(r *gin.Engine, options RunnerV3Options) {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	handlers := &runnerV3{registry: options.Registry, tokens: options.Tokens, policy: options.Policy, admin: options.Admin, now: now}

	v3 := r.Group("/api/v3")
	v3.POST("/runners/enroll", handlers.enroll)
	v3.POST("/runners/:id/approve", handlers.requireAdmin, handlers.approve)
	v3.POST("/runners/:id/revoke", handlers.requireAdmin, handlers.revoke)
	v3.POST("/runner-leases/claim", handlers.requireDevice, handlers.claim)
	v3.POST("/runner-leases/:id/heartbeat", handlers.requireDevice, handlers.heartbeat)
	v3.POST("/executions/:id/complete", handlers.requireDevice, handlers.complete)
	v3.GET("/runners/me", handlers.requireDevice, handlers.me)
}

type runnerV3 struct {
	registry *store.PostgresStore
	tokens   *identity.DeviceTokenMinter
	policy   *identity.Policy
	admin    *OIDCMiddleware
	now      func() time.Time
}

type enrollRequestBody struct {
	EnrollmentCode  string   `json:"enrollment_code"`
	DevicePublicKey string   `json:"device_public_key"`
	DisplayName     string   `json:"display_name"`
	Capabilities    []string `json:"capabilities"`
}

// enroll exchanges a one-time, project-bound code for a pending Runner
// identity and its first device token (runner.yaml: 201 + credential).
func (h *runnerV3) enroll(c *gin.Context) {
	var body enrollRequestBody
	if err := c.ShouldBindJSON(&body); err != nil || body.EnrollmentCode == "" ||
		body.DevicePublicKey == "" || body.DisplayName == "" || len(body.Capabilities) < 3 {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Enrollment body does not match the runner contract")
		return
	}

	ctx := c.Request.Context()
	enrollment, projectID, err := h.registry.RunnerRegistry().EnrollmentByCodeHash(ctx, identity.HashEnrollmentCode(body.EnrollmentCode))
	if err != nil {
		staticErrorReply(c, http.StatusBadRequest, "ENROLLMENT_CODE_INVALID", "Enrollment code is unknown")
		return
	}
	if err := h.registry.RunnerRegistry().ConsumeEnrollment(ctx, enrollment.ID, identity.HashEnrollmentCode(body.EnrollmentCode)); err != nil {
		switch err {
		case store.ErrEnrollmentExpired:
			staticErrorReply(c, http.StatusGone, "ENROLLMENT_CODE_EXPIRED", "Enrollment code has expired")
		case store.ErrEnrollmentConsumed:
			staticErrorReply(c, http.StatusConflict, "ENROLLMENT_CODE_REUSED", "Enrollment code was already used")
		default:
			staticErrorReply(c, http.StatusBadRequest, "ENROLLMENT_CODE_INVALID", "Enrollment code was rejected")
		}
		return
	}

	device := &model.RunnerDevice{
		DisplayName:   body.DisplayName,
		DeviceKeyHash: hashDeviceKey(body.DevicePublicKey),
		Status:        model.RunnerStatusPendingApproval,
		Capabilities:  marshalCapabilities(body.Capabilities),
	}
	if err := h.registry.RunnerRegistry().CreateRunner(ctx, device, &model.RunnerBinding{ProjectID: projectID}); err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Enrollment could not be recorded")
		return
	}
	token, expiresAt, err := h.tokens.Mint(device.ID)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Device credential could not be minted")
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"runner_id":    device.ID,
		"state":        device.Status,
		"access_token": token,
		"expires_at":   expiresAt.UTC().Format(time.RFC3339),
	})
}

// me answers a device-authenticated runner with its own registry state.
func (h *runnerV3) me(c *gin.Context) {
	device := c.MustGet(runnerContextKey).(*model.RunnerDevice)
	c.JSON(http.StatusOK, gin.H{
		"runner_id":         device.ID,
		"state":             device.Status,
		"generation":        device.Generation,
		"capabilities":      device.Capabilities,
		"last_heartbeat_at": device.LastHeartbeatAt,
	})
}

func (h *runnerV3) approve(c *gin.Context) {
	if err := h.registry.RunnerRegistry().UpdateRunnerStatus(c.Request.Context(),
		c.Param("id"), model.RunnerStatusPendingApproval, model.RunnerStatusApproved); err != nil {
		runnerStatusReply(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"runner_id": c.Param("id"), "state": model.RunnerStatusApproved})
}

func (h *runnerV3) revoke(c *gin.Context) {
	if err := h.registry.RunnerRegistry().RevokeRunner(c.Request.Context(), c.Param("id")); err != nil {
		runnerStatusReply(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"runner_id": c.Param("id"), "state": model.RunnerStatusRevoked})
}

// claim dispatches at most one queued work item to the authenticated
// runner (runner.yaml: 200 lease or explicit no-work).
func (h *runnerV3) claim(c *gin.Context) {
	var body struct {
		ProtocolVersion      string   `json:"protocol_version"`
		ConnectionGeneration string   `json:"connection_generation"`
		Capabilities         []string `json:"capabilities"`
		WaitSeconds          int      `json:"wait_seconds"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.ProtocolVersion == "" ||
		body.ConnectionGeneration == "" || body.WaitSeconds < 1 || body.WaitSeconds > 30 {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Claim body does not match the runner contract")
		return
	}
	device := c.MustGet(runnerContextKey).(*model.RunnerDevice)

	// The presented queue token rides the Idempotency-Key suffix; the
	// contract requires the key, and the daemon derives it per claim.
	idempotencyKey := c.GetHeader("Idempotency-Key")
	queueToken, parseErr := queueTokenFromIdempotencyKey(idempotencyKey)
	if parseErr != nil {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A valid Idempotency-Key is required")
		return
	}

	claim, err := h.registry.ClaimNextWorkItem(c.Request.Context(),
		device.ID, body.ConnectionGeneration, queueToken, leaseTTLFromConfig())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNoAvailableTask):
			c.JSON(http.StatusOK, gin.H{"available": false, "retry_after_seconds": body.WaitSeconds})
		case errors.Is(err, store.ErrConcurrentConflict):
			staticErrorReply(c, http.StatusConflict, "CONCURRENCY_CONFLICT", "Queue token is stale; re-observe and retry")
		case errors.Is(err, store.ErrRunnerNotFound), errors.Is(err, store.ErrRunnerNotBound):
			staticErrorReply(c, http.StatusForbidden, "FORBIDDEN", "Runner is not bound to any project")
		case errors.Is(err, store.ErrRunnerStatusInvalid):
			staticErrorReply(c, http.StatusForbidden, "FORBIDDEN", "Runner is not eligible to claim")
		default:
			slog.Error("v3 runner claim failed", "error", err)
			staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Lease dispatch failed")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id": claim.LeaseID, "version": claim.LeaseVersion, "epoch": claim.LeaseEpoch,
		"execution_id": claim.ExecutionID, "work_item_id": claim.WorkItemID,
		"project_id":    claim.ProjectID,
		"queue_version": claim.QueueVersion,
		"expires_at":    time.Now().UTC().Add(leaseTTLFromConfig()).Format(time.RFC3339),
	})
}

// heartbeat renews the runner's active lease (200 on success).
func (h *runnerV3) heartbeat(c *gin.Context) {
	var body struct {
		LeaseVersion         int64     `json:"lease_version"`
		ConnectionGeneration string    `json:"connection_generation"`
		ObservedAt           time.Time `json:"observed_at"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.LeaseVersion < 1 || body.ConnectionGeneration == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Heartbeat body does not match the runner contract")
		return
	}
	device := c.MustGet(runnerContextKey).(*model.RunnerDevice)
	newVersion, err := h.registry.RunnerLeaseHeartbeat(c.Request.Context(),
		c.Param("id"), device.ID, body.ConnectionGeneration, body.LeaseVersion, leaseTTLFromConfig())
	if err != nil {
		leaseErrorReply(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"lease_id": c.Param("id"), "lease_version": newVersion})
}

// complete records the terminal execution outcome (202 accepted).
func (h *runnerV3) complete(c *gin.Context) {
	var body struct {
		LeaseID              string  `json:"lease_id"`
		LeaseVersion         int64   `json:"lease_version"`
		ConnectionGeneration string  `json:"connection_generation"`
		WorkspaceGeneration  int64   `json:"workspace_generation"`
		Outcome              string  `json:"outcome"`
		CommitSHA            *string `json:"commit_sha"`
		Summary              string  `json:"summary"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Outcome == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Completion body does not match the runner contract")
		return
	}
	if body.Outcome == "completed" && (body.CommitSHA == nil || *body.CommitSHA == "") {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A completed execution must carry its commit_sha")
		return
	}
	device := c.MustGet(runnerContextKey).(*model.RunnerDevice)
	if err := h.registry.CompleteExecution(c.Request.Context(),
		c.Param("id"), device.ID, body.ConnectionGeneration, body.Outcome, body.CommitSHA, body.Summary); err != nil {
		switch err {
		case store.ErrRunnerGenerationStale, store.ErrLeaseVersionMismatch:
			staticErrorReply(c, http.StatusConflict, "LEASE_VERSION_MISMATCH", "The lease was fenced by a newer generation")
		case store.ErrLeaseNotFound:
			staticErrorReply(c, http.StatusGone, "LEASE_EXPIRED", "The lease no longer exists")
		default:
			slog.Error("v3 runner complete failed", "error", err)
			staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Completion failed")
		}
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"execution_id": c.Param("id"), "outcome": body.Outcome})
}

// queueTokenFromIdempotencyKey extracts the trailing numeric queue token
// from a daemon-shaped key (daemon-<gen>-claim-<seq>-q<token>).
func queueTokenFromIdempotencyKey(key string) (int64, error) {
	if key == "" {
		return 0, fmt.Errorf("empty idempotency key")
	}
	index := strings.LastIndex(key, "-q")
	if index < 0 {
		return 0, fmt.Errorf("no queue token")
	}
	return strconv.ParseInt(key[index+2:], 10, 64)
}

func leaseTTLFromConfig() time.Duration { return 90 * time.Second }

func leaseErrorReply(c *gin.Context, err error) {
	switch err {
	case store.ErrLeaseNotFound:
		staticErrorReply(c, http.StatusGone, "LEASE_EXPIRED", "The lease no longer exists")
	case store.ErrLeaseVersionMismatch:
		staticErrorReply(c, http.StatusConflict, "LEASE_VERSION_MISMATCH", "The lease version is stale")
	default:
		slog.Error("v3 runner heartbeat failed", "error", err)
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Heartbeat failed")
	}
}

const runnerContextKey = "maestro.runner"

// requireDevice verifies the bearer device token and loads a live
// (non-revoked) registry row; revoked devices are 410 Gone per contract.
func (h *runnerV3) requireDevice(c *gin.Context) {
	header := c.GetHeader(deviceTokenHeader)
	if len(header) < 8 || header[:7] != "Bearer " {
		c.Header("WWW-Authenticate", `Bearer realm="maestro-runner"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Device authentication is required")
		return
	}
	runnerID, err := h.tokens.Verify(header[7:])
	if err != nil {
		c.Header("WWW-Authenticate", `Bearer realm="maestro-runner", error="invalid_token"`)
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "Device token is invalid or expired")
		return
	}
	device, err := h.registry.RunnerRegistry().GetRunner(c.Request.Context(), runnerID)
	if err != nil {
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "Device identity is no longer enrolled")
		return
	}
	if device.Status == model.RunnerStatusRevoked {
		c.Abort()
		staticErrorReply(c, http.StatusGone, "RUNNER_REVOKED", "Device has been revoked")
		return
	}
	c.Set(runnerContextKey, device)
	c.Next()
}

// requireAdmin demands an authenticated principal holding runner
// approval authority in the runner's bound project (project_admin per
// the frozen matrix). The decision runs through the SAME policy as every
// other surface.
func (h *runnerV3) requireAdmin(c *gin.Context) {
	if h.admin != nil && !h.admin.authenticateBearer(c) {
		return
	}
	principal := PrincipalFromContext(c)
	if principal == nil {
		c.Abort()
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}
	projectID, err := h.registry.RunnerRegistry().ProjectOfRunner(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.Abort()
		staticErrorReply(c, http.StatusNotFound, "ROUTE_NOT_FOUND", "Runner not found")
		return
	}
	if decision := h.policy.Authorize(c.Request.Context(), principal, "runner.approve",
		model.Resource{Type: "runner", ProjectID: projectID}); !decision.Allow {
		c.Abort()
		// No membership hides the runner (404); explicit denial is 403.
		if strings.HasPrefix(decision.Reasons[0], "no membership") {
			public := publicerror.Classify(nil)
			c.JSON(http.StatusNotFound, gin.H{"error": "ROUTE_NOT_FOUND", "error_code": "ROUTE_NOT_FOUND", "correlation_id": public.CorrelationID})
			return
		}
		staticErrorReply(c, http.StatusForbidden, "FORBIDDEN", "Action is not permitted for this principal")
		return
	}
	c.Next()
}

func runnerStatusReply(c *gin.Context, err error) {
	switch err {
	case store.ErrRunnerNotFound:
		staticErrorReply(c, http.StatusNotFound, "ROUTE_NOT_FOUND", "Runner not found")
	case store.ErrRunnerStatusInvalid, store.ErrRunnerRevoked:
		staticErrorReply(c, http.StatusConflict, "RUNNER_STATE_INVALID", "Runner state transition rejected")
	default:
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Runner operation failed")
	}
}
