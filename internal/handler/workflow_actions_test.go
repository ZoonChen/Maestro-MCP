package handler

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
)

// W6-3 (S2C-A3): the governance execution surface. The S2B development
// slice's four store-face operations — asset signoff, gate binding,
// claim, execution completion — replay through the real /api/v3 tree
// with the frozen permission map deciding who may act.

const (
	w6FlowProjectID = "018f7900-0000-7000-8000-000000000021"
	w6FlowItemID    = "018f7900-0000-7000-8000-000000000022"
	w6FlowRunnerID  = "018f7900-0000-7000-8000-000000000023"
)

type workflowActionsFixture struct {
	router  *gin.Engine
	pg      *store.PostgresStore
	db      *sql.DB
	devTK   string
	coordTK string
	tlTK    string
	viewTK  string
}

func newWorkflowActionsFixture(t *testing.T) *workflowActionsFixture {
	t.Helper()
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
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'w6 flow team')`, qTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'w6-flow', 'W6 Flow', 'active')`,
		w6FlowProjectID, qTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO work_items (id, project_id, title, status, version) VALUES ($1, $2, 'w6 flow item', 'queued', 1)`,
		w6FlowItemID, w6FlowProjectID)
	require.NoError(t, err)
	// A dedicated claim runner: approved and bound, the S2B shape.
	_, err = db.ExecContext(ctx, `
		INSERT INTO runners (id, display_name, device_key_hash, status)
		VALUES ($1, 'w6 claim runner', 'x', 'approved')`, w6FlowRunnerID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO runner_bindings (project_id, runner_id) VALUES ($1, $2)`, w6FlowProjectID, w6FlowRunnerID)
	require.NoError(t, err)

	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	idp := newHandlerTestIdP(t)
	resolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"dev-1":    {w6FlowProjectID: "developer"},
		"coord-1":  {w6FlowProjectID: "coordinator"},
		"tl-1":     {w6FlowProjectID: "viewer"},
		"viewer-1": {w6FlowProjectID: "viewer"},
	}, Functional: map[string][]string{
		"tl-1": {"technical_lead"},
	}}
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, resolver)

	router := gin.New()
	router.Use(mw.Authenticate)
	RegisterControlPlane(router, ControlPlaneOptions{
		Identity: mw,
		Workflow: NewWorkflowActionsHandler(pg, pg.Assets()),
		Scope:    pg.Instances(),
	})
	return &workflowActionsFixture{
		router: router, pg: pg, db: db,
		devTK:   idp.signedToken(t, "dev-1"),
		coordTK: idp.signedToken(t, "coord-1"),
		tlTK:    idp.signedToken(t, "tl-1"),
		viewTK:  idp.signedToken(t, "viewer-1"),
	}
}

func (f *workflowActionsFixture) request(t *testing.T, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	t.Logf("request %s %s -> %d %s", method, path, response.Code, response.Body.String())
	return response
}

