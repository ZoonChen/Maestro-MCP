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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
	"github.com/gin-gonic/gin"
)

// J4 (DEC-2 terminal state): the console seal routes on
// workgraph.seal — the technical_lead functional plane walks it, the
// qa_owner functional plane (and every project role) is 403, and the
// read family serves every project membership. PG-gated end to end
// through the real OIDC authorize tree.

const j4ProjectID = "018f7500-0000-7000-8000-000000000014"

type workgraphConsoleFixture struct {
	router   *gin.Engine
	pg       *store.PostgresStore
	db       *sql.DB
	viewerTK string
	qaTK     string
	tlTK     string
	tlPID    string
	planID   string
}

func newWorkgraphConsoleFixture(t *testing.T) *workgraphConsoleFixture {
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
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'j4 e2e team')`, qTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'j4-e2e', 'J4 E2E', 'active')`, j4ProjectID, qTeamID)
	require.NoError(t, err)

	// One plan with an APPLIED proposal behind it: the draft revision
	// the HITL seal freezes (the J2b protocol path, reused as seed).
	graph := pg.WorkGraph()
	pattern, err := graph.CreateWorkPattern(ctx, store.WorkPattern{
		ProjectID: j4ProjectID, Name: "j4-seal", Version: 1, Status: "active",
		Body: []byte(`{"slots":["api","impl"]}`), CreatedBy: "tech-lead",
	})
	require.NoError(t, err)
	plan, err := graph.CreateWorkPlan(ctx, store.CreateWorkPlanInput{
		ProjectID: j4ProjectID, Title: "J4 seal plan", HumanCode: "MST-WP-00401",
		RootSpec: []byte(`{"goal":"j4 seal e2e"}`), Actor: "tech-lead",
	})
	require.NoError(t, err)
	root, err := graph.GetWorkNode(ctx, plan.ID, plan.RootNodeID)
	require.NoError(t, err)
	prio := 2
	proposal := workgraph.DecompositionProposal{
		PlanID:               plan.ID,
		WorkPattern:          workgraph.WorkPatternRef{PatternID: pattern.ID, Version: 1},
		ExpectedGraphVersion: plan.GraphVersion,
		Nodes: []workgraph.ProposalNode{{
			LocalID: "api", Parent: "ext:" + root.ID, NodeType: workgraph.NodeTypeItem,
			SlotKey: "api", HumanCode: "MST-WI-00401",
			Spec: workgraph.NodeSpec{
				Title: "J4 API 切片", Repo: "peixun-java",
				AcceptanceCriteria: []string{"seal permission e2e green"},
				BaselineSHA:        strings.Repeat("a1", 20),
				WorkspacePaths:     []string{"src/main/java"},
				OwningCapability:   "backend.java",
				BudgetUnits:        100, Priority: &prio,
				FailurePolicy: workgraph.FailureCollectAll, CancelPolicy: workgraph.CancelNone,
			},
		}},
	}
	record, err := graph.SubmitDecompositionProposal(ctx, store.SubmitDecompositionProposalInput{
		Proposal: proposal, IdempotencyKey: "j4-seed-proposal-0001", ProjectID: j4ProjectID, SubmittedBy: "coordinator-1",
		Limits: workgraph.ProposalLimits{MaxNodes: 10, MaxContainmentDepth: 4, MaxFanOut: 8,
			BudgetCeilingUnits: 10000, Now: time.Now().UTC()},
	})
	require.NoError(t, err)
	require.Equal(t, "applied", record.Status, "the seed proposal must apply so a draft revision exists")

	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	idp := newHandlerTestIdP(t)
	resolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"viewer-1": {j4ProjectID: "viewer"},
		"qa-1":     {j4ProjectID: "viewer"},
		"tl-1":     {j4ProjectID: "viewer"},
	}, Functional: map[string][]string{
		"qa-1": {"qa_owner"},
		"tl-1": {"technical_lead"},
	}}
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, resolver)

	router := gin.New()
	router.Use(mw.Authenticate)
	RegisterControlPlane(router, ControlPlaneOptions{
		Identity:    mw,
		WorkGraph:   NewWorkGraphHandler(pg.WorkGraph(), pg.Assets()),
		Scope:       pg.Instances(),
		DeadLetters: NewDeadLetterHandler(pg.Webhooks()),
	})
	return &workgraphConsoleFixture{
		router: router, pg: pg, db: db,
		viewerTK: idp.signedToken(t, "viewer-1"),
		qaTK:     idp.signedToken(t, "qa-1"),
		tlTK:     idp.signedToken(t, "tl-1"),
		tlPID:    idp.server.URL + "/tl-1",
		planID:   plan.ID,
	}
}

