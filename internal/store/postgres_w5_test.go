package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
)

// PG-gated W5 contract-cleanup storage: the multi-sign asset release
// gate (single sign does not flip, all required roles flip), the
// project-namespaced proposal idempotency key, and the downstream
// waiting surface for stale gate bindings.

func w5RegisterAsset(t *testing.T, assets pgAssetsStore, projectID, assetID, assetType string, declaredRoles []string) *Asset {
	t.Helper()
	asset, err := assets.RegisterAsset(context.Background(), Asset{
		AssetID: assetID, Version: 1, ProjectID: projectID, AssetType: assetType,
		Title: "W5 walk " + assetID, Status: AssetStatusDraft, OwnerPrincipal: "session:owner-1",
		Reviewers: []string{"tech-lead"}, Sensitivity: SensitivityInternal,
		SourceDigest: "sha256:" + strings.Repeat("c5", 32), ContentRef: "assets/" + assetID + "/v1.md",
		RequiredApproverRoles: declaredRoles,
	}, "session:owner-1")
	require.NoError(t, err)
	return asset
}

func TestAssetMultiSignGatePG(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	store := tNewStore(t, db)
	assets := store.Assets()
	projectID := tSeedProject(t, db, "w5a", 0x51)

	// The release-note class carries the frozen product+technical pair
	// even when the registration declares nothing.
	note := w5RegisterAsset(t, assets, projectID, "ART-release-note-501", "release-note", nil)
	assert.Equal(t, []string{FunctionalRoleProductOwner, FunctionalRoleTechnicalLead}, note.RequiredApproverRoles)

	// Declared roles only ever ADD to the type floor and are validated
	// against the functional catalog.
	design := w5RegisterAsset(t, assets, projectID, "ART-detailed-design-501", "detailed-design",
		[]string{FunctionalRoleQAOwner})
	assert.Equal(t, []string{FunctionalRoleQAOwner}, design.RequiredApproverRoles)
	_, err := assets.RegisterAsset(ctx, Asset{
		AssetID: "ART-bom-501", Version: 1, ProjectID: projectID, AssetType: "bom",
		Title: "bad roles", Status: AssetStatusDraft, OwnerPrincipal: "session:owner-1",
		Sensitivity: SensitivityInternal, SourceDigest: "sha256:" + strings.Repeat("c5", 32),
		RequiredApproverRoles: []string{"ceo"},
	}, "session:owner-1")
	assert.ErrorIs(t, err, ErrInvalidParameter)

	// Review precedes every release decision.
	_, err = assets.ReviewAsset(ctx, note.AssetID, 1, "user:reviewer-1")
	require.NoError(t, err)

	// Single sign: the product_owner signature counts, the flip does NOT
	// land, and the partial signoff ledger is visible.
	afterFirst, err := assets.ApproveAsset(ctx, note.AssetID, 1, "user:approver-product", []string{FunctionalRoleProductOwner})
	require.NoError(t, err)
	assert.Equal(t, AssetStatusReviewed, afterFirst.Status, "one signature must not release a multi-sign asset")
	approvals, err := assets.ListAssetApprovals(ctx, note.AssetID, 1)
	require.NoError(t, err)
	require.Len(t, approvals, 1)
	assert.Equal(t, FunctionalRoleProductOwner, approvals[0].ApproverRole)
	assert.Equal(t, "user:approver-product", approvals[0].ApproverPrincipal)

	// A caller holding NONE of the required roles fails closed before
	// any signoff lands.
	_, err = assets.ApproveAsset(ctx, note.AssetID, 1, "user:approver-qa", []string{FunctionalRoleQAOwner})
	assert.ErrorIs(t, err, ErrAssetApprovalRoleRequired)

	// Second sign: technical_lead completes the pair and the flip lands.
	afterSecond, err := assets.ApproveAsset(ctx, note.AssetID, 1, "user:approver-tech", []string{FunctionalRoleTechnicalLead})
	require.NoError(t, err)
	assert.Equal(t, AssetStatusApproved, afterSecond.Status, "both required roles signed: the flip lands")
	assert.NotEmpty(t, afterSecond.ApprovedAt)
	approvals, err = assets.ListAssetApprovals(ctx, note.AssetID, 1)
	require.NoError(t, err)
	assert.Len(t, approvals, 2, "one signoff row per required role")

	// Re-signing an already-released version is an idempotent replay.
	replay, err := assets.ApproveAsset(ctx, note.AssetID, 1, "user:approver-tech", []string{FunctionalRoleTechnicalLead})
	require.NoError(t, err)
	assert.Equal(t, AssetStatusApproved, replay.Status)
	approvals, err = assets.ListAssetApprovals(ctx, note.AssetID, 1)
	require.NoError(t, err)
	assert.Len(t, approvals, 2)

	// A dual-sign release cascades the supersede + stale flip exactly
	// like the single-approve contract (v2 replaces the approved v1).
	_, err = assets.ReviewAsset(ctx, design.AssetID, 1, "user:reviewer-1")
	require.NoError(t, err)
	_, err = assets.ApproveAsset(ctx, design.AssetID, 1, "user:approver-qa", []string{FunctionalRoleQAOwner})
	require.NoError(t, err)

	workItemID := fmt.Sprintf("018f7500-0000-7000-8000-%012d", 0x52)
	_, err = db.ExecContext(ctx,
		`INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, 'w5 waiting gate item', 'queued')`,
		workItemID, projectID)
	require.NoError(t, err)
	_, err = assets.BindAssetGate(ctx, projectID, workItemID, note.AssetID, 1, "w5-gate", "w5-harness")
	require.NoError(t, err)

	// v2 of the release note: register → review → dual sign. The approve
	// that completes the pair supersedes v1 and flips its binding stale.
	v2, err := assets.RegisterAsset(ctx, Asset{
		AssetID: note.AssetID, Version: 2, ProjectID: projectID, AssetType: "release-note",
		Title: "W5 walk v2", Status: AssetStatusDraft, OwnerPrincipal: "session:owner-1",
		Sensitivity: SensitivityInternal, SourceDigest: "sha256:" + strings.Repeat("d6", 32),
		SupersedesRef: note.AssetID + "@1",
	}, "session:owner-1")
	require.NoError(t, err)
	require.Equal(t, []string{FunctionalRoleProductOwner, FunctionalRoleTechnicalLead}, v2.RequiredApproverRoles)
	_, err = assets.ReviewAsset(ctx, note.AssetID, 2, "user:reviewer-1")
	require.NoError(t, err)
	_, err = assets.ApproveAsset(ctx, note.AssetID, 2, "user:approver-product", []string{FunctionalRoleProductOwner})
	require.NoError(t, err)
	completing, err := assets.ApproveAsset(ctx, note.AssetID, 2, "user:approver-tech", []string{FunctionalRoleTechnicalLead})
	require.NoError(t, err)
	assert.Equal(t, AssetStatusApproved, completing.Status)

	superseded, err := assets.GetAsset(ctx, note.AssetID, 1)
	require.NoError(t, err)
	assert.Equal(t, AssetStatusSuperseded, superseded.Status)

	// The waiting surface answers which version the stale gate waits for.
	waiting, err := assets.ListWaitingGateBindings(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, waiting, 1)
	assert.Equal(t, "w5-gate", waiting[0].GateID)
	assert.Equal(t, workItemID, waiting[0].WorkItemID)
	assert.Equal(t, 1, waiting[0].BoundVersion)
	assert.Equal(t, 2, waiting[0].LatestVersion, "the gate waits for the approved successor v2")
	assert.Equal(t, AssetStatusApproved, waiting[0].LatestStatus)

	// The claim-time consumption check fails closed on the stale binding.
	err = assets.CheckAssetGateConsumption(ctx, projectID, workItemID)
	assert.ErrorIs(t, err, ErrAssetGateNotSatisfied)
}

