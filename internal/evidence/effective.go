package evidence

import (
	"fmt"
	"slices"
)

// EffectivePolicy is the resolved policy for one project plus its
// provenance chain (EffectiveQualityPolicy wire shape).
type EffectivePolicy struct {
	Policy       *Policy
	PolicyDigest string
	Provenance   []PolicyLayer
}

// PolicyLayer records one layer that contributed to the resolution
// (the wire provenance shape).
type PolicyLayer struct {
	Scope    string `json:"scope"` // company | project | task
	PolicyID string `json:"policy_id"`
	Version  string `json:"version"`
	Digest   string `json:"digest"`
}

// ErrPolicyWeakened reports a project overlay that tries to relax the
// company baseline (QG-RULE-001): weakening is rejected, never clamped.
type ErrPolicyWeakened struct{ Reason string }

func (e *ErrPolicyWeakened) Error() string {
	return "policy weakening rejected: " + e.Reason
}

// ResolveEffective merges the company baseline with an optional project
// overlay (task overlays are a future scope; the merge rules are the
// same). The effective required set is the two-tier formula (D2):
//
//	required = core ∪ {g ∈ capability_gates : g.capability ∈ project.capabilities}
//
// The overlay may only ADD gates (via capability declarations — every
// addition carries its producer anchor), RAISE the changed-lines floor,
// TIGHTEN the total-drop ceiling, and EXPAND blocking severities and the
// license denylist. Anything else fails closed. Core gates can never be
// removed by an overlay (the ratchet is unchanged); capability
// declarations only ever add gates. Undeclared capability gates are not
// required — and therefore never appear as gate snapshots, ending the
// permanent-pending noise of producerless gates.
func ResolveEffective(company *Policy, project *Policy) (*EffectivePolicy, error) {
	if err := company.Validate(); err != nil {
		return nil, fmt.Errorf("company baseline: %w", err)
	}
	if company.Scope != "company" {
		return nil, fmt.Errorf("company baseline has scope %q", company.Scope)
	}

	companyDigest, err := company.Digest()
	if err != nil {
		return nil, err
	}
	layers := []PolicyLayer{{
		Scope: "company", PolicyID: company.ID, Version: company.Version, Digest: companyDigest,
	}}

	effective := *company
	if project == nil {
		digest, err := effective.Digest()
		if err != nil {
			return nil, err
		}
		return &EffectivePolicy{Policy: &effective, PolicyDigest: digest, Provenance: layers}, nil
	}

	if err := project.Validate(); err != nil {
		return nil, fmt.Errorf("project overlay: %w", err)
	}
	if project.Scope != "project" {
		return nil, fmt.Errorf("project overlay has scope %q, must be project", project.Scope)
	}
	if project.Extends != nil && *project.Extends != company.ID {
		return nil, fmt.Errorf("project overlay extends %q, company baseline is %q", *project.Extends, company.ID)
	}
	projectDigest, err := project.Digest()
	if err != nil {
		return nil, err
	}

	// QG-RULE-001 monotonic strengthening. The company core is the
	// floor: removing or reordering away a core gate is weakening. Gate
	// additions arrive only through capability declarations (each already
	// validated to carry its producer anchor), so the union below is
	// exactly core ∪ declared-capability gates.
	for _, gate := range company.RequiredGates {
		if !slices.Contains(project.RequiredGates, gate) {
			return nil, &ErrPolicyWeakened{Reason: fmt.Sprintf("project %s drops required gate %q", project.ID, gate)}
		}
	}
	if project.Coverage.ChangedLinesMinPercent < company.Coverage.ChangedLinesMinPercent {
		return nil, &ErrPolicyWeakened{Reason: fmt.Sprintf(
			"project %s lowers changed_lines_min_percent from %v to %v",
			project.ID, company.Coverage.ChangedLinesMinPercent, project.Coverage.ChangedLinesMinPercent)}
	}
	if project.Coverage.MaxTotalDropPoints > company.Coverage.MaxTotalDropPoints {
		return nil, &ErrPolicyWeakened{Reason: fmt.Sprintf(
			"project %s widens max_total_drop_points from %v to %v",
			project.ID, company.Coverage.MaxTotalDropPoints, project.Coverage.MaxTotalDropPoints)}
	}
	for _, severity := range company.Security.BlockSeverities {
		if !slices.Contains(project.Security.BlockSeverities, severity) {
			return nil, &ErrPolicyWeakened{Reason: fmt.Sprintf(
				"project %s unblocks severity %q", project.ID, severity)}
		}
	}
	for _, license := range company.Security.LicenseDenylist {
		if !slices.Contains(project.Security.LicenseDenylist, license) {
			return nil, &ErrPolicyWeakened{Reason: fmt.Sprintf(
				"project %s removes license %q from the denylist", project.ID, license)}
		}
	}

	effective = *project
	effective.RequiredGates = sortedUnionGates(company, project)
	effective.Coverage = CoveragePolicy{
		ChangedLinesMinPercent: project.Coverage.ChangedLinesMinPercent,
		MaxTotalDropPoints:     project.Coverage.MaxTotalDropPoints,
	}
	effective.Security = SecurityPolicy{
		BlockSeverities: append([]string(nil), project.Security.BlockSeverities...),
		LicenseDenylist: append([]string(nil), project.Security.LicenseDenylist...),
	}
	// Declarations ride along so the digest (and policy_integrity) sees
	// them; the capability CATALOG stays company-owned — a project-scoped
	// document must not carry one.
	effective.Capabilities = append([]CapabilityDeclaration(nil), project.Capabilities...)
	effective.CapabilityGates = nil
	// The merged document is project-scoped: it is the policy in force
	// for this project, provenance records both contributing layers.
	effective.Scope = "project"

	digest, err := effective.Digest()
	if err != nil {
		return nil, err
	}
	layers = append(layers, PolicyLayer{
		Scope: "project", PolicyID: project.ID, Version: project.Version, Digest: projectDigest,
	})
	return &EffectivePolicy{Policy: &effective, PolicyDigest: digest, Provenance: layers}, nil
}

// sortedUnionGates merges both gate lists deterministically: company
// gates in company order first, project-only additions sorted after.
func sortedUnionGates(company, project *Policy) []string {
	merged := append([]string(nil), company.RequiredGates...)
	additions := []string{}
	for _, gate := range project.RequiredGates {
		if !slices.Contains(merged, gate) {
			additions = append(additions, gate)
		}
	}
	slices.Sort(additions)
	return append(merged, additions...)
}
