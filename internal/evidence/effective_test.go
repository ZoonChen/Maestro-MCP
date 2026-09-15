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

func TestResolveEffectiveCoreOnlyForUndeclaredCapabilities(t *testing.T) {
	// D2-4 shape: a pilot with zero declarations gets exactly the core
	// six — undeclared capability gates are not required and never
	// surface as pending snapshots.
	resolved, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{
		GateBuild, GateUnit, GateSecretScan,
		GatePolicyIntegrity, GateBaselineFreshness, GateBoundary,
	}, resolved.Policy.RequiredGates)

	// An overlay that declares nothing resolves to the same six.
	empty := projectOverlay("acme-plain", nil)
	overlayResolved, err := ResolveEffective(testCompanyPolicy(), empty)
	require.NoError(t, err)
	assert.Equal(t, resolved.Policy.RequiredGates, overlayResolved.Policy.RequiredGates)
	for _, gate := range overlayResolved.Policy.RequiredGates {
		assert.NotContains(t, []string{
			GateCoverage, GateLintTypecheck, GateLicense, GateSAST,
			GateDependency, GateImage, GateIntegration, GateContract,
		}, gate)
	}
}

func TestResolveEffectiveCapabilityDeclarationsAddGates(t *testing.T) {
	overlay := projectOverlay("acme-capable", func(p *Policy) {
		declaredCapability(p, "quality.coverage", GateCoverage, "acme/backend", "coverage")
		declaredCapability(p, "security.sast", GateSAST, "acme/backend", "sast")
		p.Coverage.ChangedLinesMinPercent = 85
	})
	resolved, err := ResolveEffective(testCompanyPolicy(), overlay)
	require.NoError(t, err)

	// required = core ∪ declared: the two declared capability gates join
	// after the frozen core order, sorted.
	require.Len(t, resolved.Policy.RequiredGates, len(coreGates)+2)
	assert.Equal(t, []string{GateCoverage, GateSAST}, resolved.Policy.RequiredGates[len(coreGates):])
	assert.Equal(t, 85.0, resolved.Policy.Coverage.ChangedLinesMinPercent)

	// The declarations ride on the effective document (digest covers
	// them); the catalog stays company-owned.
	require.Len(t, resolved.Policy.Capabilities, 2)
	assert.Empty(t, resolved.Policy.CapabilityGates)

	// Determinism across runs.
	again, err := ResolveEffective(testCompanyPolicy(), overlay)
	require.NoError(t, err)
	assert.Equal(t, resolved.PolicyDigest, again.PolicyDigest)
}

func TestResolveEffectiveStrengthensMonotonically(t *testing.T) {
	overlay := projectOverlay("acme-strict", func(p *Policy) {
		declaredCapability(p, "integration.enabled", GateIntegration, "acme/integration", "integration")
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
		declaredCapability(p, "integration.enabled", GateIntegration, "acme/integration", "integration")
		declaredCapability(p, "contract.openapi", GateContract, "acme/contracts", "contract")
	})
	twoResolved, err := ResolveEffective(testCompanyPolicy(), withTwo)
	require.NoError(t, err)
	assert.Equal(t,
		[]string{GateContract, GateIntegration},
		twoResolved.Policy.RequiredGates[len(coreGates):])
}

func TestResolveEffectiveRejectsWeakening(t *testing.T) {
	cases := []struct {
		name          string
		companyMutate func(*Policy)
		overlayMutate func(*Policy)
	}{
		{
			name:          "drops a required gate",
			overlayMutate: func(p *Policy) { p.RequiredGates = p.RequiredGates[:len(p.RequiredGates)-1] },
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
