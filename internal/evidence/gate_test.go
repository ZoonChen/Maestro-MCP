package evidence

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var evalNow = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func evalTuple() Tuple {
	return Tuple{
		ProjectID:     "018f7000-0000-7000-8000-000000000001",
		WorkItemID:    "018f7000-0000-7000-8000-000000000002",
		SourceSHA:     sha(40),
		TargetSHA:     sha(41)[:40],
		PolicyVersion: "3.0.0",
	}
}

func gateRecord(id, check, authority, status string, attempt int) Record {
	pipeline := int64(99)
	job := int64(1000 + attempt)
	record := Record{
		EvidenceID:    id,
		ProjectID:     "018f7000-0000-7000-8000-000000000001",
		WorkItemID:    "018f7000-0000-7000-8000-000000000002",
		Kind:          check,
		Authority:     authority,
		Status:        status,
		SourceSHA:     sha(40),
		TargetSHA:     sha(41)[:40],
		PolicyVersion: "3.0.0",
		Attempt:       attempt,
	}
	if authority == AuthorityMergeGate {
		record.PipelineID = &pipeline
		record.JobID = &job
		record.Producer = Producer{Type: "gitlab_job", ID: "ci-pipeline", Version: "1.0"}
	} else {
		record.Producer = Producer{Type: "runner_profile", ID: "local-runner", Version: "1.0"}
	}
	return record
}

func fullPassSet() []Record {
	records := []Record{}
	for index, check := range testCompanyPolicy().RequiredGates {
		records = append(records, gateRecord("ev-"+string(rune('a'+index))+"-1", check, AuthorityMergeGate, EvidencePassed, 1))
	}
	return records
}

func gateState(t *testing.T, verdict *Verdict, check string) GateResult {
	t.Helper()
	for _, gate := range verdict.Gates {
		if gate.Check == check {
			return gate
		}
	}
	t.Fatalf("gate %s missing from verdict", check)
	return GateResult{}
}

func TestEvaluateAllPassedIsReady(t *testing.T) {
	resolved, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)

	verdict, err := Evaluate(evalTuple(), resolved, fullPassSet(), nil, evalNow)
	require.NoError(t, err)
	assert.True(t, verdict.Ready)
	require.Len(t, verdict.Gates, 12)
	for _, gate := range verdict.Gates {
		assert.Equal(t, GatePassed, gate.State, gate.Check)
		assert.Empty(t, gate.Reason, gate.Check)
	}
}

func TestEvaluateMissingEvidenceBlocks(t *testing.T) {
	resolved, _ := ResolveEffective(testCompanyPolicy(), nil)
	records := []Record{}
	for index, check := range testCompanyPolicy().RequiredGates {
		if check == GateSAST {
			continue
		}
		records = append(records, gateRecord("ev-"+string(rune('a'+index))+"-1", check, AuthorityMergeGate, EvidencePassed, 1))
	}

	verdict, err := Evaluate(evalTuple(), resolved, records, nil, evalNow)
	require.NoError(t, err)
	assert.False(t, verdict.Ready)
	sast := gateState(t, verdict, GateSAST)
	assert.Equal(t, GatePending, sast.State)
	assert.Equal(t, "missing", sast.Reason)
}

func TestEvaluateDiagnosticNeverSatisfies(t *testing.T) {
	resolved, _ := ResolveEffective(testCompanyPolicy(), nil)
	records := []Record{}
	for index, check := range testCompanyPolicy().RequiredGates {
		if check == GateSAST {
			continue
		}
		records = append(records, gateRecord("ev-"+string(rune('a'+index))+"-1", check, AuthorityMergeGate, EvidencePassed, 1))
	}
	records = append(records, gateRecord("ev-diag-1", GateSAST, AuthorityDiagnostic, EvidencePassed, 1))

	verdict, err := Evaluate(evalTuple(), resolved, records, nil, evalNow)
	require.NoError(t, err)
	assert.False(t, verdict.Ready, "diagnostic evidence must not satisfy a required gate")
	sast := gateState(t, verdict, GateSAST)
	assert.Equal(t, GatePending, sast.State)
	assert.Equal(t, "missing merge_gate authority (diagnostic evidence present)", sast.Reason)
}

