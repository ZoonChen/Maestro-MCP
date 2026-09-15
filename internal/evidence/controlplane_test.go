package evidence

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// D1 self-attestation tests: the three engine-oracle gates mint
// control_plane evidence from mechanical judgments, compose with CI
// producers under EVIDENCE-RULE-003, and never treat a missing fact
// as a pass.

// controlPlaneFacts is the complete, healthy fact set for evalTuple().
func controlPlaneFacts(resolved *EffectivePolicy) *ControlPlaneFacts {
	facts := &ControlPlaneFacts{
		StoredDigestMatch: true,
		Boundary: &BoundaryFact{
			ExecutionID:      "018f7000-0000-7000-8000-0000000000aa",
			RunnerID:         "018f7000-0000-7000-8000-0000000000bb",
			RunnerRegistered: true,
			RunnerStatus:     "approved",
			ProfileRef:       "maven-build@1.0.0@sha256:" + strings.Repeat("ab", 32),
		},
	}
	facts.CompanyDigest = resolved.Provenance[0].Digest
	if len(resolved.Provenance) > 1 {
		facts.ProjectDigest = resolved.Provenance[1].Digest
	}
	return facts
}

func controlPlaneGate(t *testing.T, verdict *Verdict, check string) GateResult {
	t.Helper()
	for _, gate := range verdict.Gates {
		if gate.Check == check {
			return gate
		}
	}
	t.Fatalf("gate %s missing from verdict", check)
	return GateResult{}
}

func TestControlPlaneSelfAttestationHappyPath(t *testing.T) {
	resolved, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)

	verdict, err := Evaluate(evalTuple(), resolved, controlPlaneFacts(resolved), nil, nil, evalNow)
	require.NoError(t, err)

	require.Len(t, verdict.ControlPlane, 3, "policy_integrity + baseline_freshness + boundary mint evidence")
	for _, record := range verdict.ControlPlane {
		require.NoError(t, record.Validate())
		assert.Equal(t, AuthorityControlPlane, record.Authority)
		assert.Equal(t, ControlPlaneProducerID, record.Producer.ID)
		assert.Equal(t, AuthorityControlPlane, record.Producer.Type)
		assert.Nil(t, record.PipelineID)
		assert.Nil(t, record.JobID)
		assert.Equal(t, EvidencePassed, record.Status)
		assert.Equal(t, 1, record.Attempt)
	}
	for _, check := range []string{GatePolicyIntegrity, GateBaselineFreshness, GateBoundary} {
		gate := controlPlaneGate(t, verdict, check)
		assert.Equal(t, GatePassed, gate.State, "%s reason=%s", check, gate.Reason)
		assert.Contains(t, gate.EvidenceIDs, controlPlaneEvidenceIDFor(t, verdict, check))
	}

	// Idempotent identity: the same facts re-mint the same evidence IDs.
	repeat, err := Evaluate(evalTuple(), resolved, controlPlaneFacts(resolved), nil, nil, evalNow.Add(time.Hour))
	require.NoError(t, err)
	for index := range verdict.ControlPlane {
		assert.Equal(t, verdict.ControlPlane[index].EvidenceID, repeat.ControlPlane[index].EvidenceID)
		assert.Equal(t, 1, repeat.ControlPlane[index].Attempt, "an unchanged judgment never bumps the attempt")
	}

	// Reload path: the prior minted rows are in the input set — the
	// identical judgment inherits its stored attempt instead of bumping.
	reloaded, err := Evaluate(evalTuple(), resolved, controlPlaneFacts(resolved), verdict.ControlPlane, nil, evalNow.Add(2*time.Hour))
	require.NoError(t, err)
	for index := range verdict.ControlPlane {
		assert.Equal(t, verdict.ControlPlane[index].EvidenceID, reloaded.ControlPlane[index].EvidenceID)
		assert.Equal(t, verdict.ControlPlane[index].Attempt, reloaded.ControlPlane[index].Attempt)
	}
}

func controlPlaneEvidenceIDFor(t *testing.T, verdict *Verdict, check string) string {
	t.Helper()
	for _, record := range verdict.ControlPlane {
		if record.Kind == check {
			return record.EvidenceID
		}
	}
	t.Fatalf("no minted record for %s", check)
	return ""
}

