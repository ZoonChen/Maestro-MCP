package workgraph

import "fmt"

// Pure parent-state aggregation (J2b-4, WGP-REQ-003 / WGM-INV-010):
// the projection from child outcomes + JoinPolicy + failure/cancel
// policies to the parent status. It is a total, deterministic function
// of its input — the same child facts always project to the same
// parent status, so event replay converges (ADR-009 §8).

// Child status vocabulary entering aggregation.
const (
	ChildRunning   = "running"
	ChildSucceeded = "succeeded"
	ChildFailed    = "failed"
	ChildCancelled = "cancelled"
	ChildNeedsHuman = "needs_human"
)

// Aggregated parent statuses (work_nodes.status targets).
const (
	ParentAggregating = "aggregating"
	ParentSatisfied   = "satisfied"
	ParentFailed      = "failed"
	ParentNeedsHuman  = "needs_human"
)

// ChildState is one contained child's terminal-or-current outcome as
// observed by the server (never self-reported completion: a child is
// "succeeded" only after its own gate/evaluation pass).
type ChildState struct {
	NodeID string
	Status string
}

// AggregateInput is the complete aggregation basis for one parent.
type AggregateInput struct {
	Policies NodePolicies
	Children []ChildState
	// IntegrationEvidence reports whether independent
	// integration/evaluation evidence exists for the PARENT scope
	// (WGM-INV-010: children-done alone never satisfies a parent).
	IntegrationEvidence bool
}

// NodePolicies are the frozen policy enums stored on the aggregating
// node's spec.
type NodePolicies struct {
	SuccessThreshold SuccessThreshold
	FailurePolicy    string
	CancelPolicy     string
}

// AggregateResult is the projection outcome.
type AggregateResult struct {
	Status string
	Reason string
}

// Aggregate projects the parent status. Precedence (fixed, so the
// function is unambiguous):
//
//  1. fail_fast + any failed child            -> failed (short-circuit)
//  2. needs_human policy + any failed child   -> needs_human
//  3. any child not terminal                  -> aggregating (wait)
//  4. any child failed (collect_all reached)  -> failed / needs_human per policy
//  5. any child cancelled                     -> needs_human (re-plan decision)
//  6. threshold over succeeded children       -> failed when not met
//  7. independent evidence missing            -> needs_human (fail-closed)
//  8. otherwise                               -> satisfied
func Aggregate(in AggregateInput) AggregateResult {
	policies := in.Policies
	if policies.SuccessThreshold.Kind == "" {
		policies.SuccessThreshold.Kind = ThresholdAll
	}
	if policies.FailurePolicy == "" {
		policies.FailurePolicy = FailureCollectAll
	}

	var failed, cancelled, succeeded, running, needsHuman int
	for _, child := range in.Children {
		switch child.Status {
		case ChildFailed:
			failed++
		case ChildCancelled:
			cancelled++
		case ChildSucceeded:
			succeeded++
		case ChildNeedsHuman:
			needsHuman++
		default: // running or unknown: not terminal
			running++
		}
	}

	// 1-2: failure short-circuits.
	if failed > 0 {
		switch policies.FailurePolicy {
		case FailureFailFast:
			return AggregateResult{Status: ParentFailed, Reason: "fail_fast: a child failed"}
		case FailureNeedsHuman:
			return AggregateResult{Status: ParentNeedsHuman, Reason: "failure_policy needs_human: a child failed"}
		}
	}
	// 3: nothing terminal-unfinished pending?
	if running > 0 || needsHuman > 0 {
		return AggregateResult{Status: ParentAggregating, Reason: "children still in flight"}
	}
	if len(in.Children) == 0 {
		return AggregateResult{Status: ParentAggregating, Reason: "no children to aggregate"}
	}
	// 4: all terminal, failures resolve per policy.
	if failed > 0 {
		if policies.FailurePolicy == FailureNeedsHuman {
			return AggregateResult{Status: ParentNeedsHuman, Reason: "failure_policy needs_human: a child failed"}
		}
		return AggregateResult{Status: ParentFailed, Reason: "a child failed"}
	}
	// 5: cancelled children need a human re-plan decision.
	if cancelled > 0 {
		return AggregateResult{Status: ParentNeedsHuman, Reason: "a child was cancelled; re-plan required"}
	}
	// 6: JoinPolicy threshold.
	var met bool
	switch policies.SuccessThreshold.Kind {
	case ThresholdAll:
		met = succeeded == len(in.Children)
	case ThresholdAny:
		met = succeeded >= 1
	case ThresholdQuorum:
		met = succeeded >= policies.SuccessThreshold.K
	default:
		return AggregateResult{Status: ParentNeedsHuman, Reason: fmt.Sprintf("unknown success_threshold %q", policies.SuccessThreshold.Kind)}
	}
	if !met {
		return AggregateResult{Status: ParentFailed, Reason: fmt.Sprintf(
			"success threshold %s not met: %d of %d children succeeded",
			policies.SuccessThreshold.Kind, succeeded, len(in.Children))}
	}
	// 7: evidence fail-closed.
	if !in.IntegrationEvidence {
		return AggregateResult{Status: ParentNeedsHuman,
			Reason: "independent integration/evaluation evidence missing (WGM-INV-010)"}
	}
	// 8.
	return AggregateResult{Status: ParentSatisfied, Reason: "success threshold met with independent evidence"}
}

// DescendantEdge is one requires edge out of the cancellation graph.
type DescendantEdge struct {
	From, To    string
	Requirement string
}

// CancellationProjection returns which descendants to cancel and which
// to detach when node `cancelled` is cancelled, per its cancel_policy:
//
//   - none:             cancel nobody, detach nobody
//   - cascade_required: cancel every descendant reachable through any
//     requires path (the spine is dead, optional work included)
//   - detach_optional:  cancel only descendants reachable through
//     required edges; descendants reachable only via optional edges
//     are detached (they stay independently schedulable)
//
// This is the J2b frozen reading of PRD §5's two cascade verbs; a
// revision changes the model doc, not this comment.
func CancellationProjection(cancelled string, policy string, edges []DescendantEdge) (cancel []string, detach []string) {
	switch policy {
	case CancelNone, "":
		return nil, nil
	case CancelCascadeRequired:
		reach := reachable(cancelled, edges, nil)
		return sortedKeys(reach), nil
	case CancelDetachOptional:
		requiredOnly := filterRequired(edges, true)
		cancelSet := reachable(cancelled, requiredOnly, nil)
		all := reachable(cancelled, edges, nil)
		detachSet := difference(all, cancelSet)
		return sortedKeys(cancelSet), sortedKeys(detachSet)
	default:
		// Unknown policy fails closed to the widest cancellation.
		reach := reachable(cancelled, edges, nil)
		return sortedKeys(reach), nil
	}
}

func filterRequired(edges []DescendantEdge, required bool) []DescendantEdge {
	var out []DescendantEdge
	for _, e := range edges {
		isRequired := e.Requirement == "" || e.Requirement == DependencyRequired
		if isRequired == required {
			out = append(out, e)
		}
	}
	return out
}

// reachable walks the descendant closure over the given edge set.
func reachable(from string, edges []DescendantEdge, seen map[string]bool) map[string]bool {
	if seen == nil {
		seen = map[string]bool{}
	}
	for _, e := range edges {
		if e.From == from && !seen[e.To] {
			seen[e.To] = true
			reachable(e.To, edges, seen)
		}
	}
	return seen
}

func difference(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	// deterministic ordering keeps the projection replay-stable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
