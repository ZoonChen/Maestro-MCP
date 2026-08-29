package handler

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
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
	v3.POST("/runner-leases/claim", handlers.requireDevice, handlers.claimNotReady)
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

// claimNotReady is the honest fail-closed reply for lease claiming: the
// work-item store still lives on the SQLite baseline; pretending to
// serve leases would break the fencing contract.
func (h *runnerV3) claimNotReady(c *gin.Context) {
	staticErrorReply(c, http.StatusServiceUnavailable, "LEASE_DISPATCH_UNAVAILABLE",
		"Lease dispatch activates with the PostgreSQL work-item cutover")
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
