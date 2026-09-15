package evidence

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Control-plane self-attestation (D1, F14 structural fix): the three
// gates whose natural oracle is not a CI job but the evaluation
// engine's own deterministic judgment — policy_integrity,
// baseline_freshness and boundary — are evaluated by the engine itself
// at every run that carries ControlPlaneFacts. Each judgment mints an
// immutable evidence record (authority=control_plane) that joins the
// same per-producer aggregation as CI evidence: equal standing, a
// standing failure on either side blocks (EVIDENCE-RULE-003), and the
// append-only discipline is unchanged (EVIDENCE-RULE-001).
//
// Self-attestation is opt-in at the engine level: Evaluate without
// facts behaves exactly as before, so pure-CI tuples and the frozen CI
// judgment semantics are untouched.

// ControlPlaneProducerID identifies the engine build as the evidence
// producer; the version mirrors the judgment semantics (bump when a
// judgment rule changes, parser-version discipline).
const (
	ControlPlaneProducerID    = "maestro-evaluator"
	ControlPlaneEngineVersion = "d1.1"
)

// AuthorityControlPlane marks evidence produced by the evaluation
// engine itself. Such records satisfy only the three control-plane
// gates and never carry pipeline/job identifiers.
const AuthorityControlPlane = "control_plane"

// controlPlaneEvidenceSpace namespaces deterministic self-attestation
// identities (derived from the gate namespace, distinct name).
var controlPlaneEvidenceSpace = uuid.NewSHA1(gateNamespace, []byte("control-plane-evidence"))

// controlPlaneGates is the frozen set of gates whose oracle is the
// engine itself (quality-policy.schema.json $defs.gate_producer_kinds).
var controlPlaneGates = map[string]struct{}{
	GatePolicyIntegrity: {}, GateBaselineFreshness: {}, GateBoundary: {},
}

// IsControlPlaneGate reports whether a gate kind's declared producer
// kinds include control_plane.
func IsControlPlaneGate(check string) bool {
	_, ok := controlPlaneGates[check]
	return ok
}

// BoundaryFact is the mechanical derivation of the boundary gate from
// execution records: the work item's latest completed execution names
// a runner and the latest profile-bearing validation run names the
// command profile. Registration/approval is judged from those facts;
// a missing piece is named in the gate reason, never silently passed.
type BoundaryFact struct {
	ExecutionID string `json:"execution_id"`
	RunnerID    string `json:"runner_id"`
	// RunnerRegistered is true when the runner's persisted status is in
	// the registered set (approved/online), not pending, suspect,
	// draining or revoked.
	RunnerRegistered bool   `json:"runner_registered"`
	RunnerStatus     string `json:"runner_status"`
	// ProfileRef is the id@version@digest reference of the command
	// profile the validation run executed under; empty when no
	// profile-bearing validation exists.
	ProfileRef string `json:"profile_ref"`
}

// ControlPlaneFacts carries the ground truth the judgments compare
// against. The service assembles it from the documents it actually
// loaded (digests) and the store's persisted facts; a nil Boundary
// means the work item has no execution records at all, in which case
// the boundary gate mints no self-attestation and falls back to CI
// evidence (pending when none exists).
type ControlPlaneFacts struct {
	// CompanyDigest is the digest of the company baseline document the
	// caller actually loaded for this evaluation.
	CompanyDigest string
	// ProjectDigest is the digest of the project overlay document the
	// caller actually loaded ("" when the evaluation runs on the
	// company baseline alone).
	ProjectDigest string
	// StoredDigestMatch reports that the stored project policy row's
	// digest column still matches the recomputed document digest (the
	// store-level tamper check); true when no overlay row exists.
	StoredDigestMatch bool
	// Boundary carries the execution facts; nil = no execution records.
	Boundary *BoundaryFact
}

// controlPlaneJudgment is one gate's mechanical outcome plus the
// human-readable reason recorded on the minted evidence.
type controlPlaneJudgment struct {
	check  string
	status string
	reason string
}

