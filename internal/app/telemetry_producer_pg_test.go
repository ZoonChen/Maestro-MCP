package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/config"
	"github.com/ZoonChen/Maestro-MCP/internal/handler"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// PG-gated M4-OBS-001 producer integration (F2/F3/F4): real
// telemetry_aggregates upserts with window convergence, the webhook
// inbox/DLQ/outbox depth query over injected rows (G2), and the full
// 埋点 → producer → slo-snapshot flow returning measured (non-no_data)
// api_p95 and availability.

const (
	telTeamID     = "018f7e00-0000-7000-8000-00000000f001"
	telProjectID  = "018f7e00-0000-7000-8000-00000000f002"
	telPlatformID = "018f7e00-0000-7000-8000-00000000f003"
	telInstanceID = "018f7e00-0000-7000-8000-00000000f004"
)

func TestTelemetryProducerEndToEndPostgres(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_telemetry_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_telemetry_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_telemetry_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := store.OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_telemetry_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'telemetry')`, telTeamID)
	require.NoError(t, err)
	for _, project := range []struct{ id, key string }{
		{telProjectID, "tel"}, {telPlatformID, "tel-platform"},
	} {
		_, err = db.ExecContext(ctx,
			`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, $3, 'TEL', 'active')`,
			project.id, telTeamID, project.key)
		require.NoError(t, err)
	}

	// G2 fixture: one dead-lettered inbox row and one stuck backlog row
	// behind a real GitLab instance.
	_, err = db.ExecContext(ctx, `INSERT INTO gitlab_instances
		(id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ($1, 'https://gitlab.tel.test', 'TEL instance', 'env:BOT', 'env:HOOK')`,
		telInstanceID)
	require.NoError(t, err)
	insertInbox := func(id, externalID, status string, receivedAt time.Time) {
		_, err = db.ExecContext(ctx, `INSERT INTO webhook_inbox
			(id, gitlab_instance_id, external_event_id, event_kind, payload_digest, received_at, status)
			VALUES ($1, $2, $3, 'push', $4, $5, $6)`,
			id, telInstanceID, externalID,
			"sha256:"+strings.Repeat("1", 64), receivedAt, status)
		require.NoError(t, err)
	}
	asOf := time.Now().UTC()
	insertInbox("018f7e00-0000-7000-8000-00000000f011", "evt-dead", "dead_letter", asOf.Add(-2*time.Hour))
	insertInbox("018f7e00-0000-7000-8000-00000000f012", "evt-stuck", "received", asOf.Add(-90*time.Second))

	// Depth query (F3 read side): backlog, DLQ and aging percentiles.
	depths, err := pg.Observability().PlatformDepths(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, depths.InboxBacklog)
	assert.EqualValues(t, 1, depths.DLQDepth)
	require.NotNil(t, depths.InboxLagP95Seconds)
	assert.InDelta(t, 90.0, *depths.InboxLagP95Seconds, 5.0, "the stuck row ages ~90s")

	// Producer over the real store faces with a captured logger.
	sampler := handler.NewRequestSampler(0, 0)
	logs := &bytes.Buffer{}
	producer := newTelemetryProducer(TelemetryProducerOptions{
		Sampler:           sampler,
		Store:             pg.Observability(),
		Depths:            pg.Observability(),
		Interval:          time.Minute,
		RedactionVersion:  "telemetry-v3",
		PlatformProjectID: telPlatformID,
		Logger:            slog.New(slog.NewTextHandler(logs, nil)),
	})

	// Three healthy project-scoped requests: availability stays 100% and
	// the latency objective is measurable.
	for range 3 {
		sampler.Observe(telProjectID, "/api/v1/projects/:id/tasks", http.MethodGet, 200, 10*time.Millisecond)
	}

	now := asOf.Truncate(time.Minute).Add(time.Minute)
	require.NoError(t, producer.Flush(ctx, now))
	// F2 window convergence: re-flushing the same window converges on
	// the same rows instead of duplicating them.
	require.NoError(t, producer.Flush(ctx, now))

	var buckets, samples int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(sample_count), 0) FROM telemetry_aggregates
		WHERE project_id = $1 AND metric = 'http_requests_total'`,
		telProjectID).Scan(&buckets, &samples))
	assert.EqualValues(t, 1, buckets, "re-reported windows converge to one row")
	assert.EqualValues(t, 3, samples, "the upsert wins, no double count")

	windows, err := pg.Observability().LatestMetricWindows(ctx, telProjectID,
		[]string{"api_p95_latency_ms"}, now.Add(-time.Minute), now.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, windows, 1)
	require.NotNil(t, windows[0].P95)
	assert.InDelta(t, 10.0, *windows[0].P95, 1e-9)

	// G2 evidence: the DLQ depth lands as a platform telemetry window,
	// the alert fires with its runbook reference, and the latest depth
	// snapshot (the /api/v1/metrics face) carries it.
	var dlqRows int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM telemetry_aggregates
		WHERE project_id = $1 AND metric = 'webhook_dlq_depth'`, telPlatformID).Scan(&dlqRows))
	assert.EqualValues(t, 1, dlqRows)
	assert.Contains(t, logs.String(), `alert=webhook_dlq_depth`)
	assert.Contains(t, logs.String(), `severity=critical`)
	assert.Contains(t, logs.String(), `runbook_ref=runbooks/webhook-pipeline-failure`)
	assert.Contains(t, logs.String(), `alert=inbox_lag_p95_seconds`, "the 90s backlog crosses the 30s lag threshold")
	assert.EqualValues(t, 1, producer.LatestPlatformDepths().DLQDepth)

	// F4: the maestro.yaml.example SLO policy reads the producer's
	// canonical metric names and answers with measured values.
	sloHandler, err := handler.NewSLOSnapshotHandler(
		&config.SLOConfig{
			Window: "rolling_30d",
			Availability: config.SLOAvailabilityConfig{
				SuccessMetric:                     MetricHTTPRequestsSuccess,
				TotalMetric:                       MetricHTTPRequestsTotal,
				AtRiskErrorBudgetRemainingPercent: 25,
			},
			Objectives: []config.SLOObjectiveConfig{{
				Kind: "api_p95_latency_ms", Metric: MetricAPIP95LatencyMS,
				Target: 500, AtRiskThreshold: 400, Unit: "ms",
				LowerIsBetter: true, RunbookRef: "runbooks/webhook-pipeline-failure",
			}},
		},
		config.BackupConfig{}, pg.Observability(), pg.Reliability())
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet,
		"/api/v3/projects/"+telProjectID+"/slo-snapshot", nil)
	ginContext.Params = gin.Params{{Key: "pid", Value: telProjectID}}
	sloHandler.GetSLOSnapshot(ginContext)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var snapshot struct {
		Availability struct {
			MeasuredPercent float64 `json:"measured_percent"`
			State           string  `json:"state"`
		} `json:"availability"`
		Objectives []struct {
			Kind     string  `json:"kind"`
			State    string  `json:"state"`
			Measured float64 `json:"measured"`
		} `json:"objectives"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &snapshot))
	assert.InDelta(t, 100.0, snapshot.Availability.MeasuredPercent, 1e-6)
	assert.Equal(t, "healthy", snapshot.Availability.State)
	require.Len(t, snapshot.Objectives, 1)
	assert.Equal(t, "api_p95_latency_ms", snapshot.Objectives[0].Kind)
	assert.NotEqual(t, "no_data", snapshot.Objectives[0].State,
		"the producer-fed window makes the declared objective measurable")
	assert.Equal(t, "healthy", snapshot.Objectives[0].State)
	assert.InDelta(t, 10.0, snapshot.Objectives[0].Measured, 1e-6)
}
