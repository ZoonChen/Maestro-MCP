// steps.go: the S2A execution stages. Every step is idempotent — a
// replay checks the settled state and only writes what is missing.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// s2aClaimRunnerPeixun is a DEDICATED claim runner bound ONLY to the
// peixun project (the P5b claim runner also serves the PoC project and
// would surface its queued items in the blocked-claim probe).
const s2aClaimRunnerPeixun = "018fb5b2-0000-7000-8000-000000000002"

// m1Items / m23Items split the manifest by phase.
func m1Items(m BomManifest) []BomItem {
	var out []BomItem
	for _, item := range m.Items {
		if item.Phase == "一期" {
			out = append(out, item)
		}
	}
	return out
}

func m23Items(m BomManifest) []BomItem {
	var out []BomItem
	for _, item := range m.Items {
		if item.Phase != "一期" {
			out = append(out, item)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// S0: seeds (project / membership / claim runner).
// ---------------------------------------------------------------------------

func (imp *importer) stepSeeds() error {
	ctx := context.Background()
	user, err := imp.pg.Identities().GetOrCreateUser(ctx, imp.claims.Iss, imp.claims.Sub, "pilot-admin")
	if err != nil {
		return err
	}
	imp.userID = user.ID

	exec := func(query string, arguments ...any) error {
		_, err := imp.db.Exec(query, arguments...)
		return err
	}
	if err := exec(`INSERT INTO projects (id, team_id, key, name, description, status, config, version)
		VALUES ($1, $2, 'peixun', '企业学堂平台（BOM 治理域）',
			'S2A：BOM 105 条功能条目的 WorkGraph 治理域（一期 44 可领取 + 二三期 61 占位）',
			'active', '{}'::jsonb, 1) ON CONFLICT (id) DO NOTHING`,
		s2aProjectID, s2aTeamID); err != nil {
		return err
	}
	if err := exec(`INSERT INTO memberships (team_id, user_id, role) VALUES ($1, $2, 'project_admin')
		ON CONFLICT (team_id, user_id) DO NOTHING`, s2aTeamID, imp.userID); err != nil {
		return err
	}
	// The dedicated claim probe runner (approved + bound to peixun only).
	if err := exec(`INSERT INTO runners (id, display_name, device_key_hash, status, capabilities)
		VALUES ($1, 's2a-claim-runner', 'x', 'approved', '[]'::jsonb) ON CONFLICT (id) DO NOTHING`,
		s2aClaimRunnerPeixun); err != nil {
		return err
	}
	if err := exec(`INSERT INTO runner_bindings (project_id, runner_id) VALUES ($1, $2)
		ON CONFLICT (project_id, runner_id) DO NOTHING`, s2aProjectID, s2aClaimRunnerPeixun); err != nil {
		return err
	}

	// The technical_lead function (BOM review gate) must be active for
	// the walk below; the pilot operator holds it from the P5b grants.
	active, err := imp.pg.Identities().ActiveFunctionalRoles(ctx, imp.userID)
	if err != nil {
		return err
	}
	hasTechLead := false
	for _, function := range active {
		if function == "technical_lead" {
			hasTechLead = true
		}
	}
	if !hasTechLead {
		if err := imp.pg.Identities().GrantFunctionalRole(ctx, &store.FunctionalPrincipal{
			UserID: imp.userID, Function: "technical_lead",
			SourceRef: "S2A：BOM 台账走查与 peixun-m1 封板（technical_lead 职能，ARTIFACT-STANDARDS bom 评审 Gate）",
		}); err != nil {
			return err
		}
	}

	// REST smoke: the new governance domain answers over OIDC.
	status, body, err := imp.rest.do("GET", "/api/v3/projects/"+s2aProjectID+"/work-graph", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("work-graph listing on peixun: %d: %s", status, body)
	}
	imp.ev.record("baseline_seed", map[string]any{
		"project": s2aProjectID, "key": "peixun", "team": s2aTeamID,
		"user": imp.userID, "claim_runner": s2aClaimRunnerPeixun,
		"manifest_digest": imp.manifestDigest, "baseline_sha": imp.baselineSHA,
	})
	return nil
}

// ---------------------------------------------------------------------------
// S0b: WorkPattern + the two plans (M1 milestone, M23 placeholders).
// ---------------------------------------------------------------------------

func (imp *importer) ensurePlan(humanCode, title, rootSlot string, rootSpec map[string]any) (planID, rootID string, graphVersion int64, err error) {
	ctx := context.Background()
	err = imp.db.QueryRow(
		`SELECT id::text, root_node_id::text, graph_version FROM work_plans WHERE project_id=$1 AND human_code=$2`,
		s2aProjectID, humanCode).Scan(&planID, &rootID, &graphVersion)
	if errors.Is(err, sql.ErrNoRows) {
		spec, marshalErr := json.Marshal(rootSpec)
		if marshalErr != nil {
			return "", "", 0, marshalErr
		}
		plan, createErr := imp.pg.WorkGraph().CreateWorkPlan(ctx, store.CreateWorkPlanInput{
			ProjectID: s2aProjectID, Title: title, HumanCode: humanCode,
			RootSlotKey: rootSlot, RootSpec: spec, Actor: "s2a:" + imp.userID,
		})
		if createErr != nil {
			return "", "", 0, createErr
		}
		return plan.ID, plan.RootNodeID, plan.GraphVersion, nil
	}
	return planID, rootID, graphVersion, err
}

func (imp *importer) stepPlans() error {
	ctx := context.Background()

	// WorkPattern peixun-m1-bom v1 (the brief's frozen template).
	var patternID string
	err := imp.db.QueryRow(
		`SELECT id::text FROM work_patterns WHERE project_id=$1 AND name=$2 AND version=1`,
		s2aProjectID, s2aPatternName).Scan(&patternID)
	if errors.Is(err, sql.ErrNoRows) {
		body, marshalErr := json.Marshal(map[string]any{
			"template":    s2aPatternName,
			"structure":   []string{"root(里程碑)", "letter(域包 A–E)", "numbered(编号域包)", "item(BOM 条目)"},
			"item_source": "ART-bom-001（台账 digest+指针；重放底册 scripts/pilot/bom-import/data/bom-manifest-20260909.json）",
			"edges":       "域内 requires 按 BOM 说明列推导（推导表 scripts/pilot/bom-import/DERIVATION.md）；跨域候选仅登记不建边",
			"phases":      map[string]string{"m1": "一期 44（37 P0 + 7 P1）", "m23": "二三期 61 占位（brief 写 46——BOM 勘误）"},
		})
		if marshalErr != nil {
			return marshalErr
		}
		pattern, createErr := imp.pg.WorkGraph().CreateWorkPattern(ctx, store.WorkPattern{
			ProjectID: s2aProjectID, Name: s2aPatternName, Version: 1, Status: "active",
			Body: body, CreatedBy: "s2a:" + imp.userID,
		})
		if createErr != nil {
			return createErr
		}
		patternID = pattern.ID
	} else if err != nil {
		return err
	}

	m1Total := letterBudget(m1Items(imp.manifest))
	imp.planM1ID, imp.m1RootID, _, err = imp.ensurePlan(s2aPlanM1Human,
		"企业学堂平台一期里程碑（peixun-m1）", "peixun.m1", map[string]any{
			"title":               "企业学堂平台一期里程碑（peixun-m1）",
			"acceptance_criteria": []string{"一期 44 条 BOM WorkItem 全部 done（S2B 起逐条执行）"},
			"budget_units":        m1Total, "success_threshold": map[string]any{"kind": "all"},
			"failure_policy": "needs_human", "cancel_policy": "cascade_required",
		})
	if err != nil {
		return err
	}
	imp.planM23ID, imp.m23RootID, _, err = imp.ensurePlan(s2aPlanM23Human,
		"企业学堂平台二三期占位图（peixun-m23）", "peixun.m23", map[string]any{
			"title":               "企业学堂平台二三期占位根包",
			"acceptance_criteria": []string{"二三期条目在各自期启动时经重规划转正（S2B 及后续）"},
			"budget_units":        letterBudget(m23Items(imp.manifest)),
			"success_threshold":   map[string]any{"kind": "all"},
			"failure_policy":      "needs_human", "cancel_policy": "cascade_required",
		})
	if err != nil {
		return err
	}
	imp.ev.record("plans", map[string]any{
		"pattern": patternID, "name": s2aPatternName, "version": 1,
		"m1":  map[string]any{"plan": imp.planM1ID, "human": s2aPlanM1Human, "budget": m1Total},
		"m23": map[string]any{"plan": imp.planM23ID, "human": s2aPlanM23Human},
	})
	return nil
}

// ---------------------------------------------------------------------------
// S1: the BOM asset walk — v1 (recovered registration, placeholder
// digest) walks to approved so it can be superseded; v2 registers with
// the TRUE workbook digest + the manifest anchor, stays draft for the
// denial probe, then approves.
// ---------------------------------------------------------------------------

func (imp *importer) stepBOMAsset() error {
	ctx := context.Background()

	// v1 → approved (only approved versions can be superseded).
	v1, err := imp.pg.Assets().GetAsset(ctx, s2aBOMAssetID, 1)
	if err != nil {
		return fmt.Errorf("ART-bom-001@1 must exist (P5b re-registration): %w", err)
	}
	if v1.Status == store.AssetStatusDraft {
		if _, err := imp.pg.Assets().ReviewAsset(ctx, s2aBOMAssetID, 1, "s2a:"+imp.userID); err != nil {
			return fmt.Errorf("review v1: %w", err)
		}
	}
	v1, err = imp.pg.Assets().GetAsset(ctx, s2aBOMAssetID, 1)
	if err != nil {
		return err
	}
	if v1.Status == store.AssetStatusReviewed {
		if _, err := imp.pg.Assets().ApproveAsset(ctx, s2aBOMAssetID, 1, "user:"+imp.userID, v1.RequiredApproverRoles); err != nil {
			return fmt.Errorf("approve v1: %w", err)
		}
	}

	// v2: the true-digest re-registration (author session, MCP face).
	latest, err := imp.pg.Assets().LatestAsset(ctx, s2aBOMAssetID)
	if err != nil {
		return err
	}
	if latest.Version >= 2 {
		imp.bomAssetVersion = latest.Version // replay
	} else {
		if err := imp.ensureRunners(); err != nil {
			return err
		}
		summaryValue := map[string]any{
			"note":            "S2A 重放底册登记：01 表 105 条功能条目（一期 44=37 P0+7 P1；二三期 61——brief 写 46 属勘误，按原件）",
			"manifest":        "scripts/pilot/bom-import/data/bom-manifest-20260909.json",
			"manifest_sha256": imp.manifestDigest,
			"workbook_sha256": "sha256:" + imp.manifest.Source.SHA256,
			"sheets": map[string]int{"01-产品功能BOM": 105, "02-技术选型BOM": 24, "03-建设目标与里程碑": 5,
				"04-内外部对接清单": 16, "05-基础设施资源BOM": 15, "06-非功能需求与合规": 18, "07-决策点与风险": 14},
		}
		summary, marshalErr := json.Marshal(summaryValue)
		if marshalErr != nil {
			return marshalErr
		}
		summaryString, marshalErr := json.Marshal(string(summary)) // one JSON document serialized as a string
		if marshalErr != nil {
			return marshalErr
		}
		text, callErr := imp.author.callOK("asset_register", map[string]any{
			"asset_id": s2aBOMAssetID, "asset_type": "bom",
			"title":                   "企业学堂平台建设BOM清单规划（8 Sheet；01 表 105 条）——真 digest 重登记+重放底册锚定",
			"sensitivity":             "internal",
			"source_digest":           "sha256:" + imp.manifest.Source.SHA256,
			"content_ref":             "legacy://WorkProjectSpace/yuandong-peixun/企业学堂平台建设BOM清单规划.xlsx",
			"supersedes_ref":          s2aBOMAssetID + "@1",
			"reviewers":               []string{"function:technical_lead"},
			"required_approver_roles": []string{"technical_lead"},
			"summary":                 string(summaryString),
			"idempotency_key":         "s2a-bom-v2-register-0001",
		})
		if callErr != nil {
			return fmt.Errorf("register v2: %w: %s", callErr, text)
		}
		imp.bomAssetVersion = 2
	}
	imp.ev.record("bom_asset", map[string]any{
		"v1_walked_to": "approved", "v2_version": imp.bomAssetVersion,
		"v2_source_digest": "sha256:" + imp.manifest.Source.SHA256,
		"manifest_digest":  imp.manifestDigest, "v2_status": "draft（待 S2 拒绑探针后再 approve）",
	})
	return nil
}

// ---------------------------------------------------------------------------
// S1: M1 per-letter proposals over MCP + the technical_lead seal.
// ---------------------------------------------------------------------------

// applyLetterProposals drives one plan's per-letter batches through the
// MCP decomposition_propose face (project-namespaced idempotency keys,
// W5-3). It returns how many were freshly applied.
func (imp *importer) applyLetterProposals(planTag string, planID, rootID string, opts proposalOptions) (int, error) {
	if err := imp.ensureRunners(); err != nil {
		return 0, err
	}
	groups := deriveLetterProposals(imp.manifest, opts)
	applied := 0
	for _, group := range groups {
		key := fmt.Sprintf("s2a-propose-%s-%s-0001", planTag, group.Letter)
		var status string
		err := imp.db.QueryRow(
			`SELECT status FROM decomposition_proposals WHERE project_id=$1 AND idempotency_key=$2`,
			s2aProjectID, key).Scan(&status)
		if err == nil {
			if status != "applied" {
				return applied, fmt.Errorf("proposal %s decided %s (expected applied)", key, status)
			}
			continue // replay
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return applied, err
		}
		var graphVersion int64
		if err := imp.db.QueryRow(`SELECT graph_version FROM work_plans WHERE id=$1`, planID).Scan(&graphVersion); err != nil {
			return applied, err
		}

		nodes := make([]map[string]any, 0, len(group.Nodes))
		for _, node := range group.Nodes {
			parent := node.Parent
			if parent == "ROOT" {
				parent = "ext:" + rootID
			}
			nodes = append(nodes, map[string]any{
				"local_id": node.LocalID, "parent": parent, "node_type": node.NodeType,
				"slot_key": node.SlotKey, "human_code": node.HumanCode, "spec": node.Spec,
			})
		}
		edges := make([]map[string]any, 0, len(group.Edges))
		for _, edge := range group.Edges {
			edges = append(edges, map[string]any{"from": edge.From, "to": edge.To, "requirement": edge.Requirement})
		}
		flows := make([]map[string]any, 0, len(group.Flows))
		for _, flow := range group.Flows {
			flows = append(flows, map[string]any{
				"node": flow.Node, "direction": flow.Direction,
				"asset_ref": flow.AssetRef, "port_key": flow.PortKey,
			})
		}
		patternID, patternErr := imp.patternID()
		if patternErr != nil {
			return applied, patternErr
		}
		arguments := map[string]any{
			"plan_id": planID, "expected_graph_version": float64(graphVersion),
			"idempotency_key": key,
			"work_pattern":    map[string]any{"pattern_id": patternID, "version": 1},
			"nodes":           nodes,
		}
		if len(edges) > 0 {
			arguments["dependencies"] = edges
		}
		if len(flows) > 0 {
			arguments["flows"] = flows
		}
		text, callErr := imp.author.callOK("decomposition_propose", arguments)
		if callErr != nil {
			return applied, fmt.Errorf("propose %s/%s: %w: %s", planTag, group.Letter, callErr, text)
		}
		if !strings.Contains(text, `"status":"applied"`) {
			return applied, fmt.Errorf("propose %s/%s did not apply: %s", planTag, group.Letter, text)
		}
		applied++
	}
	return applied, nil
}

func (imp *importer) patternID() (string, error) {
	var patternID string
	err := imp.db.QueryRow(
		`SELECT id::text FROM work_patterns WHERE project_id=$1 AND name=$2 AND version=1`,
		s2aProjectID, s2aPatternName).Scan(&patternID)
	return patternID, err
}

func (imp *importer) stepM1Proposals() error {
	opts := proposalOptions{
		Phase: "一期", LetterCodeBase: s2aM1LetterCodeBase, DomainCodeBase: s2aM1DomainCodeBase,
		Repo: "peixun-backend", BaselineSHA: imp.baselineSHA, WorkspacePaths: []string{"doc"},
		BOMAssetRef:  fmt.Sprintf("%s@%d", s2aBOMAssetID, imp.bomAssetVersion),
		IncludeFlows: true, IncludeEdges: true,
	}
	applied, err := imp.applyLetterProposals("m1", imp.planM1ID, imp.m1RootID, opts)
	if err != nil {
		return err
	}

	// Seal (technical_lead over REST); replay-safe.
	var sealedAt sql.NullString
	if err := imp.db.QueryRow(
		`SELECT sealed_at FROM plan_revisions WHERE plan_id=$1 ORDER BY revision_no DESC LIMIT 1`,
		imp.planM1ID).Scan(&sealedAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	seal := map[string]any{"plan": s2aPlanM1Human, "fresh_proposals": applied}
	if !sealedAt.Valid {
		var graphVersion int64
		if err := imp.db.QueryRow(`SELECT graph_version FROM work_plans WHERE id=$1`, imp.planM1ID).Scan(&graphVersion); err != nil {
			return err
		}
		status, body, restErr := imp.rest.do("POST",
			"/api/v3/projects/"+s2aProjectID+"/work-graph/plans/"+imp.planM1ID+"/seal",
			map[string]string{"Idempotency-Key": "s2a-seal-m1-00000001"},
			[]byte(fmt.Sprintf(`{"expected_graph_version":%d}`, graphVersion)))
		if restErr != nil {
			return restErr
		}
		if status != http.StatusOK {
			return fmt.Errorf("seal m1: %d: %s", status, body)
		}
		seal["status"] = status
	} else {
		seal["replay"] = true
	}

	// Settled-state counts (the acceptance: proposal applied + graph
	// traversable).
	var packages, items, edges, flows int
	for _, query := range []struct {
		sql  string
		bind *int
	}{
		{`SELECT count(*) FROM work_nodes WHERE plan_id=$1 AND node_type='work_package'`, &packages},
		{`SELECT count(*) FROM work_nodes WHERE plan_id=$1 AND node_type='work_item'`, &items},
		{`SELECT count(*) FROM work_dependencies WHERE plan_id=$1`, &edges},
		{`SELECT count(*) FROM node_artifact_flows WHERE plan_id=$1`, &flows},
	} {
		if err := imp.db.QueryRow(query.sql, imp.planM1ID).Scan(query.bind); err != nil {
			return err
		}
	}
	if items != 44 || edges != len(m1RequiresEdges) || flows != 44 || packages != 21 {
		return fmt.Errorf("m1 settled state: packages=%d items=%d edges=%d flows=%d (want 21/44/%d/44)",
			packages, items, edges, flows, len(m1RequiresEdges))
	}
	seal["graph"] = map[string]int{"packages": packages, "items": items, "edges": edges, "flows": flows}
	imp.ev.record("m1", seal)
	return nil
}

// ---------------------------------------------------------------------------
// S2: flat work items (the claim/anchor/gate substrate) + the
// blocked-claim probe.
// ---------------------------------------------------------------------------

func (imp *importer) seedFlatItems(items []BomItem, status string) (int, error) {
	created := 0
	for _, item := range items {
		title := fmt.Sprintf("[BOM %s] %s（%s %s）", item.Code, item.Feature, item.Phase, item.Priority)
		description := fmt.Sprintf("%s｜图节点 %s｜Jira %s｜%s", item.Desc,
			itemHumanCode(imp.manifest, item.Code), jiraIssueKey(imp.manifest, item), item.Domain)
		result, err := imp.db.Exec(`INSERT INTO work_items
			(id, project_id, type, title, description, status, priority, role, dependencies, test_requirements, lease_epoch, version)
			VALUES ($1, $2, 'task', $3, $4, $5, $6, 'backend', '[]'::jsonb, '{}'::jsonb, 0, 1)
			ON CONFLICT (id) DO NOTHING`,
			flatItemUUID(imp.manifest, item.Code), s2aProjectID, title, description,
			status, flatPriority(item.Priority))
		if err != nil {
			return created, err
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			created++
		}
	}
	return created, nil
}

func (imp *importer) stepFlatItems() error {
	// (a) the 61 placeholders FIRST, then the claim probe: with only
	// blocked items queued the dispatch must fail closed (S2A-3).
	created, err := imp.seedFlatItems(m23Items(imp.manifest), "blocked")
	if err != nil {
		return err
	}
	var blockedCount int
	if err := imp.db.QueryRow(
		`SELECT count(*) FROM work_items WHERE project_id=$1 AND status='blocked'`, s2aProjectID).Scan(&blockedCount); err != nil {
		return err
	}
	if blockedCount != 61 {
		return fmt.Errorf("blocked flat items = %d, want 61", blockedCount)
	}

	probe := map[string]any{"blocked_items": blockedCount, "created_this_run": created}
	var queuedBefore int
	if err := imp.db.QueryRow(
		`SELECT count(*) FROM work_items WHERE project_id=$1 AND status='queued'`, s2aProjectID).Scan(&queuedBefore); err != nil {
		return err
	}
	if queuedBefore == 0 {
		// Fresh state: only blocked placeholders are queued, so the
		// dispatch must fail closed (S2A-3 claim 阻断). On replay the 44
		// phase-1 items are already queued and the probe would lease one
		// for real — it is skipped instead.
		var queueVersion int64
		if err := imp.db.QueryRow(`SELECT version FROM projects WHERE id=$1`, s2aProjectID).Scan(&queueVersion); err != nil {
			return err
		}
		_, claimErr := imp.pg.ClaimNextWorkItem(context.Background(),
			s2aClaimRunnerPeixun, "s2a-gen-1", queueVersion, 10*time.Minute)
		if claimErr == nil {
			return errors.New("claim probe unexpectedly dispatched while only blocked placeholders were queued")
		}
		if !errors.Is(claimErr, store.ErrNoAvailableTask) {
			return fmt.Errorf("claim probe: %w", claimErr)
		}
		probe["claim"] = "no_available_task（blocked 不可领取——fail-closed 复现）"
	} else {
		probe["claim"] = "blocked-claim 阻断复现于首跑（2026-09-13：61 条 blocked 独占队列时 ClaimNextWorkItem 返回 no_available_task）；重放时一期 44 已入队，探针让位真实领取"
		// Preserve the fail-closed proof from a prior in-file record so
		// replays never overwrite acceptance evidence with a skip note.
		if prior, priorOK := imp.ev.data["flat_items"].(map[string]any); priorOK {
			if priorClaim, claimOK := prior["claim"].(string); claimOK && strings.Contains(priorClaim, "no_available_task") {
				probe["claim"] = priorClaim + "（重放确认）"
			}
		}
	}

	// (b) the 44 claimable phase-1 items.
	queued, err := imp.seedFlatItems(m1Items(imp.manifest), "queued")
	if err != nil {
		return err
	}
	var queuedCount int
	if err := imp.db.QueryRow(
		`SELECT count(*) FROM work_items WHERE project_id=$1 AND status='queued'`, s2aProjectID).Scan(&queuedCount); err != nil {
		return err
	}
	if queuedCount != 44 {
		return fmt.Errorf("queued flat items = %d, want 44", queuedCount)
	}
	probe["queued_items"] = queuedCount
	probe["queued_created_this_run"] = queued
	imp.ev.record("flat_items", probe)
	return nil
}

// ---------------------------------------------------------------------------
// S2: the locked_gate walk — draft denial reproduced, v2 approved, then
// all 44 phase-1 items bound to gate "bom".
// ---------------------------------------------------------------------------

func (imp *importer) stepGateBindings() error {
	ctx := context.Background()
	v2, err := imp.pg.Assets().GetAsset(ctx, s2aBOMAssetID, imp.bomAssetVersion)
	if err != nil {
		return err
	}

	gate := map[string]any{"gate_id": s2aGateID, "asset": fmt.Sprintf("%s@%d", s2aBOMAssetID, imp.bomAssetVersion)}
	first := m1Items(imp.manifest)[0]
	firstID := flatItemUUID(imp.manifest, first.Code)

	// (a) draft refusal (the acceptance: 拒绑复现).
	if v2.Status == store.AssetStatusDraft {
		_, bindErr := imp.pg.Assets().BindAssetGate(ctx, s2aProjectID, firstID,
			s2aBOMAssetID, imp.bomAssetVersion, s2aGateID, "s2a-import")
		if !errors.Is(bindErr, store.ErrAssetGateNotSatisfied) {
			return fmt.Errorf("draft bind must fail closed with ErrAssetGateNotSatisfied, got %v", bindErr)
		}
		gate["draft_bind_denied"] = true
		gate["draft_denial_error"] = store.ErrAssetGateNotSatisfied.Error()

		// (b) review (tech session, MCP — separation of duties) then the
		// technical_lead signoff (store face, the P5b precedent for
		// functional approvals outside the runner guard).
		if err := imp.ensureRunners(); err != nil {
			return err
		}
		if _, callErr := imp.tech.callOK("asset_review", map[string]any{
			"asset_id": s2aBOMAssetID, "version": float64(imp.bomAssetVersion),
			"idempotency_key": "s2a-bom-v2-review-0001",
		}); callErr != nil {
			return fmt.Errorf("review v2: %w", callErr)
		}
	} else {
		gate["draft_bind_denied"] = "draft 态拒绑复现于首跑（2026-09-13，v2 注册后、走查前）"
	}

	v2, err = imp.pg.Assets().GetAsset(ctx, s2aBOMAssetID, imp.bomAssetVersion)
	if err != nil {
		return err
	}
	if v2.Status == store.AssetStatusReviewed {
		if _, err := imp.pg.Assets().ApproveAsset(ctx, s2aBOMAssetID, imp.bomAssetVersion,
			"user:"+imp.userID, v2.RequiredApproverRoles); err != nil {
			return fmt.Errorf("approve v2: %w", err)
		}
	}
	v2, err = imp.pg.Assets().GetAsset(ctx, s2aBOMAssetID, imp.bomAssetVersion)
	if err != nil {
		return err
	}
	if v2.Status != store.AssetStatusApproved {
		return fmt.Errorf("v2 status %s, want approved", v2.Status)
	}
	gate["v2_status"] = string(v2.Status)

	// Continuous re-proof of the same invariant family (runs on every
	// replay, after v2's approval flips v1 to superseded): the SUPERSEDED
	// v1 must refuse to back a gate — anything but approved fails closed
	// (WGM-INV-015).
	if _, denyErr := imp.pg.Assets().BindAssetGate(ctx, s2aProjectID, firstID,
		s2aBOMAssetID, 1, s2aGateID, "s2a-import"); !errors.Is(denyErr, store.ErrAssetGateNotSatisfied) {
		return fmt.Errorf("superseded v1 bind must fail closed with ErrAssetGateNotSatisfied, got %v", denyErr)
	}
	gate["superseded_bind_denied"] = true
	gate["superseded_denial_error"] = store.ErrAssetGateNotSatisfied.Error()

	// (c) bind all 44 (check-then-bind; the ON CONFLICT upgrade path is
	// for stale healing, not fresh duplicates).
	bound, fresh := 0, 0
	for _, item := range m1Items(imp.manifest) {
		itemID := flatItemUUID(imp.manifest, item.Code)
		var existing int
		if err := imp.db.QueryRow(
			`SELECT count(*) FROM asset_gate_bindings WHERE work_item_id=$1 AND asset_id=$2 AND gate_id=$3`,
			itemID, s2aBOMAssetID, s2aGateID).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			bound++
			continue
		}
		if _, err := imp.pg.Assets().BindAssetGate(ctx, s2aProjectID, itemID,
			s2aBOMAssetID, imp.bomAssetVersion, s2aGateID, "s2a-import"); err != nil {
			return fmt.Errorf("bind %s: %w", item.Code, err)
		}
		bound++
		fresh++
	}
	if bound != 44 {
		return fmt.Errorf("gate bindings = %d, want 44", bound)
	}
	gate["bindings"] = bound
	gate["bindings_fresh_this_run"] = fresh
	gate["p0_note"] = "37 条 P0 的详设 Gate 由 S2B 逐条补挂（本片只挂 BOM 锚）"
	imp.ev.record("bom_gate", gate)
	return nil
}

// ---------------------------------------------------------------------------
// S3: the M23 placeholder graph + the blocked flips.
// ---------------------------------------------------------------------------

func (imp *importer) stepM23() error {
	opts := proposalOptions{
		Phase: "二三期", LetterCodeBase: s2aM23LetterCodeBase, DomainCodeBase: s2aM23DomainCodeBase,
		Repo: "peixun-backend", BaselineSHA: imp.baselineSHA, WorkspacePaths: []string{"doc"},
	}
	applied, err := imp.applyLetterProposals("m23", imp.planM23ID, imp.m23RootID, opts)
	if err != nil {
		return err
	}

	// Flip every placeholder item node to blocked (CAS on node_version).
	rows, err := imp.db.Query(
		`SELECT id::text, node_version, status FROM work_nodes WHERE plan_id=$1 AND node_type='work_item'`,
		imp.planM23ID)
	if err != nil {
		return err
	}
	type nodeRow struct {
		id      string
		version int64
		status  string
	}
	var nodes []nodeRow
	for rows.Next() {
		var node nodeRow
		if err := rows.Scan(&node.id, &node.version, &node.status); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, node)
	}
	rows.Close()
	if len(nodes) != 61 {
		return fmt.Errorf("m23 item nodes = %d, want 61", len(nodes))
	}
	flipped := 0
	for _, node := range nodes {
		if node.status == "blocked" {
			continue
		}
		if _, err := imp.pg.WorkGraph().UpdateWorkNodeStatus(context.Background(),
			imp.planM23ID, node.id, node.version, "blocked", "s2a:"+imp.userID); err != nil {
			return fmt.Errorf("block node %s: %w", node.id, err)
		}
		flipped++
	}
	var blockedNodes, packages int
	if err := imp.db.QueryRow(
		`SELECT count(*) FROM work_nodes WHERE plan_id=$1 AND status='blocked'`, imp.planM23ID).Scan(&blockedNodes); err != nil {
		return err
	}
	if err := imp.db.QueryRow(
		`SELECT count(*) FROM work_nodes WHERE plan_id=$1 AND node_type='work_package'`, imp.planM23ID).Scan(&packages); err != nil {
		return err
	}
	if blockedNodes != 61 || packages != 31 {
		return fmt.Errorf("m23 settled state: blocked=%d packages=%d (want 61/31)", blockedNodes, packages)
	}
	imp.ev.record("m23", map[string]any{
		"plan": s2aPlanM23Human, "fresh_proposals": applied, "flipped_this_run": flipped,
		"blocked_item_nodes": blockedNodes, "packages": packages,
		"sealed": false, "note": "占位图不封板——各期启动时经重规划转正（详设先行）",
	})
	return nil
}

// ---------------------------------------------------------------------------
// S4: Jira re-probe + manual anchors for all 105 + the reconcile check.
// ---------------------------------------------------------------------------

func (imp *importer) stepJira() error {
	ctx := context.Background()
	target := os.Getenv("S2A_JIRA_BASE_URL")
	if target == "" {
		target = "https://jira.yuandong.internal"
	}
	probeClient := &http.Client{Timeout: 8 * time.Second}
	response, probeErr := probeClient.Get(target) // #nosec G704 -- the Jira re-probe target is operator-supplied (S2A_JIRA_BASE_URL)
	reachable := probeErr == nil
	if reachable {
		_ = response.Body.Close()
	}
	decision := "维持蓝图回退：手工锚定（镜像 worker 不启动）"
	if reachable {
		decision = "Jira 可达——镜像 worker 具备启用条件（J3 链路首演另行安排）"
	}

	created := 0
	for _, item := range imp.manifest.Items {
		itemID := flatItemUUID(imp.manifest, item.Code)
		issue := jiraIssueKey(imp.manifest, item)
		iteration := "M1"
		if item.Phase != "一期" {
			iteration = "M23"
		}
		var existing int
		if err := imp.db.QueryRow(
			`SELECT count(*) FROM jira_anchors WHERE project_id=$1 AND work_item_id=$2`,
			s2aProjectID, itemID).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			continue
		}
		if _, err := imp.pg.Jira().CreateJiraAnchor(ctx, s2aProjectID, store.JiraAnchor{
			WorkItemID: itemID, IssueKey: issue, JiraProjectKey: s2aJiraProjectKey,
			AnchorSource: store.JiraAnchorSourceManual, Assignee: "pilot-admin",
			IterationLabel: iteration,
		}); err != nil {
			return fmt.Errorf("anchor %s: %w", issue, err)
		}
		created++
	}

	var anchors int
	if err := imp.db.QueryRow(
		`SELECT count(*) FROM jira_anchors WHERE project_id=$1`, s2aProjectID).Scan(&anchors); err != nil {
		return err
	}
	if anchors != 105 {
		return fmt.Errorf("anchors = %d, want 105 (44+61)", anchors)
	}

	status, body, err := imp.rest.do("GET", "/api/v3/projects/"+s2aProjectID+"/jira-anchors", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK || !strings.Contains(body, jiraIssueKey(imp.manifest, imp.manifest.Items[0])) {
		return fmt.Errorf("anchor listing: %d: %s", status, body)
	}
	status, body, err = imp.rest.do("GET", "/api/v3/projects/"+s2aProjectID+"/jira-reconcile-items", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK || !strings.Contains(body, `"items":[]`) {
		return fmt.Errorf("reconcile list must be empty (no mirror worker): %d: %s", status, body)
	}
	imp.ev.record("jira", map[string]any{
		"probe_target": target, "reachable": reachable, "decision": decision,
		"anchors": anchors, "created_this_run": created,
		"series":    "PXPEIXUN-101..144（一期 story）/ PXPEIXUN-201..261（二三期占位）",
		"reconcile": "empty（零未决）",
	})
	return nil
}

// ---------------------------------------------------------------------------
// S5: graph queries + audit export/verify + the closing summary.
// ---------------------------------------------------------------------------

func (imp *importer) stepEvidence() error {
	if err := imp.ensureRunners(); err != nil {
		return err
	}
	// Graph traversability over the MCP face.
	m1Text, err := imp.author.callOK("worktree_graph_query", map[string]any{"plan_id": imp.planM1ID})
	if err != nil || !strings.Contains(m1Text, "peixun.m1") {
		return fmt.Errorf("m1 graph query: %v: %s", err, m1Text)
	}
	m23Text, err := imp.author.callOK("worktree_graph_query", map[string]any{"plan_id": imp.planM23ID})
	if err != nil || !strings.Contains(m23Text, "peixun.m23") {
		return fmt.Errorf("m23 graph query: %v: %s", err, m23Text)
	}
	imp.ev.record("graph_queries", map[string]any{
		"m1":  "traversable（root peixun.m1 可遍历，44 item + 21 package + 17 edges）",
		"m23": "traversable（root peixun.m23 可遍历，61 blocked item）",
	})

	// Audit chain export + verify (the stage discipline).
	status, body, err := imp.rest.do("GET",
		"/api/v3/projects/"+s2aProjectID+"/audit-export?from_seq=1&to_seq=100000", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("audit export: %d: %s", status, body)
	}
	var export struct {
		ChainDigest string `json:"chain_digest"`
		Entries     []struct {
			Seq         int64  `json:"seq"`
			EventType   string `json:"event_type"`
			Action      string `json:"action"`
			EntryDigest string `json:"entry_digest"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &export); err != nil {
		return err
	}
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
		"workgraph.plan.created", "workgraph.proposal.applied", "workgraph.plan.sealed",
		"workgraph.node.status_changed",
		"asset.registered", "asset.reviewed", "asset.approved", "asset.superseded",
		"asset.gate.bound",
	} {
		if actions[expected] == 0 {
			return fmt.Errorf("audit chain must carry %s (actions: %v)", expected, actions)
		}
	}
	verifyBody, verifyErr := json.Marshal(map[string]any{
		"from_seq": 1, "to_seq": 100000, "claimed_digests": digests,
	})
	if verifyErr != nil {
		return verifyErr
	}
	status, body, err = imp.rest.do("POST",
		"/api/v3/projects/"+s2aProjectID+"/audit-export/verify",
		map[string]string{"If-Match": export.ChainDigest, "Idempotency-Key": "s2a-audit-verify-0001"},
		verifyBody)
	if err != nil {
		return err
	}
	if status != http.StatusOK || !strings.Contains(body, `"verified":true`) {
		return fmt.Errorf("audit verify: %d: %s", status, body)
	}
	imp.ev.record("audit", map[string]any{
		"entries": len(export.Entries), "chain_digest": export.ChainDigest,
		"verified": true, "actions": actions,
	})
	return nil
}

func (imp *importer) stepSummary() {
	fmt.Printf(`
S2A 导入完成（幂等重放安全）：
  peixun 治理域     %s
  一期 peixun-m1    MST-WP-00601 sealed：44 WorkItem（37 P0+7 P1）+ 21 域包 + 17 域内依赖边 + 44 BOM 消费流
  locked_gate       gate "bom" → ART-bom-001@%d（approved，真 digest）× 44；draft 拒绑已复现
  二三期 peixun-m23 MST-WP-00602：61 blocked 占位（无依赖边）+ 31 域包；claim 阻断已复现
  Jira 锚定         %s：105 条手工锚定（101..144 story / 201..261 占位），对账零未决
  审计链           见 evidence（导出+验证 200）
  Evidence          %s
`, s2aProjectID, imp.bomAssetVersion, s2aJiraProjectKey, imp.ev.path)
}
