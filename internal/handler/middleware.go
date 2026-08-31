package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
)

// MaxBodySize returns a Gin middleware that limits the request body to maxBytes.
// Requests exceeding the limit receive a 413 Payload Too Large response.
func MaxBodySize(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}

// CORS returns a fail-closed CORS middleware. Origins must either be exact
// same-origin or appear in the explicit allowlist; wildcard and prefix matches
// are intentionally unsupported.
func CORS(allowedOrigins ...string) gin.HandlerFunc {
	allowlist := buildOriginAllowlist(allowedOrigins)

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			normalized, valid := normalizeOrigin(origin)
			if !valid || !requestOriginAllowed(c.Request, normalized, allowlist) {
				c.Abort()
				staticErrorReply(c, http.StatusForbidden, "ORIGIN_NOT_ALLOWED", "Request origin is not allowed")
				return
			}
			c.Header("Vary", "Origin")
			c.Header("Access-Control-Allow-Origin", normalized)
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key, If-Match")
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Max-Age", "600")
		}
		if c.Request.Method == http.MethodOptions {
			if origin == "" {
				c.Abort()
				staticErrorReply(c, http.StatusBadRequest, "ORIGIN_REQUIRED", "Preflight request is missing Origin")
				return
			}
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func buildOriginAllowlist(allowedOrigins []string) map[string]struct{} {
	allowlist := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		if normalized, ok := normalizeOrigin(origin); ok {
			allowlist[normalized] = struct{}{}
		}
	}
	return allowlist
}

// RemoteWriteGuard is the engine-wide write master gate: every non-read
// request (REST mutations, HTTP MCP write tools, /api/v3 Runner mutations)
// is rejected unless trusted server configuration enables it explicitly.
// Default off; doubling as an emergency write stop. It does not affect
// local stdio MCP calls.
func RemoteWriteGuard(enabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		if enabled {
			c.Next()
			return
		}

		// Streamable HTTP MCP uses POST even for initialization and discovery.
		// Permit only an explicit read-only protocol allowlist; tools/call and
		// unknown methods remain disabled with remote_write=false.
		if strings.HasPrefix(c.Request.URL.Path, "/mcp") && mcpRequestIsReadOnly(c.Request) {
			c.Next()
			return
		}

		if !enabled {
			c.Abort()
			staticErrorReply(c, http.StatusForbidden, "REMOTE_WRITE_DISABLED", "Remote write operations are disabled")
			return
		}
		c.Next()
	}
}

// DrainGuard rejects every new state-changing request after BeginDrain. A nil
// callback is fail-open only for composition roots that do not expose a
// runtime lifecycle (principally focused middleware tests).
func DrainGuard(isDraining func() bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isDraining == nil || !isDraining() {
			c.Next()
			return
		}
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/mcp") && mcpRequestIsReadOnly(c.Request) {
			c.Next()
			return
		}
		c.Abort()
		staticErrorReply(c, http.StatusServiceUnavailable, "RUNTIME_DRAINING", "Runtime is draining")
	}
}

var readOnlyMCPMethods = map[string]struct{}{
	"initialize":                {},
	"notifications/initialized": {},
	"notifications/cancelled":   {},
	"ping":                      {},
	"tools/list":                {},
	"resources/list":            {},
	"resources/templates/list":  {},
	"resources/read":            {},
	"prompts/list":              {},
	"prompts/get":               {},
	"completion/complete":       {},
}

func mcpRequestIsReadOnly(r *http.Request) bool {
	if r.Body == nil {
		return false
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))

	var batch []struct {
		Method string `json:"method"`
	}
	if len(data) > 0 && data[0] == '[' {
		if err := json.Unmarshal(data, &batch); err != nil || len(batch) == 0 {
			return false
		}
	} else {
		var request struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(data, &request); err != nil {
			return false
		}
		batch = append(batch, request)
	}

	for _, request := range batch {
		if _, ok := readOnlyMCPMethods[request.Method]; !ok {
			return false
		}
	}
	return true
}

func normalizeOrigin(origin string) (string, bool) {
	origin = strings.TrimSpace(origin)
	if origin == "" || origin == "null" || strings.ContainsAny(origin, "\r\n") {
		return "", false
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), true
}

func requestOriginAllowed(r *http.Request, normalized string, allowlist map[string]struct{}) bool {
	if _, ok := allowlist[normalized]; ok {
		return true
	}
	u, err := url.Parse(normalized)
	if err != nil || !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	if r.TLS != nil {
		return u.Scheme == "https"
	}
	return u.Scheme == "http"
}

// visitorTracker tracks per-IP request counts for rate limiting.
type visitorTracker struct {
	mu        sync.Mutex
	visitors  map[string]*visitorInfo
	lastSweep time.Time
}

type visitorInfo struct {
	count    int
	lastSeen time.Time
}

// RateLimit returns a Gin middleware that limits requests per IP.
// maxRequests is the number of requests allowed within the window duration.
func RateLimit(maxRequests int, window time.Duration) gin.HandlerFunc {
	tracker := &visitorTracker{
		visitors:  make(map[string]*visitorInfo),
		lastSweep: time.Now(),
	}

	return func(c *gin.Context) {
		ip := c.ClientIP()

		tracker.mu.Lock()
		now := time.Now()
		if now.Sub(tracker.lastSweep) >= window {
			cutoff := now.Add(-window)
			for trackedIP, tracked := range tracker.visitors {
				if tracked.lastSeen.Before(cutoff) {
					delete(tracker.visitors, trackedIP)
				}
			}
			tracker.lastSweep = now
		}
		v, exists := tracker.visitors[ip]
		if !exists || now.Sub(v.lastSeen) > window {
			tracker.visitors[ip] = &visitorInfo{count: 1, lastSeen: now}
			tracker.mu.Unlock()
			c.Next()
			return
		}
		v.count++
		v.lastSeen = now
		count := v.count
		tracker.mu.Unlock()

		if count > maxRequests {
			c.Abort()
			staticErrorReply(c, http.StatusTooManyRequests, "RATE_LIMIT_EXCEEDED", "Rate limit exceeded")
			return
		}
		c.Next()
	}
}

// ProjectGuard returns a Gin middleware that validates the project ID from the
// URL parameter ":id". It ensures the project exists and is not archived
// (except for archive/restore endpoints which are always allowed).
// On success, it sets the project object in the Gin context under key "project".
func ProjectGuard(projectSvc *service.ProjectService) gin.HandlerFunc {
	return func(c *gin.Context) {
		pid := c.Param("id")
		if pid == "" {
			c.Abort()
			invalidRequestReply(c)
			return
		}

		project, err := projectSvc.GetProject(c.Request.Context(), pid)
		if err != nil {
			c.Abort()
			errorReply(c, err)
			return
		}

		// Allow archive/restore endpoints and GET requests to proceed even for archived projects.
		// Only block mutating operations (POST/PATCH/DELETE) on archived projects,
		// except for archive and restore actions themselves.
		if project.Status == "archived" {
			method := c.Request.Method
			path := c.Request.URL.Path
			isArchiveRestore := len(path) >= 8 && (path[len(path)-8:] == "/archive" || path[len(path)-8:] == "/restore")
			if method != "GET" && !isArchiveRestore {
				c.Abort()
				errorReply(c, store.ErrProjectArchived)
				return
			}
		}

		c.Set("project", project)
		c.Next()
	}
}
