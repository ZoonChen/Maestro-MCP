package handler

import (
	"context"
	"encoding/json"
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
	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// PG-gated M4 SLO snapshot endpoint: the frozen slo-status wire
// evaluated from telemetry aggregates plus the backup ledger, with the
// fail-closed availability refusal and project.read authorization.

const (
	sloTeamID    = "018f7e00-0000-7000-8000-00000000b101"
	sloProjectID = "018f7e00-0000-7000-8000-00000000b102"
	// emptyProjectID has no telemetry: the snapshot must refuse.
	emptyProjectID = "018f7e00-0000-7000-8000-00000000b103"
)

func TestSLOSnapshotEndpoint(t *testing.T) {
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
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'slo handler')`, sloTeamID)
	require.NoError(t, err)
	for index, projectID := range []string{sloProjectID, emptyProjectID} {
		_, err = db.ExecContext(ctx,
			`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, $3, 'SLO', 'active')`,
			projectID, sloTeamID, "slo-"+string(rune('0'+index)))
		require.NoError(t, err)
	}

	// Telemetry: availability SLI counts and one breached latency.
	obs := pg.Observability()
	asOf := time.Now().UTC()
	latency := 610.0
	seed := []store.TelemetryPoint{
		{ProjectID: sloProjectID, Metric: "http_success", WindowKind: "1h",
			WindowStart: asOf.Add(-30 * time.Minute).Format(time.RFC3339),
			SampleCount: 9970, Sum: 9970, RedactionVersion: "redact-v1"},
		{ProjectID: sloProjectID, Metric: "http_total", WindowKind: "1h",
			WindowStart: asOf.Add(-30 * time.Minute).Format(time.RFC3339),
			SampleCount: 10000, Sum: 10000, RedactionVersion: "redact-v1"},
		{ProjectID: sloProjectID, Metric: "api_p95_latency_ms", WindowKind: "1h",
			WindowStart: asOf.Add(-30 * time.Minute).Format(time.RFC3339),
			SampleCount: 100, Sum: 61000, P95: &latency, RedactionVersion: "redact-v1"},
	}
	for _, point := range seed {
		require.NoError(t, obs.RecordTelemetry(ctx, point))
	}

	// Backup ledger: one verified full backup five minutes ago.
	rel := pg.Reliability()
	backupID, err := rel.RecordBackupStart(ctx, "full", asOf.Add(-10*time.Minute))
	require.NoError(t, err)
	finished := asOf.Add(-5 * time.Minute)
	require.NoError(t, rel.CompleteBackup(ctx, backupID, finished, 1<<20,
		"sha256:"+strings.Repeat("a", 64), "env:MAESTRO_BACKUP_TEST"))
	require.NoError(t, rel.RecordRestoreVerification(ctx, backupID, finished.Add(time.Minute),
		"restore-host-test", true, true))

	sloConfig := &config.SLOConfig{
		Window: "rolling_30d",
		Availability: config.SLOAvailabilityConfig{
			SuccessMetric: "http_success", TotalMetric: "http_total",
			AtRiskErrorBudgetRemainingPercent: 25,
		},
		Objectives: []config.SLOObjectiveConfig{{
			Kind: "api_p95_latency_ms", Metric: "api_p95_latency_ms",
			Target: 500, AtRiskThreshold: 400, Unit: "ms",
			LowerIsBetter: true, RunbookRef: "runbooks/webhook-pipeline-failure",
		}},
	}
	backupConfig := config.BackupConfig{
		FullBackupIntervalHours: 24, WALArchive: true, RPOMinutes: 15, RTOMinutes: 240,
	}
	sloHandler, err := NewSLOSnapshotHandler(sloConfig, backupConfig, pg.Observability(), pg.Reliability())
	require.NoError(t, err)

	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	idp := newHandlerTestIdP(t)
	resolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"viewer-1": {sloProjectID: "viewer", emptyProjectID: "viewer"},
	}}
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, resolver)
	router := gin.New()
	router.Use(mw.Authenticate)
	RegisterControlPlane(router, ControlPlaneOptions{
		Identity: mw, SLO: sloHandler, Scope: pg.Instances(),
	})
	viewerTK := idp.signedToken(t, "viewer-1")

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+viewerTK)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/api/v3/projects/" + sloProjectID + "/slo-snapshot")
	require.Equal(t, http.StatusOK, rec.Code)
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
	assert.Equal(t, "3.0", snapshot["schema_version"])
	availability := snapshot["availability"].(map[string]any)
	assert.Equal(t, 99.5, availability["target_percent"])
	assert.InDelta(t, 99.7, availability["measured_percent"], 1e-6)
	assert.Equal(t, "healthy", availability["state"])

	objectives := snapshot["objectives"].([]any)
	require.Len(t, objectives, 4) // declared api + rpo + rto + backup rate
	byKind := map[string]map[string]any{}
	for _, raw := range objectives {
		entry := raw.(map[string]any)
		byKind[entry["kind"].(string)] = entry
	}
	api := byKind["api_p95_latency_ms"]
	assert.Equal(t, "breached", api["state"], "610ms crosses the 500ms target")
	alert := api["alert"].(map[string]any)
	assert.Equal(t, true, alert["firing"])
	assert.Equal(t, "critical", alert["severity"])
	assert.Equal(t, "runbooks/webhook-pipeline-failure", alert["runbook_ref"])

	rpo := byKind["rpo_minutes"]
	assert.Equal(t, "healthy", rpo["state"], "the verified backup is five minutes old")
	assert.InDelta(t, 5.0, rpo["measured"], 2.0)
	backupRate := byKind["backup_success_rate_percent"]
	assert.Equal(t, "healthy", backupRate["state"], "one verified run, zero failures")
	assert.InDelta(t, 100.0, backupRate["measured"], 1e-6)
	rto := byKind["rto_minutes"]
	assert.Equal(t, "no_data", rto["state"], "RTO is drill-measured, not runtime-measured")

	// Missing availability telemetry refuses instead of inventing 100%.
	rec = get("/api/v3/projects/" + emptyProjectID + "/slo-snapshot")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "SLO_AVAILABILITY_UNMEASURED")
}
