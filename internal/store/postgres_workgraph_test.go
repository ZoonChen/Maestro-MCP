package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated work graph storage (J2a, migration 0017): the CAS discipline,
// the sealed-revision wall (application guard + trigger backstop), the
// executable-only requires graph with application-side cycle detection,
// lineage/artifact-flow edges and the atomic audit+outbox pair.

func TestWorkGraphLifecycle(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	graph := tNewStore(t, db).WorkGraph()
	projectID := tSeedProject(t, db, "wgm", 0x30)

	plan, err := graph.CreateWorkPlan(ctx, CreateWorkPlanInput{
		ProjectID: projectID, Title: "Pilot wave", HumanCode: "MST-WP-00001",
		RootSpec: []byte(`{"goal":"ship pilot"}`), Actor: "tech-lead",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), plan.GraphVersion)
	assert.NotEmpty(t, plan.RootNodeID)
	assert.Equal(t, "draft", plan.Status)

	root, err := graph.GetWorkNode(ctx, plan.ID, plan.RootNodeID)
	require.NoError(t, err)
	assert.Equal(t, WorkNodeTypePackage, root.NodeType)
	assert.Equal(t, 0, root.Depth)
	assert.Empty(t, root.ParentNodeID)

	// Structural additions advance the graph version.
	backend, err := graph.AddWorkNode(ctx, AddWorkNodeInput{
		PlanID: plan.ID, ParentNodeID: plan.RootNodeID, NodeType: WorkNodeTypeItem,
		SlotKey: "backend.contract", HumanCode: "MST-WI-00101",
		Spec: []byte(`{"repo":"peixun-java","baseline":"sha256:ab"}`), ExpectedGraphVersion: 1, Actor: "tech-lead",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, backend.Depth)
	assert.Equal(t, int64(1), backend.NodeVersion)

	frontend, err := graph.AddWorkNode(ctx, AddWorkNodeInput{
		PlanID: plan.ID, ParentNodeID: plan.RootNodeID, NodeType: WorkNodeTypeItem,
		SlotKey: "frontend.contract", HumanCode: "MST-WI-00102",
		Spec: []byte(`{"repo":"peixun-vue","baseline":"sha256:cd"}`), ExpectedGraphVersion: 2, Actor: "tech-lead",
	})
	require.NoError(t, err)

	// Stale graph versions conflict (WGM-INV: CAS replay semantics).
	_, err = graph.AddWorkDependency(ctx, AddWorkDependencyInput{
		PlanID: plan.ID, FromNodeID: backend.ID, ToNodeID: frontend.ID,
		ExpectedGraphVersion: 1, Actor: "tech-lead",
	})
	assert.ErrorIs(t, err, ErrGraphVersionMismatch)

	_, err = graph.AddWorkDependency(ctx, AddWorkDependencyInput{
		PlanID: plan.ID, FromNodeID: backend.ID, ToNodeID: frontend.ID,
		ExpectedGraphVersion: 3, Actor: "tech-lead",
	})
	require.NoError(t, err)

	// The reverse edge closes a cycle and is rejected (WGM-INV-003).
	_, err = graph.AddWorkDependency(ctx, AddWorkDependencyInput{
		PlanID: plan.ID, FromNodeID: frontend.ID, ToNodeID: backend.ID,
		ExpectedGraphVersion: 4, Actor: "tech-lead",
	})
	assert.ErrorIs(t, err, ErrCircularDependency)

	// Work packages never enter the requires graph.
	_, err = graph.AddWorkDependency(ctx, AddWorkDependencyInput{
		PlanID: plan.ID, FromNodeID: plan.RootNodeID, ToNodeID: backend.ID,
		ExpectedGraphVersion: 4, Actor: "tech-lead",
	})
	assert.ErrorIs(t, err, ErrInvalidParameter)

	// Duplicate slot under the same parent is rejected by the unique
	// index (WGM-INV-001).
	_, err = graph.AddWorkNode(ctx, AddWorkNodeInput{
		PlanID: plan.ID, ParentNodeID: plan.RootNodeID, NodeType: WorkNodeTypeItem,
		SlotKey: "backend.contract", HumanCode: "MST-WI-00103",
		Spec: []byte(`{}`), ExpectedGraphVersion: 4, Actor: "tech-lead",
	})
	require.Error(t, err)

	// Node status CAS.
	updated, err := graph.UpdateWorkNodeStatus(ctx, plan.ID, backend.ID, 1, "queued", "scheduler")
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.NodeVersion)
	_, err = graph.UpdateWorkNodeStatus(ctx, plan.ID, backend.ID, 1, "executing", "scheduler")
	assert.ErrorIs(t, err, ErrNodeVersionMismatch)

	// Seal: revision freezes, manifest snapshot recorded.
	revision, err := graph.SealPlanRevision(ctx, plan.ID, 4, "tech-lead")
	require.NoError(t, err)
	assert.Equal(t, "sealed", revision.Status)
	assert.NotEmpty(t, revision.SealedAt)
	assert.Contains(t, string(revision.NodeManifest), "backend.contract")
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, revision.SpecDigest)

	sealedPlan, err := graph.GetWorkPlan(ctx, plan.ID)
	require.NoError(t, err)
	assert.Equal(t, "sealed", sealedPlan.Status)
	assert.Equal(t, int64(5), sealedPlan.GraphVersion)

	// Post-seal structural writes fail closed (application guard).
	_, err = graph.AddWorkNode(ctx, AddWorkNodeInput{
		PlanID: plan.ID, ParentNodeID: plan.RootNodeID, NodeType: WorkNodeTypeItem,
		SlotKey: "late.node", HumanCode: "MST-WI-00104",
		Spec: []byte(`{}`), ExpectedGraphVersion: 5, Actor: "tech-lead",
	})
	assert.ErrorIs(t, err, ErrRevisionSealed)

	// Trigger backstop: sealed revisions and node revisions are
	// tamper-evident at the database level.
	_, err = db.ExecContext(ctx, `UPDATE plan_revisions SET spec_digest = 'sha256:`+"0000000000000000000000000000000000000000000000000000000000000000"+`' WHERE id = $1`, revision.ID)
	require.Error(t, err, "sealed revision UPDATE must be rejected by the trigger")
	_, err = db.ExecContext(ctx, `DELETE FROM work_node_revisions WHERE node_id = $1`, backend.ID)
	require.Error(t, err, "node revision DELETE must be rejected by the trigger")
	_, err = db.ExecContext(ctx, `UPDATE work_nodes SET slot_key = 'hacked.slot' WHERE id = $1`, backend.ID)
	require.Error(t, err, "node structural identity must be immutable")

	// Atomic audit + outbox pairs landed with the graph writes.
	for _, action := range []string{"workgraph.plan.created", "workgraph.node.added", "workgraph.plan.sealed"} {
		var auditCount, outboxCount int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_events WHERE project_id = $1 AND action = $2`, projectID, action).Scan(&auditCount))
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM outbox_events WHERE project_id = $1 AND event_type = $2`, projectID, action).Scan(&outboxCount))
		assert.GreaterOrEqual(t, auditCount, 1, action)
		assert.GreaterOrEqual(t, outboxCount, 1, action)
	}
}

