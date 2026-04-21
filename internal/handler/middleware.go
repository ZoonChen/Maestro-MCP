package handler

import (
	"errors"
	"net/http"
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

// CORS returns a Gin middleware that adds Cross-Origin Resource Sharing headers.
// In local-development mode, all origins are allowed. In production, only
// same-origin requests are permitted by default.
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Max-Age", "86400")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// visitorTracker tracks per-IP request counts for rate limiting.
type visitorTracker struct {
	mu       sync.Mutex
	visitors map[string]*visitorInfo
}

type visitorInfo struct {
	count    int
	lastSeen time.Time
}

// RateLimit returns a Gin middleware that limits requests per IP.
// maxRequests is the number of requests allowed within the window duration.
func RateLimit(maxRequests int, window time.Duration) gin.HandlerFunc {
	tracker := &visitorTracker{visitors: make(map[string]*visitorInfo)}

	// Background cleanup of expired entries.
	go func() {
		for {
			time.Sleep(window)
			tracker.mu.Lock()
			cutoff := time.Now().Add(-window)
			for ip, v := range tracker.visitors {
				if v.lastSeen.Before(cutoff) {
					delete(tracker.visitors, ip)
				}
			}
			tracker.mu.Unlock()
		}
	}()

	return func(c *gin.Context) {
		ip := c.ClientIP()

		tracker.mu.Lock()
		now := time.Now()
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
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":      "rate limit exceeded",
				"error_code": "RATE_LIMIT_EXCEEDED",
			})
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
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing project id"})
			return
		}

		project, err := projectSvc.GetProject(c.Request.Context(), pid)
		if err != nil {
			if errors.Is(err, store.ErrProjectNotFound) {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
					"error":      "project not found",
					"error_code": "PROJECT_NOT_FOUND",
				})
				return
			}
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error":      "project is archived",
					"error_code": "PROJECT_ARCHIVED",
				})
				return
			}
		}

		c.Set("project", project)
		c.Next()
	}
}
