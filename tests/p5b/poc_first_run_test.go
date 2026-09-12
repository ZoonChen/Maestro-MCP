//go:build p5b

package p5b_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// Deterministic pilot identifiers: the first run seeds them once and
// every replay (this harness, the evidence report, future sessions)
// addresses the same rows.
const (
	p5bTeamID       = "018fb5b0-0000-7000-8000-000000000001"
	p5bBackendPID   = "018fb5b0-0000-7000-8000-000000000002"
	p5bWebPID       = "018fb5b0-0000-7000-8000-000000000003"
	p5bPocPID       = "018fb5b0-0000-7000-8000-000000000004"
	p5bPlayeduPID   = "018fb5b0-0000-7000-8000-000000000005"
	p5bInstanceID   = "018fb5b1-0000-7000-8000-000000000001"
	p5bClaimRunner  = "018fb5b2-0000-7000-8000-000000000001"
	p5bGateID       = "poc-detailed-design"
	p5bPlanHuman    = "MST-WP-00501"
	p5bContrastPlan = "MST-WP-00502"
	p5bGraphPlan    = "MST-WP-00503"
	p5bPatternName  = "poc-two-track"
	p5bJiraProject  = "PXPOC"
)

// PoC work items: fixed UUIDs so anchors/gates stay stable across runs.
var (
	p5bWItemEval     = "018fb5b3-0000-7000-8000-000000000101" // 对比评测执行（gated）
	p5bWItemLoad     = "018fb5b3-0000-7000-8000-000000000102" // 直播压测（方案冻结，执行二期）
	p5bWItemRisk     = "018fb5b3-0000-7000-8000-000000000103" // 风险评估（stale 传播演示位）
	p5bWItemEstimate = "018fb5b3-0000-7000-8000-000000000104" // 工时评估
	p5bWItemGate     = "018fb5b3-0000-7000-8000-000000000105" // D1 决策（Gate 本体）
)

type p5bFixture struct {
	db     *sql.DB
	pg     *store.PostgresStore
	rest   *p5bREST
	claims p5bClaims
	userID string
	config string // stdio runner config (sqlite baseline + postgres overlay)
	ev     *p5bEvidence
}

func p5bOpen(t *testing.T) *p5bFixture {
	t.Helper()
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	require.NotEmpty(t, dsn, "MAESTRO_TEST_POSTGRES_DSN must point at the LIVE pilot maestro database")
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	// The stdio runner carries its SQLite local baseline plus the PG
	// control-plane overlay (the m0/J2c composition).
	localDB := filepath.Join(t.TempDir(), "p5b-runner.db")
	migrateOut, err := exec.Command(p5bMaestroBinary, "migrate", "up", "--db", localDB).CombinedOutput()
	require.NoError(t, err, "migrate local baseline: %s", migrateOut)
	configPath := filepath.Join(t.TempDir(), "p5b-runner.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(
		"db_path: %q\nhttp_addr: \"127.0.0.1:0\"\nremote_write: false\ndatabase:\n  driver: postgres\n", localDB)), 0o600))

	rest := p5bNewREST(t)
	claims := rest.claims(t)
	return &p5bFixture{db: db, pg: pg, rest: rest, claims: claims, config: configPath, ev: p5bNewEvidence(t)}
}