func TestEvaluateFailureDominatesAndStatusesMap(t *testing.T) {
	resolved, _ := ResolveEffective(testCompanyPolicy(), nil)

	cases := []struct {
		status string
		state  string
	}{
		{EvidenceFailed, GateFailed},
		{EvidenceCancelled, GateFailed},
		{EvidenceSkipped, GateFailed},
		{EvidenceError, GateError},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			records := fullPassSet()
			records = append(records, gateRecord("ev-fail-1", GateUnit, AuthorityMergeGate, tc.status, 2))
			verdict, err := Evaluate(evalTuple(), resolved, records, nil, evalNow)
			require.NoError(t, err)
			assert.False(t, verdict.Ready)
			unit := gateState(t, verdict, GateUnit)
			assert.Equal(t, tc.state, unit.State)
			assert.Contains(t, unit.Reason, tc.status)
		})
	}
}

func TestEvaluateMultiProducerAllMustPass(t *testing.T) {
	resolved, _ := ResolveEffective(testCompanyPolicy(), nil)
	records := fullPassSet()
	pass := gateRecord("ev-sast-a", GateSAST, AuthorityMergeGate, EvidencePassed, 1)
	pass.Producer.ID = "scanner-a"
	fail := gateRecord("ev-sast-b", GateSAST, AuthorityMergeGate, EvidenceFailed, 1)
	fail.Producer.ID = "scanner-b"
	records = append(records, pass, fail)

	verdict, err := Evaluate(evalTuple(), resolved, records, nil, evalNow)
	require.NoError(t, err)
	assert.False(t, verdict.Ready, "a standing failure cannot be covered by another producer's success")
	sast := gateState(t, verdict, GateSAST)
	assert.Equal(t, GateFailed, sast.State)
}

func TestEvaluateNewestAttemptWinsPerProducer(t *testing.T) {
	resolved, _ := ResolveEffective(testCompanyPolicy(), nil)
	records := fullPassSet()
	records = append(records,
		gateRecord("ev-unit-f1", GateUnit, AuthorityMergeGate, EvidenceFailed, 1),
		gateRecord("ev-unit-p2", GateUnit, AuthorityMergeGate, EvidencePassed, 2),
	)

	verdict, err := Evaluate(evalTuple(), resolved, records, nil, evalNow)
	require.NoError(t, err)
	unit := gateState(t, verdict, GateUnit)
	assert.Equal(t, GatePassed, unit.State, "the one allowed retry converges on the newest attempt")
	assert.Contains(t, unit.EvidenceIDs, "ev-unit-p2")
	assert.NotContains(t, unit.EvidenceIDs, "ev-unit-f1", "superseded history does not decide")
}

func TestEvaluateSupersedesChainReplaces(t *testing.T) {
	resolved, _ := ResolveEffective(testCompanyPolicy(), nil)
	records := fullPassSet()
	failed := gateRecord("ev-build-f1", GateBuild, AuthorityMergeGate, EvidenceFailed, 1)
	correction := gateRecord("ev-build-c2", GateBuild, AuthorityMergeGate, EvidencePassed, 1)
	correction.Supersedes = "ev-build-f1"
	records = append(records, failed, correction)

	verdict, err := Evaluate(evalTuple(), resolved, records, nil, evalNow)
	require.NoError(t, err)
	build := gateState(t, verdict, GateBuild)
	assert.Equal(t, GatePassed, build.State, "corrections flow through the supersedes chain")
}

func TestEvaluateIgnoresOtherTuples(t *testing.T) {
	resolved, _ := ResolveEffective(testCompanyPolicy(), nil)
	records := fullPassSet()
	drifted := gateRecord("ev-drift", GateUnit, AuthorityMergeGate, EvidencePassed, 1)
	drifted.SourceSHA = sha(40)[:39] + "z"
	drifted.SourceSHA = sha(42)
	records = append(records, drifted)

	verdict, err := Evaluate(evalTuple(), resolved, records, nil, evalNow)
	require.NoError(t, err)
	unit := gateState(t, verdict, GateUnit)
	assert.Equal(t, GatePassed, unit.State)
	assert.Len(t, unit.EvidenceIDs, 1, "drifted-SHA evidence does not answer for this tuple")
}

