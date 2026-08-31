package evidence

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveEffectiveWithoutOverlay(t *testing.T) {
	resolved, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)
	assert.Equal(t, "company", resolved.Policy.Scope)
	require.Len(t, resolved.Provenance, 1)
	assert.Equal(t, "company", resolved.Provenance[0].Scope)

	again, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)
	assert.Equal(t, resolved.PolicyDigest, again.PolicyDigest, "no-overlay resolution is deterministic")
}

func TestResolveEffectiveStrengthensMonotonically(t *testing.T) {
	overlay := projectOverlay("acme-strict", func(p *Policy) {
		p.RequiredGates = append(p.RequiredGates, GateIntegration)
		p.Coverage.ChangedLinesMinPercent = 85
		p.Coverage.MaxTotalDropPoints = 0.3
		p.Security.BlockSeverities = []string{"critical", "high", "medium"}
		p.Security.LicenseDenylist = append(p.Security.LicenseDenylist, "GPL-2.0-only")
	})
	resolved, err := ResolveEffective(testCompanyPolicy(), overlay)
	require.NoError(t, err)

	assert.Contains(t, resolved.Policy.RequiredGates, GateIntegration)
	assert.Equal(t, 85.0, resolved.Policy.Coverage.ChangedLinesMinPercent)
	assert.Equal(t, 0.3, resolved.Policy.Coverage.MaxTotalDropPoints)
	assert.Equal(t, []string{"critical", "high", "medium"}, resolved.Policy.Security.BlockSeverities)
	require.Len(t, resolved.Provenance, 2)
	assert.Equal(t, "project", resolved.Provenance[1].Scope)

	// Determinism across runs.
	again, err := ResolveEffective(testCompanyPolicy(), overlay)
	require.NoError(t, err)
	assert.Equal(t, resolved.PolicyDigest, again.PolicyDigest)

	// Gate additions land after the frozen company order, sorted.
	withTwo := projectOverlay("acme-strict", func(p *Policy) {
		p.RequiredGates = append(p.RequiredGates, GateIntegration, GateContract)
	})
	twoResolved, err := ResolveEffective(testCompanyPolicy(), withTwo)
	require.NoError(t, err)
	assert.Equal(t,
		[]string{GateContract, GateIntegration},
		twoResolved.Policy.RequiredGates[12:])
}

func TestResolveEffectiveRejectsWeakening(t *testing.T) {
	cases := []struct {
		name          string
		companyMutate func(*Policy)
		overlayMutate func(*Policy)
	}{
		{
			name:          "drops a required gate",
			overlayMutate: func(p *Policy) { p.RequiredGates = p.RequiredGates[:11] },
		},
		{
			name: "lowers the changed-lines floor",
			overlayMutate: func(p *Policy) {
				company := testCompanyPolicy()
				p.Coverage.ChangedLinesMinPercent = company.Coverage.ChangedLinesMinPercent - 0.1
			},
		},
		{
			name:          "widens the total-drop ceiling",
			companyMutate: func(p *Policy) { p.Coverage.MaxTotalDropPoints = 0.3 },
			overlayMutate: func(p *Policy) { p.Coverage.MaxTotalDropPoints = 0.5 },
		},
		{
			name: "unblocks a severity",
			overlayMutate: func(p *Policy) {
				p.Security.BlockSeverities = []string{"critical", "medium"}
			},
		},
		{
			name: "removes a denied license",
			overlayMutate: func(p *Policy) {
				p.Security.LicenseDenylist = p.Security.LicenseDenylist[:1]
			},
		},
		{
			name: "wrong extends",
			overlayMutate: func(p *Policy) {
				other := "someone-else"
				p.Extends = &other
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			company := testCompanyPolicy()
			if tc.companyMutate != nil {
				tc.companyMutate(company)
			}
			overlay := projectOverlay("acme-weak", tc.overlayMutate)
			_, err := ResolveEffective(company, overlay)
			require.Error(t, err, tc.name)
		})
	}
}

func TestResolveEffectiveRejectsWrongScopes(t *testing.T) {
	company := testCompanyPolicy()
	task := projectOverlay("task-scope", func(p *Policy) { p.Scope = "task" })
	_, err := ResolveEffective(company, task)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scope")

	notCompany := projectOverlay("not-company", nil)
	notCompany.Scope = "company"
	_, err = ResolveEffective(notCompany, projectOverlay("p", nil))
	require.Error(t, err)
}
