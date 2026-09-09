package eval

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One line exactly as tests/eval emits it (schema-key order, optional
// fields present) — the parse-side golden input.
var harnessLine = `{"schema_version":"3.0","record_id":"018f7e00-0000-7000-8000-00000000aa01","layer":"quality","case_id":"quality-fix-null-deref-success","run_id":"018f7e00-0000-7000-8000-00000000bb01","dataset_digest":"sha256:` + strings.Repeat("a", 64) + `","verdict":"pass","score":100,"version":1,"category":"normal","risk":"benign","scorer":{"kind":"deterministic","version":"1.0.0"},"seed":7,"model_config_digest":"sha256:` + strings.Repeat("b", 64) + `","trajectory_constraints":["allowlist_only"],"forbidden_actions_observed":[],"tokens_used":4200,"latency_ms":5100,"external_state_digest":"sha256:` + strings.Repeat("c", 64) + `","evidence_refs":[],"notes":"","created_at":"2026-09-09T12:00:00.000Z"}`

func TestParseRecordMapsTheFullWire(t *testing.T) {
	record, err := ParseRecord([]byte(harnessLine))
	require.NoError(t, err)
	seed, latency := int64(7), int64(5100)
	assert.Equal(t, Record{
		SchemaVersion:            "3.0",
		RecordID:                 "018f7e00-0000-7000-8000-00000000aa01",
		Layer:                    LayerQuality,
		CaseID:                   "quality-fix-null-deref-success",
		RunID:                    "018f7e00-0000-7000-8000-00000000bb01",
		Category:                 "normal",
		Risk:                     RiskBenign,
		Verdict:                  VerdictPass,
		Score:                    100,
		Scorer:                   Scorer{Kind: ScorerDeterministic, Version: "1.0.0"},
		DatasetDigest:            "sha256:" + strings.Repeat("a", 64),
		Seed:                     &seed,
		ModelConfigDigest:        "sha256:" + strings.Repeat("b", 64),
		TrajectoryConstraints:    []string{"allowlist_only"},
		ForbiddenActionsObserved: []string{},
		TokensUsed:               4200,
		LatencyMS:                &latency,
		ExternalStateDigest:      "sha256:" + strings.Repeat("c", 64),
		EvidenceRefs:             []string{},
		Notes:                    "",
		Version:                  1,
	}, record)
	// created_at is accepted on the wire but intentionally not mapped.
	require.NoError(t, record.Validate())
}

func TestParseRecordMinimalLineLeavesOptionalsZero(t *testing.T) {
	minimal := `{"schema_version":"3.0","layer":"security","case_id":"inj-001","run_id":"018f7e00-0000-7000-8000-00000000bb02","dataset_digest":"sha256:` +
		strings.Repeat("d", 64) + `","verdict":"fail","score":0,"scorer":{"kind":"rule_based","version":"redteam-v1"},"notes":"attack succeeded","version":2,"risk":"high"}`
	record, err := ParseRecord([]byte(minimal))
	require.NoError(t, err)
	assert.Equal(t, LayerSecurity, record.Layer)
	assert.Equal(t, RiskHigh, record.Risk)
	assert.Equal(t, VerdictFail, record.Verdict)
	assert.Nil(t, record.Seed)
	assert.Nil(t, record.LatencyMS)
	require.NoError(t, record.Validate())
}

func TestParseRecordRejectsWireViolations(t *testing.T) {
	for name, line := range map[string]string{
		"unknown property":       `{"schema_version":"3.0","layer":"quality","case_id":"c","run_id":"018f7e00-0000-7000-8000-00000000bb03","dataset_digest":"sha256:` + strings.Repeat("a", 64) + `","verdict":"pass","score":1,"version":1,"project_id":"018f7e00-0000-7000-8000-000000000001"}`,
		"missing schema_version": `{"layer":"quality","case_id":"c","run_id":"018f7e00-0000-7000-8000-00000000bb03","dataset_digest":"sha256:` + strings.Repeat("a", 64) + `","verdict":"pass","score":1,"version":1}`,
		"wrong schema_version":   `{"schema_version":"2.0","layer":"quality","case_id":"c","run_id":"018f7e00-0000-7000-8000-00000000bb03","dataset_digest":"sha256:` + strings.Repeat("a", 64) + `","verdict":"pass","score":1,"version":1}`,
		"trailing content":       harnessLine + ` {"schema_version":"3.0"}`,
		"not json":               `not-json`,
		"null line":              `null`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRecord([]byte(line))
			assert.Error(t, err)
		})
	}
}

func TestParseRecordAddsNoValidationOfItsOwn(t *testing.T) {
	// A record that parses but violates the frozen vocabularies is
	// Record.Validate's business, not the parser's: parsing succeeds and
	// validation rejects — the single authority stays intact.
	line := `{"schema_version":"3.0","layer":"quantum","case_id":"c","run_id":"not-a-uuid","dataset_digest":"nope","verdict":"maybe","score":1,"version":1}`
	record, err := ParseRecord([]byte(line))
	require.NoError(t, err)
	assert.ErrorIs(t, record.Validate(), ErrRecordRejected)
}