func TestEvaluateWaiverWaivesAndInvalidations(t *testing.T) {
	resolved, _ := ResolveEffective(testCompanyPolicy(), nil)
	records := fullPassSet()
	records = append(records, gateRecord("ev-unit-f1", GateUnit, AuthorityMergeGate, EvidenceFailed, 1))
	tup := evalTuple()

	t.Run("valid waiver waives the failed gate", func(t *testing.T) {
		waivers := []Waiver{{
			ID: "w1", GateID: StableGateID(tup, GateUnit), Check: GateUnit,
			SourceSHA: tup.SourceSHA, State: WaiverApproved,
			Requester: "req-1", Approver: "app-1", ExpiresAt: evalNow.Add(time.Hour),
		}}
		verdict, err := Evaluate(tup, resolved, records, waivers, evalNow)
		require.NoError(t, err)
		unit := gateState(t, verdict, GateUnit)
		assert.Equal(t, GateWaived, unit.State)
	})

	t.Run("expired waiver does not waive", func(t *testing.T) {
		waivers := []Waiver{{
			ID: "w2", GateID: StableGateID(tup, GateUnit), Check: GateUnit,
			SourceSHA: tup.SourceSHA, State: WaiverApproved,
			ExpiresAt: evalNow.Add(-time.Minute),
		}}
		verdict, err := Evaluate(tup, resolved, records, waivers, evalNow)
		require.NoError(t, err)
		assert.Equal(t, GateFailed, gateState(t, verdict, GateUnit).State)
	})

	t.Run("waiver for an old SHA does not apply", func(t *testing.T) {
		waivers := []Waiver{{
			ID: "w3", GateID: StableGateID(tup, GateUnit), Check: GateUnit,
			SourceSHA: sha(77), State: WaiverApproved,
			ExpiresAt: evalNow.Add(time.Hour),
		}}
		verdict, err := Evaluate(tup, resolved, records, waivers, evalNow)
		require.NoError(t, err)
		assert.Equal(t, GateFailed, gateState(t, verdict, GateUnit).State)
	})

	t.Run("non-waivable principle is never waived", func(t *testing.T) {
		waivers := []Waiver{{
			ID: "w4", GateID: StableGateID(tup, GatePolicyIntegrity), Check: GatePolicyIntegrity,
			SourceSHA: tup.SourceSHA, State: WaiverApproved,
			ExpiresAt: evalNow.Add(time.Hour),
		}}
		verdict, err := Evaluate(tup, resolved, fullPassSet(), waivers, evalNow)
		require.NoError(t, err)
		assert.Equal(t, GatePassed, gateState(t, verdict, GatePolicyIntegrity).State,
			"a non-waivable check ignores waivers entirely")
	})
}

func TestStableGateIDDriftsOnTupleChange(t *testing.T) {
	tup := evalTuple()
	base := StableGateID(tup, GateUnit)

	changedSHA := tup
	changedSHA.SourceSHA = sha(43)
	assert.NotEqual(t, base, StableGateID(changedSHA, GateUnit), "SHA drift mints a new identity")

	changedPolicy := tup
	changedPolicy.PolicyVersion = "3.1.0"
	assert.NotEqual(t, base, StableGateID(changedPolicy, GateUnit), "policy drift mints a new identity")

	assert.Equal(t, base, StableGateID(tup, GateUnit), "stable for the same tuple")
}

func TestStaleGateIDs(t *testing.T) {
	tup := evalTuple()
	existing := []StoredSnapshot{
		{GateID: "g-current", Check: GateUnit, SourceSHA: tup.SourceSHA, TargetSHA: tup.TargetSHA, PolicyVersion: tup.PolicyVersion},
		{GateID: "g-old-sha", Check: GateUnit, SourceSHA: sha(55), TargetSHA: tup.TargetSHA, PolicyVersion: tup.PolicyVersion},
		{GateID: "g-old-policy", Check: GateUnit, SourceSHA: tup.SourceSHA, TargetSHA: tup.TargetSHA, PolicyVersion: "2.0.0"},
	}
	stale := StaleGateIDs(existing, tup)
	assert.Equal(t, []string{"g-old-sha", "g-old-policy"}, stale)
}
