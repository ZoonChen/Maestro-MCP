package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// PG-gated M4-PILOT-001 rollout-flag endpoints: the frozen pilot wire
// over the guarded lifecycle, RBAC on pilot.read/pilot.write (platform
// admin writes, project roles read), and the pilot.decision.recorded
// audit trail.

const (
	pilotTeamID    = "018f7e00-0000-7000-8000-00000000b101"
	pilotProjectID = "018f7e00-0000-7000-8000-00000000b102"
)

func TestPilotFlagEndpoints(t *testing.T) {
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
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'pilot handler') ON CONFLICT DO NOTHING`, pilotTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'pilot-h', 'PILOT-H', 'active') ON CONFLICT DO NOTHING`, pilotProjectID, pilotTeamID)
	require.NoError(t, err)

	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	idp := newHandlerTestIdP(t)
	resolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"plat-1": {pilotProjectID: "platform_admin"},
		"pa-1":   {pilotProjectID: "project_admin"},
		"dev-1":  {pilotProjectID: "developer"},
	}}
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, resolver)

	router := gin.New()
	router.Use(mw.Authenticate)
	RegisterControlPlane(router, ControlPlaneOptions{
		Identity: mw, Pilot: NewPilotHandler(pg.Pilot()), Scope: pg.Instances(),
	})
	platTK, paTK, devTK := idp.signedToken(t, "plat-1"), idp.signedToken(t, "pa-1"), idp.signedToken(t, "dev-1")

	request := func(token, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if method == http.MethodPut {
			req.Header.Set("Idempotency-Key", "pilot-key")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	put := func(token, flag, body string) *httptest.ResponseRecorder {
		return request(token, http.MethodPut, "/api/v3/projects/"+pilotProjectID+"/pilot-flags/"+flag, body)
	}
	auditCount := func() int {
		var count int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_events WHERE project_id = $1 AND action = 'pilot.decision.recorded'`,
			pilotProjectID).Scan(&count))
		return count
	}

	// The guarded walk over the wire: register → shadow → gray → full →
	// rolled_back.
	walk := []struct {
		body    string
		status  int
		stage   string
		percent int
	}{
		{`{"stage":"off","reason":"register agent autofix for pilot"}`, http.StatusCreated, "off", 0},
		{`{"stage":"shadow","reason":"shadow phase over the two repos"}`, http.StatusOK, "shadow", 0},
		{`{"stage":"gray","gray_percent":25,"reason":"quarter of the pilot cohort first"}`, http.StatusOK, "gray", 25},
		{`{"stage":"gray","gray_percent":60,"reason":"raise exposure after clean run"}`, http.StatusOK, "gray", 60},
		{`{"stage":"full","reason":"grayscale converged, full exposure"}`, http.StatusOK, "full", 0},
		{`{"stage":"rolled_back","reason":"kill switch after budget overrun"}`, http.StatusOK, "rolled_back", 0},
	}
	for index, step := range walk {
		rec := put(platTK, "agent_autofix", step.body)
		require.Equal(t, step.status, rec.Code, "step %d: %s", index, rec.Body.String())
		var flag struct {
			Stage       string `json:"stage"`
			GrayPercent int    `json:"gray_percent"`
			ChangedBy   string `json:"changed_by"`
			Reason      string `json:"reason"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &flag))
		assert.Equal(t, step.stage, flag.Stage)
		assert.Equal(t, step.percent, flag.GrayPercent)
		assert.True(t, strings.HasSuffix(flag.ChangedBy, "/plat-1"),
			"actor comes from the server-side principal, got %q", flag.ChangedBy)
		assert.Equal(t, index+1, auditCount(), "step %d audits in lockstep", index)
	}

	// The list wire.
	rec := request(platTK, http.MethodGet, "/api/v3/projects/"+pilotProjectID+"/pilot-flags", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var listed struct {
		ProjectID string           `json:"project_id"`
		Flags     []map[string]any `json:"flags"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	assert.Equal(t, pilotProjectID, listed.ProjectID)
	require.Len(t, listed.Flags, 1)
	assert.Equal(t, "rolled_back", listed.Flags[0]["stage"])
	assert.Equal(t, float64(0), listed.Flags[0]["gray_percent"], "non-gray stages carry zero")

	// Illegal transitions are 409s and audit nothing.
	before := auditCount()
	negatives := []struct {
		flag, body string
		code       int
		codeName   string
	}{
		{"agent_autofix", `{"stage":"shadow","reason":"restart after terminal rollback"}`, http.StatusConflict, "PILOT_TRANSITION_INVALID"},
		{"skip_shadow", `{"stage":"gray","gray_percent":25,"reason":"gray cannot open a lifecycle"}`, http.StatusConflict, "PILOT_TRANSITION_INVALID"},
		{"bad_fields", `{"stage":"paused","reason":"unknown stage must be rejected"}`, http.StatusUnprocessableEntity, "PILOT_DECISION_INVALID"},
		{"bad_fields", `{"stage":"shadow","reason":"too short"}`, http.StatusUnprocessableEntity, "PILOT_DECISION_INVALID"},
		{"bad_fields", `{"stage":"shadow","gray_percent":10,"reason":"percent only belongs to gray"}`, http.StatusUnprocessableEntity, "PILOT_DECISION_INVALID"},
		{"bad_fields", `{"stage":"shadow"}`, http.StatusBadRequest, "PILOT_DECISION_INVALID"},
	}
	for _, negative := range negatives {
		rec := put(platTK, negative.flag, negative.body)
		assert.Equal(t, negative.code, rec.Code, "%s: %s", negative.flag, negative.body)
		assert.Contains(t, rec.Body.String(), negative.codeName)
	}
	assert.Equal(t, before, auditCount(), "rejected decisions never audit")

	// Missing idempotency key on the write path.
	req := httptest.NewRequest(http.MethodPut,
		"/api/v3/projects/"+pilotProjectID+"/pilot-flags/idem", strings.NewReader(`{"stage":"off","reason":"missing idempotency key"}`))
	req.Header.Set("Authorization", "Bearer "+platTK)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	// Identical replays are idempotent no-ops: nothing re-audited.
	before = auditCount()
	rec = put(platTK, "agent_autofix", `{"stage":"rolled_back","reason":"kill switch after budget overrun"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"stage":"rolled_back"`)
	assert.Equal(t, before, auditCount(), "no-op replays never audit")

	// RBAC: project roles read, only platform_admin writes.
	rec = request(paTK, http.MethodGet, "/api/v3/projects/"+pilotProjectID+"/pilot-flags", "")
	assert.Equal(t, http.StatusOK, rec.Code, "project_admin holds pilot.read")
	rec = request(devTK, http.MethodGet, "/api/v3/projects/"+pilotProjectID+"/pilot-flags", "")
	assert.Equal(t, http.StatusOK, rec.Code, "developer holds pilot.read")
	rec = put(paTK, "pa_write", `{"stage":"off","reason":"project admin cannot write flags"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, "project_admin lacks pilot.write")
	rec = put(devTK, "dev_write", `{"stage":"off","reason":"developer cannot write flags"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, "developer lacks pilot.write")

	// Anonymous stays out and unknown scopes hide as 404.
	rec = request("", http.MethodGet, "/api/v3/projects/"+pilotProjectID+"/pilot-flags", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = request(platTK, http.MethodGet, "/api/v3/projects/018f7e00-0000-7000-8000-00000000ffff/pilot-flags", "")
	assert.Equal(t, http.StatusNotFound, rec.Code, "unknown scopes hide as 404")

	// The audit trail carries actor, flag and reason verbatim.
	var actor, reason, resource string
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT actor_principal, reason, resource_type || ':' || resource_id
		FROM audit_events WHERE project_id = $1 AND action = 'pilot.decision.recorded'
		ORDER BY id DESC LIMIT 1`, pilotProjectID).Scan(&actor, &reason, &resource))
	assert.True(t, strings.HasSuffix(actor, "/plat-1"), "audit actor is the server-side principal, got %q", actor)
	assert.Equal(t, "kill switch after budget overrun", reason)
	assert.Equal(t, "pilot_flag:agent_autofix", resource)
}
