// Package evidence implements the M2-QG-001 quality engine: versioned
// policy resolution with monotonic strengthening (company → project),
// deterministic effective-policy digests, immutable evidence validation,
// exact-SHA gate aggregation with the seven-state model, and the waiver
// lifecycle rules — all against the frozen quality-policy.schema.json,
// evidence.schema.json and QUAL-QUALITY-POLICY / QUAL-GATES-EVIDENCE.
//
// The engine is deterministic and side-effect free: evaluation maps
// (effective policy, SHA tuple, evidence set, waivers) to gate snapshots
// and a ready verdict. Persistence lives in internal/store; transport in
// internal/handler. Nothing here treats a missing, errored or stale
// input as a pass (QG-REQ-002 fail-closed).
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
)

// Gate kinds from the frozen required_gates enum.
const (
	GateBaselineFreshness = "baseline_freshness"
	GateBoundary          = "boundary"
	GatePolicyIntegrity   = "policy_integrity"
	GateBuild             = "build"
	GateUnit              = "unit"
	GateLintTypecheck     = "lint_typecheck"
	GateCoverage          = "coverage"
	GateIntegration       = "integration"
	GateContract          = "contract"
	GateSecretScan        = "secret_scan"
	GateSAST              = "sast"
	GateDependency        = "dependency"
	GateImage             = "image"
	GateLicense           = "license"
)

// gateKinds is the frozen enum: an unknown kind never validates.
var gateKinds = map[string]struct{}{
	GateBaselineFreshness: {}, GateBoundary: {}, GatePolicyIntegrity: {},
	GateBuild: {}, GateUnit: {}, GateLintTypecheck: {}, GateCoverage: {},
	GateIntegration: {}, GateContract: {}, GateSecretScan: {}, GateSAST: {},
	GateDependency: {}, GateImage: {}, GateLicense: {},
}

// mandatoryGates must appear in every policy (the frozen allOf block).
var mandatoryGates = []string{
	GateBaselineFreshness, GateBoundary, GatePolicyIntegrity, GateBuild,
	GateUnit, GateLintTypecheck, GateCoverage, GateSecretScan, GateSAST,
	GateDependency, GateImage, GateLicense,
}

// nonWaivablePrinciples are the frozen QG-RULE-005 set: they are
// principles rather than gate kinds and can never be waived.
var nonWaivablePrinciples = []string{
	"identity_isolation", "sha_integrity", "policy_integrity", "webhook_authenticity",
}

var (
	policyIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	policySemver      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	severityLevels    = []string{"critical", "high", "medium", "low"}
	companySeverities = []string{"critical", "high"}
)

// Policy mirrors quality-policy.schema.json. Field order is frozen for
// the canonical marshaling the digest is computed over.
type Policy struct {
	ID              string         `json:"id"`
	Version         string         `json:"version"`
	Scope           string         `json:"scope"`
	Extends         *string        `json:"extends,omitempty"`
	RequiredGates   []string       `json:"required_gates"`
	Coverage        CoveragePolicy `json:"coverage"`
	Security        SecurityPolicy `json:"security"`
	FlakyRetryCount int            `json:"flaky_retry_count"`
	Waiver          WaiverPolicy   `json:"waiver"`
}

type CoveragePolicy struct {
	ChangedLinesMinPercent float64 `json:"changed_lines_min_percent"`
	MaxTotalDropPoints     float64 `json:"max_total_drop_points"`
}

type SecurityPolicy struct {
	BlockSeverities []string `json:"block_severities"`
	LicenseDenylist []string `json:"license_denylist"`
}

type WaiverPolicy struct {
	MaxDays                  int      `json:"max_days"`
	RequiresDistinctApprover bool     `json:"requires_distinct_approver"`
	NonWaivableGates         []string `json:"non_waivable_gates"`
}