func TestControlPlanePolicyIntegrityBrokenChain(t *testing.T) {
	cases := map[string]func(*ControlPlaneFacts, *EffectivePolicy){
		"stored digest drift": func(f *ControlPlaneFacts, _ *EffectivePolicy) {
			f.StoredDigestMatch = false
		},
		"company layer digest mismatch": func(f *ControlPlaneFacts, _ *EffectivePolicy) {
			f.CompanyDigest = "sha256:" + strings.Repeat("ff", 32)
		},
		"merged document digest mismatch": func(_ *ControlPlaneFacts, p *EffectivePolicy) {
			p.PolicyDigest = "sha256:" + strings.Repeat("ee", 32)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fresh, resolveErr := ResolveEffective(testCompanyPolicy(), nil)
			require.NoError(t, resolveErr)
			facts := controlPlaneFacts(fresh)
			mutate(facts, fresh)
			verdict, evalErr := Evaluate(evalTuple(), fresh, facts, nil, nil, evalNow)
			require.NoError(t, evalErr)
			gate := controlPlaneGate(t, verdict, GatePolicyIntegrity)
			assert.Equal(t, GateFailed, gate.State)
			assert.Contains(t, gate.Reason, "producer maestro-evaluator reported failed")
		})
	}

	// A loaded overlay without its provenance layer breaks the chain.
	overlay := projectOverlay("overlay-chain", nil)
	merged, err := ResolveEffective(testCompanyPolicy(), overlay)
	require.NoError(t, err)
	broken := &EffectivePolicy{Policy: merged.Policy, PolicyDigest: merged.PolicyDigest, Provenance: merged.Provenance[:1]}
	facts := controlPlaneFacts(merged)
	facts.ProjectDigest = merged.Provenance[1].Digest
	verdict, err := Evaluate(evalTuple(), broken, facts, nil, nil, evalNow)
	require.NoError(t, err)
	gate := controlPlaneGate(t, verdict, GatePolicyIntegrity)
	assert.Equal(t, GateFailed, gate.State, "an overlay in force without its provenance layer must fail")
}

func TestControlPlaneBaselineFreshnessDrift(t *testing.T) {
	resolved, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)

	pinned := evalTuple()
	pinned.PolicyVersion = "2.9.0" // the replay pin: superseded policy
	verdict, err := Evaluate(pinned, resolved, controlPlaneFacts(resolved), nil, nil, evalNow)
	require.NoError(t, err)
	gate := controlPlaneGate(t, verdict, GateBaselineFreshness)
	assert.Equal(t, GateFailed, gate.State)
	assert.Contains(t, gate.Reason, "producer maestro-evaluator reported failed")
	for _, record := range verdict.ControlPlane {
		if record.Kind == GateBaselineFreshness {
			assert.Contains(t, record.Summary, "policy drift")
			assert.Contains(t, record.Summary, "2.9.0")
		}
	}
	assert.False(t, verdict.Ready, "a drifted tuple never composes to ready")
}

func TestControlPlaneBoundaryJudgments(t *testing.T) {
	resolved, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)

	t.Run("no execution facts mints no boundary evidence", func(t *testing.T) {
		facts := controlPlaneFacts(resolved)
		facts.Boundary = nil
		verdict, err := Evaluate(evalTuple(), resolved, facts, nil, nil, evalNow)
		require.NoError(t, err)
		assert.Len(t, verdict.ControlPlane, 2, "policy_integrity + baseline_freshness only")
		gate := controlPlaneGate(t, verdict, GateBoundary)
		assert.Equal(t, GatePending, gate.State, "no self-attestation, no CI producer: pending")
		assert.Equal(t, "missing", gate.Reason)
	})

	t.Run("unregistered runner fails naming the status", func(t *testing.T) {
		facts := controlPlaneFacts(resolved)
		facts.Boundary.RunnerRegistered = false
		facts.Boundary.RunnerStatus = "revoked"
		verdict, err := Evaluate(evalTuple(), resolved, facts, nil, nil, evalNow)
		require.NoError(t, err)
		gate := controlPlaneGate(t, verdict, GateBoundary)
		assert.Equal(t, GateFailed, gate.State)
		for _, record := range verdict.ControlPlane {
			if record.Kind == GateBoundary {
				assert.Contains(t, record.Summary, "revoked")
			}
		}
	})

	t.Run("execution without an approved profile fails", func(t *testing.T) {
		facts := controlPlaneFacts(resolved)
		facts.Boundary.ProfileRef = ""
		verdict, err := Evaluate(evalTuple(), resolved, facts, nil, nil, evalNow)
		require.NoError(t, err)
		gate := controlPlaneGate(t, verdict, GateBoundary)
		assert.Equal(t, GateFailed, gate.State)
	})
}