// judgeControlPlane derives the three judgments from the evaluation
// identity, the resolved policy and the ground-truth facts. Every
// branch is mechanical: no input is treated as a pass.
func judgeControlPlane(tup Tuple, policy *EffectivePolicy, facts *ControlPlaneFacts) []controlPlaneJudgment {
	return []controlPlaneJudgment{
		judgePolicyIntegrity(policy, facts),
		judgeBaselineFreshness(tup, policy),
		judgeBoundary(facts),
	}
}

// judgePolicyIntegrity: the effective policy chain loaded completely —
// the merged document matches its digest, the provenance layers match
// the digests of the documents actually loaded, and the stored overlay
// row's digest column still matches its document.
func judgePolicyIntegrity(policy *EffectivePolicy, facts *ControlPlaneFacts) controlPlaneJudgment {
	const check = GatePolicyIntegrity
	recomputed, err := policy.Policy.Digest()
	if err != nil {
		return controlPlaneJudgment{check, EvidenceError, fmt.Sprintf("effective policy failed validation: %v", err)}
	}
	if recomputed != policy.PolicyDigest {
		return controlPlaneJudgment{check, EvidenceFailed, fmt.Sprintf(
			"policy chain broken: merged document digest %s does not match declared digest %s", recomputed, policy.PolicyDigest)}
	}
	if len(policy.Provenance) == 0 || policy.Provenance[0].Scope != "company" {
		return controlPlaneJudgment{check, EvidenceFailed, "policy chain broken: company baseline layer is missing"}
	}
	if facts.CompanyDigest != "" && policy.Provenance[0].Digest != facts.CompanyDigest {
		return controlPlaneJudgment{check, EvidenceFailed, fmt.Sprintf(
			"policy chain broken: company layer digest %s does not match the loaded baseline %s",
			policy.Provenance[0].Digest, facts.CompanyDigest)}
	}
	if facts.ProjectDigest == "" {
		if len(policy.Provenance) != 1 {
			return controlPlaneJudgment{check, EvidenceFailed,
				"policy chain broken: no project overlay loaded but the provenance chain has a project layer"}
		}
	} else {
		if len(policy.Provenance) != 2 || policy.Provenance[1].Scope != "project" {
			return controlPlaneJudgment{check, EvidenceFailed,
				"policy chain broken: project overlay loaded but the provenance chain lacks its layer"}
		}
		if policy.Provenance[1].Digest != facts.ProjectDigest {
			return controlPlaneJudgment{check, EvidenceFailed, fmt.Sprintf(
				"policy chain broken: project layer digest %s does not match the loaded overlay %s",
				policy.Provenance[1].Digest, facts.ProjectDigest)}
		}
	}
	if !facts.StoredDigestMatch {
		return controlPlaneJudgment{check, EvidenceFailed,
			"policy chain broken: stored project policy digest does not match the recomputed document"}
	}
	return controlPlaneJudgment{check, EvidencePassed, fmt.Sprintf(
		"effective policy chain intact (%d layer(s), merged digest %s)", len(policy.Provenance), policy.PolicyDigest)}
}

// judgeBaselineFreshness: the tuple's pinned policy version is the
// version in force for this evaluation — a replay against a tuple
// pinned to a superseded policy is drift, never a pass.
func judgeBaselineFreshness(tup Tuple, policy *EffectivePolicy) controlPlaneJudgment {
	const check = GateBaselineFreshness
	if tup.PolicyVersion == policy.Policy.Version {
		return controlPlaneJudgment{check, EvidencePassed, fmt.Sprintf(
			"tuple policy version %s matches the active effective policy", tup.PolicyVersion)}
	}
	return controlPlaneJudgment{check, EvidenceFailed, fmt.Sprintf(
		"policy drift: tuple pinned version %s, active effective policy %s", tup.PolicyVersion, policy.Policy.Version)}
}

