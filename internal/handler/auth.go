package handler

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// AuthMiddleware returns a Gin middleware that validates Bearer tokens.
// If authToken is empty, all requests are allowed (auth disabled).
// Exempt paths: /dashboard, /dashboard/assets/*, and the root redirect.
func AuthMiddleware(authToken string) gin.HandlerFunc {
	enabled := authToken != ""
	return func(c *gin.Context) {
		if !enabled {
			c.Next()
			return
		}

		// Exempt static assets and dashboard from auth.
		path := c.Request.URL.Path
		if path == "/" || strings.HasPrefix(path, "/dashboard") {
			c.Next()
			return
		}

		// Extract Bearer token from Authorization header.
		header := c.GetHeader("Authorization")
		if header == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":      "missing Authorization header",
				"error_code": "AUTH_REQUIRED",
			})
			return
		}

		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":      "invalid Authorization header format, expected: Bearer <token>",
				"error_code": "AUTH_INVALID_FORMAT",
			})
			return
		}

		// Constant-time comparison to prevent timing attacks.
		if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(authToken)) != 1 {
			slog.Warn("auth: invalid token",
				"remote_addr", c.ClientIP(),
				"path", path,
				"method", c.Request.Method,
			)
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":      "invalid or expired token",
				"error_code": "AUTH_INVALID_TOKEN",
			})
			return
		}

		c.Next()
	}
}
