package app

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/handler"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// M4-OBS-001 F2/F3 unit level: the producer's flush mapping (buckets →
// canonical metric windows), the platform rollup, the depth gauges and
// the OBS §11 alert semantics with a captured logger. PostgreSQL
// behavior (window upsert convergence, the depth query and the
// slo-snapshot flow) lives in the PG-gated test below.

type fakeTelemetryRecorder struct {
	points []store.TelemetryPoint
	failOn map[string]error
}

func (f *fakeTelemetryRecorder) RecordTelemetry(_ context.Context, point store.TelemetryPoint) error {
	if err, ok := f.failOn[point.Metric]; ok {
		return err
	}
	f.points = append(f.points, point)
	return nil
}

func (f *fakeTelemetryRecorder) byMetric() map[string]store.TelemetryPoint {
	points := map[string]store.TelemetryPoint{}
	for _, point := range f.points {
		points[point.Metric+"/"+point.ProjectID] = point
	}
	return points
}

type fakeDepthSource struct {
	depths store.PlatformDepths
}

func (f *fakeDepthSource) PlatformDepths(context.Context) (store.PlatformDepths, error) {
	return f.depths, nil
}

func newTestProducer(recorder *fakeTelemetryRecorder, depths *fakeDepthSource, sampler *handler.RequestSampler, logger *slog.Logger) *telemetryProducer {
	return newTelemetryProducer(TelemetryProducerOptions{
		Sampler:           sampler,
		Store:             recorder,
		Depths:            depths,
		Interval:          time.Minute,
		RedactionVersion:  "redact-v1",
		PlatformProjectID: "platform-1",
		Logger:            logger,
	})
}

func capturedLogger() (*slog.Logger, *bytes.Buffer) {
	buffer := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug})), buffer
}

func TestTelemetryProducerFlushMapping(t *testing.T) {
	recorder := &fakeTelemetryRecorder{}
	depths := &fakeDepthSource{}
	logger, logs := capturedLogger()
	sampler := handler.NewRequestSampler(0, 0)
	producer := newTestProducer(recorder, depths, sampler, logger)

	// One project's API traffic: one 2xx, one 5xx (availability hit)
	// plus the webhook ingest route (its own latency objective); one
	// unscoped health probe that must roll up to the platform project
	// without joining any latency metric.
	sampler.Observe("proj-1", "/api/v1/projects/:id/tasks", http.MethodGet, 200, 10*time.Millisecond)
	sampler.Observe("proj-1", "/api/v1/projects/:id/tasks", http.MethodGet, 500, 100*time.Millisecond)
	sampler.Observe("proj-1", "/api/v3/webhooks/gitlab/:instance_id", http.MethodPost, 200, 5*time.Millisecond)
	sampler.Observe("", "/health", http.MethodGet, 200, time.Millisecond)

	now := time.Date(2026, 9, 9, 10, 1, 0, 0, time.UTC)
	require.NoError(t, producer.Flush(context.Background(), now))

	points := recorder.byMetric()
	windowStart := "2026-09-09T10:00:00Z"

	total := points["http_requests_total/proj-1"]
	assert.EqualValues(t, 3, total.SampleCount, "every sampled request counts toward total")
	assert.Equal(t, float64(3), total.Sum)
	assert.Equal(t, "1m", total.WindowKind)
	assert.Equal(t, windowStart, total.WindowStart)
	assert.Equal(t, "redact-v1", total.RedactionVersion)

	success := points["http_requests_success/proj-1"]
	assert.EqualValues(t, 2, success.SampleCount, "5xx is the only availability failure class")

	apiLatency := points["api_p95_latency_ms/proj-1"]
	require.NotNil(t, apiLatency.P95)
	assert.InDelta(t, 100.0, *apiLatency.P95, 1e-9, "p95 over [10ms, 100ms] is 100ms (nearest-rank)")
	assert.InDelta(t, 10.0, *apiLatency.P50, 1e-9)
	assert.InDelta(t, 10.0, *apiLatency.Min, 1e-9)
	assert.InDelta(t, 100.0, *apiLatency.Max, 1e-9)
	assert.InDelta(t, 110.0, apiLatency.Sum, 1e-9)

	ingestLatency := points["webhook_ingest_p95_latency_ms/proj-1"]
	require.NotNil(t, ingestLatency.P95)
	assert.InDelta(t, 5.0, *ingestLatency.P95, 1e-9)

	platformTotal := points["http_requests_total/platform-1"]
	assert.EqualValues(t, 1, platformTotal.SampleCount, "project-less requests roll up to the platform project")
	_, hasPlatformLatency := points["api_p95_latency_ms/platform-1"]
	assert.False(t, hasPlatformLatency, "unmatched-latency routes (health) join no latency metric")

	assert.NotContains(t, logs.String(), "overflowed", "no bounds were hit")
}

