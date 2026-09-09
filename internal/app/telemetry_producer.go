package app

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/handler"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// M4-OBS-001 telemetry producer: the single process-wide worker that
// drains the request sampler (F1) and the platform depth gauges (F3)
// each interval and upserts pre-aggregated, redacted windows into
// telemetry_aggregates via store.RecordTelemetry. Raw samples never
// leave the process — the redaction policy version travels with every
// written window.

// Canonical metric names the producer writes. The SLO policy names
// these same strings in its availability/objectives sections (see the
// slo example in maestro.yaml.example); the mapping is frozen by
// slo-status.schema.json's objective vocabulary.
const (
	MetricHTTPRequestsTotal         = "http_requests_total"
	MetricHTTPRequestsSuccess       = "http_requests_success"
	MetricAPIP95LatencyMS           = "api_p95_latency_ms"
	MetricWebhookIngestP95LatencyMS = "webhook_ingest_p95_latency_ms"
	MetricWebhookInboxDepth         = "webhook_inbox_depth"
	MetricWebhookDLQDepth           = "webhook_dlq_depth"
	MetricWebhookOutboxPending      = "webhook_outbox_pending"
	MetricInboxLagP95Seconds        = "inbox_lag_p95_seconds"
	MetricOutboxLagP95Seconds       = "outbox_lag_p95_seconds"
)

// Alert thresholds frozen by docs/operations/observability-and-audit.md
// §11: Inbox/Outbox P95 > 30s and DLQ > 0 alert immediately. Every
// alert carries its runbook reference (OBS-RULE-005).
const (
	inboxLagAlertSeconds  = 30.0
	outboxLagAlertSeconds = 30.0
	webhookRunbookRef     = "runbooks/webhook-pipeline-failure"
)

// TelemetryRecorder is the write side of the observability store.
type TelemetryRecorder interface {
	RecordTelemetry(ctx context.Context, point store.TelemetryPoint) error
}

// validateTelemetryProducerOptions rejects a partially wired producer
// at composition time: sampling without a flush target, a non-positive
// interval or a missing redaction identity would silently lose (or
// misattribute) telemetry, so startup fails closed instead.
func validateTelemetryProducerOptions(opts *TelemetryProducerOptions) error {
	if opts == nil {
		return nil
	}
	switch {
	case opts.Sampler == nil:
		return errors.New("telemetry producer requires a request sampler")
	case opts.Store == nil:
		return errors.New("telemetry producer requires a telemetry store")
	case opts.Interval <= 0:
		return errors.New("telemetry producer requires a positive flush interval")
	case opts.RedactionVersion == "":
		return errors.New("telemetry producer requires a redaction version")
	case opts.PlatformProjectID == "":
		return errors.New("telemetry producer requires a platform project id")
	}
	return nil
}

// PlatformDepthSource reads the webhook inbox/DLQ/outbox gauges.
type PlatformDepthSource interface {
	PlatformDepths(ctx context.Context) (store.PlatformDepths, error)
}

// TelemetryProducerOptions wires the producer worker; the composition
// root (PostgreSQL mode) supplies the sampler, both store faces and
// the trusted configuration values.
type TelemetryProducerOptions struct {
	Sampler           *handler.RequestSampler
	Store             TelemetryRecorder
	Depths            PlatformDepthSource
	Interval          time.Duration
	RedactionVersion  string
	PlatformProjectID string
	// Logger receives flush errors and threshold alerts; nil uses the
	// process default (tests inject a captured logger).
	Logger *slog.Logger
}

// telemetryProducer owns the flush loop and the latest platform depth
// snapshot exposed through GET /api/v1/metrics.
type telemetryProducer struct {
	opts   TelemetryProducerOptions
	logger *slog.Logger
	latest atomic.Pointer[store.PlatformDepths]
}

func newTelemetryProducer(opts TelemetryProducerOptions) *telemetryProducer {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &telemetryProducer{opts: opts, logger: logger}
}

// LatestPlatformDepths returns the most recent depth sample; the zero
// value (all gauges 0, lags nil) is served before the first tick.
func (p *telemetryProducer) LatestPlatformDepths() store.PlatformDepths {
	if snapshot := p.latest.Load(); snapshot != nil {
		return *snapshot
	}
	return store.PlatformDepths{}
}

// Run flushes on every tick until the background context is cancelled.
func (p *telemetryProducer) Run(ctx context.Context) {
	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.Flush(ctx, time.Now().UTC()); err != nil {
				p.logger.Error("telemetry producer flush failed", "error", err)
			}
		}
	}
}

// projectRollup accumulates one project's drained interval.
type projectRollup struct {
	total, success int64
	latencies      map[string][]float64 // metric name → milliseconds
}