// TestWorkflowActionsS2BEquivalence replays the S2B development slice
// through the API: functional signoff → gate binding → claim →
// completion — the four operations the retired temporary driver drove
// through the store face.
func TestWorkflowActionsS2BEquivalence(t *testing.T) {
	f := newWorkflowActionsFixture(t)
	ctx := context.Background()

	// Seed a reviewed detailed-design asset (single-approve contract).
	ledger := f.pg.Assets()
	_, err := ledger.RegisterAsset(ctx, store.Asset{
		AssetID: "ART-detailed-design-601", Version: 1, ProjectID: w6FlowProjectID,
		AssetType: "detailed-design", Title: "W6 详设", Status: store.AssetStatusDraft,
		OwnerPrincipal: "session:w6-owner", Sensitivity: store.SensitivityInternal,
		SourceDigest: "sha256:" + strings.Repeat("12", 32),
	}, "session:w6-owner")
	require.NoError(t, err)
	_, err = ledger.ReviewAsset(ctx, "ART-detailed-design-601", 1, "user:w6-reviewer")
	require.NoError(t, err)

	t.Run("technical_lead signs the asset off over the API", func(t *testing.T) {
		reply := f.request(t, f.tlTK, http.MethodPost,
			"/api/v3/projects/"+w6FlowProjectID+"/assets/ART-detailed-design-601/versions/1/approve", "")
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), `"status":"approved"`)
	})

	t.Run("coordinator binds the gate to the approved version", func(t *testing.T) {
		reply := f.request(t, f.coordTK, http.MethodPost,
			"/api/v3/projects/"+w6FlowProjectID+"/work-items/"+w6FlowItemID+"/gate-bindings",
			`{"asset_id":"ART-detailed-design-601","asset_version":1,"gate_id":"detailed-design"}`)
		require.Equal(t, http.StatusCreated, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), `"status":"bound"`)
	})

	var executionID string
	t.Run("developer claims the queued item", func(t *testing.T) {
		reply := f.request(t, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w6FlowProjectID+"/work-items/claim",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w6-gen-1"}`, w6FlowRunnerID))
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), `"available":true`)
		assert.Contains(t, reply.Body.String(), `"work_item_id":"`+w6FlowItemID+`"`)
		executionID = replyJSONStringField(t, reply.Body.String(), "execution_id")
		require.NotEmpty(t, executionID)
	})

	t.Run("developer completes the execution with its commit", func(t *testing.T) {
		reply := f.request(t, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w6FlowProjectID+"/executions/"+executionID+"/complete",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w6-gen-1","outcome":"completed","commit_sha":"%s"}`,
				w6FlowRunnerID, strings.Repeat("9c", 20)))
		require.Equal(t, http.StatusAccepted, reply.Code, reply.Body.String())

		var status string
		require.NoError(t, f.db.QueryRow(
			`SELECT status FROM work_items WHERE id = $1`, w6FlowItemID).Scan(&status))
		assert.Equal(t, "validating", status, "the terminal mapping lands validating; W6-1 owns the rest of the chain")
	})
}

func TestWorkflowActionsPermissionDenials(t *testing.T) {
	f := newWorkflowActionsFixture(t)

	t.Run("claim is developer-plane", func(t *testing.T) {
		reply := f.request(t, f.viewTK, http.MethodPost,
			"/api/v3/projects/"+w6FlowProjectID+"/work-items/claim",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"g"}`, w6FlowRunnerID))
		assert.Equal(t, http.StatusForbidden, reply.Code)
	})

	t.Run("gate binding is propose-plane", func(t *testing.T) {
		reply := f.request(t, f.viewTK, http.MethodPost,
			"/api/v3/projects/"+w6FlowProjectID+"/work-items/"+w6FlowItemID+"/gate-bindings",
			`{"asset_id":"ART-none-001","asset_version":1,"gate_id":"detailed-design"}`)
		assert.Equal(t, http.StatusForbidden, reply.Code)
	})

	t.Run("approve is functional-plane", func(t *testing.T) {
		reply := f.request(t, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w6FlowProjectID+"/assets/ART-none-001/versions/1/approve", "")
		assert.Equal(t, http.StatusForbidden, reply.Code, "project roles never carry asset.approve")
	})

	t.Run("unbound runner never claims", func(t *testing.T) {
		const stranger = "018f7900-0000-7000-8000-000000000099"
		reply := f.request(t, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w6FlowProjectID+"/work-items/claim",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"g"}`, stranger))
		assert.Equal(t, http.StatusForbidden, reply.Code, "cross-scope claims refuse before any lease side effect")
	})

	t.Run("completed execution requires its commit", func(t *testing.T) {
		reply := f.request(t, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w6FlowProjectID+"/executions/018f7900-0000-7000-8000-0000000000f1/complete",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"g","outcome":"completed"}`, w6FlowRunnerID))
		assert.Equal(t, http.StatusBadRequest, reply.Code)
	})
}

func replyJSONStringField(t *testing.T, body, field string) string {
	t.Helper()
	// Minimal JSON field extraction for flat handler replies.
	prefix := `"` + field + `":"`
	start := strings.Index(body, prefix)
	if start < 0 {
		return ""
	}
	rest := body[start+len(prefix):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}
