package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated end-to-end tests for the frozen Quality endpoints through the
// real OIDC authorize tree: policy read/strengthen, gate and evidence
// reads, and the waiver lifecycle with the frozen permission matrix.

const (
	qProjectID  = "018f7500-0000-7000-8000-000000000002"
	qWorkItemID = "018f7500-0000-7000-8000-000000000003"
	qTeamID     = "018f7500-0000-7000-8000-000000000001"
)

type qualityFixture struct {
	router   *gin.Engine
	db       *sql.DB
	pg       *store.PostgresStore
	quality  *QualityHandler
	adminTK  string
	viewerTK string
	devTK    string
	platTK   string
}

func newQualityFixture(t *testing.T) *qualityFixture {
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
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'quality e2e team')`, qTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'q-e2e', 'Q E2E', 'active')`, qProjectID, qTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO work_items (id, project_id, title) VALUES ($1, $2, 'quality work item')`, qWorkItemID, qProjectID)
	require.NoError(t, err)

	quality, err := NewQualityHandler(pg.Quality())
	require.NoError(t, err)

	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	idp := newHandlerTestIdP(t)
	resolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"admin-1":  {qProjectID: "project_admin"},
		"viewer-1": {qProjectID: "viewer"},
		"dev-1":    {qProjectID: "developer"},
		"plat-1":   {qProjectID: "platform_admin"},
	}}
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, resolver)

	// The control-plane tree carries its own authentication and the
	// frozen permission map under /api/v3.
	router := gin.New()
	router.Use(mw.Authenticate)
	RegisterControlPlane(router, ControlPlaneOptions{
		Identity: mw, Quality: quality,
		GitLab: NewGitLabHandler(pg.Instances()), Scope: pg.Instances(),
	})
	return &qualityFixture{
		router: router, db: db, pg: pg, quality: quality,
		adminTK:  idp.signedToken(t, "admin-1"),
		viewerTK: idp.signedToken(t, "viewer-1"),
		devTK:    idp.signedToken(t, "dev-1"),
		platTK:   idp.signedToken(t, "plat-1"),
	}
}

func (f *qualityFixture) request(t *testing.T, token, method, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	t.Logf("request %s %s -> %d %s", method, path, response.Code, response.Body.String())
	return response
}

func strengtheningOverlayJSON(id, semver string, strengthen bool) string {
	severities := `["critical","high"]`
	floor := 80.0
	denylist := `["AGPL-3.0-only"]` // subset of the company denylist: weakening
	if strengthen {
		severities = `["critical","high","medium"]`
		floor = 85
		denylist = `["AGPL-3.0-only","AGPL-3.0-or-later"]`
	}
	return fmt.Sprintf(`{
		"id": %q, "version": %q, "scope": "project", "extends": "company-baseline",
		"required_gates": ["baseline_freshness","boundary","policy_integrity","build","unit","lint_typecheck","coverage","secret_scan","sast","dependency","image","license"],
		"coverage": {"changed_lines_min_percent": %v, "max_total_drop_points": 0.5},
		"security": {"block_severities": %s, "license_denylist": %s},
		"flaky_retry_count": 1,
		"waiver": {"max_days": 7, "requires_distinct_approver": true,
			"non_waivable_gates": ["identity_isolation","sha_integrity","policy_integrity","webhook_authenticity"]}
	}`, id, semver, floor, severities, denylist)
}

func TestQualityPolicyEndpoints(t *testing.T) {
	f := newQualityFixture(t)

	t.Run("without overlay the company baseline is effective", func(t *testing.T) {
		response := f.request(t, f.viewerTK, http.MethodGet, "/api/v3/projects/"+qProjectID+"/quality-policy", nil, "")
		require.Equal(t, http.StatusOK, response.Code)
		assert.Contains(t, response.Body.String(), `"company-baseline"`)
		assert.NotContains(t, response.Body.String(), "ETag")
	})

	t.Run("viewer cannot strengthen", func(t *testing.T) {
		response := f.request(t, f.viewerTK, http.MethodPut, "/api/v3/projects/"+qProjectID+"/quality-policy",
			map[string]string{"If-None-Match": "*", "Idempotency-Key": "k1"}, strengtheningOverlayJSON("acme", "3.0.0", true))
		assert.Equal(t, http.StatusForbidden, response.Code)
	})

	t.Run("create, weaken, conflict and replace", func(t *testing.T) {
		base := "/api/v3/projects/" + qProjectID + "/quality-policy"

		create := f.request(t, f.adminTK, http.MethodPut, base,
			map[string]string{"If-None-Match": "*", "Idempotency-Key": "k2"}, strengtheningOverlayJSON("acme", "3.0.0", true))
		require.Equal(t, http.StatusCreated, create.Code, create.Body.String())
		assert.Equal(t, `"1"`, create.Header().Get("ETag"))

		// Weakening answers 422 and never touches the stored row.
		weaken := f.request(t, f.adminTK, http.MethodPut, base,
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "k3"}, strengtheningOverlayJSON("acme", "3.1.0", false))
		assert.Equal(t, http.StatusUnprocessableEntity, weaken.Code)
		assert.Contains(t, weaken.Body.String(), "POLICY_WEAKENED")

		// Missing preconditions answer 428.
		bare := f.request(t, f.adminTK, http.MethodPut, base,
			map[string]string{"Idempotency-Key": "k4"}, strengtheningOverlayJSON("acme", "3.1.0", true))
		assert.Equal(t, http.StatusPreconditionRequired, bare.Code)

		// Wrong version answers 412.
		stale := f.request(t, f.adminTK, http.MethodPut, base,
			map[string]string{"If-Match": `"9"`, "Idempotency-Key": "k5"}, strengtheningOverlayJSON("acme", "3.1.0", true))
		assert.Equal(t, http.StatusPreconditionFailed, stale.Code)

		// Identical content under the right version is an idempotent
		// replace (200, version bumps).
		replay := f.request(t, f.adminTK, http.MethodPut, base,
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "k6"}, strengtheningOverlayJSON("acme", "3.0.0", true))
		require.Equal(t, http.StatusOK, replay.Code)
		assert.Equal(t, `"2"`, replay.Header().Get("ETag"))

		// Same semver with different content answers 409.
		conflict := f.request(t, f.adminTK, http.MethodPut, base,
			map[string]string{"If-Match": `"2"`, "Idempotency-Key": "k7"}, strengtheningOverlayJSON("other-id", "3.0.0", true))
		assert.Equal(t, http.StatusConflict, conflict.Code)

		// A right-version replace strengthens further.
		replace := f.request(t, f.adminTK, http.MethodPut, base,
			map[string]string{"If-Match": `"2"`, "Idempotency-Key": "k8"}, strengtheningOverlayJSON("acme", "3.1.0", true))
		require.Equal(t, http.StatusOK, replace.Code, replace.Body.String())
		assert.Equal(t, `"3"`, replace.Header().Get("ETag"))

		get := f.request(t, f.viewerTK, http.MethodGet, base, nil, "")
		require.Equal(t, http.StatusOK, get.Code)
		assert.Equal(t, `"3"`, get.Header().Get("ETag"))
		assert.Contains(t, get.Body.String(), "3.1.0")
	})
}

// seedVerdictWithGate persists a passing verdict so a real gate snapshot
// exists to waive against, and returns one gate's snapshot identity.
func seedVerdictWithGate(t *testing.T, f *qualityFixture) evidence.StoredSnapshot {
	t.Helper()
	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	resolved, err := evidence.ResolveEffective(company, nil)
	require.NoError(t, err)

	tuple := evidence.Tuple{
		ProjectID: qProjectID, WorkItemID: qWorkItemID,
		SourceSHA: strings.Repeat("5", 40), TargetSHA: strings.Repeat("6", 40),
		PolicyVersion: "3.0.0",
	}
	pipeline, job := int64(30), int64(300)
	records := []evidence.Record{}
	for index, check := range resolved.Policy.RequiredGates {
		record := evidence.Record{
			EvidenceID: fmt.Sprintf("018f7600-0000-7000-8000-%012d", 200+index),
			ProjectID:  qProjectID, WorkItemID: qWorkItemID, Kind: check,
			Authority: evidence.AuthorityMergeGate, Status: evidence.EvidencePassed,
			SourceSHA: tuple.SourceSHA, TargetSHA: tuple.TargetSHA,
			PipelineID: &pipeline, JobID: &job, PolicyVersion: "3.0.0", Attempt: 1,
			Producer: evidence.Producer{Type: "gitlab_job", ID: "ci", Version: "1.0"},
		}
		require.NoError(t, f.pg.Quality().AppendEvidence(context.Background(), &record))
		records = append(records, record)
	}
	verdict, err := evidence.Evaluate(tuple, resolved, records, nil, time.Now())
	require.NoError(t, err)
	require.True(t, verdict.Ready)
	require.NoError(t, f.pg.Quality().PersistVerdict(context.Background(), verdict))

	snapshots, err := f.pg.Quality().ListGateSnapshots(context.Background(), qProjectID, qWorkItemID)
	require.NoError(t, err)
	require.Len(t, snapshots, 12)
	for _, snapshot := range snapshots {
		if snapshot.Check == evidence.GateUnit {
			return snapshot
		}
	}
	t.Fatal("unit gate snapshot missing")
	return evidence.StoredSnapshot{}
}

func TestQualityGatesAndEvidenceReads(t *testing.T) {
	f := newQualityFixture(t)
	seedVerdictWithGate(t, f)

	gates := f.request(t, f.devTK, http.MethodGet,
		"/api/v3/projects/"+qProjectID+"/work-items/"+qWorkItemID+"/gates", nil, "")
	require.Equal(t, http.StatusOK, gates.Code)
	assert.Contains(t, gates.Body.String(), `"passed"`)
	assert.Contains(t, gates.Body.String(), `"lint_typecheck"`)

	evidenceList := f.request(t, f.devTK, http.MethodGet,
		"/api/v3/projects/"+qProjectID+"/work-items/"+qWorkItemID+"/evidence", nil, "")
	require.Equal(t, http.StatusOK, evidenceList.Code)
	assert.Contains(t, evidenceList.Body.String(), `"merge_gate"`)
	assert.Contains(t, evidenceList.Body.String(), `"schema_version":"3.0"`)

	unknown := f.request(t, f.devTK, http.MethodGet,
		"/api/v3/projects/"+qProjectID+"/work-items/018f7500-0000-7000-8000-00000000dead/gates", nil, "")
	assert.Equal(t, http.StatusNotFound, unknown.Code)
}

func TestQualityWaiverLifecycle(t *testing.T) {
	f := newQualityFixture(t)
	gate := seedVerdictWithGate(t, f)

	waiverPath := "/api/v3/projects/" + qProjectID + "/gates/" + gate.GateID + "/waivers"
	body := fmt.Sprintf(`{"source_sha": %q, "merge_request_iid": 7, "check": %q,
		"reason": "documented infra flake ticket-777", "expires_at": %q}`,
		gate.SourceSHA, gate.Check, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))

	t.Run("developer cannot request waivers", func(t *testing.T) {
		response := f.request(t, f.devTK, http.MethodPost, waiverPath,
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "w0"}, body)
		assert.Equal(t, http.StatusForbidden, response.Code)
	})

	t.Run("project admin requests with gate version and tuple binding", func(t *testing.T) {
		missing := f.request(t, f.adminTK, http.MethodPost, waiverPath,
			map[string]string{"Idempotency-Key": "w1"}, body)
		assert.Equal(t, http.StatusPreconditionRequired, missing.Code)

		stale := f.request(t, f.adminTK, http.MethodPost, waiverPath,
			map[string]string{"If-Match": `"9"`, "Idempotency-Key": "w2"}, body)
		assert.Equal(t, http.StatusPreconditionFailed, stale.Code)

		drifted := f.request(t, f.adminTK, http.MethodPost, waiverPath,
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "w3"},
			strings.Replace(body, gate.SourceSHA, strings.Repeat("9", 40), 1))
		assert.Equal(t, http.StatusUnprocessableEntity, drifted.Code)

		created := f.request(t, f.adminTK, http.MethodPost, waiverPath,
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "w4"}, body)
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
		assert.Contains(t, created.Body.String(), `"requested"`)

		duplicate := f.request(t, f.adminTK, http.MethodPost, waiverPath,
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "w5"}, body)
		assert.Equal(t, http.StatusConflict, duplicate.Code)
	})

	t.Run("approval is held by functional approvers only", func(t *testing.T) {
		waivers, err := f.pg.Quality().ListWaiversForWorkItem(context.Background(), qProjectID, qWorkItemID)
		require.NoError(t, err)
		require.Len(t, waivers, 1)
		waiver := waivers[0]

		// The frozen matrix grants waiver.approve to no project role, so
		// even the project admin is denied — the honest fail-closed
		// decision until functional-role identity lands.
		response := f.request(t, f.adminTK, http.MethodPost,
			"/api/v3/projects/"+qProjectID+"/waivers/"+waiver.ID+"/approve",
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "a1"},
			`{"reason": "independent security review complete"}`)
		assert.Equal(t, http.StatusForbidden, response.Code)
	})

	t.Run("revocation works for the project admin", func(t *testing.T) {
		waivers, err := f.pg.Quality().ListWaiversForWorkItem(context.Background(), qProjectID, qWorkItemID)
		require.NoError(t, err)
		waiver := waivers[0]

		missing := f.request(t, f.adminTK, http.MethodPost,
			"/api/v3/projects/"+qProjectID+"/waivers/"+waiver.ID+"/revoke",
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "r1"}, `{"reason": "short"}`)
		assert.Equal(t, http.StatusBadRequest, missing.Code) // reason too short

		revoked := f.request(t, f.adminTK, http.MethodPost,
			"/api/v3/projects/"+qProjectID+"/waivers/"+waiver.ID+"/revoke",
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "r2"}, `{"reason": "superseded by the fix"}`)
		require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())
		assert.Contains(t, revoked.Body.String(), `"revoked"`)
	})
}

func TestGitLabRegistryEndpoints(t *testing.T) {
	f := newQualityFixture(t)

	t.Run("platform listing requires the frozen permission", func(t *testing.T) {
		denied := f.request(t, f.adminTK, http.MethodGet, "/api/v3/gitlab/instances", nil, "")
		assert.Equal(t, http.StatusForbidden, denied.Code, "project roles never list the platform registry")
	})

	t.Run("instance create, duplicate and sanitized list", func(t *testing.T) {
		body := `{"base_url": "https://gitlab.acme.example", "bot_secret_ref": "env:MAESTRO_GITLAB_BOT_A", "webhook_secret_ref": "env:MAESTRO_GITLAB_HOOK_A"}`
		missing := f.request(t, f.platTK, http.MethodPost, "/api/v3/gitlab/instances",
			map[string]string{"Idempotency-Key": "i1"}, body)
		assert.Equal(t, http.StatusPreconditionRequired, missing.Code)

		created := f.request(t, f.platTK, http.MethodPost, "/api/v3/gitlab/instances",
			map[string]string{"Idempotency-Key": "i2", "If-None-Match": "*"}, body)
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
		assert.Contains(t, created.Body.String(), `"https://gitlab.acme.example"`)
		assert.NotContains(t, created.Body.String(), "secret", "sanitized projections never carry secret refs")

		duplicate := f.request(t, f.platTK, http.MethodPost, "/api/v3/gitlab/instances",
			map[string]string{"Idempotency-Key": "i3", "If-None-Match": "*"}, body)
		assert.Equal(t, http.StatusConflict, duplicate.Code)

		ipLiteral := f.request(t, f.platTK, http.MethodPost, "/api/v3/gitlab/instances",
			map[string]string{"Idempotency-Key": "i4", "If-None-Match": "*"},
			`{"base_url": "https://10.0.0.1", "bot_secret_ref": "env:A", "webhook_secret_ref": "env:B"}`)
		assert.Equal(t, http.StatusUnprocessableEntity, ipLiteral.Code)

		httpURL := f.request(t, f.platTK, http.MethodPost, "/api/v3/gitlab/instances",
			map[string]string{"Idempotency-Key": "i5", "If-None-Match": "*"},
			`{"base_url": "http://gitlab.acme.example", "bot_secret_ref": "env:A", "webhook_secret_ref": "env:B"}`)
		assert.Equal(t, http.StatusUnprocessableEntity, httpURL.Code)
	})

	t.Run("mapping lifecycle with CAS", func(t *testing.T) {
		var instance struct {
			ID string `json:"id"`
		}
		created := f.request(t, f.platTK, http.MethodPost, "/api/v3/gitlab/instances",
			map[string]string{"Idempotency-Key": "m0", "If-None-Match": "*"},
			`{"base_url": "https://gitlab.map.example", "bot_secret_ref": "env:A", "webhook_secret_ref": "env:B"}`)
		require.Equal(t, http.StatusCreated, created.Code)
		require.NoError(t, json.Unmarshal(created.Body.Bytes(), &instance))

		absent := f.request(t, f.adminTK, http.MethodGet, "/api/v3/projects/"+qProjectID+"/gitlab-mapping", nil, "")
		assert.Equal(t, http.StatusNotFound, absent.Code)

		body := fmt.Sprintf(`{"gitlab_instance_id": %q, "gitlab_project_numeric_id": 9001, "target_branch": "main"}`, instance.ID)
		put := f.request(t, f.adminTK, http.MethodPut, "/api/v3/projects/"+qProjectID+"/gitlab-mapping",
			map[string]string{"Idempotency-Key": "m1", "If-None-Match": "*"}, body)
		require.Equal(t, http.StatusCreated, put.Code, put.Body.String())
		assert.Equal(t, `"1"`, put.Header().Get("ETag"))

		viewerPut := f.request(t, f.viewerTK, http.MethodPut, "/api/v3/projects/"+qProjectID+"/gitlab-mapping",
			map[string]string{"Idempotency-Key": "m2", "If-None-Match": "*"}, body)
		assert.Equal(t, http.StatusForbidden, viewerPut.Code)

		stale := f.request(t, f.adminTK, http.MethodPut, "/api/v3/projects/"+qProjectID+"/gitlab-mapping",
			map[string]string{"Idempotency-Key": "m3", "If-Match": `"9"`}, body)
		assert.Equal(t, http.StatusPreconditionFailed, stale.Code)

		replaced := f.request(t, f.adminTK, http.MethodPut, "/api/v3/projects/"+qProjectID+"/gitlab-mapping",
			map[string]string{"Idempotency-Key": "m4", "If-Match": `"1"`},
			fmt.Sprintf(`{"gitlab_instance_id": %q, "gitlab_project_numeric_id": 9002, "target_branch": "release"}`, instance.ID))
		require.Equal(t, http.StatusOK, replaced.Code)
		assert.Equal(t, `"2"`, replaced.Header().Get("ETag"))
	})

	t.Run("unknown project scope hides", func(t *testing.T) {
		response := f.request(t, f.adminTK, http.MethodGet,
			"/api/v3/projects/018f7500-0000-7000-8000-00000000dead/gitlab-mapping", nil, "")
		assert.Equal(t, http.StatusNotFound, response.Code)
	})
}
