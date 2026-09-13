package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/config"
	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// W6-6 (S2C-A6) PG-gated: availability window aggregation and the
// stale-snapshot fallback. The pre-W6 endpoint measured the NEWEST
// telemetry window only — a quiet project's single 5xx flipped it
// breached (sample starvation) — and answered a bare 503 UNMEASURED
// when the window had no data, which itself entered the availability
// denominator it was measuring (observer effect).

const (
	w6SloTeamID     = "018f7e00-0000-7000-8000-00000000b201"
	w6BusyProjectID = "018f7e00-0000-7000-8000-00000000b202"
	w6QuietProject  = "018f7e00-0000-7000-8000-00000000b203"
	w6StaleProject  = "018f7e00-0000-7000-8000-00000000b204"
)

func TestSLOSnapshotWindowAggregationAndStaleFallback(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_handler_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_handler_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_handler_test WITH (FORCE)`)
		_ = admin.Close()
	})
	db, err := store.OpenPostgres(context.Background(), testDatabaseDSN(t))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'w6 slo team')`, w6SloTeamID)
	require.NoError(t, err)
	asOf := time.Now().UTC()
	for index, projectID := range []string{w6BusyProjectID, w6QuietProject, w6StaleProject} {
		_, err = db.ExecContext(ctx,
			`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, $3, 'W6 SLO', 'active')`,
			projectID, w6SloTeamID, "w6-slo-"+string(rune('0'+index)))
		require.NoError(t, err)
	}

	obs := pg.Observability()
	point := func(projectID, metric string, start time.Time, sum float64) store.TelemetryPoint {
		return store.TelemetryPoint{ProjectID: projectID, Metric: metric, WindowKind: "1h",
			WindowStart: start.Format(time.RFC3339), SampleCount: int64(sum), Sum: sum,
			RedactionVersion: "redact-v1"}
	}
	// Busy project: healthy history plus a sparse newest window whose
	// single 5xx starved the pre-W6 newest-window read into 50%.
	require.NoError(t, obs.RecordTelemetry(ctx, point(w6BusyProjectID, "http_success", asOf.Add(-8*time.Hour), 1000)))
	require.NoError(t, obs.RecordTelemetry(ctx, point(w6BusyProjectID, "http_total", asOf.Add(-8*time.Hour), 1000)))
	require.NoError(t, obs.RecordTelemetry(ctx, point(w6BusyProjectID, "http_success", asOf.Add(-10*time.Minute), 1)))
	require.NoError(t, obs.RecordTelemetry(ctx, point(w6BusyProjectID, "http_total", asOf.Add(-10*time.Minute), 2)))
	// Quiet project: one sparse window, one failure — even aggregated,
	// 1/2 is genuinely below target; this pins that aggregation does not
	// bury REAL breaches, only starvation artifacts.
	require.NoError(t, obs.RecordTelemetry(ctx, point(w6QuietProject, "http_success", asOf.Add(-10*time.Minute), 1)))
	require.NoError(t, obs.RecordTelemetry(ctx, point(w6QuietProject, "http_total", asOf.Add(-10*time.Minute), 2)))
	// Stale project: telemetry only OUTSIDE the 30-day window.
	require.NoError(t, obs.RecordTelemetry(ctx, point(w6StaleProject, "http_success", asOf.Add(-40*24*time.Hour), 500)))
	require.NoError(t, obs.RecordTelemetry(ctx, point(w6StaleProject, "http_total", asOf.Add(-40*24*time.Hour), 500)))

	sloConfig := &config.SLOConfig{
		Window: "rolling_30d",
		Availability: config.SLOAvailabilityConfig{
			SuccessMetric: "http_success", TotalMetric: "http_total",
			AtRiskErrorBudgetRemainingPercent: 25,
		},
	}
	sloHandler, err := NewSLOSnapshotHandler(sloConfig, config.BackupConfig{},
		pg.Observability(), pg.Observability(), pg.Reliability())
	require.NoError(t, err)

	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	idp := newHandlerTestIdP(t)
	resolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"viewer-1": {w6BusyProjectID: "viewer", w6QuietProject: "viewer", w6StaleProject: "viewer"},
	}}
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, resolver)
	router := gin.New()
	router.Use(mw.Authenticate)
	RegisterControlPlane(router, ControlPlaneOptions{Identity: mw, SLO: sloHandler, Scope: pg.Instances()})
	viewerTK := idp.signedToken(t, "viewer-1")

	get := func(path string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+viewerTK)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		body := map[string]any{}
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		}
		return rec.Code, body
	}

	t.Run("aggregation keeps one quiet-window 5xx from flipping the project", func(t *testing.T) {
		code, snapshot := get("/api/v3/projects/" + w6BusyProjectID + "/slo-snapshot")
		require.Equal(t, http.StatusOK, code)
		availability := snapshot["availability"].(map[string]any)
		assert.InDelta(t, 1001.0/1002.0*100, availability["measured_percent"], 1e-6,
			"the window's ratio, not the newest bucket's 50%%")
		assert.Equal(t, "healthy", availability["state"])
		assert.NotContains(t, snapshot, "stale")
	})

	t.Run("a real breach still breaches", func(t *testing.T) {
		code, snapshot := get("/api/v3/projects/" + w6QuietProject + "/slo-snapshot")
		require.Equal(t, http.StatusOK, code)
		availability := snapshot["availability"].(map[string]any)
		assert.InDelta(t, 50.0, availability["measured_percent"], 1e-6)
		assert.Equal(t, "breached", availability["state"])
	})

	t.Run("no in-window telemetry serves the last valid measurement with the stale marker", func(t *testing.T) {
		code, snapshot := get("/api/v3/projects/" + w6StaleProject + "/slo-snapshot")
		require.Equal(t, http.StatusOK, code, "the observer effect is gone: no bare 503 feeding the denominator")
		assert.Equal(t, true, snapshot["stale"])
		availability := snapshot["availability"].(map[string]any)
		assert.InDelta(t, 100.0, availability["measured_percent"], 1e-6)
		assert.Equal(t, "healthy", availability["state"])
	})
}
