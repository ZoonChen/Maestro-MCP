package handler

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// AuthMiddleware returns a Gin middleware that validates Bearer tokens.
// Only liveness/readiness endpoints are anonymous. An empty server token is a
// fail-closed configuration: every non-health request is rejected.
func AuthMiddleware(authToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if isAnonymousHealthPath(path) {
			c.Next()
			return
		}

		if authToken == "" {
			c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
			c.Abort()
			staticErrorReply(c, http.StatusUnauthorized, "AUTH_NOT_CONFIGURED", "Authentication is not configured")
			return
		}

		// Extract Bearer token from Authorization header.
		header := c.GetHeader("Authorization")
		if header == "" {
			c.Header("WWW-Authenticate", `Bearer realm="maestro"`)
			c.Abort()
			staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
			return
		}

		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.Header("WWW-Authenticate", `Bearer realm="maestro", error="invalid_request"`)
			c.Abort()
			staticErrorReply(c, http.StatusUnauthorized, "AUTH_INVALID_FORMAT", "Authorization header format is invalid")
			return
		}

		// Constant-time comparison to prevent timing attacks.
		if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(authToken)) != 1 {
			route := c.FullPath()
			if route == "" {
				route = "<unmatched>"
			}
			slog.Warn("auth: invalid token",
				"route", route,
				"method", c.Request.Method,
			)
			c.Header("WWW-Authenticate", `Bearer realm="maestro", error="invalid_token"`)
			c.Abort()
			staticErrorReply(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "Authentication token is invalid or expired")
			return
		}

		c.Next()
	}
}

func isAnonymousHealthPath(path string) bool {
	switch path {
	case "/health", "/livez", "/readyz":
		return true
	default:
		return false
	}
}
