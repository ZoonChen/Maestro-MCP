package workgraph

import (
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var aggregatePolicies = NodePolicies{
	SuccessThreshold: SuccessThreshold{Kind: ThresholdAll},
	FailurePolicy:    FailureCollectAll,
	CancelPolicy:     CancelNone,
}

func children(statuses ...string) []ChildState {
	out := make([]ChildState, 0, len(statuses))
	for i, s := range statuses {
		out = append(out, ChildState{NodeID: string(rune('a' + i)), Status: s})
	}
	return out
}

// Threshold / join semantics.
func TestAggregateJoinThresholds(t *testing.T) {
	policies := aggregatePolicies
	policies.SuccessThreshold = SuccessThreshold{Kind: ThresholdAll}
	assert.Equal(t, ParentSatisfied, Aggregate(AggregateInput{
		Policies: policies, Children: children(ChildSucceeded, ChildSucceeded), IntegrationEvidence: true,
	}).Status)

	policies.SuccessThreshold = SuccessThreshold{Kind: ThresholdAny}
	assert.Equal(t, ParentSatisfied, Aggregate(AggregateInput{
		Policies: policies, Children: children(ChildSucceeded, ChildSucceeded), IntegrationEvidence: true,
	}).Status)
	// Failure resolution precedes the threshold: even "any" cannot
	// satisfy past a failed child under collect_all.
	assert.Equal(t, ParentFailed, Aggregate(AggregateInput{
		Policies: policies, Children: children(ChildSucceeded, ChildFailed),
	}).Status)

	policies.SuccessThreshold = SuccessThreshold{Kind: ThresholdQuorum, K: 2}
	assert.Equal(t, ParentSatisfied, Aggregate(AggregateInput{
		Policies: policies, Children: children(ChildSucceeded, ChildSucceeded, ChildSucceeded), IntegrationEvidence: true,
	}).Status)
	assert.Equal(t, ParentFailed, Aggregate(AggregateInput{
		Policies: policies, Children: children(ChildSucceeded, ChildSucceeded, ChildFailed),
	}).Status, "quorum met on successes but a failure resolves first under collect_all")
}

// Failure policies: fail_fast short-circuits while siblings run;
// collect_all waits; needs_human suspends.
func TestAggregateFailurePolicies(t *testing.T) {
	failFast := aggregatePolicies
	failFast.FailurePolicy = FailureFailFast
	assert.Equal(t, ParentFailed, Aggregate(AggregateInput{
		Policies: failFast, Children: children(ChildFailed, ChildRunning),
	}).Status, "fail_fast fires immediately despite a running sibling")

	collect := aggregatePolicies
	collect.FailurePolicy = FailureCollectAll
	assert.Equal(t, ParentAggregating, Aggregate(AggregateInput{
		Policies: collect, Children: children(ChildFailed, ChildRunning),
	}).Status, "collect_all waits for every child")
	assert.Equal(t, ParentFailed, Aggregate(AggregateInput{
		Policies: collect, Children: children(ChildFailed, ChildSucceeded),
	}).Status, "collect_all fails once every child is terminal")

	human := aggregatePolicies
	human.FailurePolicy = FailureNeedsHuman
	assert.Equal(t, ParentNeedsHuman, Aggregate(AggregateInput{
		Policies: human, Children: children(ChildFailed, ChildRunning),
	}).Status, "needs_human suspends on the first failure")
}

// WGM-INV-010: children-done never satisfies without independent
// integration/evaluation evidence.
func TestAggregateEvidenceFailClosed(t *testing.T) {
	got := Aggregate(AggregateInput{
		Policies: aggregatePolicies,
		Children: children(ChildSucceeded, ChildSucceeded),
	})
	assert.Equal(t, ParentNeedsHuman, got.Status)
	assert.Contains(t, got.Reason, "WGM-INV-010")
}

// Cancelled children escalate to a human re-plan decision.
func TestAggregateCancelledChildNeedsHuman(t *testing.T) {
	got := Aggregate(AggregateInput{
		Policies: aggregatePolicies,
		Children: children(ChildSucceeded, ChildCancelled),
	})
	assert.Equal(t, ParentNeedsHuman, got.Status)
}

// Replay properties (J2b-4 acceptance): determinism, child-order
// independence and idempotence over randomized inputs.
func TestAggregateReplayProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(20260910)) //nolint:gosec // deterministic property seed, not crypto
	statuses := []string{ChildRunning, ChildSucceeded, ChildFailed, ChildCancelled, ChildNeedsHuman}
	thresholds := []SuccessThreshold{
		{Kind: ThresholdAll}, {Kind: ThresholdAny}, {Kind: ThresholdQuorum, K: 2},
	}
	failures := []string{FailureFailFast, FailureCollectAll, FailureNeedsHuman}

	for i := range 500 {
		n := rng.Intn(6)
		states := make([]ChildState, n)
		for j := range n {
			states[j] = ChildState{NodeID: string(rune('a' + j)), Status: statuses[rng.Intn(len(statuses))]}
		}
		input := AggregateInput{
			Policies: NodePolicies{
				SuccessThreshold: thresholds[rng.Intn(len(thresholds))],
				FailurePolicy:    failures[rng.Intn(len(failures))],
				CancelPolicy:     CancelNone,
			},
			Children:            states,
			IntegrationEvidence: rng.Intn(2) == 1,
		}

		first := Aggregate(input)
		// Determinism: identical input, identical projection.
		require.Equal(t, first, Aggregate(input), "input %d", i)

		// Order independence: shuffled children project identically.
		shuffled := append([]ChildState(nil), states...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		shuffledInput := input
		shuffledInput.Children = shuffled
		require.Equal(t, first.Status, Aggregate(shuffledInput).Status, "input %d order independence", i)

		// Idempotent replay: re-projecting the same snapshot is a
		// fixed point (the terminal statuses never re-derive).
		require.Equal(t, first.Status, Aggregate(input).Status, "input %d idempotence", i)

		// Status vocabulary stays closed.
		switch first.Status {
		case ParentAggregating, ParentSatisfied, ParentFailed, ParentNeedsHuman:
		default:
			t.Fatalf("input %d projected unknown status %q", i, first.Status)
		}
	}
}