// judgeBoundary: the work item executed through a registered runner
// under an approved command profile. A nil fact mints no evidence at
// all (the gate falls back to CI producers); an execution that exists
// but lacks registration or an approved profile fails closed with the
// gap named.
func judgeBoundary(facts *ControlPlaneFacts) controlPlaneJudgment {
	const check = GateBoundary
	if facts.Boundary == nil {
		return controlPlaneJudgment{check: check, status: ""} // no self-attestation
	}
	boundary := facts.Boundary
	if !boundary.RunnerRegistered {
		return controlPlaneJudgment{check, EvidenceFailed, fmt.Sprintf(
			"execution %s ran on runner %s whose registration status is %q",
			boundary.ExecutionID, boundary.RunnerID, boundary.RunnerStatus)}
	}
	if boundary.ProfileRef == "" {
		return controlPlaneJudgment{check, EvidenceFailed, fmt.Sprintf(
			"execution %s on registered runner %s has no approved command profile on record",
			boundary.ExecutionID, boundary.RunnerID)}
	}
	return controlPlaneJudgment{check, EvidencePassed, fmt.Sprintf(
		"execution %s ran on registered runner %s (%s) under approved profile %s",
		boundary.ExecutionID, boundary.RunnerID, boundary.RunnerStatus, boundary.ProfileRef)}
}

// mintControlPlaneRecords materializes the judgments as evidence
// records. Identity is deterministic over the judgment content, so an
// unchanged judgment re-collapses onto the same row (idempotent
// re-append) while a changed judgment mints a new record with the next
// attempt number — newest attempt wins the producer aggregation, the
// older judgment stays in history.
func mintControlPlaneRecords(tup Tuple, policy *EffectivePolicy, facts *ControlPlaneFacts, heads []Record, now time.Time) []Record {
	minted := []Record{}
	for _, judgment := range judgeControlPlane(tup, policy, facts) {
		if judgment.status == "" {
			continue
		}
		identity := controlPlaneEvidenceID(tup, judgment)
		attempt := 1
		for _, record := range heads {
			if record.Kind != judgment.check || record.Authority != AuthorityControlPlane ||
				record.Producer.Type != AuthorityControlPlane || !record.MatchesTuple(tup.SourceSHA, tup.TargetSHA) {
				continue
			}
			if record.EvidenceID == identity {
				// Same judgment re-mints the same identity: inherit the
				// stored attempt so the verdict matches the row.
				attempt = record.Attempt
				break
			}
			if record.Attempt >= attempt {
				attempt = record.Attempt + 1
			}
		}
		record := Record{
			EvidenceID:    identity,
			ProjectID:     tup.ProjectID,
			WorkItemID:    tup.WorkItemID,
			Kind:          judgment.check,
			Authority:     AuthorityControlPlane,
			Status:        judgment.status,
			SourceSHA:     tup.SourceSHA,
			TargetSHA:     tup.TargetSHA,
			PolicyVersion: tup.PolicyVersion,
			Producer:      Producer{Type: AuthorityControlPlane, ID: ControlPlaneProducerID, Version: ControlPlaneEngineVersion},
			Attempt:       attempt,
			ObservedAt:    now.UTC().Format(time.RFC3339),
			Summary:       judgment.reason,
		}
		minted = append(minted, record)
	}
	return minted
}

// controlPlaneEvidenceID derives the record identity from the tuple
// plus the judgment CONTENT (check, status, reason) — never from
// timestamps, so re-evaluations of an unchanged state collapse.
func controlPlaneEvidenceID(tup Tuple, judgment controlPlaneJudgment) string {
	material := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		tup.ProjectID, tup.WorkItemID, judgment.check, judgment.status,
		tup.SourceSHA, tup.TargetSHA, tup.PolicyVersion,
		ControlPlaneProducerID, judgment.reason)
	digest := sha256.Sum256([]byte(material))
	return uuid.NewSHA1(controlPlaneEvidenceSpace, digest[:]).String()
}