func (f *p5bFixture) digest(t *testing.T, relative string) string {
	t.Helper()
	root := os.Getenv("P5B_ASSETS_DIR")
	require.NotEmpty(t, root, "P5B_ASSETS_DIR must point at the peixun-backend clone carrying the artifact content files")
	raw, err := os.ReadFile(filepath.Join(root, relative))
	require.NoError(t, err, "artifact content file %s", relative)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (f *p5bFixture) queueVersion(t *testing.T) int64 {
	t.Helper()
	var version int64
	require.NoError(t, f.db.QueryRow(`SELECT version FROM projects WHERE id=$1`, p5bPocPID).Scan(&version))
	return version
}

// seedWorkItem inserts one queued work item (import-shaped row).
func (f *p5bFixture) seedWorkItem(t *testing.T, id, title, role string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO work_items
		(id, project_id, type, title, description, status, priority, role, dependencies, test_requirements, lease_epoch, version)
		VALUES ($1, $2, 'task', $3, $4, 'queued', 'normal', $5, '[]'::jsonb, '[]'::jsonb, 0, 1)
		ON CONFLICT (id) DO NOTHING`, id, p5bPocPID, title, title, role)
	require.NoError(t, err)
}

// gitLabSHA resolves one repo's default-branch HEAD SHA (the proposal
// validator pins one exact baseline per node).
func (f *p5bFixture) gitLabSHA(t *testing.T, gitlabProject int) string {
	t.Helper()
	pat := os.Getenv("P5B_PILOT_GITLAB_PAT")
	require.NotEmpty(t, pat, "P5B_PILOT_GITLAB_PAT (sandbox root PAT)")
	api := os.Getenv("P5B_GITLAB_API")
	if api == "" {
		api = "http://127.0.0.1:8181"
	}
	request, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/v4/projects/%d/repository/commits/main", api, gitlabProject), nil)
	require.NoError(t, err)
	request.Header.Set("PRIVATE-TOKEN", pat)
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "gitlab commit lookup: %s", raw)
	var commit struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &commit))
	return commit.ID
}

// p5bAssetSpec describes one artifact's ledger identity for the walk.
type p5bAssetSpec struct {
	assetID     string
	assetType   string
	title       string
	sensitivity string
	contentRef  string
	reviewers   []string
	key         string
}

func (s p5bAssetSpec) idem(stage string) string {
	return fmt.Sprintf("%s-%s-0000000001", s.key, stage)
}

// mustJSONString wraps free text as a JSON string: the register tool's
// summary reaches the store as raw JSON bytes, so a bare Chinese
// sentence is a 500 (finding recorded in the retrospective).
func p5bJSONSummary(text string) string {
	raw, err := json.Marshal(text)
	if err != nil {
		return `""`
	}
	return string(raw)
}

// TestP5bStageOne walks the first-run chain end to end on the LIVE pilot
// control plane: baseline re-seed (ART-incident-002 recovery), WorkPattern
// v1 封板 + plan + decomposition proposal over real MCP stdio runners,
// the plan seal (J1 functional-plane first use, 403→grant→200), the
// artifact ledger walk with the full locked-gate lifecycle, Jira
// manual anchoring and the audit-chain export/verify. Stages are
// ordered and idempotent: a replay asserts the settled state.
func TestP5bStageOne(t *testing.T) {
	f := p5bOpen(t)
	ctx := context.Background()
	started := time.Now()

	// ------------------------------------------------------------------
	// S0: baseline re-seed (idempotent).
	// ------------------------------------------------------------------
	user, err := f.pg.Identities().GetOrCreateUser(ctx, f.claims.Iss, f.claims.Sub, "pilot-admin")
	require.NoError(t, err)
	f.userID = user.ID

	exec := func(query string, arguments ...any) {
		_, err := f.db.Exec(query, arguments...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO teams (id, name) VALUES ($1, 'peixun-pilot') ON CONFLICT (id) DO NOTHING`, p5bTeamID)
	for _, project := range []struct{ id, key, name, description string }{
		{p5bBackendPID, "peixun-backend", "企业学堂 backend（RuoYi 底座）", "D1 路线 B 底座仓（P5a 导入）"},
		{p5bWebPID, "peixun-web", "企业学堂 web", "RuoYi-Vue3 前端仓（P5a 导入）"},
		{p5bPocPID, "peixun-poc", "D1 PoC 治理域", "PlayEdu vs RuoYi 两周对比 PoC（0→1 首演）"},
		{p5bPlayeduPID, "playedu-eval", "PlayEdu 部署评估", "D1 路线 A 评估仓（P5b 导入，Apache-2.0）"},
	} {
		exec(`INSERT INTO projects (id, team_id, key, name, description, status, config, version)
			VALUES ($1, $2, $3, $4, $5, 'active', '{}'::jsonb, 1) ON CONFLICT (id) DO NOTHING`,
			project.id, p5bTeamID, project.key, project.name, project.description)
	}
	exec(`INSERT INTO memberships (team_id, user_id, role) VALUES ($1, $2, 'project_admin')
		ON CONFLICT (team_id, user_id) DO NOTHING`, p5bTeamID, f.userID)
	exec(`INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ($1, 'https://gitlab-pilot:8443', 'pilot sandbox GitLab (TLS face)', 'env:MAESTRO_PILOT_GITLAB_PAT', 'env:MAESTRO_PILOT_WEBHOOK_KEY')
		ON CONFLICT (base_url) DO NOTHING`, p5bInstanceID)
	for _, mapping := range []struct {
		gid             int
		projectID       string
		branch, repoURL string
	}{
		{3, p5bBackendPID, "main", "https://gitlab-pilot:8443/peixun/peixun-backend.git"},
		{4, p5bWebPID, "main", "https://gitlab-pilot:8443/peixun/peixun-web.git"},
		{5, p5bPlayeduPID, "main", "https://gitlab-pilot:8443/peixun/playedu-eval.git"},
	} {
		exec(`INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, default_branch, repository_url)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (gitlab_instance_id, gitlab_project_id) DO NOTHING`,
			p5bInstanceID, mapping.gid, mapping.projectID, mapping.branch, mapping.repoURL)
	}
	// The v3 claim substrate: one approved runner bound to the PoC project.
	exec(`INSERT INTO runners (id, display_name, device_key_hash, status, capabilities)
		VALUES ($1, 'p5b-claim-runner', 'x', 'approved', '[]'::jsonb) ON CONFLICT (id) DO NOTHING`, p5bClaimRunner)
	exec(`INSERT INTO runner_bindings (project_id, runner_id) VALUES ($1, $2)
		ON CONFLICT (project_id, runner_id) DO NOTHING`, p5bPocPID, p5bClaimRunner)

	t.Run("S0 baseline reachable over OIDC REST", func(t *testing.T) {
		status, body := f.rest.do(t, "GET", "/api/v3/projects/"+p5bPocPID+"/work-graph", nil, nil)
		require.Equal(t, 200, status, "work-graph listing must be reachable for project_admin: %s", body)
		f.ev.record(t, "baseline_seed", map[string]any{
			"projects": []string{p5bBackendPID, p5bWebPID, p5bPocPID, p5bPlayeduPID},
			"user":     f.userID, "team": p5bTeamID, "instance": p5bInstanceID,
			"mappings": []int{3, 4, 5}, "origin": "ART-incident-002 recovery, idempotent reseed",
		})
	})

	// ------------------------------------------------------------------
	// S1: WorkPattern v1 封板 + plan + decomposition proposal (real MCP).
	// ------------------------------------------------------------------
	graph := f.pg.WorkGraph()
	patternID := ""
	if err := f.db.QueryRow(
		`SELECT id::text FROM work_patterns WHERE project_id=$1 AND name=$2 AND version=1`,
		p5bPocPID, p5bPatternName).Scan(&patternID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		require.NoError(t, err)
	}
	if patternID == "" {
		pattern, err := graph.CreateWorkPattern(ctx, store.WorkPattern{
			ProjectID: p5bPocPID, Name: p5bPatternName, Version: 1, Status: "active",
			Body:      []byte(`{"template":"poc-two-track","fanout":"track","children":["eval","loadtest","estimate","risk"],"gate":"d1-selection"}`),
			CreatedBy: "p5b:" + f.userID,
		})
		require.NoError(t, err)
		patternID = pattern.ID
	}
	planID, rootNodeID := "", ""
	var graphVersion int64
	err = f.db.QueryRow(
		`SELECT id::text, root_node_id::text, graph_version FROM work_plans WHERE project_id=$1 AND human_code=$2`,
		p5bPocPID, p5bPlanHuman).Scan(&planID, &rootNodeID, &graphVersion)
	if errors.Is(err, sql.ErrNoRows) {
		plan, planErr := graph.CreateWorkPlan(ctx, store.CreateWorkPlanInput{
			ProjectID: p5bPocPID, Title: "D1 PoC：PlayEdu vs RuoYi 两周对比（0→1 首演）",
			HumanCode: p5bPlanHuman, RootSlotKey: "poc.d1",
			RootSpec: []byte(`{"title":"D1 PoC 决策（父）","success_threshold":{"kind":"all"},"failure_policy":"needs_human","cancel_policy":"cascade_required"}`),
			Actor:    "p5b:" + f.userID,
		})
		require.NoError(t, planErr)
		planID, rootNodeID, graphVersion = plan.ID, plan.RootNodeID, plan.GraphVersion
	} else {
		require.NoError(t, err)
	}

	// The formal graph plan: the original 00501 was sealed (root only)
	// by the accidental interim-mapping seal before any proposal could
	// apply, and a sealed revision cannot accept structural changes —
	// the 0→1 父子图 therefore lives on its own plan, proposed first and
	// sealed last (the honest J2b order).
	// Pick the first UNSEALED plan slot in the 00503+ series: a slot
	// sealed before its proposal landed (an earlier ordering bug in this
	// harness) is burnt forever — sealed revisions take no structural
	// changes — so the graph simply moves to the next slot.
	graphPlanID, graphRootID := "", ""
	var graphPlanVersion int64
	for slot := 3; slot <= 10; slot++ {
		code := fmt.Sprintf("MST-WP-0050%d", slot)
		var id, root string
		var version int64
		var sealed sql.NullString
		lookupErr := f.db.QueryRow(
			`SELECT p.id::text, p.root_node_id::text, p.graph_version,
				(SELECT sealed_at FROM plan_revisions r WHERE r.plan_id = p.id ORDER BY revision_no DESC LIMIT 1)
			 FROM work_plans p WHERE p.project_id=$1 AND p.human_code=$2`,
			p5bPocPID, code).Scan(&id, &root, &version, &sealed)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			created, createErr := graph.CreateWorkPlan(ctx, store.CreateWorkPlanInput{
				ProjectID: p5bPocPID, Title: "D1 PoC 工作图（0→1 首演正式版）",
				HumanCode: code, RootSlotKey: "poc.d1.formal",
				RootSpec: []byte(`{"title":"D1 PoC 决策（父）","success_threshold":{"kind":"all"},"failure_policy":"needs_human","cancel_policy":"cascade_required"}`),
				Actor:    "p5b:" + f.userID,
			})
			require.NoError(t, createErr)
			graphPlanID, graphRootID, graphPlanVersion = created.ID, created.RootNodeID, created.GraphVersion
			break
		}
		require.NoError(t, lookupErr)
		if !sealed.Valid {
			graphPlanID, graphRootID, graphPlanVersion = id, root, version
			break
		}
		t.Logf("slot %s is sealed; moving to the next", code)
	}
	require.NotEmpty(t, graphPlanID, "no unsealed graph-plan slot in 00503..00510")

	author := p5bStartRunner(t, f.config, p5bPocPID, "p5b-author")
	tech := p5bStartRunner(t, f.config, p5bPocPID, "p5b-tech")
	product := p5bStartRunner(t, f.config, p5bPocPID, "p5b-product")
	qa := p5bStartRunner(t, f.config, p5bPocPID, "p5b-qa")
	ops := p5bStartRunner(t, f.config, p5bPocPID, "p5b-ops")

	// The ledger walk helpers (replay-safe: an existing chain asserts the
	// settled state instead of re-walking transitions).
	register := func(t *testing.T, spec p5bAssetSpec) *store.Asset {
		t.Helper()
		if latest, err := f.pg.Assets().LatestAsset(ctx, spec.assetID); err == nil {
			return latest // replay: the version chain already exists
		} else if !errors.Is(err, store.ErrAssetNotFound) {
			t.Fatalf("latest asset %s: %v", spec.assetID, err)
		}
		arguments := map[string]any{
			"asset_id": spec.assetID, "asset_type": spec.assetType, "title": spec.title,
			"sensitivity": spec.sensitivity, "source_digest": f.digest(t, spec.contentRef),
			"content_ref": spec.contentRef, "idempotency_key": spec.idem("register"),
		}
		if len(spec.reviewers) > 0 {
			arguments["reviewers"] = spec.reviewers
		}
		text := author.callOK(t, "asset_register", arguments)
		assert.Contains(t, text, `"status":"draft"`)
		latest, err := f.pg.Assets().LatestAsset(ctx, spec.assetID)
		require.NoError(t, err)
		return latest
	}
	moveTo := func(t *testing.T, spec p5bAssetSpec, version int, stage string, runner *p5bStdioRunner) {
		t.Helper()
		asset, err := f.pg.Assets().GetAsset(ctx, spec.assetID, version)
		require.NoError(t, err)
		if asset.Status == store.AssetStatusDraft && stage == "review" {
			runner.callOK(t, "asset_review", map[string]any{
				"asset_id": spec.assetID, "version": float64(version), "idempotency_key": spec.idem("review"),
			})
		}
		if asset.Status == store.AssetStatusReviewed && stage == "approve" {
			runner.callOK(t, "asset_approve", map[string]any{
				"asset_id": spec.assetID, "version": float64(version), "idempotency_key": spec.idem("approve"),
			})
		}
	}
	requireApproved := func(t *testing.T, assetID string, version int) {
		t.Helper()
		asset, err := f.pg.Assets().GetAsset(ctx, assetID, version)
		require.NoError(t, err)
		assert.Equal(t, store.AssetStatusApproved, asset.Status, "%s@%d", assetID, version)
	}

	blueprint := p5bAssetSpec{assetID: "ART-blueprint-001", assetType: "blueprint",
		title:       "PoC 决策卡：D1 底座路线选型（PlayEdu vs RuoYi 自建）",
		sensitivity: "internal", contentRef: "assets/ART-blueprint-001/blueprint.md",
		reviewers: []string{"function:product_owner", "function:technical_lead"}, key: "p5b-bp"}
	design := p5bAssetSpec{assetID: "ART-detailed-design-001", assetType: "detailed-design",
		title: "详设：D1 对比评测维度与压测方案", sensitivity: "internal",
		contentRef: "assets/ART-detailed-design-001/design.md",
		reviewers:  []string{"function:technical_lead"}, key: "p5b-dd"}
	testPlan := p5bAssetSpec{assetID: "ART-test-plan-001", assetType: "test-plan",
		title: "测试方案：D1 PoC 构建链路压测与双路线冒烟", sensitivity: "internal",
		contentRef: "assets/ART-test-plan-001/test-plan.md",
		reviewers:  []string{"function:qa_owner"}, key: "p5b-tp"}
	incident := p5bAssetSpec{assetID: "ART-incident-002", assetType: "incident",
		title: "事故报告：试点控制面 PG 数据丢失（P5b 开工检查发现）", sensitivity: "internal",
		contentRef: "assets/ART-incident-002/incident.md",
		reviewers:  []string{"function:operations_owner"}, key: "p5b-inc2"}

	t.Run("S1 graph query sees the root package", func(t *testing.T) {
		text := author.callOK(t, "worktree_graph_query", map[string]any{"plan_id": planID})
		assert.Contains(t, text, rootNodeID)
		f.ev.record(t, "work_pattern", map[string]any{
			"pattern_id": patternID, "name": p5bPatternName, "version": 1, "status": "active",
			"plan_id": planID, "root_node": rootNodeID,
		})
	})

	t.Run("S1 decomposition proposal applies the PoC 父子图", func(t *testing.T) {
		designRef := "ART-detailed-design-001@1"
		if latest, err := f.pg.Assets().LatestAsset(ctx, design.assetID); err == nil && latest.Version >= 2 {
			designRef = fmt.Sprintf("ART-detailed-design-001@%d", latest.Version)
		}
		// The flows reference ledger assets, so the blueprint and the
		// detailed-design draft register BEFORE the proposal (the ledger
		// is the authority the validator checks against).
		register(t, blueprint)
		register(t, design)

		var appliedCount, proposalCount int
		require.NoError(t, f.db.QueryRow(
			`SELECT count(*) FROM decomposition_proposals WHERE plan_id=$1 AND status='applied'`, graphPlanID).Scan(&appliedCount))
		require.NoError(t, f.db.QueryRow(
			`SELECT count(*) FROM decomposition_proposals WHERE plan_id=$1`, graphPlanID).Scan(&proposalCount))
		if appliedCount == 0 {
			var nodeCount int
			require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM work_nodes WHERE plan_id=$1`, graphPlanID).Scan(&nodeCount))
			require.Equal(t, 1, nodeCount, "expected only the root before the first applied proposal")
			backendSHA := f.gitLabSHA(t, 3)
			webSHA := f.gitLabSHA(t, 4)
			// A rejected proposal is a decided record keyed by its
			// idempotency key, so each attempt carries its own key.
			// Idempotency keys are a GLOBAL namespace on proposals (a
			// same-key call replays the stored decision even for another
			// plan — observation recorded in the retrospective), so the
			// key carries the plan and the attempt number.
			idempotencyKey := fmt.Sprintf("p5b-propose-%s-%02d", graphPlanID[:8], proposalCount+1)
			text := author.callOK(t, "decomposition_propose", map[string]any{
				"plan_id": graphPlanID, "expected_graph_version": float64(graphPlanVersion),
				"idempotency_key": idempotencyKey,
				"work_pattern":    map[string]any{"pattern_id": patternID, "version": 1},
				"nodes": []map[string]any{
					{"local_id": "eval", "parent": "ext:" + graphRootID, "node_type": "work_item", "slot_key": "poc.eval", "human_code": "MST-WI-00511",
						"spec": map[string]any{"title": "对比评测（双路线功能映射 + 构建 A/B）", "repo": "peixun-backend", "baseline_sha": backendSHA, "workspace_paths": []string{"ci-smoke", "doc"}, "budget_units": 60, "owning_capability": "poc.eval", "acceptance_criteria": []string{"评测矩阵齐", "构建 A/B 双轮实测"}, "failure_policy": "fail_fast"}},
					{"local_id": "loadtest", "parent": "ext:" + graphRootID, "node_type": "work_item", "slot_key": "poc.loadtest", "human_code": "MST-WI-00512",
						"spec": map[string]any{"title": "直播压测（方案冻结；执行依赖二期 srs-loadtest Profile）", "repo": "peixun-web", "baseline_sha": webSHA, "workspace_paths": []string{"ci-e2e"}, "budget_units": 40, "owning_capability": "poc.loadtest", "acceptance_criteria": []string{"压测方案 approved"}, "failure_policy": "needs_human"}},
					{"local_id": "estimate", "parent": "ext:" + graphRootID, "node_type": "work_item", "slot_key": "poc.estimate", "human_code": "MST-WI-00513",
						"spec": map[string]any{"title": "工时评估（对照 BOM 221 人日基线）", "repo": "peixun-backend", "baseline_sha": backendSHA, "workspace_paths": []string{"doc"}, "budget_units": 20, "owning_capability": "poc.estimate", "acceptance_criteria": []string{"两路线工时对照表"}}},
					{"local_id": "risk", "parent": "ext:" + graphRootID, "node_type": "work_item", "slot_key": "poc.risk", "human_code": "MST-WI-00514",
						"spec": map[string]any{"title": "风险评估（许可证/社区/技术债/等保）", "repo": "peixun-backend", "baseline_sha": backendSHA, "workspace_paths": []string{"doc"}, "budget_units": 20, "owning_capability": "poc.risk", "acceptance_criteria": []string{"风险四象限清单"}}},
					{"local_id": "d1-gate", "parent": "ext:" + graphRootID, "node_type": "gate", "slot_key": "poc.gate.d1", "human_code": "MST-WI-00515",
						"spec": map[string]any{"title": "D1 选型 Gate（product+technical 双签）", "baseline_sha": backendSHA, "workspace_paths": []string{"doc"}, "budget_units": 1, "acceptance_criteria": []string{"release-note approved", "双签审计链完整"}}},
				},
				"dependencies": []map[string]any{
					{"from": "eval", "to": "d1-gate", "requirement": "required"},
					{"from": "loadtest", "to": "d1-gate", "requirement": "required"},
					{"from": "estimate", "to": "d1-gate", "requirement": "required"},
					{"from": "risk", "to": "d1-gate", "requirement": "required"},
				},
				"flows": []map[string]any{
					{"node": "eval", "direction": "consumes", "asset_ref": designRef, "port_key": "poc.eval.design"},
					{"node": "d1-gate", "direction": "consumes", "asset_ref": "ART-blueprint-001@1", "port_key": "poc.gate.card"},
				},
			})
			assert.Contains(t, text, `"status":"applied"`)
		} else {
			t.Log("proposal already applied (replay); asserting settled state")
			var nodes int
			require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM work_nodes WHERE plan_id=$1`, graphPlanID).Scan(&nodes))
			assert.Equal(t, 6, nodes, "root + 4 children + gate")
		}
		f.ev.record(t, "decomposition", map[string]any{
			"plan_id": graphPlanID, "pattern": p5bPatternName,
			"children": []string{"eval", "loadtest", "estimate", "risk"}, "gate": "d1-gate",
		})
	})

	// ------------------------------------------------------------------
	// S2: plan seal — the J1 functional-plane first use over REST.
	//
	// Operational reality (recorded in the evidence): the resident
	// server on :8080 is the pre-J4 image whose interim mapping
	// (workgraph.seal → project_policy.strengthen, held by
	// project_admin) sealed the main plan during the first attempt.
	// The J4 terminal mapping is therefore demonstrated on a dedicated
	// contrast plan against the current-code server (P5B_PILOT_API):
	// revoke technical_lead → 403, re-grant → 200.
	// ------------------------------------------------------------------
	var mainSealedAt sql.NullString
	require.NoError(t, f.db.QueryRow(
		`SELECT sealed_at FROM plan_revisions WHERE plan_id=$1 ORDER BY revision_no DESC LIMIT 1`, planID).Scan(&mainSealedAt))
	f.ev.record(t, "seal_main_plan", map[string]any{
		"plan_id": planID, "sealed_at": nullStringValue(mainSealedAt),
		"note": "sealed on the resident pre-J4 server image via the interim project_policy.strengthen mapping (project_admin); the J4 terminal workgraph.seal mapping is demonstrated on the contrast plan",
	})

	contrastID := ""
	err = f.db.QueryRow(
		`SELECT id::text FROM work_plans WHERE project_id=$1 AND human_code=$2`,
		p5bPocPID, p5bContrastPlan).Scan(&contrastID)
	if errors.Is(err, sql.ErrNoRows) {
		contrast, planErr := graph.CreateWorkPlan(ctx, store.CreateWorkPlanInput{
			ProjectID: p5bPocPID, Title: "PoC 对照计划——seal 职能映射演示（J4 终态）",
			HumanCode: p5bContrastPlan, RootSlotKey: "poc.contrast",
			RootSpec: []byte(`{"title":"seal 权限对照演示根包","success_threshold":{"kind":"all"}}`),
			Actor:    "p5b:" + f.userID,
		})
		require.NoError(t, planErr)
		contrastID = contrast.ID
	} else {
		require.NoError(t, err)
	}
	var contrastSealedAt sql.NullString
	require.NoError(t, f.db.QueryRow(
		`SELECT sealed_at FROM plan_revisions WHERE plan_id=$1 ORDER BY revision_no DESC LIMIT 1`, contrastID).Scan(&contrastSealedAt))
	contrastSealPath := "/api/v3/projects/" + p5bPocPID + "/work-graph/plans/" + contrastID + "/seal"
	var contrastGraphVersion int64
	require.NoError(t, f.db.QueryRow(`SELECT graph_version FROM work_plans WHERE id=$1`, contrastID).Scan(&contrastGraphVersion))

	// Grant the full pilot function set first (idempotent): the J1
	// authorization bookkeeping the rest of the run relies on.
	for _, function := range []string{"technical_lead", "product_owner", "qa_owner", "security_owner", "operations_owner"} {
		active, err := f.pg.Identities().ActiveFunctionalRoles(ctx, f.userID)
		require.NoError(t, err)
		if p5bContains(active, function) {
			continue
		}
		require.NoError(t, f.pg.Identities().GrantFunctionalRole(ctx, &store.FunctionalPrincipal{
			UserID: f.userID, Function: function,
			SourceRef: "p5b 首演授权：单操作员试点，五职能集于 pilot-admin；授权书记录于 release-note Evidence（BLUEPRINT §4.2 [待评审] 缺口登记）",
		}), "grant %s", function)
	}

	t.Run("S2 seal is denied without the technical_lead function", func(t *testing.T) {
		if contrastSealedAt.Valid {
			t.Log("replay: contrast plan already sealed; skipping the denial probe")
			f.ev.record(t, "seal_before_grant", map[string]any{"status": "skipped-replay"})
			return
		}
		var grantID string
		require.NoError(t, f.db.QueryRow(
			`SELECT id::text FROM functional_principals WHERE user_id=$1 AND function='technical_lead' AND revoked_at IS NULL`,
			f.userID).Scan(&grantID))
		require.NoError(t, f.pg.Identities().RevokeFunctionalRole(ctx, grantID), "revoke technical_lead for the denial probe")
		status, body := f.rest.do(t, "POST", contrastSealPath,
			map[string]string{"Idempotency-Key": "p5b-seal-denied-00000001"},
			[]byte(fmt.Sprintf(`{"expected_graph_version":%d}`, contrastGraphVersion)))
		assert.Equal(t, 403, status, "seal without technical_lead must deny: %s", body)
		f.ev.record(t, "seal_before_grant", map[string]any{"status": status, "body": body, "plan": p5bContrastPlan})
	})

	t.Run("S2 grant functions then seal as technical_lead", func(t *testing.T) {
		if !contrastSealedAt.Valid {
			require.NoError(t, f.pg.Identities().GrantFunctionalRole(ctx, &store.FunctionalPrincipal{
				UserID: f.userID, Function: "technical_lead",
				SourceRef: "p5b 首演授权：对照计划封板（J1 通路首用）",
			}), "re-grant technical_lead")
		}
		idempotencyKey := "p5b-seal-approve-0000001"
		if contrastSealedAt.Valid {
			idempotencyKey = "p5b-seal-replay-0000001"
		}
		status, body := f.rest.do(t, "POST", contrastSealPath,
			map[string]string{"Idempotency-Key": idempotencyKey},
			[]byte(fmt.Sprintf(`{"expected_graph_version":%d}`, contrastGraphVersion)))
		require.Equal(t, 200, status, "seal with technical_lead must succeed (or replay): %s", body)
		if contrastSealedAt.Valid {
			assert.Contains(t, body, `"replay":true`)
		} else {
			assert.Contains(t, body, `"status":"sealed"`)
		}
		f.ev.record(t, "seal", map[string]any{
			"status": status, "actor": "pilot-admin(technical_lead)", "plan_id": contrastID,
			"replay": contrastSealedAt.Valid, "api": f.rest.base,
		})

		// The formal graph plan seals LAST (proposal → human seal, the
		// honest J2b order) — and only once its proposal has landed.
		var formalSealedAt sql.NullString
		require.NoError(t, f.db.QueryRow(
			`SELECT sealed_at FROM plan_revisions WHERE plan_id=$1 ORDER BY revision_no DESC LIMIT 1`, graphPlanID).Scan(&formalSealedAt))
		var formalNodes int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM work_nodes WHERE plan_id=$1`, graphPlanID).Scan(&formalNodes))
		if formalNodes < 6 && !formalSealedAt.Valid {
			t.Logf("formal plan %s has %d nodes (proposal not applied yet); deferring its seal", graphPlanID, formalNodes)
			return
		}
		var formalVersion int64
		require.NoError(t, f.db.QueryRow(`SELECT graph_version FROM work_plans WHERE id=$1`, graphPlanID).Scan(&formalVersion))
		formalStatus, formalBody := f.rest.do(t, "POST", "/api/v3/projects/"+p5bPocPID+"/work-graph/plans/"+graphPlanID+"/seal",
			map[string]string{"Idempotency-Key": "p5b-seal-formal-0000001"},
			[]byte(fmt.Sprintf(`{"expected_graph_version":%d}`, formalVersion)))
		require.Equal(t, 200, formalStatus, "seal of the formal graph plan (technical_lead) must succeed (or replay): %s", formalBody)
		f.ev.record(t, "seal_formal_plan", map[string]any{
			"plan_id": graphPlanID, "status": formalStatus, "replay": formalSealedAt.Valid,
			"order": "proposal applied → technical_lead seal",
		})
	})

	// ------------------------------------------------------------------
	// S3: artifact ledger walk (MCP, real runners, separation of duties).
	// ------------------------------------------------------------------
	t.Run("S3 blueprint walk（product 签评审 + technical 签放行）", func(t *testing.T) {
		register(t, blueprint)                     // replay-safe (draft registered before the proposal)
		moveTo(t, blueprint, 1, "review", product) // product sign at review
		moveTo(t, blueprint, 1, "approve", tech)   // technical sign at release
		requireApproved(t, blueprint.assetID, 1)
		f.ev.record(t, "blueprint_walk", map[string]any{
			"asset": "ART-blueprint-001@1", "review_actor": "session:p5b-product-session", "approve_actor": "session:p5b-tech-session",
		})
	})

	t.Run("S3 detailed-design locked_gate 全生命周期", func(t *testing.T) {
		latest := register(t, design)
		f.seedWorkItem(t, p5bWItemEval, "PoC 对比评测执行（locked_gate=poc-detailed-design）", "backend")

		if latest.Version == 1 && latest.Status == store.AssetStatusDraft {
			// (a) fail closed: a draft asset cannot back a gate.
			_, bindErr := f.pg.Assets().BindAssetGate(ctx, p5bPocPID, p5bWItemEval, design.assetID, 1, p5bGateID, "p5b-harness")
			require.ErrorIs(t, bindErr, store.ErrAssetGateNotSatisfied, "draft detailed-design must NOT be bindable")
			f.ev.record(t, "locked_gate_draft_bind_denied", map[string]any{"asset": design.assetID + "@1", "gate": p5bGateID})

			// (b) approve v1 then bind: the gate is consumable.
			moveTo(t, design, 1, "review", tech)
			moveTo(t, design, 1, "approve", tech)
		}
		bindingCount := 0
		require.NoError(t, f.db.QueryRow(
			`SELECT count(*) FROM asset_gate_bindings WHERE work_item_id=$1 AND gate_id=$2`, p5bWItemEval, p5bGateID).
			Scan(&bindingCount))
		if bindingCount == 0 {
			_, bindErr := f.pg.Assets().BindAssetGate(ctx, p5bPocPID, p5bWItemEval, design.assetID, 1, p5bGateID, "p5b-harness")
			require.NoError(t, bindErr, "bind approved detailed-design v1")
		}

		// (c) claim: the gate is satisfied → dispatch.
		var evalStatus string
		require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id=$1`, p5bWItemEval).Scan(&evalStatus))
		if evalStatus == "queued" {
			claim, claimErr := f.pg.ClaimNextWorkItem(ctx, p5bClaimRunner, "p5b-gen-1", f.queueVersion(t), 10*time.Minute)
			require.NoError(t, claimErr, "gate-satisfied item must be claimable")
			assert.Equal(t, p5bWItemEval, claim.WorkItemID)
			f.ev.record(t, "locked_gate_claim_allowed", map[string]any{"work_item": p5bWItemEval, "lease": claim.LeaseID})
		} else {
			t.Log("replay: eval item already dispatched")
			f.ev.record(t, "locked_gate_claim_allowed", map[string]any{"work_item": p5bWItemEval, "replay": true})
		}

		// (d) two more gated items bound to v1, then v2 supersedes v1 —
		// ApproveAsset flips all v1 bindings stale in the same
		// transaction (ADR-009 §8), and dispatch fails closed.
		f.seedWorkItem(t, p5bWItemLoad, "直播压测执行（二期 Profile）", "devops")
		f.seedWorkItem(t, p5bWItemRisk, "风险评估", "backend")
		for _, itemID := range []string{p5bWItemLoad, p5bWItemRisk} {
			var count int
			require.NoError(t, f.db.QueryRow(
				`SELECT count(*) FROM asset_gate_bindings WHERE work_item_id=$1 AND gate_id=$2`, itemID, p5bGateID).Scan(&count))
			if count == 0 {
				_, bindErr := f.pg.Assets().BindAssetGate(ctx, p5bPocPID, itemID, design.assetID, 1, p5bGateID, "p5b-harness")
				require.NoError(t, bindErr, "bind %s to v1", itemID)
			}
		}
		if latest2, err := f.pg.Assets().LatestAsset(ctx, design.assetID); err == nil && latest2.Version == 1 {
			text := author.callOK(t, "asset_register", map[string]any{
				"asset_id": design.assetID, "asset_type": design.assetType, "title": design.title,
				"sensitivity": design.sensitivity, "source_digest": f.digest(t, "assets/ART-detailed-design-001/design-v2.md"),
				"content_ref":    "assets/ART-detailed-design-001/design-v2.md",
				"supersedes_ref": design.assetID + "@1",
				"reviewers":      design.reviewers, "idempotency_key": design.idem("register-v2"),
			})
			assert.Contains(t, text, `"version":2`)
			moveTo(t, design, 2, "review", tech)
			moveTo(t, design, 2, "approve", tech) // v1 superseded + bindings flip stale here
		}
		var staleCount int
		require.NoError(t, f.db.QueryRow(
			`SELECT count(*) FROM asset_gate_bindings WHERE gate_id=$1 AND status='stale'`, p5bGateID).Scan(&staleCount))
		if latest2, err := f.pg.Assets().LatestAsset(ctx, design.assetID); err == nil && latest2.Version == 1 {
			// This run performed the supersede: all three v1 bindings
			// flipped stale in the same transaction.
			assert.Equal(t, 3, staleCount, "v1 carried three live bindings; supersede must flip all of them stale")
		} else {
			assert.GreaterOrEqual(t, staleCount, 1, "the un-healed eval binding stays stale on replay")
		}
		f.ev.record(t, "locked_gate_supersede_stale_flip", map[string]any{
			"asset": design.assetID + "@1→@2", "stale_bindings": staleCount,
		})

		// (e) dispatch fails closed while only stale-gated items remain
		// queued (the other items are not seeded yet).
		var loadStatus, riskStatus string
		require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id=$1`, p5bWItemLoad).Scan(&loadStatus))
		require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id=$1`, p5bWItemRisk).Scan(&riskStatus))
		if loadStatus == "queued" && riskStatus == "queued" {
			_, claimErr := f.pg.ClaimNextWorkItem(ctx, p5bClaimRunner, "p5b-gen-1", f.queueVersion(t), 10*time.Minute)
			require.ErrorIs(t, claimErr, store.ErrNoAvailableTask, "stale bindings must fail dispatch closed")
			f.ev.record(t, "locked_gate_stale_blocks_claim", map[string]any{
				"work_items": []string{p5bWItemLoad, p5bWItemRisk}, "error": "no available task",
			})
		} else {
			t.Log("replay: stale-block stage already consumed")
		}

		// (f) re-binding against approved v2 heals the gate in place.
		p5bHealToV2(t, f, p5bWItemLoad)
		if loadStatus == "queued" {
			claim, claimErr := f.pg.ClaimNextWorkItem(ctx, p5bClaimRunner, "p5b-gen-1", f.queueVersion(t), 10*time.Minute)
			require.NoError(t, claimErr, "healed item must dispatch")
			assert.Equal(t, p5bWItemLoad, claim.WorkItemID)
			f.ev.record(t, "locked_gate_v2_rebind_claim", map[string]any{"work_item": p5bWItemLoad, "lease": claim.LeaseID})
		}
		p5bHealToV2(t, f, p5bWItemRisk)
		requireApproved(t, design.assetID, 2)
	})

	t.Run("S3 test-plan 与事故报告 walk", func(t *testing.T) {
		register(t, testPlan)
		moveTo(t, testPlan, 1, "review", qa)
		moveTo(t, testPlan, 1, "approve", qa)
		requireApproved(t, testPlan.assetID, 1)

		register(t, incident)
		moveTo(t, incident, 1, "review", ops)
		moveTo(t, incident, 1, "approve", ops)
		requireApproved(t, incident.assetID, 1)
	})

	t.Run("S3 存量三件重登记（ART-incident-002 恢复；SoD 拒绝演示）", func(t *testing.T) {
		research := p5bAssetSpec{assetID: "ART-research-001", assetType: "research",
			title: "调研报告-企业培训平台开源选型（存量重登记）", sensitivity: "internal",
			contentRef: "assets/ART-research-001/调研报告-企业培训平台开源选型.md", key: "p5b-rs"}
		if _, err := f.pg.Assets().LatestAsset(ctx, research.assetID); errors.Is(err, store.ErrAssetNotFound) {
			register(t, research)
			result, text := author.call(t, "asset_review", map[string]any{
				"asset_id": research.assetID, "version": 1.0, "idempotency_key": research.idem("selfreview"),
			})
			assert.True(t, result.IsError, "the registering session must not review its own asset")
			assert.Contains(t, text, "separation of duties")
			f.ev.record(t, "sod_self_review_denied", map[string]any{"asset": research.assetID})
			moveTo(t, research, 1, "review", tech)
			moveTo(t, research, 1, "approve", tech)
		}
		requireApproved(t, research.assetID, 1)

		if _, err := f.pg.Assets().LatestAsset(ctx, "ART-bom-001"); errors.Is(err, store.ErrAssetNotFound) {
			author.callOK(t, "asset_register", map[string]any{
				"asset_id": "ART-bom-001", "asset_type": "bom",
				"title":       "企业学堂平台建设BOM清单规划（8 Sheet 112 条；digest 口径，原件不动）",
				"sensitivity": "internal", "source_digest": "sha256:" + strings.Repeat("00", 32),
				"content_ref":     "legacy://yuandong-peixun/BOM清单规划.xlsx",
				"summary":         p5bJSONSummary("重登记（ART-incident-002 恢复）：原件在试点源目录，台账持 digest 与指针；首登记摘要行已随数据丢失事件消失，恢复版以源目录原件为准。"),
				"idempotency_key": "p5b-bom-reintake-0001",
			})
		}
		if _, err := f.pg.Assets().LatestAsset(ctx, "ART-legacy-intake-001"); errors.Is(err, store.ErrAssetNotFound) {
			author.callOK(t, "asset_register", map[string]any{
				"asset_id": "ART-legacy-intake-001", "asset_type": "legacy-intake",
				"title":       "会话记录存量摄取（confidential，仅摘要+指针）",
				"sensitivity": "confidential", "source_digest": "sha256:" + strings.Repeat("01", 32),
				"content_ref":     "legacy://yuandong-peixun/.zcode/plans/",
				"summary":         p5bJSONSummary("重登记（ART-incident-002 恢复）：正文不入库；指针指向试点源目录会话记录。"),
				"idempotency_key": "p5b-legacy-reintake-001",
			})
		}
	})

	// ------------------------------------------------------------------
	// S4: Jira re-probe + manual anchoring (blueprint fallback) + the
	// empty reconcile list (对账零未决).
	// ------------------------------------------------------------------
	t.Run("S4 Jira 复测与手工锚定回退", func(t *testing.T) {
		f.seedWorkItem(t, p5bWItemEstimate, "工时评估", "backend")
		f.seedWorkItem(t, p5bWItemGate, "D1 选型 Gate（双签收口）", "coordinator")

		jiraTarget := os.Getenv("P5B_JIRA_BASE_URL")
		if jiraTarget == "" {
			jiraTarget = "https://jira.yuandong.internal"
		}
		probeErr := p5bProbeHTTPS(jiraTarget, 8*time.Second)
		reachable := probeErr == nil
		decision := "维持蓝图回退：手工锚定（镜像 worker 不启动）"
		if reachable {
			decision = "镜像 worker 可启用（J3 链路首演条件具备）"
		}
		f.ev.record(t, "jira_probe", map[string]any{
			"target": jiraTarget, "reachable": reachable, "error": errString(probeErr), "decision": decision,
		})

		anchors := []struct{ workItemID, issueKey string }{
			{p5bWItemEval, "PXPOC-1"}, {p5bWItemLoad, "PXPOC-2"},
			{p5bWItemRisk, "PXPOC-3"}, {p5bWItemEstimate, "PXPOC-4"}, {p5bWItemGate, "PXPOC-5"},
		}
		for _, anchor := range anchors {
			var count int
			require.NoError(t, f.db.QueryRow(
				`SELECT count(*) FROM jira_anchors WHERE work_item_id=$1 AND issue_key=$2`,
				anchor.workItemID, anchor.issueKey).Scan(&count))
			if count == 0 {
				_, err := f.pg.Jira().CreateJiraAnchor(ctx, p5bPocPID, store.JiraAnchor{
					WorkItemID: anchor.workItemID, IssueKey: anchor.issueKey,
					JiraProjectKey: p5bJiraProject, AnchorSource: store.JiraAnchorSourceManual,
					Assignee: "pilot-admin", IterationLabel: "POC-W1W2",
				})
				require.NoError(t, err, "manual anchor %s", anchor.issueKey)
			}
		}
		status, body := f.rest.do(t, "GET", "/api/v3/projects/"+p5bPocPID+"/jira-anchors", nil, nil)
		require.Equal(t, 200, status, "anchor listing: %s", body)
		for _, anchor := range anchors {
			assert.Contains(t, body, anchor.issueKey)
		}
		assert.Contains(t, body, `"anchor_source":"manual"`)
		f.ev.record(t, "jira_anchors", map[string]any{
			"source": "manual", "issue_keys": []string{"PXPOC-1", "PXPOC-2", "PXPOC-3", "PXPOC-4", "PXPOC-5"},
		})

		status, body = f.rest.do(t, "GET", "/api/v3/projects/"+p5bPocPID+"/jira-reconcile-items", nil, nil)
		require.Equal(t, 200, status, "reconcile listing: %s", body)
		assert.Contains(t, body, `"items":[]`, "对账清单必须为空（无镜像 worker 运行=无分歧源）")
	})

	// ------------------------------------------------------------------
	// S5: audit chain export + verify (the stage-audit discipline).
	// ------------------------------------------------------------------
	t.Run("S5 审计链导出与验证", func(t *testing.T) {
		status, body := f.rest.do(t, "GET", "/api/v3/projects/"+p5bPocPID+"/audit-export?from_seq=1&to_seq=10000", nil, nil)
		require.Equal(t, 200, status, "audit export: %s", body)
		var export struct {
			ChainDigest string `json:"chain_digest"`
			Entries     []struct {
				Seq         int64  `json:"seq"`
				EventType   string `json:"event_type"`
				Action      string `json:"action"`
				EntryDigest string `json:"entry_digest"`
			} `json:"entries"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &export))
		actions := map[string]int{}
		digests := make([]string, 0, len(export.Entries))
		for _, entry := range export.Entries {
			key := entry.Action
			if key == "" {
				key = entry.EventType
			}
			actions[key]++
			digests = append(digests, entry.EntryDigest)
		}
		for _, expected := range []string{
			"asset.registered", "asset.reviewed", "asset.approved", "asset.gate.bound",
			"workgraph.plan.created", "workgraph.proposal.applied", "workgraph.plan.sealed",
		} {
			assert.Greater(t, actions[expected], 0, "audit chain must carry %s", expected)
		}
		f.ev.record(t, "audit_export_stage_one", map[string]any{
			"entries": len(export.Entries), "chain_digest": export.ChainDigest, "actions": actions,
		})

		verifyStatus, verifyBody := f.rest.do(t, "POST", "/api/v3/projects/"+p5bPocPID+"/audit-export/verify",
			map[string]string{"If-Match": export.ChainDigest, "Idempotency-Key": "p5b-audit-verify-0001"},
			mustJSON(t, map[string]any{"from_seq": 1, "to_seq": 10000, "claimed_digests": digests}))
		require.Equal(t, 200, verifyStatus, "audit verify: %s", verifyBody)
		assert.Contains(t, verifyBody, `"verified":true`)
		f.ev.record(t, "stage_one_completed", map[string]any{
			"wall_seconds": time.Since(started).Seconds(), "at": time.Now().Format(time.RFC3339),
		})
	})
}

// p5bHealToV2 re-binds one gated work item to the approved design v2;
// an already-live v2 binding (replay) is the settled state.
func p5bHealToV2(t *testing.T, f *p5bFixture, workItemID string) {
	t.Helper()
	var status string
	var boundVersion int
	err := f.db.QueryRow(
		`SELECT status, bound_version FROM asset_gate_bindings WHERE work_item_id=$1 AND gate_id=$2`,
		workItemID, p5bGateID).Scan(&status, &boundVersion)
	require.NoError(t, err)
	if status == "bound" && boundVersion == 2 {
		return // replay: already healed
	}
	_, err = f.pg.Assets().BindAssetGate(context.Background(), p5bPocPID, workItemID,
		"ART-detailed-design-001", 2, p5bGateID, "p5b-harness")
	require.NoError(t, err, "re-bind %s against approved v2 heals", workItemID)
}

func p5bContains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func nullStringValue(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}
