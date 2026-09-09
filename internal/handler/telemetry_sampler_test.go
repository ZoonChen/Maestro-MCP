package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M4-OBS-001 F1: the in-process request sampler — bucket attribution
// (project scope from the matched template, route template, method,
// status class), bounded memory (key cap and ring eviction both count
// losses), the drain swap, and the middleware's sampling dimension.

func TestRequestSamplerBucketAttribution(t *testing.T) {
	sampler := NewRequestSampler(0, 0)
	sampler.Observe("proj-1", "/api/v1/projects/:id/tasks", http.MethodGet, 200, 10*time.Millisecond)
	sampler.Observe("proj-1", "/api/v1/projects/:id/tasks", http.MethodGet, 200, 30*time.Millisecond)
	sampler.Observe("proj-1", "/api/v1/projects/:id/tasks", http.MethodGet, 500, 90*time.Millisecond)
	sampler.Observe("proj-2", "/api/v1/projects/:id/tasks", http.MethodGet, 200, 20*time.Millisecond)
	// Project-less request: attribution stays empty until the producer
	// rolls it up into the platform project.
	sampler.Observe("", "/api/v1/projects", http.MethodGet, 200, 5*time.Millisecond)

	buckets := sampler.Drain()
	byKey := map[string]SampleBucket{}
	for _, bucket := range buckets {
		key := fmt.Sprintf("%s|%s|%s|%d", bucket.ProjectID, bucket.Route, bucket.Method, bucket.StatusClass)
		byKey[key] = bucket
	}

	scoped := byKey["proj-1|/api/v1/projects/:id/tasks|GET|2"]
	require.Len(t, scoped.Values, 2, "status class splits buckets")
	failed := byKey["proj-1|/api/v1/projects/:id/tasks|GET|5"]
	require.Len(t, failed.Values, 1)
	otherProject := byKey["proj-2|/api/v1/projects/:id/tasks|GET|2"]
	require.Len(t, otherProject.Values, 1)
	platform := byKey["|/api/v1/projects|GET|2"]
	require.Len(t, platform.Values, 1, "project-less requests sample under the empty scope")

	// Drain swaps: a second drain is empty, further observes start fresh.
	assert.Empty(t, sampler.Drain())
	sampler.Observe("proj-1", "/api/v1/projects/:id/tasks", http.MethodGet, 200, time.Millisecond)
	assert.Len(t, sampler.Drain(), 1)
}

func TestRequestSamplerBoundedMemory(t *testing.T) {
	t.Run("key cap drops new buckets and counts", func(t *testing.T) {
		sampler := NewRequestSampler(2, 4)
		sampler.Observe("p1", "/r1", http.MethodGet, 200, time.Millisecond)
		sampler.Observe("p2", "/r2", http.MethodGet, 200, time.Millisecond)
		sampler.Observe("p3", "/r3", http.MethodGet, 200, time.Millisecond) // beyond the cap
		sampler.Observe("p4", "/r4", http.MethodGet, 200, time.Millisecond) // beyond the cap
		keyDrops, evicted := sampler.Overflow()
		assert.Equal(t, int64(2), keyDrops, "new keys beyond the cap are dropped")
		assert.Equal(t, uint64(0), evicted)
		assert.Len(t, sampler.Drain(), 2, "existing buckets are never evicted by the cap")
	})

	t.Run("full ring evicts the oldest sample and counts", func(t *testing.T) {
		sampler := NewRequestSampler(4, 3)
		for index := range 5 {
			sampler.Observe("p1", "/r1", http.MethodGet, 200, time.Duration(index+1)*time.Millisecond)
		}
		keyDrops, evicted := sampler.Overflow()
		assert.Equal(t, int64(0), keyDrops)
		assert.Equal(t, uint64(2), evicted, "two oldest samples were evicted by the ring")

		buckets := sampler.Drain()
		require.Len(t, buckets, 1)
		// The ring keeps the newest three samples (3ms, 4ms, 5ms).
		assert.ElementsMatch(t, []float64{
			float64(3 * time.Millisecond),
			float64(4 * time.Millisecond),
			float64(5 * time.Millisecond),
		}, buckets[0].Values)
	})
}

func TestRequestTelemetryMiddlewareSamplingDimension(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sampler := NewRequestSampler(0, 0)
	router := gin.New()
	router.Use(RequestTelemetryMiddleware(sampler))
	router.GET("/api/v1/projects/:id/tasks", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.GET("/api/v3/projects/:pid/slo-snapshot", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": "ROUTE_NOT_FOUND"})
	})

	for _, path := range []string{
		"/api/v1/projects/proj-9/tasks",
		"/api/v3/projects/proj-9/slo-snapshot",
		"/health",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, path)
	}
	// An unmatched path answers 404 and still samples as <unmatched>.
	req := httptest.NewRequest(http.MethodGet, "/does/not/exist", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	buckets := sampler.Drain()
	require.Len(t, buckets, 4)
	routes := map[string]SampleBucket{}
	for _, bucket := range buckets {
		routes[bucket.Route] = bucket
	}
	// The v1 tree's :id and the v3 tree's :pid both carry the project
	// scope; unscoped routes sample under the empty project.
	assert.Equal(t, "proj-9", routes["/api/v1/projects/:id/tasks"].ProjectID)
	assert.Equal(t, "proj-9", routes["/api/v3/projects/:pid/slo-snapshot"].ProjectID)
	assert.Equal(t, "", routes["/health"].ProjectID)
	// Unmatched raw paths collapse to a closed token — the sampler never
	// records a raw URL.
	assert.Contains(t, routes, "<unmatched>")
	assert.Equal(t, "", routes["<unmatched>"].ProjectID)
}
