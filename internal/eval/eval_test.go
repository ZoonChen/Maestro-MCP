package eval

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pure M4-EVAL-001 record core: every frozen wire constraint
// mirrored fail-closed, plus the pass^k estimator.

func seedPtr(value int64) *int64 { return &value }

func validRecord() Record {
	return Record{
		SchemaVersion: SchemaVersion,
		Layer:         LayerQuality,
		CaseID:        "case-golden-001",
		RunID:         "018f7e00-0000-7000-8000-0000000000aa",
		Verdict:       VerdictPass,
		Score:         92.5,
		Scorer:        Scorer{Kind: ScorerDeterministic, Version: "golden-v3"},
		DatasetDigest: "sha256:" + strings.Repeat("a", 64),
		Version:       1,
	}
}

func TestRecordValidateAcceptsTheHonestShapes(t *testing.T) {
	require.NoError(t, validRecord().Validate())

	security := validRecord()
	security.Layer = LayerSecurity
	security.Risk = RiskElevated
	security.Verdict = VerdictFail
	security.Notes = "the injection pulled a forbidden tool call"
	security.ForbiddenActionsObserved = []string{"git.push"}
	security.TrajectoryConstraints = []string{"no-protected-ref", "budget-capped"}
	require.NoError(t, security.Validate())
}

func TestRecordValidateRejectsWireViolations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Record)
	}{
		{"wrong schema version", func(r *Record) { r.SchemaVersion = "2.1" }},
		{"non-uuid record id", func(r *Record) { r.RecordID = "not-a-uuid" }},
		{"non-uuid run id", func(r *Record) { r.RunID = "run-1" }},
		{"layer outside the frozen set", func(r *Record) { r.Layer = Layer("performance") }},
		{"empty case id", func(r *Record) { r.CaseID = "" }},
		{"case id over 128 runes", func(r *Record) { r.CaseID = strings.Repeat("c", 129) }},
		{"security without risk", func(r *Record) { r.Layer = LayerSecurity; r.Risk = "" }},
		{"risk outside the frozen set", func(r *Record) { r.Risk = Risk("severe") }},
		{"verdict outside the frozen set", func(r *Record) { r.Verdict = Verdict("flaky") }},
		{"score above 100", func(r *Record) { r.Score = 100.5 }},
		{"negative score", func(r *Record) { r.Score = -1 }},
		{"scorer kind outside the frozen set", func(r *Record) { r.Scorer.Kind = ScorerKind("vibe") }},
		{"empty scorer version", func(r *Record) { r.Scorer.Version = "" }},
		{"dataset digest not sha256", func(r *Record) { r.DatasetDigest = "md5:" + strings.Repeat("a", 32) }},
		{"negative seed", func(r *Record) { r.Seed = seedPtr(-1) }},
		{"model config digest not sha256", func(r *Record) { r.ModelConfigDigest = "sha256:short" }},
		{"trajectory constraint over 200 runes", func(r *Record) {
			r.TrajectoryConstraints = []string{strings.Repeat("t", 201)}
		}},
		{"duplicate forbidden actions", func(r *Record) {
			r.ForbiddenActionsObserved = []string{"git.push", "git.push"}
		}},
		{"negative tokens", func(r *Record) { r.TokensUsed = -5 }},
		{"negative latency", func(r *Record) { r.LatencyMS = seedPtr(-2) }},
		{"external state digest not sha256", func(r *Record) { r.ExternalStateDigest = "digest" }},
		{"evidence ref not a uuid", func(r *Record) {
			r.EvidenceRefs = []string{"evidence-1"}
		}},
		{"duplicate evidence refs", func(r *Record) {
			r.EvidenceRefs = []string{"018f7e00-0000-7000-8000-0000000000bb", "018f7e00-0000-7000-8000-0000000000bb"}
		}},
		{"notes over 4000 runes", func(r *Record) { r.Notes = strings.Repeat("n", 4001) }},
		{"fail without notes", func(r *Record) { r.Verdict = VerdictFail }},
		{"version below one", func(r *Record) { r.Version = 0 }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			record := validRecord()
			testCase.mutate(&record)
			require.ErrorIs(t, record.Validate(), ErrRecordRejected)
		})
	}
}

func TestPassPowerK(t *testing.T) {
	half, err := PassPowerK(10, 5, 1)
	require.NoError(t, err)
	assert.InDelta(t, 0.5, half, 1e-12, "pass^1 is the observed pass rate")

	// C(5,2)/C(10,2) = 10/45 = 2/9
	two, err := PassPowerK(10, 5, 2)
	require.NoError(t, err)
	assert.InDelta(t, 2.0/9.0, two, 1e-12)

	all, err := PassPowerK(8, 8, 8)
	require.NoError(t, err)
	assert.InDelta(t, 1.0, all, 1e-12)

	none, err := PassPowerK(8, 0, 1)
	require.NoError(t, err)
	assert.InDelta(t, 0.0, none, 1e-12)

	// Fewer passes than k can never yield all-k-pass.
	mixed, err := PassPowerK(4, 1, 2)
	require.NoError(t, err)
	assert.InDelta(t, 0.0, mixed, 1e-12)

	// The authority's key-scenario gate: pass^3 with 2 of 3 trials
	// passing is C(2,3)/C(3,3) = 0 — partial consistency is no pass^3.
	strict, err := PassPowerK(3, 2, 3)
	require.NoError(t, err)
	assert.InDelta(t, 0.0, strict, 1e-12)

	// Monotonicity: pass^1 >= pass^2 >= ... for the same trial set.
	previous := 1.1
	for k := 1; k <= 10; k++ {
		value, err := PassPowerK(10, 7, k)
		require.NoError(t, err)
		assert.LessOrEqual(t, value, previous+1e-12)
		previous = value
	}

	_, err = PassPowerK(10, 5, 0)
	require.ErrorIs(t, err, ErrPassAtKInvalid)
	_, err = PassPowerK(10, 5, 11)
	require.ErrorIs(t, err, ErrPassAtKInvalid)
	_, err = PassPowerK(0, 0, 1)
	require.ErrorIs(t, err, ErrPassAtKInvalid)
	_, err = PassPowerK(10, 11, 1)
	require.ErrorIs(t, err, ErrPassAtKInvalid)
}
