package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
)

// PG-gated J2b protocol storage (migration 0019): proposal apply /
// reject / idempotency, seal + replan with stale propagation and
// attempt pinning, graph-path claim with the ExecutionEnvelope and
// budget integration, lease fencing, expiry sweep, aggregation and
// cancellation projection.

type graphFixture struct {
	db        *sql.DB
	graph     pgWorkGraphStore
	projectID string
	pattern   *WorkPattern
	plan      *WorkPlan
	root      *WorkNode
}

func seedGraphFixture(t *testing.T, salt int) *graphFixture {
	t.Helper()
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	graph := tNewStore(t, db).WorkGraph()
	projectID := tSeedProject(t, db, "wgp", salt)

	pattern, err := graph.CreateWorkPattern(ctx, WorkPattern{
		ProjectID: projectID, Name: "pilot-decompose", Version: 1, Status: "active",
		Body: []byte(`{"slots":["api","impl"]}`), CreatedBy: "tech-lead",
	})
	require.NoError(t, err)

	plan, err := graph.CreateWorkPlan(ctx, CreateWorkPlanInput{
		ProjectID: projectID, Title: "Pilot wave", HumanCode: "MST-WP-00001",
		RootSpec: []byte(`{"goal":"ship pilot"}`), Actor: "tech-lead",
	})
	require.NoError(t, err)
	root, err := graph.GetWorkNode(ctx, plan.ID, plan.RootNodeID)
	require.NoError(t, err)
	return &graphFixture{db: db, graph: graph, projectID: projectID, pattern: pattern, plan: plan, root: root}
}

func wgItemSpec(budget int64, capability string) workgraph.NodeSpec {
	prio := 2
	return workgraph.NodeSpec{
		Title: "Implement API", AcceptanceCriteria: []string{"contract tests green"},
		Repo: "peixun-java", BaselineSHA: strings.Repeat("a1", 20),
		WorkspacePaths: []string{"src/main/java"}, OwningCapability: capability,
		BudgetUnits: budget, Priority: &prio,
		FailurePolicy: workgraph.FailureCollectAll, CancelPolicy: workgraph.CancelNone,
	}
}

func twoItemProposal(f *graphFixture) workgraph.DecompositionProposal {
	return workgraph.DecompositionProposal{
		PlanID:               f.plan.ID,
		WorkPattern:          workgraph.WorkPatternRef{PatternID: f.pattern.ID, Version: 1},
		ExpectedGraphVersion: f.plan.GraphVersion,
		Nodes: []workgraph.ProposalNode{
			{LocalID: "u", Parent: "ext:" + f.root.ID, NodeType: workgraph.NodeTypeItem,
				SlotKey: "upstream", HumanCode: "MST-WI-00101", Spec: wgItemSpec(100, "backend.java")},
			{LocalID: "d", Parent: "ext:" + f.root.ID, NodeType: workgraph.NodeTypeItem,
				SlotKey: "downstream", HumanCode: "MST-WI-00102", Spec: wgItemSpec(200, "backend.java")},
		},
		Dependencies: []workgraph.ProposalEdge{{From: "d", To: "u"}},
	}
}

func submitProposal(t *testing.T, f *graphFixture, p workgraph.DecompositionProposal, key string) *DecompositionProposalRecord {
	t.Helper()
	record, err := f.graph.SubmitDecompositionProposal(context.Background(), SubmitDecompositionProposalInput{
		Proposal: p, IdempotencyKey: key, SubmittedBy: "coordinator-1",
		Limits: workgraph.ProposalLimits{MaxNodes: 10, MaxContainmentDepth: 4, MaxFanOut: 8, BudgetCeilingUnits: 10000,
			Now: time.Now().UTC()},
	})
	require.NoError(t, err)
	return record
}

// --- J2b-1: proposal apply / reject / idempotency -------------------------