// Flush drains the sampler and depth source and upserts every
// aggregate window for the interval that just closed. The window is
// labeled by its start; re-flushing the same (now, interval) converges
// on the same rows because RecordTelemetry upserts on the window key.
func (p *telemetryProducer) Flush(ctx context.Context, now time.Time) error {
	buckets := p.opts.Sampler.Drain()
	if keyDrops, evicted := p.opts.Sampler.Overflow(); keyDrops > 0 || evicted > 0 {
		p.logger.Warn("telemetry sampler overflowed its bounds",
			"dropped_new_buckets", keyDrops, "evicted_samples", evicted,
			"policy", "bounded memory, samples are lost not queued")
	}

	windowStart := now.Add(-p.opts.Interval)
	kind := windowKindFor(p.opts.Interval)

	rollups := make(map[string]*projectRollup)
	for _, bucket := range buckets {
		project := bucket.ProjectID
		if project == "" {
			// Requests without a project scope roll up into the declared
			// platform project: skipping them would undercount the
			// availability SLI (every request counts, scoped or not).
			project = p.opts.PlatformProjectID
		}
		rollup, ok := rollups[project]
		if !ok {
			rollup = &projectRollup{latencies: make(map[string][]float64)}
			rollups[project] = rollup
		}
		rollup.total += int64(len(bucket.Values))
		if bucket.StatusClass < 5 {
			rollup.success += int64(len(bucket.Values))
		}
		if metric := latencyMetricForRoute(bucket.Route); metric != "" {
			for _, nanos := range bucket.Values {
				rollup.latencies[metric] = append(rollup.latencies[metric], nanos/1e6)
			}
		}
	}

	for project, rollup := range rollups {
		if rollup.total > 0 {
			if err := p.opts.Store.RecordTelemetry(ctx, store.TelemetryPoint{
				ProjectID: project, Metric: MetricHTTPRequestsTotal, WindowKind: kind,
				WindowStart: windowStart.Format(time.RFC3339), SampleCount: rollup.total,
				Sum: float64(rollup.total), RedactionVersion: p.opts.RedactionVersion,
			}); err != nil {
				return err
			}
		}
		if rollup.success > 0 {
			if err := p.opts.Store.RecordTelemetry(ctx, store.TelemetryPoint{
				ProjectID: project, Metric: MetricHTTPRequestsSuccess, WindowKind: kind,
				WindowStart: windowStart.Format(time.RFC3339), SampleCount: rollup.success,
				Sum: float64(rollup.success), RedactionVersion: p.opts.RedactionVersion,
			}); err != nil {
				return err
			}
		}
		for _, metric := range sortedMetricNames(rollup.latencies) {
			if err := p.recordLatency(ctx, project, metric, kind, windowStart, rollup.latencies[metric]); err != nil {
				return err
			}
		}
	}

	if p.opts.Depths != nil {
		if err := p.sampleDepths(ctx, kind, windowStart); err != nil {
			return err
		}
	}
	return nil
}

