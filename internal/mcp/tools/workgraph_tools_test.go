package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// v3-contract tests for the J2c work-graph/asset tools: graph read
// views, the Coordinator proposal write, ledger registration with
// server-side derivation, separation of duties, honest degradation on
// store-less deployments and the frozen-permission routing.

// fakeWorkGraph is an in-memory WorkGraphStore.
type fakeWorkGraph struct {
	plan      *store.WorkPlan
	nodes     []*store.WorkNode
	revision  *store.PlanRevision
	specs     []*store.WorkNodeRevision
	submitted []store.SubmitDecompositionProposalInput
	records   map[string]*store.DecompositionProposalRecord
	nextErr   error
}

func (f *fakeWorkGraph) GetWorkPlan(_ context.Context, _ string) (*store.WorkPlan, error) {
	if f.plan == nil {
		return nil, store.ErrWorkPlanNotFound
	}
	return f.plan, nil
}

func (f *fakeWorkGraph) ListWorkNodes(_ context.Context, _ string) ([]*store.WorkNode, error) {
	return f.nodes, nil
}

func (f *fakeWorkGraph) CurrentPlanRevision(_ context.Context, _ string) (*store.PlanRevision, error) {
	if f.revision == nil {
		return nil, store.ErrWorkPlanNotFound
	}
	return f.revision, nil
}

func (f *fakeWorkGraph) ListNodeRevisionSpecs(_ context.Context, _, _ string) ([]*store.WorkNodeRevision, error) {
	return f.specs, nil
}

func (f *fakeWorkGraph) SubmitDecompositionProposal(_ context.Context, in store.SubmitDecompositionProposalInput) (*store.DecompositionProposalRecord, error) {
	if f.nextErr != nil {
		return nil, f.nextErr
	}
	// Idempotent replay: the decided record returns verbatim, no second
	// submit (mirrors the store's key replay).
	if existing, ok := f.records[in.IdempotencyKey]; ok {
		return existing, nil
	}
	f.submitted = append(f.submitted, in)
	record := &store.DecompositionProposalRecord{
		ID: "proposal-1", PlanID: in.Proposal.PlanID, Status: "applied",
		ExpectedGraphVersion: in.Proposal.ExpectedGraphVersion,
		AppliedNodeIDs:       []byte(`["node-1"]`),
		DecidedAt:            "2026-09-10T00:00:00Z",
		IdempotencyKey:       in.IdempotencyKey,
	}
	f.records[in.IdempotencyKey] = record
	return record, nil
}

// fakeAssets is an in-memory AssetStore with the lifecycle machine.
type fakeAssets struct {
	rows map[string]*store.Asset
}

func newFakeAssets() *fakeAssets {
	return &fakeAssets{rows: map[string]*store.Asset{}}
}

func (f *fakeAssets) RegisterAsset(_ context.Context, asset store.Asset, _ string) (*store.Asset, error) {
	key := fmt.Sprintf("%s@%d", asset.AssetID, asset.Version)
	if _, exists := f.rows[key]; exists {
		return nil, fmt.Errorf("%w: %s", store.ErrAssetAlreadyRegistered, key)
	}
	if err := store.ValidateAssetRegistration(asset); err != nil {
		return nil, err
	}
	asset.Status = store.AssetStatusDraft
	stored := asset
	f.rows[key] = &stored
	return &stored, nil
}

func (f *fakeAssets) transition(assetID string, version int, to string) (*store.Asset, error) {
	asset, ok := f.rows[fmt.Sprintf("%s@%d", assetID, version)]
	if !ok {
		return nil, store.ErrAssetNotFound
	}
	if !store.AssetTransitionAllowed(asset.Status, to) {
		return nil, fmt.Errorf("%w: %s -> %s", store.ErrAssetTransitionInvalid, asset.Status, to)
	}
	asset.Status = to
	return asset, nil
}

func (f *fakeAssets) ReviewAsset(_ context.Context, assetID string, version int, _ string) (*store.Asset, error) {
	return f.transition(assetID, version, store.AssetStatusReviewed)
}

func (f *fakeAssets) ApproveAsset(_ context.Context, assetID string, version int, _ string, _ []string) (*store.Asset, error) {
	return f.transition(assetID, version, store.AssetStatusApproved)
}

func (f *fakeAssets) ListAssetApprovals(_ context.Context, _ string, _ int) ([]*store.AssetApproval, error) {
	return nil, nil
}

