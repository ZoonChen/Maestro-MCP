package evidence

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Gate states (the frozen seven-state model). running is set by pipeline
// observation (S4a); the pure engine never invents it from evidence.
const (
	GatePending = "pending"
	GateRunning = "running"
	GatePassed  = "passed"
	GateFailed  = "failed"
	GateError   = "error"
	GateStale   = "stale"
	GateWaived  = "waived"
)

// gateNamespace namespaces deterministic gate identities.
var gateNamespace = uuid.MustParse("019207c0-0000-7000-8000-00000000aaa1")

// Tuple is the exact evaluation identity: the evaluation unique key is
// (project, source_sha, target_sha, policy digest) plus the work item
// the snapshots hang off.
type Tuple struct {
	ProjectID     string
	WorkItemID    string
	SourceSHA     string
	TargetSHA     string
	PolicyVersion string
}

// StableGateID derives the gate identity from the tuple: re-evaluations
// of the same (work item, check, SHA pair, policy version) address the
// same row, while ANY drift (SHA or policy) mints a new identity —
// which is exactly how a waiver bound to an old gate stops applying
// (QG-RULE-003 invalidation without a sweep).
func StableGateID(tup Tuple, check string) string {
	return uuid.NewSHA1(gateNamespace, []byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		tup.ProjectID, tup.WorkItemID, check, tup.SourceSHA, tup.TargetSHA, tup.PolicyVersion))).String()
}

// GateResult is one evaluated gate snapshot (wire Gate shape).
type GateResult struct {
	GateID      string   `json:"id"`
	Check       string   `json:"check"`
	Required    bool     `json:"required"`
	State       string   `json:"state"`
	EvidenceIDs []string `json:"evidence_ids"`
	Reason      string   `json:"reason,omitempty"`
}

// Verdict is the deterministic evaluation output for one tuple.
type Verdict struct {
	Tuple        Tuple
	PolicyDigest string
	Gates        []GateResult
	Ready        bool
}

// Evaluate aggregates immutable evidence into gate snapshots per
// QG-RULE-004 and EVIDENCE-RULE-002/003/004:
//
//   - only merge_gate evidence bound to the EXACT SHA tuple counts;
//     diagnostic evidence never satisfies a required gate
//   - per producer the newest attempt (or the head of a supersedes
//     chain) is that producer's current state; history is never erased
//   - a gate passes only when every contributing producer's current
//     state is passed — a later success elsewhere cannot cover a
//     standing failure
//   - cancelled/skipped map to failed, parser/system errors to error
//   - missing evidence is pending with the blocking reason "missing"
//   - a valid, unexpired waiver for a waivable check yields waived
//
// Ready requires every required gate to be passed or waived.
func Evaluate(tup Tuple, policy *EffectivePolicy, records []Record, waivers []Waiver, now time.Time) (*Verdict, error) {
	if policy == nil || policy.Policy == nil {
		return nil, fmt.Errorf("evaluate: effective policy is required")
	}
	if err := validateTuple(tup); err != nil {
		return nil, err
	}

	// Superseded records leave the working set entirely: the correction
	// path replaces them (EVIDENCE-RULE-001), history stays in storage.
	superseded := map[string]bool{}
	seen := map[string]bool{}
	working := make([]Record, 0, len(records))
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return nil, err
		}
		if record.Supersedes != "" {
			superseded[record.Supersedes] = true
		}
		if seen[record.EvidenceID] {
			continue
		}
		seen[record.EvidenceID] = true
		working = append(working, record)
	}
	heads := make([]Record, 0, len(working))
	for _, record := range working {
		if !superseded[record.EvidenceID] {
			heads = append(heads, record)
		}
	}

	verdict := &Verdict{Tuple: tup, PolicyDigest: policy.PolicyDigest}
	for _, check := range policy.Policy.RequiredGates {
		gate := GateResult{
			GateID:      StableGateID(tup, check),
			Check:       check,
			Required:    true, // every gate in required_gates is required
			EvidenceIDs: []string{},
		}

		if waiver, ok := applicableWaiver(waivers, tup, check, now); ok &&
			!slices.Contains(policy.Policy.Waiver.NonWaivableGates, check) {
			gate.State = GateWaived
			gate.Reason = "waived by " + waiver.ID + " until " + waiver.ExpiresAt.Format(time.RFC3339)
			verdict.Gates = append(verdict.Gates, gate)
			continue
		}

		// Current state per producer: newest attempt wins within a
		// producer; supersedes already removed corrections.
		current := map[string]Record{}
		for _, record := range heads {
			if record.Kind != check || record.Authority != AuthorityMergeGate ||
				!record.MatchesTuple(tup.SourceSHA, tup.TargetSHA) {
				continue
			}
			existing, present := current[record.Producer.ID]
			if !present || record.Attempt >= existing.Attempt {
				current[record.Producer.ID] = record
			}
		}

		for _, record := range current {
			gate.EvidenceIDs = append(gate.EvidenceIDs, record.EvidenceID)
		}
		slices.Sort(gate.EvidenceIDs)

		if len(current) == 0 {
			// Also surface diagnostic-only attempts for visibility: they
			// exist but never satisfy the gate (TC-EVIDENCE-004).
			hasDiagnostic := false
			for _, record := range heads {
				if record.Kind == check && record.Authority == AuthorityDiagnostic &&
					record.MatchesTuple(tup.SourceSHA, tup.TargetSHA) {
					hasDiagnostic = true
				}
			}
			gate.State = GatePending
			gate.Reason = "missing"
			if hasDiagnostic {
				gate.Reason = "missing merge_gate authority (diagnostic evidence present)"
			}
			verdict.Gates = append(verdict.Gates, gate)
			continue
		}

		state, reason := aggregateProducerStates(current)
		gate.State = state
		gate.Reason = reason
		verdict.Gates = append(verdict.Gates, gate)
	}

	verdict.Ready = true
	for _, gate := range verdict.Gates {
		if gate.Required && gate.State != GatePassed && gate.State != GateWaived {
			verdict.Ready = false
			break
		}
	}
	return verdict, nil
}