// Cancellation projection under the three policies.
func TestCancellationProjection(t *testing.T) {
	// Graph: X -> A (required), X -> B (optional), A -> C (required),
	// B -> C (optional).
	edges := []DescendantEdge{
		{From: "X", To: "A", Requirement: DependencyRequired},
		{From: "X", To: "B", Requirement: DependencyOptional},
		{From: "A", To: "C", Requirement: DependencyRequired},
		{From: "B", To: "C", Requirement: DependencyOptional},
	}

	cancel, detach := CancellationProjection("X", CancelNone, edges)
	assert.Empty(t, cancel)
	assert.Empty(t, detach)

	cancel, detach = CancellationProjection("X", CancelCascadeRequired, edges)
	assert.ElementsMatch(t, []string{"A", "B", "C"}, cancel, "full cascade cancels every descendant")
	assert.Empty(t, detach)

	cancel, detach = CancellationProjection("X", CancelDetachOptional, edges)
	// Required spine from X: A; from A: C. B is reachable only via an
	// optional edge and is detached.
	assert.ElementsMatch(t, []string{"A", "C"}, cancel)
	assert.ElementsMatch(t, []string{"B"}, detach)

	// Unknown policy fails closed to the widest cancellation.
	cancel, _ = CancellationProjection("X", "mystery", edges)
	assert.ElementsMatch(t, []string{"A", "B", "C"}, cancel)
}

// The envelope and context digest must be canonical: same context,
// same digest, regardless of map iteration or field order.
func TestContextSetDigestCanonical(t *testing.T) {
	a := ContextSet{
		Repo: "peixun-java", BaseSHA: "abc",
		WorkspacePaths:     []string{"src/a", "src/b"},
		Inputs:             []ContextInput{{Port: "p1", AssetRef: "ART-hld-001@2", AssetDigest: "d1"}},
		AcceptanceCriteria: []string{"c1", "c2"},
		BudgetUnits:        42,
	}
	raw, err := json.Marshal(a)
	require.NoError(t, err)
	digest, err := SpecDigest(raw)
	require.NoError(t, err)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)

	// Reordered JSON of the same content digests identically.
	reordered := `{"budget_units":42,"repo":"peixun-java","base_sha":"abc","workspace_paths":["src/a","src/b"],"inputs":[{"port":"p1","asset_ref":"ART-hld-001@2","asset_digest":"d1"}],"acceptance_criteria":["c1","c2"]}`
	digest2, err := SpecDigest([]byte(reordered))
	require.NoError(t, err)
	assert.Equal(t, digest, digest2)
}
