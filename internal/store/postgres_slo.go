package store

import (
	"context"
	"time"
)

// PostgreSQL measurement input for the M4-REL-001 SLO core: the
// latest telemetry window per requested metric inside the lookback
// half-open interval [notBefore, notAfter). Metric names are
// caller-supplied — the store never invents metric conventions; a
// metric without a window in range is simply absent so the evaluator
// classifies it no_data.

// MetricWindow is the newest telemetry window for one metric.
type MetricWindow struct {
	Metric      string
	WindowKind  string
	WindowStart time.Time
	SampleCount int64
	Sum         float64
	// P95 is nil when the window carried no percentile — the honest
	// no_data marker for the evaluator.
	P95 *float64
}

// LatestMetricWindows returns at most one row per requested metric:
// the newest window in range, chosen by window_start.
func (s pgObservabilityStore) LatestMetricWindows(ctx context.Context, projectID string, metrics []string, notBefore, notAfter time.Time) ([]MetricWindow, error) {
	if len(metrics) == 0 {
		return nil, nil
	}
	if !notBefore.Before(notAfter) {
		return nil, ErrTelemetryWindowRejected
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (metric) metric, window_kind, window_start, sample_count, sum_value, p95_value
		FROM telemetry_aggregates
		WHERE project_id = $1 AND metric = ANY($2)
			AND window_start >= $3 AND window_start < $4
		ORDER BY metric, window_start DESC`,
		projectID, metrics, notBefore, notAfter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var windows []MetricWindow
	for rows.Next() {
		window := MetricWindow{}
		var p95 *float64
		if scanErr := rows.Scan(&window.Metric, &window.WindowKind, &window.WindowStart, &window.SampleCount, &window.Sum, &p95); scanErr != nil {
			return nil, scanErr
		}
		window.P95 = p95
		windows = append(windows, window)
	}
	return windows, rows.Err()
}