func TestDecompositionProposalApplyAndReject(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x41)
	ctx := context.Background()

	legal := twoItemProposal(f)
	record := submitProposal(t, f, legal, "prop-1")
	assert.Equal(t, "applied", record.Status)
	var applied map[string]string
	require.NoError(t, json.Unmarshal(record.AppliedNodeIDs, &applied))
	assert.Len(t, applied, 2)

	nodes, err := f.graph.ListWorkNodes(ctx, f.plan.ID)
	require.NoError(t, err)
	// root + two new nodes; new work items start queued for dispatch
	// (claim still requires a sealed revision).
	require.Len(t, nodes, 3)
	for _, n := range nodes {
		if n.ID == f.root.ID {
			continue
		}
		if n.NodeType == workgraph.NodeTypeItem {
			assert.Equal(t, "queued", n.Status)
		}
	}
	updated, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	assert.Equal(t, f.plan.GraphVersion+1, updated.GraphVersion, "apply bumps the graph version")

	// Replay: same idempotency key returns the identical decision.
	replay := submitProposal(t, f, legal, "prop-1")
	assert.Equal(t, record.ID, replay.ID)
	assert.Equal(t, "applied", replay.Status)

	// Rejection with stable codes: duplicate slot under the same parent.
	bad := twoItemProposal(f)
	bad.Nodes = bad.Nodes[:1]
	bad.Dependencies = nil
	bad.ExpectedGraphVersion = updated.GraphVersion
	bad.Nodes[0].SlotKey = "upstream" // taken by the applied proposal
	rejected := submitProposal(t, f, bad, "prop-2")
	assert.Equal(t, "rejected", rejected.Status)
	var violations []workgraph.Violation
	require.NoError(t, json.Unmarshal(rejected.Violations, &violations))
	codes := map[string]bool{}
	for _, v := range violations {
		codes[v.Code] = true
	}
	assert.True(t, codes["WGP-VS-002"], "violations: %s", rejected.Violations)
}

func TestDecompositionProposalOnSealedPlanRequiresReplan(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x42)
	ctx := context.Background()

	first := submitProposal(t, f, twoItemProposal(f), "prop-seal-1")
	require.Equal(t, "applied", first.Status)
	plan, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)

	sealed, err := f.graph.SealPlanRevision(ctx, f.plan.ID, plan.GraphVersion, "tech-lead")
	require.NoError(t, err)
	assert.Equal(t, "sealed", sealed.Status)

	later := twoItemProposal(f)
	plan, err = f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	later.ExpectedGraphVersion = plan.GraphVersion
	_, err = f.graph.SubmitDecompositionProposal(ctx, SubmitDecompositionProposalInput{
		Proposal: later, IdempotencyKey: "prop-seal-2", SubmittedBy: "coordinator-1",
		Limits: workgraph.ProposalLimits{Now: time.Now().UTC()},
	})
	require.ErrorIs(t, err, ErrRevisionSealed)
}

// --- J2b-2: seal immutability, replan, stale propagation, pinning ---------