// recordLatency aggregates one metric's drained samples (milliseconds)
// into count/sum/min/max/p50/p95/p99 and upserts the window.
func (p *telemetryProducer) recordLatency(ctx context.Context, projectID, metric, kind string, windowStart time.Time, valuesMS []float64) error {
	if len(valuesMS) == 0 {
		return nil
	}
	sorted := append([]float64(nil), valuesMS...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, value := range sorted {
		sum += value
	}
	p50, p95, p99 := percentile(sorted, 0.50), percentile(sorted, 0.95), percentile(sorted, 0.99)
	minimum, maximum := sorted[0], sorted[len(sorted)-1]
	return p.opts.Store.RecordTelemetry(ctx, store.TelemetryPoint{
		ProjectID: projectID, Metric: metric, WindowKind: kind,
		WindowStart: windowStart.Format(time.RFC3339),
		SampleCount: int64(len(sorted)), Sum: sum,
		Min: &minimum, Max: &maximum, P50: &p50, P95: &p95, P99: &p99,
		RedactionVersion: p.opts.RedactionVersion,
	})
}

// sampleDepths reads the platform gauges, publishes the snapshot for
// /api/v1/metrics, records them as telemetry windows under the
// platform project and evaluates the §11 alert thresholds.
func (p *telemetryProducer) sampleDepths(ctx context.Context, kind string, windowStart time.Time) error {
	depths, err := p.opts.Depths.PlatformDepths(ctx)
	if err != nil {
		return err
	}
	p.latest.Store(&depths)

	gauge := func(metric string, value int64) error {
		if value == 0 {
			return nil // an absent gauge is no row, never a zero row
		}
		return p.opts.Store.RecordTelemetry(ctx, store.TelemetryPoint{
			ProjectID: p.opts.PlatformProjectID, Metric: metric, WindowKind: kind,
			WindowStart: windowStart.Format(time.RFC3339), SampleCount: 1, Sum: float64(value),
			RedactionVersion: p.opts.RedactionVersion,
		})
	}
	if err := gauge(MetricWebhookInboxDepth, depths.InboxBacklog); err != nil {
		return err
	}
	if err := gauge(MetricWebhookDLQDepth, depths.DLQDepth); err != nil {
		return err
	}
	if err := gauge(MetricWebhookOutboxPending, depths.OutboxPending); err != nil {
		return err
	}

	lag := func(metric string, backlog int64, mean float64, p95 *float64) error {
		if backlog == 0 || p95 == nil {
			return nil // empty backlog has no measurable lag (no_data, never 0)
		}
		return p.opts.Store.RecordTelemetry(ctx, store.TelemetryPoint{
			ProjectID: p.opts.PlatformProjectID, Metric: metric, WindowKind: kind,
			WindowStart: windowStart.Format(time.RFC3339), SampleCount: backlog,
			Sum: mean * float64(backlog), P95: p95,
			RedactionVersion: p.opts.RedactionVersion,
		})
	}
	if err := lag(MetricInboxLagP95Seconds, depths.InboxBacklog,
		depths.InboxLagMeanSeconds, depths.InboxLagP95Seconds); err != nil {
		return err
	}
	if err := lag(MetricOutboxLagP95Seconds, depths.OutboxPending,
		depths.OutboxLagMeanSeconds, depths.OutboxLagP95Seconds); err != nil {
		return err
	}

	p.evaluateAlerts(ctx, depths)
	return nil
}

// evaluateAlerts applies the OBS §11 thresholds: DLQ > 0 and
// Inbox/Outbox P95 lag > 30s alert immediately with severity, owner
// and runbook reference; the alert name is the dedup key so log
// aggregators can suppress repeats between producer ticks.
func (p *telemetryProducer) evaluateAlerts(ctx context.Context, depths store.PlatformDepths) {
	if depths.DLQDepth > 0 {
		p.alert(ctx, slog.LevelError, "webhook_dlq_depth", "critical",
			float64(depths.DLQDepth), "entries", "a dead-lettered webhook requires the two-person replay procedure")
	}
	if lag := depths.InboxLagP95Seconds; lag != nil && *lag > inboxLagAlertSeconds {
		p.alert(ctx, slog.LevelWarn, "inbox_lag_p95_seconds", "warning",
			*lag, "seconds", "inbox backlog is aging past the 30s convergence budget")
	}
	if lag := depths.OutboxLagP95Seconds; lag != nil && *lag > outboxLagAlertSeconds {
		p.alert(ctx, slog.LevelWarn, "outbox_lag_p95_seconds", "warning",
			*lag, "seconds", "outbox delivery is lagging past the 30s convergence budget")
	}
}

func (p *telemetryProducer) alert(ctx context.Context, level slog.Level, name, severity string, value float64, unit, summary string) {
	p.logger.LogAttrs(ctx, level, "telemetry alert",
		slog.String("alert", name),
		slog.String("severity", severity),
		slog.Float64("value", value),
		slog.String("unit", unit),
		slog.String("summary", summary),
		slog.String("runbook_ref", webhookRunbookRef),
		slog.String("owner_role", "operations_owner"),
		slog.String("dedup_key", name),
	)
}

// latencyMetricForRoute maps a sampled route template to its latency
// metric: the GitLab webhook receiver carries its own ingest objective,
// the API/MCP tree is the generic api_p95 objective, and static/health
// surfaces stay out of the latency objectives (they still count toward
// availability).
func latencyMetricForRoute(route string) string {
	switch {
	case strings.HasPrefix(route, "/api/v3/webhooks/"):
		return MetricWebhookIngestP95LatencyMS
	case route == "/mcp" || strings.HasPrefix(route, "/mcp/"):
		return MetricAPIP95LatencyMS
	case strings.HasPrefix(route, "/api/"):
		return MetricAPIP95LatencyMS
	default:
		return ""
	}
}

// windowKindFor snaps the producer interval onto the frozen
// telemetry_aggregates window vocabulary.
func windowKindFor(interval time.Duration) string {
	switch {
	case interval <= time.Minute:
		return "1m"
	case interval <= 5*time.Minute:
		return "5m"
	case interval <= time.Hour:
		return "1h"
	default:
		return "1d"
	}
}

// percentile is the nearest-rank percentile over an ascending slice.
func percentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(quantile * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// sortedMetricNames iterates a rollup's latency metrics in a stable
// order so flush behavior is deterministic in tests.
func sortedMetricNames(latencies map[string][]float64) []string {
	names := make([]string, 0, len(latencies))
	for name := range latencies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
