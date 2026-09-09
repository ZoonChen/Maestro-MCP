package handler

import (
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// M4-OBS-001 request telemetry sampling. The hot path only appends
// bounded latency samples into an in-process sampler; the producer
// worker (internal/app) drains the buckets each interval and upserts
// aggregated windows into telemetry_aggregates. The sampling dimension
// is the redaction allowlist itself: route TEMPLATE, method, status
// class and a project id taken from the matched path — never raw
// paths, query strings, headers, principals or bodies (OBS-RULE-003).

// Sampler bounds: memory stays capped no matter how many
// (project, route, method, status-class) combinations traffic invents.
const (
	// DefaultSamplerMaxKeys caps the number of concurrent sample
	// buckets; a brand-new key beyond the cap is dropped and counted,
	// never evicts an existing bucket.
	DefaultSamplerMaxKeys = 1024
	// DefaultSamplerCapacity is the ring size per bucket; once full the
	// oldest sample is evicted (counted) to admit the newest.
	DefaultSamplerCapacity = 512
)

// sampleKey is the bucket dimension. The strings come from trusted
// route templates and path params, so a key can never carry a raw URL.
type sampleKey struct {
	projectID   string
	route       string
	method      string
	statusClass int
}

// sampleRing is one bucket's fixed-size circular buffer of durations
// in nanoseconds.
type sampleRing struct {
	samples []float64
	head    int
	size    int
}

func (r *sampleRing) observe(durationNanos float64) {
	// The caller counts the eviction when the ring is already full.
	r.samples[r.head] = durationNanos
	if r.size < len(r.samples) {
		r.size++
	}
	r.head = (r.head + 1) % len(r.samples)
}

// RequestSampler records bounded per-bucket request latency samples.
// The steady-state Observe path takes one mutex and allocates nothing.
type RequestSampler struct {
	mu       sync.Mutex
	maxKeys  int
	capacity int
	buckets  map[sampleKey]*sampleRing
	// Loss counters (cumulative under mu; Overflow reports deltas).
	keyDrops     int64
	evictedTotal uint64
	lastKeyDrops int64
	lastEvicted  uint64
}

// NewRequestSampler builds a sampler with explicit bounds.
func NewRequestSampler(maxKeys, capacityPerKey int) *RequestSampler {
	if maxKeys <= 0 {
		maxKeys = DefaultSamplerMaxKeys
	}
	if capacityPerKey <= 0 {
		capacityPerKey = DefaultSamplerCapacity
	}
	return &RequestSampler{
		maxKeys:  maxKeys,
		capacity: capacityPerKey,
		buckets:  make(map[sampleKey]*sampleRing),
	}
}

// Observe appends one request sample. statusClass is the status code
// divided by 100 (2, 4, 5, …) so the cardinality stays closed.
func (s *RequestSampler) Observe(projectID, route, method string, status int, duration time.Duration) {
	key := sampleKey{projectID: projectID, route: route, method: method, statusClass: status / 100}
	s.mu.Lock()
	ring, ok := s.buckets[key]
	if !ok {
		if len(s.buckets) >= s.maxKeys {
			s.keyDrops++
			s.mu.Unlock()
			return
		}
		ring = &sampleRing{samples: make([]float64, s.capacity)}
		s.buckets[key] = ring
	}
	if ring.size == len(ring.samples) {
		s.evictedTotal++ // full ring: the oldest sample is the eviction
	}
	ring.observe(float64(duration.Nanoseconds()))
	s.mu.Unlock()
}

// SampleBucket is one drained bucket: its dimension plus the drained
// duration samples in nanoseconds.
type SampleBucket struct {
	ProjectID   string
	Route       string
	Method      string
	StatusClass int
	Values      []float64
}

// Drain atomically swaps out every bucket. The producer owns the
// returned slices; the sampler restarts from empty buffers.
func (s *RequestSampler) Drain() []SampleBucket {
	s.mu.Lock()
	drained := s.buckets
	s.buckets = make(map[sampleKey]*sampleRing, len(drained))
	s.mu.Unlock()

	buckets := make([]SampleBucket, 0, len(drained))
	for key, ring := range drained {
		values := make([]float64, 0, ring.size)
		for offset := range ring.size {
			index := (ring.head - ring.size + offset + len(ring.samples)) % len(ring.samples)
			values = append(values, ring.samples[index])
		}
		buckets = append(buckets, SampleBucket{
			ProjectID: key.projectID, Route: key.route, Method: key.method,
			StatusClass: key.statusClass, Values: values,
		})
	}
	return buckets
}

// Overflow reports the sampler's loss counters since the previous
// Overflow call: samples dropped because the key cap was reached, and
// samples evicted by the per-bucket rings. Both are diagnostics for
// the producer's flush log; they never block the hot path.
func (s *RequestSampler) Overflow() (keyDrops int64, evicted uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keyDrops = s.keyDrops - s.lastKeyDrops
	evicted = s.evictedTotal - s.lastEvicted
	s.lastKeyDrops = s.keyDrops
	s.lastEvicted = s.evictedTotal
	return keyDrops, evicted
}

// RequestTelemetryMiddleware samples every request's latency into the
// sampler. It mounts after MaxBodySize and CORS so rate-limit,
// authentication, drain, remote-write and handler outcomes all land in
// the availability and latency SLIs; a 5xx is the only failure class
// (auth denials and 429s are client-side outcomes, not unavailability
// — the PR's metric mapping table documents this policy).
func RequestTelemetryMiddleware(sampler *RequestSampler) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "<unmatched>"
		}
		sampler.Observe(sampleProjectID(c), route, c.Request.Method,
			c.Writer.Status(), time.Since(started))
	}
}

// sampleProjectID derives the project scope from the matched route
// template's path params — the v3 tree carries :pid and the v1 project
// group :id; everything else is platform-wide (the producer's rollup
// project). Identity and authorization state are deliberately NOT
// sampled.
func sampleProjectID(c *gin.Context) string {
	if pid := c.Param("pid"); pid != "" {
		return pid
	}
	return c.Param("id")
}