func (f *fakeAssets) GetAsset(_ context.Context, assetID string, version int) (*store.Asset, error) {
	asset, ok := f.rows[fmt.Sprintf("%s@%d", assetID, version)]
	if !ok {
		return nil, store.ErrAssetNotFound
	}
	return asset, nil
}

func (f *fakeAssets) LatestAsset(_ context.Context, assetID string) (*store.Asset, error) {
	var latest *store.Asset
	for _, asset := range f.rows {
		if asset.AssetID != assetID {
			continue
		}
		if latest == nil || asset.Version > latest.Version {
			latest = asset
		}
	}
	if latest == nil {
		return nil, store.ErrAssetNotFound
	}
	return latest, nil
}

func (f *fakeAssets) ListAssets(_ context.Context, _ string) ([]*store.Asset, error) {
	out := []*store.Asset{}
	for _, asset := range f.rows {
		out = append(out, asset)
	}
	return out, nil
}

func workGraphFixture(t *testing.T) (*Services, string, string) {
	t.Helper()
	services, _, projectID, _, sessionID, _ := newMCPContextFixture(t)
	graph := &fakeWorkGraph{records: map[string]*store.DecompositionProposalRecord{}}
	graph.plan = &store.WorkPlan{ID: "plan-1", ProjectID: projectID, Title: "Pilot", HumanCode: "MST-WP-00001", GraphVersion: 2, Status: "proposed", RootNodeID: "node-root"}
	graph.nodes = []*store.WorkNode{
		{ID: "node-root", PlanID: "plan-1", NodeType: store.WorkNodeTypePackage, HumanCode: "MST-WP-00001", SlotKey: "root", Status: "aggregating", Depth: 0},
		{ID: "node-api", PlanID: "plan-1", ParentNodeID: "node-root", NodeType: store.WorkNodeTypeItem, HumanCode: "MST-WI-00101", SlotKey: "api", Status: "done", Depth: 1},
	}
	graph.revision = &store.PlanRevision{ID: "rev-1", PlanID: "plan-1", RevisionNo: 1, Status: "draft", SpecDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000", NodeManifest: []byte(`{"plan_id":"plan-1","nodes":[]}`)}
	specBytes, err := json.Marshal(workgraph.NodeSpec{Title: "API 切片", SuccessThreshold: &workgraph.SuccessThreshold{Kind: "quorum", K: 2}})
	require.NoError(t, err)
	graph.specs = []*store.WorkNodeRevision{{NodeID: "node-root", SpecDigest: "sha256:ab", Spec: specBytes}}
	services.WorkGraph = graph
	services.Assets = newFakeAssets()
	return services, projectID, sessionID
}

func TestWorktreeGraphQueryReturnsTreeView(t *testing.T) {
	services, _, _ := workGraphFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Name = "worktree_graph_query"
	req.Params.Arguments = map[string]any{"plan_id": "plan-1"}
	result, err := handleWorktreeGraphQuery(ctxBG(), req, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	var payload struct {
		Plan    store.WorkPlan    `json:"plan"`
		Nodes   []*store.WorkNode `json:"nodes"`
		Current struct {
			Available bool   `json:"available"`
			Status    string `json:"status"`
		} `json:"current_revision"`
		NodeSpecs []struct {
			NodeID           string `json:"node_id"`
			Title            string `json:"title"`
			SuccessThreshold *struct {
				Kind string `json:"kind"`
				K    int    `json:"k"`
			} `json:"success_threshold"`
		} `json:"node_specs"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &payload))
	assert.Equal(t, "plan-1", payload.Plan.ID)
	require.Len(t, payload.Nodes, 2)
	assert.Equal(t, "node-root", payload.Nodes[0].ID)
	assert.Equal(t, "node-api", payload.Nodes[1].ID)
	assert.Equal(t, "done", payload.Nodes[1].Status)
	assert.True(t, payload.Current.Available)
	require.Len(t, payload.NodeSpecs, 1)
	require.NotNil(t, payload.NodeSpecs[0].SuccessThreshold)
	assert.Equal(t, "quorum", payload.NodeSpecs[0].SuccessThreshold.Kind)
	assert.Equal(t, 2, payload.NodeSpecs[0].SuccessThreshold.K)
}

func TestWorktreeGraphQueryScopeViolationIsNotFound(t *testing.T) {
	services, _, _ := workGraphFixture(t)
	services.WorkGraph.(*fakeWorkGraph).plan.ProjectID = "project-other"
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"plan_id": "plan-1"}
	result, err := handleWorktreeGraphQuery(ctxBG(), req, services)
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, mcpResultText(result), "WORK_PLAN_NOT_FOUND")
}

func TestWorktreeGraphQueryHonestDegradationWithoutStore(t *testing.T) {
	services, _, _, _, _, _ := newMCPContextFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"plan_id": "plan-1"}
	result, err := handleWorktreeGraphQuery(ctxBG(), req, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	assert.Contains(t, mcpResultText(result), `"available":false`)
}

func proposalArguments() map[string]any {
	return map[string]any{
		"plan_id":                "plan-1",
		"expected_graph_version": 2,
		"idempotency_key":        "j2c-propose-idem-0001",
		"work_pattern":           map[string]any{"pattern_id": "pattern-1", "version": 1},
		"nodes": []map[string]any{{
			"local_id": "api", "parent": "node-root", "node_type": "work_item",
			"slot_key": "api.contract", "human_code": "MST-WI-00201",
			"spec": map[string]any{
				"title": "API 契约切片", "repo": "peixun-java",
				"baseline_sha":      "0123456789012345678901234567890123456789",
				"workspace_paths":   []string{"services/api"},
				"owning_capability": "backend.contract",
				"budget_units":      float64(100),
			},
		}},
	}
}

func TestDecompositionProposeSubmitsAndReplays(t *testing.T) {
	services, _, sessionID := workGraphFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = proposalArguments()
	result, err := handleDecompositionPropose(ctxBG(), req, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	var record struct {
		Status      string `json:"status"`
		SubmittedBy string `json:"-"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &record))
	assert.Equal(t, "applied", record.Status)

	graph := services.WorkGraph.(*fakeWorkGraph)
	require.Len(t, graph.submitted, 1)
	assert.Equal(t, "session:"+sessionID, graph.submitted[0].SubmittedBy)
	assert.Equal(t, "plan-1", graph.submitted[0].Proposal.PlanID)
	require.Len(t, graph.submitted[0].Proposal.Nodes, 1)
	assert.Equal(t, "backend.contract", graph.submitted[0].Proposal.Nodes[0].Spec.OwningCapability)

	// Idempotent replay returns the decided record without a second submit.
	result, err = handleDecompositionPropose(ctxBG(), req, services)
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Len(t, graph.submitted, 1)
}

func TestDecompositionProposeSurfacesCASConflict(t *testing.T) {
	services, _, _ := workGraphFixture(t)
	services.WorkGraph.(*fakeWorkGraph).nextErr = store.ErrGraphVersionMismatch
	req := mcp.CallToolRequest{}
	req.Params.Arguments = proposalArguments()
	result, err := handleDecompositionPropose(ctxBG(), req, services)
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, mcpResultText(result), "GRAPH_VERSION_MISMATCH")
}