func TestReplanStalePropagationAndAttemptPinning(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x43)
	ctx := context.Background()

	require.Equal(t, "applied", submitProposal(t, f, twoItemProposal(f), "prop-rp-1").Status)
	plan, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	revision1, err := f.graph.SealPlanRevision(ctx, f.plan.ID, plan.GraphVersion, "tech-lead")
	require.NoError(t, err)

	nodes, err := f.graph.ListWorkNodes(ctx, f.plan.ID)
	require.NoError(t, err)
	nodeByCode := map[string]*WorkNode{}
	for _, n := range nodes {
		nodeByCode[n.HumanCode] = n
	}
	upstream := nodeByCode["MST-WI-00101"]

	// Claim the upstream node against revision 1, then retire the
	// attempt (failed against the old spec) before replanning — the
	// one-active-attempt invariant (WGM-INV-005) holds while it runs.
	claim, err := f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-a", Role: "backend",
		SessionID: "sess-1", WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		IdempotencyKey: "claim-rp-1", ExpectedQueueVersion: 0, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	})
	require.NoError(t, err)
	require.Equal(t, upstream.ID, claim.Envelope.NodeID)
	attempt1, err := f.graph.GetExecutionAttempt(ctx, claim.Envelope.ExecutionAttemptID)
	require.NoError(t, err)
	require.NoError(t, f.graph.CompleteWorkNodeAttempt(ctx, CompleteWorkNodeAttemptInput{
		AttemptID: attempt1.ID, WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		Outcome: "failed", Summary: "old spec red", UsageUnits: 10, Actor: "worker-a",
	}))

	// Sealed revisions are immutable at the trigger level.
	_, err = f.db.ExecContext(ctx, `UPDATE plan_revisions SET node_manifest = '[]'::jsonb WHERE id = $1`, revision1.ID)
	require.ErrorContains(t, err, "PLAN_REVISION_SEALED_IMMUTABLE")

	// Replan: override the upstream spec (budget change), carry the rest.
	newSpec := wgItemSpec(300, "backend.java")
	newSpec.Title = "Implement API v2"
	plan, err = f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	revision2, err := f.graph.ReplanPlanRevision(ctx, ReplanPlanRevisionInput{
		PlanID: f.plan.ID, ExpectedGraphVersion: plan.GraphVersion,
		SpecOverrides: map[string][]byte{upstream.ID: mustJSON(t, newSpec)}, Actor: "tech-lead",
	})
	require.NoError(t, err)
	assert.Equal(t, revision1.RevisionNo+1, revision2.RevisionNo)
	assert.Equal(t, "draft", revision2.Status)
	replannedPlan, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	assert.Equal(t, "replanned", replannedPlan.Status)

	// The old attempt stays pinned to revision 1 and its spec digest.
	pinned, err := f.graph.GetExecutionAttempt(ctx, attempt1.ID)
	require.NoError(t, err)
	assert.Equal(t, attempt1.NodeRevisionID, pinned.NodeRevisionID)
	assert.Equal(t, attempt1.SpecDigest, pinned.SpecDigest)
	assert.Equal(t, "failed", pinned.Status)

	// Node revisions are append-only: the override created a NEW row,
	// the old one still exists.
	var revisionRows int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM work_node_revisions WHERE node_id = $1`, upstream.ID).Scan(&revisionRows))
	assert.Equal(t, 2, revisionRows)

	// Stale propagation: the overridden upstream reset to queued (its
	// failed outcome was derived against the old spec) and, because the
	// current revision is a draft, nothing is claimable until re-seal.
	overridden, err := f.graph.GetWorkNode(ctx, f.plan.ID, upstream.ID)
	require.NoError(t, err)
	assert.Equal(t, "queued", overridden.Status)
	_, err = f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-a", Role: "backend",
		SessionID: "sess-2", WorkerID: "runner-1", ConnectionGeneration: "gen-2",
		IdempotencyKey: "claim-rp-2", ExpectedQueueVersion: 1, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	})
	require.ErrorIs(t, err, ErrNoAvailableTask, "draft revision blocks dispatch")

	// Re-seal revision 2: the upstream is claimable again as a retry
	// (new attempt, retry_of the pinned one), the downstream stays
	// blocked until the upstream is done.
	plan, err = f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	_, err = f.graph.SealPlanRevision(ctx, f.plan.ID, plan.GraphVersion, "tech-lead")
	require.NoError(t, err)
	resealed, err := f.graph.CurrentPlanRevision(ctx, f.plan.ID)
	require.NoError(t, err)
	claim2, err := f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-b", Role: "backend",
		SessionID: "sess-3", WorkerID: "runner-2", ConnectionGeneration: "gen-3",
		IdempotencyKey: "claim-rp-3", ExpectedQueueVersion: 1, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	})
	require.NoError(t, err)
	assert.Equal(t, upstream.ID, claim2.Envelope.NodeID)
	assert.NotEqual(t, claim2.Envelope.NodeRevisionID, attempt1.NodeRevisionID,
		"the retry binds the NEW node revision")
	attempt2, err := f.graph.GetExecutionAttempt(ctx, claim2.Envelope.ExecutionAttemptID)
	require.NoError(t, err)
	assert.Equal(t, 2, attempt2.AttemptNo)
	assert.Equal(t, attempt1.ID, attempt2.RetryOfAttemptID)
	_ = resealed

	// Stale propagation through readiness: the downstream requires the
	// upstream and cannot be claimed while it is executing.
	_, err = f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-c", Role: "backend",
		SessionID: "sess-4", WorkerID: "runner-3", ConnectionGeneration: "gen-4",
		IdempotencyKey: "claim-rp-4", ExpectedQueueVersion: 2, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	})
	require.ErrorIs(t, err, ErrNoAvailableTask)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

// --- J2b-3: claim readiness, envelope, idempotency, queue CAS -------------

func TestClaimReadinessEnvelopeIdempotency(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not run; set MAESTRO_TEST_POSTGRES_DSN")
	}
	f := seedGraphFixture(t, 0x44)
	ctx := context.Background()

	require.Equal(t, "applied", submitProposal(t, f, twoItemProposal(f), "prop-cl-1").Status)
	plan, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	_, err = f.graph.SealPlanRevision(ctx, f.plan.ID, plan.GraphVersion, "tech-lead")
	require.NoError(t, err)

	input := ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-a", Role: "backend",
		SessionID: "sess-1", WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		IdempotencyKey: "claim-cl-1", ExpectedQueueVersion: 0, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	}
	claim, err := f.graph.ClaimNextWorkNode(ctx, input)
	require.NoError(t, err)

	// The envelope carries the full WGS §7 field set.
	env := claim.Envelope
	assert.Equal(t, "MST-WI-00101", env.TaskID, "upstream first: same priority, created earlier")
	assert.NotEmpty(t, env.NodeRevisionID)
	assert.NotEmpty(t, env.ExecutionAttemptID)
	assert.Regexp(t, `^[0-9a-f-]{36}$`, env.LeaseToken)
	assert.Equal(t, int64(1), env.LeaseEpoch)
	assert.Contains(t, env.WorkspacePath, "worktrees")
	assert.Equal(t, "gen-1", env.Generation)
	assert.Equal(t, strings.Repeat("a1", 20), env.BaseSHA)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, env.ContextDigest)
	assert.Equal(t, int64(100), env.BudgetUnits)
	assert.False(t, env.LeaseExpiresAt.Before(time.Now().UTC()))

	// The stored context set is the ADR §9 minimal context.
	attempt, err := f.graph.GetExecutionAttempt(ctx, env.ExecutionAttemptID)
	require.NoError(t, err)
	var contextSet workgraph.ContextSet
	require.NoError(t, json.Unmarshal(attempt.ContextSet, &contextSet))
	assert.Equal(t, "peixun-java", contextSet.Repo)
	assert.Equal(t, strings.Repeat("a1", 20), contextSet.BaseSHA)
	assert.Equal(t, int64(100), contextSet.BudgetUnits)
	assert.NotEmpty(t, contextSet.AcceptanceCriteria)

	// Stale queue token conflicts (same discipline as the flat claim).
	stale := input
	stale.IdempotencyKey = "claim-cl-2"
	_, err = f.graph.ClaimNextWorkNode(ctx, stale)
	require.ErrorIs(t, err, ErrConcurrentConflict)

	// Idempotent replay with the same key returns the same envelope.
	fresh := input
	fresh.ExpectedQueueVersion = 1
	replayed, err := f.graph.ClaimNextWorkNode(ctx, fresh)
	require.NoError(t, err)
	assert.Equal(t, env.ExecutionAttemptID, replayed.Envelope.ExecutionAttemptID)

	// Capability routing: a worker without the owning capability gets
	// nothing (WGS-RULE-004 — role is not a capability).
	noCaps := input
	noCaps.IdempotencyKey = "claim-cl-3"
	noCaps.ExpectedQueueVersion = 1
	noCaps.Capabilities = []string{"frontend.vue"}
	_, err = f.graph.ClaimNextWorkNode(ctx, noCaps)
	require.ErrorIs(t, err, ErrNoAvailableTask)

	// Readiness conjunction: the downstream requires the (executing)
	// upstream and stays undispatched until it is done.
	downstreamClaim := input
	downstreamClaim.IdempotencyKey = "claim-cl-4"
	downstreamClaim.ExpectedQueueVersion = 1
	_, err = f.graph.ClaimNextWorkNode(ctx, downstreamClaim)
	require.ErrorIs(t, err, ErrNoAvailableTask)

	// 'done' arrives from the gate/evaluation surface (agents never
	// self-mark); simulate the gate pass and the downstream unlocks.
	require.NoError(t, f.graph.CompleteWorkNodeAttempt(ctx, CompleteWorkNodeAttemptInput{
		AttemptID: env.ExecutionAttemptID, WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		Outcome: "succeeded", Summary: "api done", UsageUnits: 40, Actor: "worker-a",
	}))
	node, err := f.graph.GetWorkNode(ctx, f.plan.ID, env.NodeID)
	require.NoError(t, err)
	assert.Equal(t, "validating", node.Status, "a succeeded attempt validates; it is never 'done' by the agent")
	_, err = f.db.ExecContext(ctx, `UPDATE work_nodes SET status = 'done' WHERE id = $1`, env.NodeID)
	require.NoError(t, err)

	downstreamClaim.ExpectedQueueVersion = 1
	unlocked, err := f.graph.ClaimNextWorkNode(ctx, downstreamClaim)
	require.NoError(t, err)
	assert.Equal(t, "MST-WI-00102", unlocked.Envelope.TaskID)
}

// --- J2b-3: budget ledger integration -------------------------------------

func TestClaimBudgetLedgerIntegration(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x45)
	ctx := context.Background()

	require.Equal(t, "applied", submitProposal(t, f, twoItemProposal(f), "prop-bu-1").Status)
	plan, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	_, err = f.graph.SealPlanRevision(ctx, f.plan.ID, plan.GraphVersion, "tech-lead")
	require.NoError(t, err)

	claim, err := f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-a", Role: "backend",
		SessionID: "sess-1", WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		IdempotencyKey: "claim-bu-1", ExpectedQueueVersion: 0, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	})
	require.NoError(t, err)
	attempt, err := f.graph.GetExecutionAttempt(ctx, claim.Envelope.ExecutionAttemptID)
	require.NoError(t, err)
	require.NotEmpty(t, attempt.BudgetLedgerID)

	// The claim reserved the full spec budget.
	var scopeKind, state string
	var reserved int64
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT scope_kind, state, reserved_units FROM budget_ledgers WHERE id = $1`,
		attempt.BudgetLedgerID).Scan(&scopeKind, &state, &reserved))
	assert.Equal(t, "work_node", scopeKind)
	assert.Equal(t, "open", state)
	assert.Equal(t, int64(100), reserved)

	// Completion settles: spend the reported usage, release the rest.
	require.NoError(t, f.graph.CompleteWorkNodeAttempt(ctx, CompleteWorkNodeAttemptInput{
		AttemptID: claim.Envelope.ExecutionAttemptID, WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		Outcome: "failed", Summary: "tests red", UsageUnits: 70, Actor: "worker-a",
	}))
	var spent, releasedReserved int64
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT spent_units, reserved_units FROM budget_ledgers WHERE id = $1`,
		attempt.BudgetLedgerID).Scan(&spent, &releasedReserved))
	assert.Equal(t, int64(70), spent)
	assert.Equal(t, int64(0), releasedReserved)

	var directions []string
	rows, err := f.db.QueryContext(ctx, `
		SELECT direction FROM budget_entries WHERE ledger_id = $1 ORDER BY entry_seq`, attempt.BudgetLedgerID)
	require.NoError(t, err)
	for rows.Next() {
		var d string
		require.NoError(t, rows.Scan(&d))
		directions = append(directions, d)
	}
	rows.Close()
	assert.Equal(t, []string{"reserve", "spend", "release"}, directions)

	// A stopped ledger removes the node from the ready set (WGS-REQ-001
	// budget reservation is a conjunction).
	require.NoError(t, f.db.QueryRowContext(ctx, `UPDATE budget_ledgers SET state='stopped', stop_reason='manual_stop' WHERE id=$1 RETURNING state`, attempt.BudgetLedgerID).Scan(new(string)))
	// reset the node to queued so only the budget gate can block it
	_, err = f.db.ExecContext(ctx, `UPDATE work_nodes SET status='queued' WHERE id = $1`, claim.Envelope.NodeID)
	require.NoError(t, err)
	_, err = f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-b", Role: "backend",
		SessionID: "sess-2", WorkerID: "runner-2", ConnectionGeneration: "gen-2",
		IdempotencyKey: "claim-bu-2", ExpectedQueueVersion: 1, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	})
	require.ErrorIs(t, err, ErrNoAvailableTask, "stopped budget ledger fails the readiness conjunction")
}

// --- J2b-3: fencing + binding immutability --------------------------------

func TestAttemptFencingAndBindingImmutability(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x46)
	ctx := context.Background()

	require.Equal(t, "applied", submitProposal(t, f, twoItemProposal(f), "prop-fn-1").Status)
	plan, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	_, err = f.graph.SealPlanRevision(ctx, f.plan.ID, plan.GraphVersion, "tech-lead")
	require.NoError(t, err)

	claim, err := f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-a", Role: "backend",
		SessionID: "sess-1", WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		IdempotencyKey: "claim-fn-1", ExpectedQueueVersion: 0, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	})
	require.NoError(t, err)

	// The five-way binding is trigger-immutable.
	_, err = f.db.ExecContext(ctx, `UPDATE execution_attempts SET worker_id = 'evil' WHERE id = $1`, claim.Envelope.ExecutionAttemptID)
	require.ErrorContains(t, err, "EXECUTION_ATTEMPT_BINDING_IMMUTABLE")
	zeroDigest := "sha256:" + strings.Repeat("0", 64)
	_, err = f.db.ExecContext(ctx, `UPDATE execution_attempts SET context_digest = $2 WHERE id = $1`,
		claim.Envelope.ExecutionAttemptID, zeroDigest)
	require.ErrorContains(t, err, "EXECUTION_ATTEMPT_BINDING_IMMUTABLE")
	_, err = f.db.ExecContext(ctx, `DELETE FROM execution_attempts WHERE id = $1`, claim.Envelope.ExecutionAttemptID)
	require.ErrorContains(t, err, "EXECUTION_ATTEMPT")

	// Heartbeat fencing: wrong generation and stale version both lose.
	_, err = f.graph.WorkNodeAttemptHeartbeat(ctx, claim.Envelope.ExecutionAttemptID, "runner-1", "gen-9", 1, time.Minute)
	require.ErrorIs(t, err, ErrRunnerGenerationStale)
	newVersion, err := f.graph.WorkNodeAttemptHeartbeat(ctx, claim.Envelope.ExecutionAttemptID, "runner-1", "gen-1", 1, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(2), newVersion)
	_, err = f.graph.WorkNodeAttemptHeartbeat(ctx, claim.Envelope.ExecutionAttemptID, "runner-1", "gen-1", 1, time.Minute)
	require.ErrorIs(t, err, ErrLeaseVersionMismatch)

	// Completion fences on worker + generation + running.
	err = f.graph.CompleteWorkNodeAttempt(ctx, CompleteWorkNodeAttemptInput{
		AttemptID: claim.Envelope.ExecutionAttemptID, WorkerID: "runner-2", ConnectionGeneration: "gen-1",
		Outcome: "succeeded", Summary: "hijack", Actor: "evil",
	})
	require.ErrorIs(t, err, ErrAttemptNotResumable)
	err = f.graph.CompleteWorkNodeAttempt(ctx, CompleteWorkNodeAttemptInput{
		AttemptID: claim.Envelope.ExecutionAttemptID, WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		Outcome: "mystery", Summary: "?", Actor: "worker-a",
	})
	require.ErrorIs(t, err, ErrInvalidParameter)

	require.NoError(t, f.graph.CompleteWorkNodeAttempt(ctx, CompleteWorkNodeAttemptInput{
		AttemptID: claim.Envelope.ExecutionAttemptID, WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		Outcome: "blocked", Summary: "missing credential", Actor: "worker-a",
	}))
	attempt, err := f.graph.GetExecutionAttempt(ctx, claim.Envelope.ExecutionAttemptID)
	require.NoError(t, err)
	assert.Equal(t, "needs_human", attempt.Status)
	// Terminal is final.
	err = f.graph.CompleteWorkNodeAttempt(ctx, CompleteWorkNodeAttemptInput{
		AttemptID: claim.Envelope.ExecutionAttemptID, WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		Outcome: "succeeded", Summary: "resurrect", Actor: "worker-a",
	})
	require.ErrorIs(t, err, ErrAttemptNotResumable)
}

// --- lease expiry sweep ----------------------------------------------------

func TestExpireStaleAttemptsSweep(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x47)
	ctx := context.Background()

	require.Equal(t, "applied", submitProposal(t, f, twoItemProposal(f), "prop-ex-1").Status)
	plan, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	_, err = f.graph.SealPlanRevision(ctx, f.plan.ID, plan.GraphVersion, "tech-lead")
	require.NoError(t, err)

	claim, err := f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-a", Role: "backend",
		SessionID: "sess-1", WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		IdempotencyKey: "claim-ex-1", ExpectedQueueVersion: 0, LeaseTTL: 200 * time.Millisecond,
		Capabilities: []string{"backend.java"},
	})
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)
	expired, err := f.graph.ExpireStaleWorkNodeAttempts(ctx, "scheduler")
	require.NoError(t, err)
	assert.Equal(t, 1, expired)

	attempt, err := f.graph.GetExecutionAttempt(ctx, claim.Envelope.ExecutionAttemptID)
	require.NoError(t, err)
	assert.Equal(t, "expired", attempt.Status)
	node, err := f.graph.GetWorkNode(ctx, f.plan.ID, claim.Envelope.NodeID)
	require.NoError(t, err)
	assert.Equal(t, "queued", node.Status)

	// The retry is a fresh attempt linked to its predecessor.
	retry, err := f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-b", Role: "backend",
		SessionID: "sess-2", WorkerID: "runner-2", ConnectionGeneration: "gen-2",
		IdempotencyKey: "claim-ex-2", ExpectedQueueVersion: 1, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	})
	require.NoError(t, err)
	retryAttempt, err := f.graph.GetExecutionAttempt(ctx, retry.Envelope.ExecutionAttemptID)
	require.NoError(t, err)
	assert.Equal(t, 2, retryAttempt.AttemptNo)
	assert.Equal(t, claim.Envelope.ExecutionAttemptID, retryAttempt.RetryOfAttemptID)
}

// --- J2b-4: aggregation projection + idempotent replay ---------------------

func TestAggregateWorkPackageProjection(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x48)
	ctx := context.Background()

	// Root package carries all/collect_all/none policies.
	packageProposal := workgraph.DecompositionProposal{
		PlanID:               f.plan.ID,
		WorkPattern:          workgraph.WorkPatternRef{PatternID: f.pattern.ID, Version: 1},
		ExpectedGraphVersion: f.plan.GraphVersion,
		Nodes: []workgraph.ProposalNode{
			{LocalID: "c1", Parent: "ext:" + f.root.ID, NodeType: workgraph.NodeTypeItem,
				SlotKey: "c1", HumanCode: "MST-WI-00101", Spec: wgItemSpec(100, "backend.java")},
			{LocalID: "c2", Parent: "ext:" + f.root.ID, NodeType: workgraph.NodeTypeItem,
				SlotKey: "c2", HumanCode: "MST-WI-00102", Spec: wgItemSpec(100, "backend.java")},
		},
	}
	require.Equal(t, "applied", submitProposal(t, f, packageProposal, "prop-ag-1").Status)
	plan, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	_, err = f.graph.SealPlanRevision(ctx, f.plan.ID, plan.GraphVersion, "tech-lead")
	require.NoError(t, err)

	aggregate := func(evidence bool) *WorkNode {
		node, err := f.graph.AggregateWorkPackage(ctx, AggregateWorkPackageInput{
			PlanID: f.plan.ID, NodeID: f.root.ID, IntegrationEvidence: evidence, Actor: "scheduler",
		})
		require.NoError(t, err)
		return node
	}

	// Children not terminal yet: aggregating.
	assert.Equal(t, "aggregating", aggregate(true).Status)

	// Both done but no independent evidence: fail-closed to needs_human
	// (WGM-INV-010).
	_, err = f.db.ExecContext(ctx, `UPDATE work_nodes SET status='done' WHERE human_code IN ('MST-WI-00101','MST-WI-00102') AND plan_id = $1`, f.plan.ID)
	require.NoError(t, err)
	assert.Equal(t, "needs_human", aggregate(false).Status)

	// Evidence present: satisfied.
	satisfied := aggregate(true)
	assert.Equal(t, "satisfied", satisfied.Status)

	// Idempotent replay: same projection, no version churn.
	version := satisfied.NodeVersion
	assert.Equal(t, "satisfied", aggregate(true).Status)
	replayed, err := f.graph.GetWorkNode(ctx, f.plan.ID, f.root.ID)
	require.NoError(t, err)
	assert.Equal(t, version, replayed.NodeVersion)

	// Failure projection: fail_fast fires on one failed child.
	_, err = f.db.ExecContext(ctx, `UPDATE work_nodes SET status='failed' WHERE human_code = 'MST-WI-00101' AND plan_id = $1`, f.plan.ID)
	require.NoError(t, err)
	_, err = f.db.ExecContext(ctx, `UPDATE work_nodes SET status='done' WHERE human_code = 'MST-WI-00102' AND plan_id = $1`, f.plan.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", aggregate(true).Status)
}

// --- J2b-4: cancellation projection ----------------------------------------

func TestCancelWorkNodeProjection(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x49)
	ctx := context.Background()

	// X requires A (required) and B (optional); cancel X with
	// detach_optional: A follows into cancelled, B detaches.
	proposal := workgraph.DecompositionProposal{
		PlanID:               f.plan.ID,
		WorkPattern:          workgraph.WorkPatternRef{PatternID: f.pattern.ID, Version: 1},
		ExpectedGraphVersion: f.plan.GraphVersion,
		Nodes: []workgraph.ProposalNode{
			{LocalID: "x", Parent: "ext:" + f.root.ID, NodeType: workgraph.NodeTypeItem,
				SlotKey: "x", HumanCode: "MST-WI-00101", Spec: func() workgraph.NodeSpec {
					spec := wgItemSpec(100, "backend.java")
					spec.CancelPolicy = workgraph.CancelDetachOptional
					return spec
				}()},
			{LocalID: "a", Parent: "ext:" + f.root.ID, NodeType: workgraph.NodeTypeItem,
				SlotKey: "a", HumanCode: "MST-WI-00102", Spec: wgItemSpec(100, "backend.java")},
			{LocalID: "b", Parent: "ext:" + f.root.ID, NodeType: workgraph.NodeTypeItem,
				SlotKey: "b", HumanCode: "MST-WI-00103", Spec: wgItemSpec(100, "backend.java")},
		},
		Dependencies: []workgraph.ProposalEdge{
			{From: "x", To: "a", Requirement: workgraph.DependencyRequired},
			{From: "x", To: "b", Requirement: workgraph.DependencyOptional},
		},
	}
	require.Equal(t, "applied", submitProposal(t, f, proposal, "prop-cx-1").Status)

	nodes, err := f.graph.ListWorkNodes(ctx, f.plan.ID)
	require.NoError(t, err)
	byCode := map[string]*WorkNode{}
	for _, n := range nodes {
		byCode[n.HumanCode] = n
	}
	x, a, b := byCode["MST-WI-00101"], byCode["MST-WI-00102"], byCode["MST-WI-00103"]

	require.NoError(t, f.graph.CancelWorkNode(ctx, f.plan.ID, x.ID, x.NodeVersion, "tech-lead"))
	for _, n := range []*WorkNode{x, a} {
		cancelled, err := f.graph.GetWorkNode(ctx, f.plan.ID, n.ID)
		require.NoError(t, err)
		assert.Equal(t, "cancelled", cancelled.Status)
	}
	detached, err := f.graph.GetWorkNode(ctx, f.plan.ID, b.ID)
	require.NoError(t, err)
	assert.Equal(t, "queued", detached.Status, "optional-only descendants detach, not cancel")

	var detachedAudits int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM audit_events WHERE action = 'workgraph.node.detached' AND resource_id = $1`,
		b.ID).Scan(&detachedAudits))
	assert.Equal(t, 1, detachedAudits)

	// Cancelled nodes never re-enter the ready set (WGS-RULE-005).
	plan, err := f.graph.GetWorkPlan(ctx, f.plan.ID)
	require.NoError(t, err)
	_, err = f.graph.SealPlanRevision(ctx, f.plan.ID, plan.GraphVersion, "tech-lead")
	require.NoError(t, err)
	_, err = f.graph.ClaimNextWorkNode(ctx, ClaimWorkNodeInput{
		ProjectID: f.projectID, Principal: "worker-a", Role: "backend",
		SessionID: "sess-1", WorkerID: "runner-1", ConnectionGeneration: "gen-1",
		IdempotencyKey: "claim-cx-1", ExpectedQueueVersion: 0, LeaseTTL: time.Minute,
		Capabilities: []string{"backend.java"},
	})
	require.NoError(t, err, "only the detached optional node remains claimable — it is the one dispatched")
}