func TestSealRejectsUnboundRequiredInputs(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	store := tNewStore(t, db)
	graph := store.WorkGraph()
	projectID := tSeedProject(t, db, "wgmp", 0x31)

	plan, err := graph.CreateWorkPlan(ctx, CreateWorkPlanInput{
		ProjectID: projectID, Title: "Ports", HumanCode: "MST-WP-00002", Actor: "tech-lead",
	})
	require.NoError(t, err)
	node, err := graph.AddWorkNode(ctx, AddWorkNodeInput{
		PlanID: plan.ID, ParentNodeID: plan.RootNodeID, NodeType: WorkNodeTypeItem,
		SlotKey: "impl.module", HumanCode: "MST-WI-00201",
		Spec: []byte(`{"required_inputs":["spec.hld"]}`), ExpectedGraphVersion: 1, Actor: "tech-lead",
	})
	require.NoError(t, err)

	// WGM-INV-004: unbound required input port blocks the seal.
	_, err = graph.SealPlanRevision(ctx, plan.ID, 2, "tech-lead")
	require.ErrorIs(t, err, ErrSealRejected)

	// The ledger asset the port needs.
	asset, err := store.Assets().RegisterAsset(ctx, Asset{
		AssetID: "ART-hld-001", Version: 1, ProjectID: projectID, AssetType: "hld",
		Title: "HLD", OwnerPrincipal: "arch", Sensitivity: SensitivityInternal,
		SourceDigest: "sha256:" + repeatHex("01", 32), ContentRef: "assets/ART-hld-001/hld.md",
	}, "arch")
	require.NoError(t, err)
	_, err = store.Assets().ReviewAsset(ctx, asset.AssetID, 1, "tech-lead")
	require.NoError(t, err)
	_, err = store.Assets().ApproveAsset(ctx, asset.AssetID, 1, "product", nil)
	require.NoError(t, err)

	require.NoError(t, graph.AddNodeArtifactFlow(ctx, AddNodeArtifactFlowInput{
		PlanID: plan.ID, NodeID: node.ID, Direction: FlowConsumes,
		AssetID: "ART-hld-001", AssetVersion: 1, PortKey: "spec.hld",
		ExpectedGraphVersion: 2, Actor: "tech-lead",
	}))
	revision, err := graph.SealPlanRevision(ctx, plan.ID, 3, "tech-lead")
	require.NoError(t, err)
	assert.Equal(t, "sealed", revision.Status)
	assert.Contains(t, string(revision.NodeManifest), "ART-hld-001@1")
}

