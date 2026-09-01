package evidence

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCompanyPolicy() *Policy {
	policy, err := CompanyPolicy()
	if err != nil {
		panic(err)
	}
	return policy
}

// projectOverlay clones the company baseline as a valid project overlay.
func projectOverlay(id string, mutate func(*Policy)) *Policy {
	overlay := *testCompanyPolicy()
	overlay.ID = id
	overlay.Scope = "project"
	extends := "company-baseline"
	overlay.Extends = &extends
	if mutate != nil {
		mutate(&overlay)
	}
	return &overlay
}

// sha builds a distinct 40-char hex-only pseudo-SHA per seed (decimal
// zero-padding is valid lowercase hex).
func sha(seed int) string {
	return fmt.Sprintf("%040d", seed)
}

func TestCompanyPolicyLoadsAndValidates(t *testing.T) {
	policy := testCompanyPolicy()
	require.NoError(t, policy.Validate())
	assert.Equal(t, "company", policy.Scope)
	assert.Len(t, policy.RequiredGates, 12)
	assert.Equal(t, 80.0, policy.Coverage.ChangedLinesMinPercent)
	assert.Equal(t, 0.5, policy.Coverage.MaxTotalDropPoints)
	assert.Equal(t, []string{"critical", "high"}, policy.Security.BlockSeverities)
}

func TestPolicyValidateRejectsDrift(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Policy)
	}{
		{"bad id", func(p *Policy) { p.ID = "X" }},
		{"bad version", func(p *Policy) { p.Version = "3.0" }},
		{"bad scope", func(p *Policy) { p.Scope = "org" }},
		{"drops mandatory gate", func(p *Policy) { p.RequiredGates = p.RequiredGates[1:] }},
		{"unknown gate", func(p *Policy) { p.RequiredGates = append(p.RequiredGates, "deploy") }},
		{"duplicate gate", func(p *Policy) { p.RequiredGates = append(p.RequiredGates, GateBuild) }},
		{"coverage floor below 80", func(p *Policy) { p.Coverage.ChangedLinesMinPercent = 79.9 }},
		{"coverage floor above 100", func(p *Policy) { p.Coverage.ChangedLinesMinPercent = 101 }},
		{"drop points above 0.5", func(p *Policy) { p.Coverage.MaxTotalDropPoints = 0.6 }},
		{"negative drop points", func(p *Policy) { p.Coverage.MaxTotalDropPoints = -0.1 }},
		{"drops critical severity", func(p *Policy) { p.Security.BlockSeverities = []string{"high", "medium"} }},
		{"unknown severity", func(p *Policy) { p.Security.BlockSeverities = []string{"critical", "high", "severe"} }},
		{"flaky retries", func(p *Policy) { p.FlakyRetryCount = 2 }},
		{"waiver window", func(p *Policy) { p.Waiver.MaxDays = 14 }},
		{"waiver without distinct approver", func(p *Policy) { p.Waiver.RequiresDistinctApprover = false }},
		{"missing non-waivable principle", func(p *Policy) { p.Waiver.NonWaivableGates = p.Waiver.NonWaivableGates[:3] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := testCompanyPolicy()
			tc.mutate(policy)
			assert.Error(t, policy.Validate())
		})
	}
}

func TestPolicyDigestIsDeterministicAndSensitive(t *testing.T) {
	first, err := testCompanyPolicy().Digest()
	require.NoError(t, err)
	second, err := testCompanyPolicy().Digest()
	require.NoError(t, err)
	assert.Equal(t, first, second, "identical policies must digest identically (QG-RULE-002)")

	changed := testCompanyPolicy()
	changed.Coverage.ChangedLinesMinPercent = 85
	other, err := changed.Digest()
	require.NoError(t, err)
	assert.NotEqual(t, first, other)

	// Round-trip: digest is over the canonical wire form.
	raw, err := json.Marshal(testCompanyPolicy())
	require.NoError(t, err)
	reparsed, err := ParsePolicy(raw)
	require.NoError(t, err)
	roundTrip, err := reparsed.Digest()
	require.NoError(t, err)
	assert.Equal(t, first, roundTrip)
}

func TestParsePolicyRejectsUnknownFields(t *testing.T) {
	raw, err := json.Marshal(testCompanyPolicy())
	require.NoError(t, err)

	drifted := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal(raw, &drifted))
	drifted["execute_command"] = json.RawMessage(`"rm -rf /"`)
	raw, err = json.Marshal(drifted)
	require.NoError(t, err)

	_, err = ParsePolicy(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}
