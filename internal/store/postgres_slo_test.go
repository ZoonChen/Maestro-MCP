package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/slo"
)

// PG-gated M4-REL-001 slice: the telemetry-backed objective
// measurement feeding the pure SLO evaluator — latest-window wins,
// out-of-range and missing metrics classify no_data.

func TestLatestMetricWindowsFeedsSLOSnapshot(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_slo_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_slo_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_slo_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_slo_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-000000000001', 'slo')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7e00-0000-7000-8000-000000000001', 'slo', 'SLO', 'active')`, m3Project)
	require.NoError(t, err)

	obs := pg.Observability()
	notAfter := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	notBefore := notAfter.Add(-30 * 24 * time.Hour)
	stale := notBefore.Add(-time.Hour)

	p95 := func(value float64) *float64 { return &value }
	seed := []TelemetryPoint{
		// Two windows for the same metric: the newest must win.
		{ProjectID: m3Project, Metric: "api_p95_latency_ms", WindowKind: "1h",
			WindowStart: notAfter.Add(-2 * time.Hour).Format(time.RFC3339), SampleCount: 12, Sum: 4200, P95: p95(280)},
		{ProjectID: m3Project, Metric: "api_p95_latency_ms", WindowKind: "1h",
			WindowStart: notAfter.Add(-time.Hour).Format(time.RFC3339), SampleCount: 30, Sum: 21000, P95: p95(610)},
		// A stale window outside the lookback must not be picked up.
		{ProjectID: m3Project, Metric: "inbox_lag_p95_seconds", WindowKind: "1h",
			WindowStart: stale.Format(time.RFC3339), SampleCount: 5, Sum: 400, P95: p95(80)},
	}
	for _, point := range seed {
		point.RedactionVersion = "redact-v1"
		require.NoError(t, obs.RecordTelemetry(ctx, point))
	}

	windows, err := obs.LatestMetricWindows(ctx, m3Project,
		[]string{"api_p95_latency_ms", "inbox_lag_p95_seconds", "backup_success_rate_percent"},
		notBefore, notAfter)
	require.NoError(t, err)
	byMetric := make(map[string]MetricWindow, len(windows))
	for _, window := range windows {
		byMetric[window.Metric] = window
	}
	require.Len(t, windows, 1, "only the in-range api metric survives with its latest window")

	api := byMetric["api_p95_latency_ms"]
	require.NotNil(t, api.P95)
	assert.Equal(t, 610.0, *api.P95, "the newest window wins")
	assert.Equal(t, int64(30), api.SampleCount)
	assert.True(t, api.WindowStart.Equal(notAfter.Add(-time.Hour)), "the same instant regardless of driver timezone")

	_, absent := byMetric["backup_success_rate_percent"]
	assert.False(t, absent, "a metric with no windows is absent, not zero")

	// Empty guard: an inverted range refuses instead of scanning.
	_, err = obs.LatestMetricWindows(ctx, m3Project, []string{"api_p95_latency_ms"}, notAfter, notBefore)
	require.ErrorIs(t, err, ErrTelemetryWindowRejected)

	// End to end: the measurement feeds the pure evaluator; the absent
	// backup-rate metric classifies no_data with its wire-required 0.
	policy := slo.Policy{
		Availability: slo.AvailabilityPolicy{AtRiskErrorBudgetRemainingPercent: 25},
		Objectives: []slo.ObjectivePolicy{
			{Kind: slo.ObjectiveAPIP95LatencyMS, Target: 500, Unit: "ms", LowerIsBetter: true,
				AtRiskThreshold: 400, RunbookRef: "runbooks/webhook-pipeline-failure"},
			{Kind: slo.ObjectiveBackupSuccessRatePercent, Target: 100, Unit: "percent", LowerIsBetter: false,
				AtRiskThreshold: 100, RunbookRef: "runbooks/database-backup-restore"},
		},
	}
	inputs := []slo.ObjectiveInput{
		{Kind: slo.ObjectiveAPIP95LatencyMS, Measured: api.P95, Since: api.WindowStart.Format(time.RFC3339)},
	}
	snapshot, err := slo.Evaluate(policy, slo.AvailabilityInput{Success: 9955, Total: 10000},
		inputs, slo.Window{Kind: slo.WindowRolling30D, From: notBefore.Format(time.RFC3339), To: notAfter.Format(time.RFC3339)},
		slo.DegradationInput{}, notAfter.Format(time.RFC3339))
	require.NoError(t, err)

	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	assert.Equal(t, "3.0", wire["schema_version"])

	require.Len(t, snapshot.Objectives, 2)
	apiEntry, backupEntry := snapshot.Objectives[0], snapshot.Objectives[1]
	assert.Equal(t, slo.StateBreached, apiEntry.State, "610ms crosses the 500ms target")
	require.NotNil(t, apiEntry.Alert)
	assert.Equal(t, slo.SeverityCritical, apiEntry.Alert.Severity)
	assert.Equal(t, slo.StateNoData, backupEntry.State)
	assert.Equal(t, float64(0), backupEntry.Measured)
}