func TestControlPlaneComposesWithCIProducers(t *testing.T) {
	resolved, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)
	tup := evalTuple()

	// A CI job named `boundary` FAILED while the engine self-attests
	// passed: equal standing means the standing failure blocks — the
	// self-attestation cannot override CI, and vice versa.
	ciBoundary := gateRecord("ev-ci-boundary-1", GateBoundary, AuthorityMergeGate, EvidenceFailed, 1)
	verdict, err := Evaluate(tup, resolved, controlPlaneFacts(resolved), []Record{ciBoundary}, nil, evalNow)
	require.NoError(t, err)
	gate := controlPlaneGate(t, verdict, GateBoundary)
	assert.Equal(t, GateFailed, gate.State, "a standing CI failure blocks despite the passing self-attestation")
	assert.Len(t, gate.EvidenceIDs, 2, "both producers' evidence is cited")

	// A passing CI boundary plus the passing self-attestation composes
	// green — the A1-4-shaped tuple where CI carries build/unit and the
	// engine carries the three oracle gates.
	records := []Record{
		gateRecord("ev-ci-build-1", GateBuild, AuthorityMergeGate, EvidencePassed, 1),
		gateRecord("ev-ci-boundary-1", GateBoundary, AuthorityMergeGate, EvidencePassed, 1),
	}
	verdict, err = Evaluate(tup, resolved, controlPlaneFacts(resolved), records, nil, evalNow)
	require.NoError(t, err)
	assert.Equal(t, GatePassed, controlPlaneGate(t, verdict, GateBoundary).State)
	assert.Equal(t, GatePassed, controlPlaneGate(t, verdict, GateBuild).State)
	assert.False(t, verdict.Ready, "the remaining CI gates without producers still block")
}

func TestControlPlaneEvidenceDomainAndAttemptChain(t *testing.T) {
	resolved, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)
	tup := evalTuple()

	// control_plane authority is structurally invalid outside its gate
	// domain and invalid as a CI-identity record.
	misplaced := Record{
		EvidenceID: "018f7400-0000-7000-8000-000000000001", ProjectID: tup.ProjectID,
		WorkItemID: tup.WorkItemID, Kind: GateBuild, Authority: AuthorityControlPlane,
		Status: EvidencePassed, SourceSHA: tup.SourceSHA, TargetSHA: tup.TargetSHA,
		PolicyVersion: tup.PolicyVersion,
		Producer:      Producer{Type: AuthorityControlPlane, ID: ControlPlaneProducerID, Version: "d1.1"},
		Attempt:       1,
	}
	assert.Error(t, misplaced.Validate(), "control_plane evidence for a CI gate never validates")

	// A changed judgment mints a new record with the next attempt; the
	// newest attempt wins the producer aggregation, history stays.
	healthy, err := Evaluate(tup, resolved, controlPlaneFacts(resolved), nil, nil, evalNow)
	require.NoError(t, err)
	brokenFacts := controlPlaneFacts(resolved)
	brokenFacts.Boundary.RunnerRegistered = false
	broken, err := Evaluate(tup, resolved, brokenFacts, healthy.ControlPlane, nil, evalNow)
	require.NoError(t, err)
	for _, record := range broken.ControlPlane {
		if record.Kind == GateBoundary {
			assert.Equal(t, 2, record.Attempt, "the changed judgment chains onto the prior attempt")
			assert.NotEqual(t, healthy.ControlPlane[2].EvidenceID, record.EvidenceID)
		}
	}
	assert.Equal(t, GateFailed, controlPlaneGate(t, broken, GateBoundary).State)
}