func TestTelemetryProducerDepthsAndAlerts(t *testing.T) {
	recorder := &fakeTelemetryRecorder{}
	lag := 40.0
	depths := &fakeDepthSource{depths: store.PlatformDepths{
		InboxBacklog: 1, InboxLagMeanSeconds: 35, InboxLagP95Seconds: &lag,
		DLQDepth:      2,
		OutboxPending: 0, // healthy outbox: no gauge row, no alert
	}}
	logger, logs := capturedLogger()
	producer := newTestProducer(recorder, depths, handler.NewRequestSampler(0, 0), logger)

	require.NoError(t, producer.Flush(context.Background(), time.Date(2026, 9, 9, 10, 1, 0, 0, time.UTC)))

	points := recorder.byMetric()
	assert.EqualValues(t, 1, points["webhook_dlq_depth/platform-1"].SampleCount)
	assert.InDelta(t, 2.0, points["webhook_dlq_depth/platform-1"].Sum, 1e-9)
	assert.EqualValues(t, 1, points["webhook_inbox_depth/platform-1"].SampleCount)
	_, hasOutboxGauge := points["webhook_outbox_pending/platform-1"]
	assert.False(t, hasOutboxGauge, "an absent gauge is no row, never a zero row")

	inboxLag := points["inbox_lag_p95_seconds/platform-1"]
	assert.EqualValues(t, 1, inboxLag.SampleCount, "lag windows carry the backlog as their sample count")
	assert.InDelta(t, 35.0, inboxLag.Sum, 1e-9)
	require.NotNil(t, inboxLag.P95)
	assert.InDelta(t, 40.0, *inboxLag.P95, 1e-9)

	// G2 semantics: DLQ>0 alerts critical, inbox lag P95>30s alerts
	// warning — both with the runbook reference; a healthy outbox stays
	// silent.
	assert.Contains(t, logs.String(), `alert=webhook_dlq_depth`)
	assert.Contains(t, logs.String(), `severity=critical`)
	assert.Contains(t, logs.String(), `runbook_ref=runbooks/webhook-pipeline-failure`)
	assert.Contains(t, logs.String(), `alert=inbox_lag_p95_seconds`)
	assert.Contains(t, logs.String(), `severity=warning`)
	assert.NotContains(t, logs.String(), `alert=outbox_lag_p95_seconds`)

	latest := producer.LatestPlatformDepths()
	assert.EqualValues(t, 2, latest.DLQDepth, "the metrics endpoint reads the latest snapshot")
}

func TestTelemetryProducerHealthyDepthsStaySilent(t *testing.T) {
	recorder := &fakeTelemetryRecorder{}
	healthy := &fakeDepthSource{}
	logger, logs := capturedLogger()
	producer := newTestProducer(recorder, healthy, handler.NewRequestSampler(0, 0), logger)

	require.NoError(t, producer.Flush(context.Background(), time.Date(2026, 9, 9, 10, 1, 0, 0, time.UTC)))
	assert.NotContains(t, logs.String(), `level=WARN`)
	assert.NotContains(t, logs.String(), `level=ERROR`)
	assert.Empty(t, recorder.points, "all-zero depths write no telemetry rows")
	assert.EqualValues(t, 0, producer.LatestPlatformDepths().DLQDepth)
}

func TestTelemetryProducerOverflowIsLogged(t *testing.T) {
	recorder := &fakeTelemetryRecorder{}
	depths := &fakeDepthSource{}
	logger, logs := capturedLogger()
	sampler := handler.NewRequestSampler(1, 2)
	sampler.Observe("p1", "/r1", http.MethodGet, 200, time.Millisecond)
	sampler.Observe("p2", "/r2", http.MethodGet, 200, time.Millisecond) // dropped at the key cap
	producer := newTestProducer(recorder, depths, sampler, logger)

	require.NoError(t, producer.Flush(context.Background(), time.Date(2026, 9, 9, 10, 1, 0, 0, time.UTC)))
	assert.Contains(t, logs.String(), "overflowed its bounds")
	assert.Contains(t, logs.String(), "dropped_new_buckets=1")
}

func TestValidateTelemetryProducerOptions(t *testing.T) {
	valid := &TelemetryProducerOptions{
		Sampler:           handler.NewRequestSampler(0, 0),
		Store:             &fakeTelemetryRecorder{},
		Interval:          time.Minute,
		RedactionVersion:  "v1",
		PlatformProjectID: "platform-1",
	}
	require.NoError(t, validateTelemetryProducerOptions(nil))
	require.NoError(t, validateTelemetryProducerOptions(valid))

	cases := map[string]func(*TelemetryProducerOptions){
		"sampler":     func(o *TelemetryProducerOptions) { o.Sampler = nil },
		"store":       func(o *TelemetryProducerOptions) { o.Store = nil },
		"interval":    func(o *TelemetryProducerOptions) { o.Interval = 0 },
		"redaction":   func(o *TelemetryProducerOptions) { o.RedactionVersion = "" },
		"platform id": func(o *TelemetryProducerOptions) { o.PlatformProjectID = "" },
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mutated := *valid
		cases[name](&mutated)
		assert.Error(t, validateTelemetryProducerOptions(&mutated), name)
	}
}

func TestWindowKindAndPercentile(t *testing.T) {
	assert.Equal(t, "1m", windowKindFor(30*time.Second))
	assert.Equal(t, "1m", windowKindFor(time.Minute))
	assert.Equal(t, "5m", windowKindFor(5*time.Minute))
	assert.Equal(t, "1h", windowKindFor(time.Hour))
	assert.Equal(t, "1d", windowKindFor(24*time.Hour))

	sorted := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	assert.InDelta(t, 50.0, percentile(sorted, 0.50), 1e-9)
	assert.InDelta(t, 100.0, percentile(sorted, 0.95), 1e-9, "nearest-rank p95 of 10 samples is the 10th (100)")
	assert.InDelta(t, 100.0, percentile(sorted, 0.99), 1e-9)
	assert.InDelta(t, 10.0, percentile(sorted, 0.01), 1e-9)
	assert.InDelta(t, 0.0, percentile(nil, 0.95), 1e-9)
}