// Validate enforces every constraint of the frozen schema; structural
// drift fails closed with a precise reason.
func (p *Policy) Validate() error {
	if !policyIDPattern.MatchString(p.ID) {
		return fmt.Errorf("policy id %q does not match ^[a-z][a-z0-9-]{2,63}$", p.ID)
	}
	if !policySemver.MatchString(p.Version) {
		return fmt.Errorf("policy %s: version %q is not semver", p.ID, p.Version)
	}
	switch p.Scope {
	case "company", "project", "task":
	default:
		return fmt.Errorf("policy %s: scope %q is not company/project/task", p.ID, p.Scope)
	}
	if p.Extends != nil && !policyIDPattern.MatchString(*p.Extends) {
		return fmt.Errorf("policy %s: extends %q is malformed", p.ID, *p.Extends)
	}
	if len(p.RequiredGates) < len(mandatoryGates) {
		return fmt.Errorf("policy %s: required_gates has %d entries, minimum is %d", p.ID, len(p.RequiredGates), len(mandatoryGates))
	}
	seen := map[string]int{}
	for _, gate := range p.RequiredGates {
		if _, known := gateKinds[gate]; !known {
			return fmt.Errorf("policy %s: required gate %q is outside the frozen enum", p.ID, gate)
		}
		seen[gate]++
		if seen[gate] > 1 {
			return fmt.Errorf("policy %s: required gate %q appears twice", p.ID, gate)
		}
	}
	for _, gate := range mandatoryGates {
		if seen[gate] == 0 {
			return fmt.Errorf("policy %s: mandatory gate %q is missing", p.ID, gate)
		}
	}
	if p.Coverage.ChangedLinesMinPercent < 80 || p.Coverage.ChangedLinesMinPercent > 100 {
		return fmt.Errorf("policy %s: changed_lines_min_percent %v outside [80,100]", p.ID, p.Coverage.ChangedLinesMinPercent)
	}
	if p.Coverage.MaxTotalDropPoints < 0 || p.Coverage.MaxTotalDropPoints > 0.5 {
		return fmt.Errorf("policy %s: max_total_drop_points %v outside [0,0.5]", p.ID, p.Coverage.MaxTotalDropPoints)
	}
	for _, severity := range p.Security.BlockSeverities {
		if !slices.Contains(severityLevels, severity) {
			return fmt.Errorf("policy %s: block severity %q is outside the enum", p.ID, severity)
		}
	}
	for _, floor := range companySeverities {
		if !slices.Contains(p.Security.BlockSeverities, floor) {
			return fmt.Errorf("policy %s: block_severities must contain %q", p.ID, floor)
		}
	}
	if p.FlakyRetryCount != 1 {
		return fmt.Errorf("policy %s: flaky_retry_count must be 1", p.ID)
	}
	if p.Waiver.MaxDays != 7 {
		return fmt.Errorf("policy %s: waiver max_days must be 7", p.ID)
	}
	if !p.Waiver.RequiresDistinctApprover {
		return fmt.Errorf("policy %s: waiver must require a distinct approver", p.ID)
	}
	if len(p.Waiver.NonWaivableGates) != len(nonWaivablePrinciples) {
		return fmt.Errorf("policy %s: non_waivable_gates must list exactly the four principles", p.ID)
	}
	for _, principle := range nonWaivablePrinciples {
		if !slices.Contains(p.Waiver.NonWaivableGates, principle) {
			return fmt.Errorf("policy %s: non-waivable principle %q is missing", p.ID, principle)
		}
	}
	return nil
}

// Digest returns the QG-RULE-002 policy digest: sha256 over the
// canonical JSON encoding. Struct field order is frozen, gates keep
// their document order, and lists are validated unique beforehand, so
// identical inputs always produce identical bytes.
func (p *Policy) Digest() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("policy digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// ParsePolicy decodes and validates a policy document.
func ParsePolicy(raw []byte) (*Policy, error) {
	var policy Policy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, fmt.Errorf("policy parse: %w", err)
	}
	// Unknown fields are structural drift, not ignorable extras.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("policy parse: %w", err)
	}
	known := map[string]bool{
		"id": true, "version": true, "scope": true, "extends": true,
		"required_gates": true, "coverage": true, "security": true,
		"flaky_retry_count": true, "waiver": true,
	}
	for field := range probe {
		if !known[field] {
			return nil, fmt.Errorf("policy parse: unknown field %q", field)
		}
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &policy, nil
}