func TestLineageEdges(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	graph := tNewStore(t, db).WorkGraph()
	projectID := tSeedProject(t, db, "wgml", 0x32)

	plan, err := graph.CreateWorkPlan(ctx, CreateWorkPlanInput{
		ProjectID: projectID, Title: "Lineage", HumanCode: "MST-WP-00003", Actor: "tech-lead",
	})
	require.NoError(t, err)
	first, err := graph.AddWorkNode(ctx, AddWorkNodeInput{
		PlanID: plan.ID, ParentNodeID: plan.RootNodeID, NodeType: WorkNodeTypeItem,
		SlotKey: "first.pass", HumanCode: "MST-WI-00301", Spec: []byte(`{}`),
		ExpectedGraphVersion: 1, Actor: "tech-lead",
	})
	require.NoError(t, err)
	followup, err := graph.AddWorkNode(ctx, AddWorkNodeInput{
		PlanID: plan.ID, ParentNodeID: plan.RootNodeID, NodeType: WorkNodeTypeItem,
		SlotKey: "followup", HumanCode: "MST-WI-00302", Spec: []byte(`{}`),
		ExpectedGraphVersion: 2, Actor: "tech-lead",
	})
	require.NoError(t, err)

	require.NoError(t, graph.AddNodeLineage(ctx, AddNodeLineageInput{
		PlanID: plan.ID, NodeID: followup.ID, PredecessorNodeID: first.ID,
		Kind: LineageFollowupOf, ExpectedGraphVersion: 3, Actor: "tech-lead",
	}))
	// Invalid kinds and cross-plan predecessors are rejected.
	require.ErrorIs(t, graph.AddNodeLineage(ctx, AddNodeLineageInput{
		PlanID: plan.ID, NodeID: followup.ID, PredecessorNodeID: first.ID,
		Kind: "retry_of", ExpectedGraphVersion: 4, Actor: "tech-lead",
	}), ErrInvalidParameter)
}

// tNewStore wraps the pool into the store aggregate for tests.
func tNewStore(t *testing.T, db *sql.DB) *PostgresStore {
	t.Helper()
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)
	return pg
}

// tSeedProject inserts one team + project row and returns the project id.
func tSeedProject(t *testing.T, db *sql.DB, key string, salt int) string {
	t.Helper()
	ctx := context.Background()
	teamID := fmt.Sprintf("018f5100-0000-7000-8000-%012d", salt)
	projectID := fmt.Sprintf("018f5200-0000-7000-8000-%012d", salt)
	_, err := db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, $2)`, teamID, "team-"+key)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, $3, $4, 'active')`,
		projectID, teamID, key, key+" project")
	require.NoError(t, err)
	return projectID
}

func repeatHex(bytePattern string, times int) string {
	out := make([]byte, 0, len(bytePattern)*times)
	for range times {
		out = append(out, bytePattern...)
	}
	return string(out)
}