// --- Intent layer -----------------------------------------------------------

func TestIntentLayerBinding(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	f := seedGraphFixture(t, 0x4a)
	ctx := context.Background()

	problem, err := f.graph.CreateBusinessProblem(ctx, BusinessProblem{
		ProjectID: f.projectID, Title: "Enterprise learning platform pilot",
		Statement: "Ship the governed pilot on two repositories with artifact gates.", CreatedBy: "product-owner",
	})
	require.NoError(t, err)
	contract, err := f.graph.CreateOutcomeContract(ctx, OutcomeContract{
		ProblemID: problem.ID, SuccessCriteria: []byte(`[{"check":"gates_green"}]`),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, contract.Version)
	capability, err := f.graph.CreateCapability(ctx, Capability{
		ProjectID: f.projectID, CapKey: "backend.java", Description: "Java service work",
	})
	require.NoError(t, err)
	require.NoError(t, f.graph.LinkProblemCapability(ctx, problem.ID, capability.ID))

	require.NoError(t, f.graph.AttachWorkPlanIntent(ctx, WorkPlanIntent{
		PlanID: f.plan.ID, ProblemID: problem.ID, OutcomeContractID: contract.ID, AttachedBy: "product-owner",
	}))

	// Exactly one primary binding per plan (WGP-REQ-001).
	second, err := f.graph.CreateBusinessProblem(ctx, BusinessProblem{
		ProjectID: f.projectID, Title: "Another problem", Statement: "Not this plan's problem.", CreatedBy: "product-owner",
	})
	require.NoError(t, err)
	secondContract, err := f.graph.CreateOutcomeContract(ctx, OutcomeContract{
		ProblemID: second.ID, SuccessCriteria: []byte(`[{"check":"other"}]`),
	})
	require.NoError(t, err)
	err = f.graph.AttachWorkPlanIntent(ctx, WorkPlanIntent{
		PlanID: f.plan.ID, ProblemID: second.ID, OutcomeContractID: secondContract.ID, AttachedBy: "product-owner",
	})
	require.Error(t, err, "a second primary binding must be rejected")

	// Contracts are append-only.
	_, err = f.db.ExecContext(ctx, `UPDATE outcome_contracts SET success_criteria = '[]'::jsonb WHERE id = $1`, contract.ID)
	require.ErrorContains(t, err, "OUTCOME_CONTRACT")
}