func TestDecompositionProposeHonestDegradationWithoutStore(t *testing.T) {
	services, _, _, _, _, _ := newMCPContextFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = proposalArguments()
	result, err := handleDecompositionPropose(ctxBG(), req, services)
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, mcpResultText(result), "OPERATION_DISABLED")
}

func registerArguments() map[string]any {
	return map[string]any{
		"asset_id":        "ART-blueprint-001",
		"asset_type":      "blueprint",
		"title":           "试点蓝图 v1",
		"sensitivity":     "internal",
		"source_digest":   "sha256:" + "11"[:0] + "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
		"content_ref":     "assets/ART-blueprint-001/blueprint.md",
		"idempotency_key": "j2c-register-idem-0001",
	}
}

func TestAssetRegisterDerivesVersionAndOwner(t *testing.T) {
	services, _, sessionID := workGraphFixture(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = registerArguments()
	result, err := handleAssetRegister(ctxBG(), req, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	var asset struct {
		Version        int    `json:"version"`
		Status         string `json:"status"`
		OwnerPrincipal string `json:"owner_principal"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &asset))
	assert.Equal(t, 1, asset.Version)
	assert.Equal(t, "session:"+sessionID, asset.OwnerPrincipal)
	assert.Equal(t, store.AssetStatusDraft, asset.Status)
}

func TestAssetRegisterLaterVersionRequiresSupersedes(t *testing.T) {
	services, _, _ := workGraphFixture(t)
	first := mcp.CallToolRequest{}
	first.Params.Arguments = registerArguments()
	_, err := handleAssetRegister(ctxBG(), first, services)
	require.NoError(t, err)

	second := mcp.CallToolRequest{}
	args := registerArguments()
	args["title"] = "试点蓝图 v2"
	second.Params.Arguments = args
	result, err := handleAssetRegister(ctxBG(), second, services)
	require.NoError(t, err)
	require.True(t, result.IsError, "a later version without supersedes_ref must fail closed")
	assert.Contains(t, mcpResultText(result), "INVALID_PARAMETER")

	args["supersedes_ref"] = "ART-blueprint-001@1"
	third := mcp.CallToolRequest{}
	third.Params.Arguments = args
	result, err = handleAssetRegister(ctxBG(), third, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	var asset struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &asset))
	assert.Equal(t, 2, asset.Version)
}

func TestAssetReviewAndApproveEnforceSeparationOfDuties(t *testing.T) {
	services, _, sessionID := workGraphFixture(t)
	register := mcp.CallToolRequest{}
	register.Params.Arguments = registerArguments()
	_, err := handleAssetRegister(ctxBG(), register, services)
	require.NoError(t, err)

	// The owning session may neither review nor approve its own asset.
	review := mcp.CallToolRequest{}
	review.Params.Arguments = map[string]any{
		"asset_id": "ART-blueprint-001", "version": float64(1),
		"idempotency_key": "j2c-review-idem-0001",
	}
	result, err := handleAssetTransition(ctxBG(), review, services, "review",
		func(ctx context.Context, assetID string, version int, actor string, approverRoles []string) (*store.Asset, error) {
			return services.Assets.ReviewAsset(ctx, assetID, version, actor)
		})
	require.NoError(t, err)
	require.True(t, result.IsError, mcpResultText(result))
	assert.Contains(t, mcpResultText(result), "FORBIDDEN")
	assert.Contains(t, mcpResultText(result), "separation of duties")

	// A different session reviews and approves cleanly.
	services.Binding.SessionID = "reviewer-session"
	approved, err := handleAssetTransition(ctxBG(), review, services, "review",
		func(ctx context.Context, assetID string, version int, actor string, approverRoles []string) (*store.Asset, error) {
			return services.Assets.ReviewAsset(ctx, assetID, version, actor)
		})
	require.NoError(t, err)
	require.False(t, err == nil && approved.IsError, mcpResultText(approved))

	approve := mcp.CallToolRequest{}
	approve.Params.Arguments = review.Params.Arguments
	result, err = handleAssetTransition(ctxBG(), approve, services, "approve",
		func(ctx context.Context, assetID string, version int, actor string, approverRoles []string) (*store.Asset, error) {
			return services.Assets.ApproveAsset(ctx, assetID, version, actor, approverRoles)
		})
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	var asset struct {
		Status         string `json:"status"`
		OwnerPrincipal string `json:"owner_principal"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &asset))
	assert.Equal(t, store.AssetStatusApproved, asset.Status)
	assert.Equal(t, "session:"+sessionID, asset.OwnerPrincipal) // ownership stayed with the registering session
}

func TestAssetQueryFiltersAndReturnsVersionChains(t *testing.T) {
	services, _, _ := workGraphFixture(t)
	register := mcp.CallToolRequest{}
	register.Params.Arguments = registerArguments()
	_, err := handleAssetRegister(ctxBG(), register, services)
	require.NoError(t, err)

	next := mcp.CallToolRequest{}
	args := registerArguments()
	args["title"] = "试点蓝图 v2"
	args["supersedes_ref"] = "ART-blueprint-001@1"
	next.Params.Arguments = args
	_, err = handleAssetRegister(ctxBG(), next, services)
	require.NoError(t, err)

	query := mcp.CallToolRequest{}
	query.Params.Arguments = map[string]any{"asset_id": "ART-blueprint-001"}
	result, err := handleAssetQuery(ctxBG(), query, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	var payload struct {
		Assets []struct {
			AssetID string `json:"asset_id"`
			Version int    `json:"version"`
		} `json:"assets"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &payload))
	require.Len(t, payload.Assets, 2)
	assert.Equal(t, "ART-blueprint-001", payload.Assets[0].AssetID)
	assert.Equal(t, 1, payload.Assets[0].Version)
	assert.Equal(t, 2, payload.Assets[1].Version)

	filtered := mcp.CallToolRequest{}
	filtered.Params.Arguments = map[string]any{"sensitivity": "confidential"}
	result, err = handleAssetQuery(ctxBG(), filtered, services)
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &payload))
	assert.Empty(t, payload.Assets)
}

func TestWorkGraphToolPermissionsRouteThroughFrozenPolicy(t *testing.T) {
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	guard, err := NewToolGuard(policy)
	require.NoError(t, err)

	membership := func(role string) *model.PrincipalContext {
		return &model.PrincipalContext{
			PrincipalID:        "user:1",
			ProjectMemberships: map[string]string{"proj-wg": role},
		}
	}

	// The J4 family: developer and coordinator sessions propose (the
	// developer-level write); viewer/verifier/project_admin may not.
	for _, role := range []string{"developer", "coordinator"} {
		decision, err := guard.Authorize(context.Background(), "decomposition_propose", membership(role), "proj-wg")
		require.NoError(t, err)
		assert.True(t, decision.Allow, "%s proposes: %v", role, decision.Reasons)
	}
	for _, role := range []string{"viewer", "verifier", "project_admin"} {
		decision, err := guard.Authorize(context.Background(), "decomposition_propose", membership(role), "proj-wg")
		require.NoError(t, err)
		assert.False(t, decision.Allow, "%s must not propose", role)
	}

	// Every project role reads the graph and the ledger (the viewer floor).
	for _, role := range []string{"project_admin", "coordinator", "developer", "verifier", "viewer"} {
		decision, err := guard.Authorize(context.Background(), "worktree_graph_query", membership(role), "proj-wg")
		require.NoError(t, err)
		assert.True(t, decision.Allow, "%s reads graph: %v", role, decision.Reasons)
		decision, err = guard.Authorize(context.Background(), "asset_query", membership(role), "proj-wg")
		require.NoError(t, err)
		assert.True(t, decision.Allow, "%s queries ledger: %v", role, decision.Reasons)
	}

	// Register routes on the developer-level write plane.
	for _, role := range []string{"developer", "coordinator"} {
		decision, err := guard.Authorize(context.Background(), "asset_register", membership(role), "proj-wg")
		require.NoError(t, err)
		assert.True(t, decision.Allow, "%s registers: %v", role, decision.Reasons)
	}
	for _, role := range []string{"viewer", "verifier", "project_admin"} {
		decision, err := guard.Authorize(context.Background(), "asset_register", membership(role), "proj-wg")
		require.NoError(t, err)
		assert.False(t, decision.Allow, "%s must not register", role)
	}

	// Review and approve route on the functional planes only.
	functional := func(function string) *model.PrincipalContext {
		return &model.PrincipalContext{
			PrincipalID:        "user:2",
			ProjectMemberships: map[string]string{"proj-wg": "viewer"},
			FunctionalRoles:    []string{function},
		}
	}
	for _, function := range []string{"technical_lead", "qa_owner"} {
		decision, err := guard.Authorize(context.Background(), "asset_review", functional(function), "proj-wg")
		require.NoError(t, err)
		assert.True(t, decision.Allow, "%s reviews: %v", function, decision.Reasons)
	}
	for _, function := range []string{"product_owner", "technical_lead", "qa_owner", "operations_owner"} {
		decision, err := guard.Authorize(context.Background(), "asset_approve", functional(function), "proj-wg")
		require.NoError(t, err)
		assert.True(t, decision.Allow, "%s releases: %v", function, decision.Reasons)
	}

	projectOnly := membership("project_admin")
	decision, err := guard.Authorize(context.Background(), "asset_approve", projectOnly, "proj-wg")
	require.NoError(t, err)
	assert.False(t, decision.Allow, "project roles never release assets")
	decision, err = guard.Authorize(context.Background(), "asset_review", projectOnly, "proj-wg")
	require.NoError(t, err)
	assert.False(t, decision.Allow, "project roles never review assets")

	// The delegated veto survives the family switch: an agent session
	// holding the functional grant still cannot release an asset.
	agent := functional("technical_lead")
	agent.DelegationID = "delegation-j4"
	decision, err = guard.Authorize(context.Background(), "asset_approve", agent, "proj-wg")
	require.NoError(t, err)
	assert.False(t, decision.Allow, "delegated principals never release assets")
	assert.Contains(t, decision.Reasons[0], "delegated principals")
}

// W5-4: the summary parameter validates BEFORE the store — an
// over-long summary and a bare non-JSON sentence are caller mistakes
// (INVALID_PARAMETER), never a 500 from the canonical-JSON encode.
func TestAssetRegisterSummaryValidatesBeforeStore(t *testing.T) {
	services, _, _ := workGraphFixture(t)

	overLong := mcp.CallToolRequest{}
	args := registerArguments()
	args["asset_id"] = "ART-research-501"
	args["idempotency_key"] = "w5-summary-long-00000001"
	args["summary"] = strings.Repeat("字", maxAssetSummaryRunes+1)
	overLong.Params.Arguments = args
	result, err := handleAssetRegister(ctxBG(), overLong, services)
	require.NoError(t, err)
	require.True(t, result.IsError, "an over-long summary must fail parameter validation")
	assert.Contains(t, mcpResultText(result), "INVALID_PARAMETER")

	bareSentence := mcp.CallToolRequest{}
	args = registerArguments()
	args["asset_id"] = "ART-research-502"
	args["idempotency_key"] = "w5-summary-bare-0000001"
	args["summary"] = "一句裸文本摘要（P5b 首演 500 复现）"
	bareSentence.Params.Arguments = args
	result, err = handleAssetRegister(ctxBG(), bareSentence, services)
	require.NoError(t, err)
	require.True(t, result.IsError, "a bare non-JSON sentence must fail parameter validation")
	// The wire carries the stable code (diagnostic detail never leaves
	// the server); the JSON requirement is asserted via the cause.
	assert.Contains(t, mcpResultText(result), "INVALID_PARAMETER")
	var public struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResultText(result)), &public))
	assert.Equal(t, "INVALID_PARAMETER", public.Code)

	// The accepted shape: one JSON document serialized as a string.
	valid := mcp.CallToolRequest{}
	args = registerArguments()
	args["asset_id"] = "ART-research-503"
	args["idempotency_key"] = "w5-summary-json-00000001"
	args["summary"] = `{"note":"试点结论","decision":"路线 B"}`
	valid.Params.Arguments = args
	result, err = handleAssetRegister(ctxBG(), valid, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
}

// W5-2: the registration declares the multi-sign floor; the fake store
// carries it onto the wire so callers see the contract they signed up
// for (release-note normalization is store-side, proven in PG).
func TestAssetRegisterCarriesRequiredApproverRoles(t *testing.T) {
	services, _, _ := workGraphFixture(t)
	req := mcp.CallToolRequest{}
	args := registerArguments()
	args["asset_id"] = "ART-test-report-501"
	args["idempotency_key"] = "w5-roles-register-000001"
	args["required_approver_roles"] = []any{"qa_owner", "technical_lead"}
	req.Params.Arguments = args
	result, err := handleAssetRegister(ctxBG(), req, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	assert.Contains(t, mcpResultText(result), `"required_approver_roles":["qa_owner","technical_lead"]`)
}

// W5-1: an identity-bound call with zero or several project memberships
// fails closed — the MCP protocol carries no project selector, and
// guessing one would silently retarget writes.
func TestIdentityScopeFailsClosedWithoutExactlyOneMembership(t *testing.T) {
	services, projectID, _ := workGraphFixture(t)

	multi := identity.WithRequestPrincipal(context.Background(), &model.PrincipalContext{
		PrincipalID: "user:multi", Type: model.PrincipalTypeHuman,
		ProjectMemberships: map[string]string{projectID: "developer", "018f6200-0000-7000-8000-0000000000f1": "viewer"},
		FunctionalRoles:    []string{"technical_lead"},
	})
	_, err := services.callProject(multi)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one active project membership")

	none := identity.WithRequestPrincipal(context.Background(), &model.PrincipalContext{
		PrincipalID: "user:none", Type: model.PrincipalTypeHuman,
		ProjectMemberships: map[string]string{},
		FunctionalRoles:    []string{"technical_lead"},
	})
	_, err = services.callProject(none)
	require.Error(t, err)

	// The delegated context (no identity principal) keeps the binding
	// scope; the identity helpers are inert there.
	single := identity.WithRequestPrincipal(context.Background(), &model.PrincipalContext{
		PrincipalID: "user:single", Type: model.PrincipalTypeHuman,
		ProjectMemberships: map[string]string{projectID: "developer"},
		FunctionalRoles:    []string{"technical_lead"},
	})
	scoped, err := services.callProject(single)
	require.NoError(t, err)
	assert.Equal(t, projectID, scoped)
	actor, err := services.callActor(single)
	require.NoError(t, err)
	assert.Equal(t, "user:user:single", actor)
}