// TestProposalIdempotencyKeyProjectNamespace reproduces the W5-3
// cross-project collision: the same caller key under a second project
// must produce that project's OWN decided proposal, never a replay of
// the first project's decision.
func TestProposalIdempotencyKeyProjectNamespace(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x53)
	// A second, entirely separate project + plan sharing the caller key
	// space (the harness reuses human-minted keys like "poc-propose-1").
	db := f.db
	ctx := context.Background()
	secondProject := tSeedProject(t, db, "w5b", 0x54)
	secondPlan, err := f.graph.CreateWorkPlan(ctx, CreateWorkPlanInput{
		ProjectID: secondProject, Title: "Second project wave", HumanCode: "MST-WP-00002",
		RootSpec: []byte(`{"goal":"collide"}`), Actor: "tech-lead",
	})
	require.NoError(t, err)
	secondRoot, err := f.graph.GetWorkNode(ctx, secondPlan.ID, secondPlan.RootNodeID)
	require.NoError(t, err)

	const sharedKey = "w5-collision-key-0001"
	first := submitProposal(t, f, twoItemProposal(f), sharedKey)
	require.Equal(t, "applied", first.Status)
	assert.Equal(t, f.projectID, first.ProjectID)

	second := workgraph.DecompositionProposal{
		PlanID:               secondPlan.ID,
		WorkPattern:          workgraph.WorkPatternRef{PatternID: f.pattern.ID, Version: 1},
		ExpectedGraphVersion: secondPlan.GraphVersion,
		Nodes: []workgraph.ProposalNode{
			{LocalID: "only", Parent: "ext:" + secondRoot.ID, NodeType: workgraph.NodeTypeItem,
				SlotKey: "solo", HumanCode: "MST-WI-00103", Spec: wgItemSpec(50, "backend.java")},
		},
	}
	secondRecord, err := f.graph.SubmitDecompositionProposal(ctx, SubmitDecompositionProposalInput{
		Proposal: second, IdempotencyKey: sharedKey, ProjectID: secondProject, SubmittedBy: "coordinator-2",
		Limits: workgraph.ProposalLimits{MaxNodes: 10, MaxContainmentDepth: 4, MaxFanOut: 8,
			BudgetCeilingUnits: 10000, Now: time.Now().UTC()},
	})
	require.NoError(t, err, "the same key under another project must NOT replay the first project's decision")
	assert.NotEqual(t, first.ID, secondRecord.ID)
	assert.Equal(t, secondProject, secondRecord.ProjectID)
	assert.Equal(t, "applied", secondRecord.Status)

	// Replay stays scoped: each project's own key lookup returns its own
	// decided record.
	replayedFirst, err := f.graph.GetDecompositionProposalByKey(ctx, f.projectID, sharedKey)
	require.NoError(t, err)
	assert.Equal(t, first.ID, replayedFirst.ID)
	replayedSecond, err := f.graph.GetDecompositionProposalByKey(ctx, secondProject, sharedKey)
	require.NoError(t, err)
	assert.Equal(t, secondRecord.ID, replayedSecond.ID)

	// The store refuses an unscoped submit: the namespace is contractual.
	_, err = f.graph.SubmitDecompositionProposal(ctx, SubmitDecompositionProposalInput{
		Proposal: twoItemProposal(f), IdempotencyKey: "w5-unscoped-key-0001", SubmittedBy: "coordinator-1",
	})
	assert.ErrorIs(t, err, ErrInvalidParameter)
}