// aggregateProducerStates folds every producer's current state into the
// gate state: failures dominate, then errors, then pass.
func aggregateProducerStates(current map[string]Record) (string, string) {
	state := GatePassed
	reason := ""
	for producerID, record := range current {
		switch record.Status {
		case EvidencePassed:
			continue
		case EvidenceFailed, EvidenceCancelled, EvidenceSkipped:
			if state != GateFailed {
				state = GateFailed
				reason = fmt.Sprintf("producer %s reported %s", producerID, record.Status)
			}
		case EvidenceError:
			if state == GatePassed {
				state = GateError
				reason = fmt.Sprintf("producer %s reported error", producerID)
			}
		default:
			// Validate() already constrained the enum.
			state = GateError
			reason = fmt.Sprintf("producer %s reported unknown status %q", producerID, record.Status)
		}
	}
	return state, reason
}

func validateTuple(tup Tuple) error {
	if tup.ProjectID == "" || tup.WorkItemID == "" {
		return fmt.Errorf("evaluate: project and work item are required")
	}
	if !shaPattern.MatchString(tup.SourceSHA) || !shaPattern.MatchString(tup.TargetSHA) {
		return fmt.Errorf("evaluate: source/target SHA must be 40-64 lowercase hex")
	}
	if tup.PolicyVersion == "" {
		return fmt.Errorf("evaluate: policy version is required")
	}
	return nil
}

// StaleGateIDs lists snapshot identities whose tuple or policy no
// longer matches the current evaluation identity: the caller marks them
// stale in the same transaction that persists the new verdict
// (QG-RULE-003: drift invalidates immediately, old rows never answer
// for a new SHA).
type StoredSnapshot struct {
	GateID        string
	WorkItemID    string
	Check         string
	SourceSHA     string
	TargetSHA     string
	PolicyVersion string
	Status        string
	Version       int64
}

func StaleGateIDs(existing []StoredSnapshot, tup Tuple) []string {
	stale := []string{}
	for _, snapshot := range existing {
		if snapshot.SourceSHA != tup.SourceSHA || snapshot.TargetSHA != tup.TargetSHA ||
			snapshot.PolicyVersion != tup.PolicyVersion {
			stale = append(stale, snapshot.GateID)
		}
	}
	return stale
}