func (f *workgraphConsoleFixture) request(t *testing.T, token, method, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
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

func TestWorkGraphSealRoutesOnTechnicalLeadOnly(t *testing.T) {
	f := newWorkgraphConsoleFixture(t)
	ctx := context.Background()

	plan, err := f.pg.WorkGraph().GetWorkPlan(ctx, f.planID)
	require.NoError(t, err)

	t.Run("every project membership reads the graph and the ledger", func(t *testing.T) {
		list := f.request(t, f.viewerTK, http.MethodGet, "/api/v3/projects/"+j4ProjectID+"/work-graph", nil, "")
		require.Equal(t, http.StatusOK, list.Code, list.Body.String())
		assert.Contains(t, list.Body.String(), "MST-WP-00401")

		detail := f.request(t, f.viewerTK, http.MethodGet,
			"/api/v3/projects/"+j4ProjectID+"/work-graph/plans/"+f.planID, nil, "")
		require.Equal(t, http.StatusOK, detail.Code, detail.Body.String())
		assert.Contains(t, detail.Body.String(), `"available":true`)

		assets := f.request(t, f.viewerTK, http.MethodGet, "/api/v3/projects/"+j4ProjectID+"/assets", nil, "")
		require.Equal(t, http.StatusOK, assets.Code, assets.Body.String())
		assert.Contains(t, assets.Body.String(), `"assets":[]`)
		assert.Contains(t, assets.Body.String(), `"waiting_gates":[]`, "the W5-5 field is always present")
	})

	t.Run("the ledger carries the W5-5 waiting surface for stale gates", func(t *testing.T) {
		const workItemID = "018f7500-0000-7000-8000-0000000000e2"
		_, err := f.db.Exec(`INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, 'waiting-gate item', 'queued')`, workItemID, j4ProjectID)
		require.NoError(t, err)

		digestV1 := "sha256:" + strings.Repeat("ab", 32)
		digestV2 := "sha256:" + strings.Repeat("cd", 32)
		ledger := f.pg.Assets()
		_, err = ledger.RegisterAsset(ctx, store.Asset{
			AssetID: "ART-detailed-design-401", Version: 1, ProjectID: j4ProjectID, AssetType: "detailed-design",
			Title: "J4 台账 v1", Status: store.AssetStatusDraft, OwnerPrincipal: "session:j4-owner",
			Sensitivity: store.SensitivityInternal, SourceDigest: digestV1,
		}, "session:j4-owner")
		require.NoError(t, err)
		_, err = ledger.ReviewAsset(ctx, "ART-detailed-design-401", 1, "user:j4-reviewer")
		require.NoError(t, err)
		_, err = ledger.ApproveAsset(ctx, "ART-detailed-design-401", 1, "user:j4-approver", nil)
		require.NoError(t, err)
		_, err = ledger.BindAssetGate(ctx, j4ProjectID, workItemID, "ART-detailed-design-401", 1, "j4-waiting-gate", "j4-harness")
		require.NoError(t, err)
		_, err = ledger.RegisterAsset(ctx, store.Asset{
			AssetID: "ART-detailed-design-401", Version: 2, ProjectID: j4ProjectID, AssetType: "detailed-design",
			Title: "J4 台账 v2", Status: store.AssetStatusDraft, OwnerPrincipal: "session:j4-owner",
			Sensitivity: store.SensitivityInternal, SourceDigest: digestV2, SupersedesRef: "ART-detailed-design-401@1",
		}, "session:j4-owner")
		require.NoError(t, err)
		_, err = ledger.ReviewAsset(ctx, "ART-detailed-design-401", 2, "user:j4-reviewer")
		require.NoError(t, err)
		_, err = ledger.ApproveAsset(ctx, "ART-detailed-design-401", 2, "user:j4-approver", nil)
		require.NoError(t, err, "the v2 approve supersedes v1 and flips its binding stale")

		waiting := f.request(t, f.viewerTK, http.MethodGet, "/api/v3/projects/"+j4ProjectID+"/assets", nil, "")
		require.Equal(t, http.StatusOK, waiting.Code, waiting.Body.String())
		body := waiting.Body.String()
		assert.Contains(t, body, `"waiting_gates":[`)
		assert.Contains(t, body, `"gate_id":"j4-waiting-gate"`)
		assert.Contains(t, body, `"bound_version":1`)
		assert.Contains(t, body, `"latest_version":2`)
		assert.Contains(t, body, `"latest_status":"approved"`)
	})

	sealPath := "/api/v3/projects/" + j4ProjectID + "/work-graph/plans/" + f.planID + "/seal"
	sealBody := fmt.Sprintf(`{"expected_graph_version":%d}`, plan.GraphVersion)

	t.Run("viewer and qa_owner are 403 on the seal", func(t *testing.T) {
		viewer := f.request(t, f.viewerTK, http.MethodPost, sealPath,
			map[string]string{"Idempotency-Key": "j4-seal-viewer-00000001"}, sealBody)
		assert.Equal(t, http.StatusForbidden, viewer.Code, viewer.Body.String())

		qa := f.request(t, f.qaTK, http.MethodPost, sealPath,
			map[string]string{"Idempotency-Key": "j4-seal-qa-00000000001"}, sealBody)
		assert.Equal(t, http.StatusForbidden, qa.Code, qa.Body.String())
	})

	t.Run("technical_lead seals the draft revision", func(t *testing.T) {
		sealed := f.request(t, f.tlTK, http.MethodPost, sealPath,
			map[string]string{"Idempotency-Key": "j4-seal-tl-000000000001"}, sealBody)
		require.Equal(t, http.StatusOK, sealed.Code, sealed.Body.String())
		assert.Contains(t, sealed.Body.String(), `"sealed"`)
		assert.Contains(t, sealed.Body.String(), `"replay":false`)

		revision, err := f.pg.WorkGraph().CurrentPlanRevision(ctx, f.planID)
		require.NoError(t, err)
		assert.Equal(t, "sealed", revision.Status)

		// The governance audit row names the sealing principal.
		var actor string
		require.NoError(t, f.db.QueryRowContext(ctx, `
			SELECT actor_principal FROM audit_events
			WHERE action = 'workgraph.plan.sealed' AND resource_id = $1`, revision.ID).Scan(&actor))
		assert.Equal(t, f.tlPID, actor)

		// Idempotent replay: sealing bumped the graph CAS, so the retry
		// rides the fresh version and collapses onto the sealed revision.
		plan, err := f.pg.WorkGraph().GetWorkPlan(ctx, f.planID)
		require.NoError(t, err)
		replayBody := fmt.Sprintf(`{"expected_graph_version":%d}`, plan.GraphVersion)
		replay := f.request(t, f.tlTK, http.MethodPost, sealPath,
			map[string]string{"Idempotency-Key": "j4-seal-tl-000000000002"}, replayBody)
		require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
		assert.Contains(t, replay.Body.String(), `"replay":true`)
	})
}
