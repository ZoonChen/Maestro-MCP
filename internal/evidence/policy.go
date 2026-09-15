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

// coreGates are the unconditionally required gates (the frozen allOf
// block): every one has a guaranteed producer — CI pipeline jobs for
// build/unit/secret_scan and the D1 control-plane self-attestation for
// the three engine-oracle gates.
var coreGates = []string{
	GateBuild, GateUnit, GateSecretScan,
	GatePolicyIntegrity, GateBaselineFreshness, GateBoundary,
}

// producerKindPipelineJob / producerKindControlPlane are the two frozen
// producer kinds (quality-policy.schema.json $defs.producer_kind).
const (
	producerKindPipelineJob  = "pipeline_job"
	producerKindControlPlane = "control_plane"
)

// CapabilityGate is one catalog entry of the two-tier baseline (D2): a
// gate that is required ONLY for projects declaring the capability, and
// whose promised producer kind is declared up front. The catalog is the
// frozen contract "声明能力 = 声明生产者" — a capability gate can never
// become an unconditional requirement without a producer behind it.
type CapabilityGate struct {
	GateID        string `json:"gate_id"`
	CapabilityKey string `json:"capability_key"`
	ProducerKind  string `json:"producer_kind"`
}

// CapabilityProducer anchors a declaration to the concrete CI producer
// that will satisfy the gate: the repository and the gate-named job. A
// declaration without this anchor is rejected — a capability promise
// with nobody producing it is exactly the F14 disease.
type CapabilityProducer struct {
	Repo string `json:"repo"`
	Job  string `json:"job"`
}

// CapabilityDeclaration is a project overlay's opt-in to one capability
// gate: the capability key plus the mandatory producer anchor.
type CapabilityDeclaration struct {
	Capability string             `json:"capability"`
	Producer   CapabilityProducer `json:"producer"`
}

// capabilityCatalog is the frozen gate↔capability mapping of the
// company baseline 3.1.0 (quality-policy.schema.json
// $defs.capability_gate_catalog). integration and contract are folded
// into the same mechanism — no more document-level special cases.
var capabilityCatalog = []CapabilityGate{
	{GateID: GateCoverage, CapabilityKey: "quality.coverage", ProducerKind: producerKindPipelineJob},
	{GateID: GateLintTypecheck, CapabilityKey: "quality.lint", ProducerKind: producerKindPipelineJob},
	{GateID: GateLicense, CapabilityKey: "supply-chain.license", ProducerKind: producerKindPipelineJob},
	{GateID: GateSAST, CapabilityKey: "security.sast", ProducerKind: producerKindPipelineJob},
	{GateID: GateDependency, CapabilityKey: "supply-chain.dependency", ProducerKind: producerKindPipelineJob},
	{GateID: GateImage, CapabilityKey: "supply-chain.image", ProducerKind: producerKindPipelineJob},
	{GateID: GateIntegration, CapabilityKey: "integration.enabled", ProducerKind: producerKindPipelineJob},
	{GateID: GateContract, CapabilityKey: "contract.openapi", ProducerKind: producerKindPipelineJob},
}

var (
	capabilityKeyByGate = map[string]string{}
	gateByCapabilityKey = map[string]string{}
)

func init() {
	for _, entry := range capabilityCatalog {
		capabilityKeyByGate[entry.GateID] = entry.CapabilityKey
		gateByCapabilityKey[entry.CapabilityKey] = entry.GateID
	}
}

// producerKindAllowed mirrors the frozen gate→producer-kinds assignment
// ($defs.gate_producer_kinds): every gate accepts pipeline_job
// producers; only the three engine-oracle gates also accept
// control_plane.
func producerKindAllowed(gate, kind string) bool {
	switch kind {
	case producerKindPipelineJob:
		return true
	case producerKindControlPlane:
		return IsControlPlaneGate(gate)
	default:
		return false
	}
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
	ID              string                  `json:"id"`
	Version         string                  `json:"version"`
	Scope           string                  `json:"scope"`
	Extends         *string                 `json:"extends,omitempty"`
	RequiredGates   []string                `json:"required_gates"`
	CapabilityGates []CapabilityGate        `json:"capability_gates,omitempty"`
	Capabilities    []CapabilityDeclaration `json:"capabilities,omitempty"`
	Coverage        CoveragePolicy          `json:"coverage"`
	Security        SecurityPolicy          `json:"security"`
	FlakyRetryCount int                     `json:"flaky_retry_count"`
	Waiver          WaiverPolicy            `json:"waiver"`
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
	if len(p.RequiredGates) < len(coreGates) {
		return fmt.Errorf("policy %s: required_gates has %d entries, minimum is %d", p.ID, len(p.RequiredGates), len(coreGates))
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
	for _, gate := range coreGates {
		if seen[gate] == 0 {
			return fmt.Errorf("policy %s: core gate %q is missing", p.ID, gate)
		}
	}
	if err := p.validateCapabilityTiers(); err != nil {
		return err
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
// validateCapabilityTiers enforces the D2 two-tier rules:
//
//   - the company scope owns the frozen capability catalog and carries
//     no declarations; a project/task overlay inherits the catalog at
//     resolution and must not repeat it
//   - every declaration names a cataloged capability and MUST carry the
//     producer anchor (repo + job) — the anti-F14 rule "声明能力 =
//     声明生产者"
//   - within one document, a capability gate listed in required_gates
//     and its declaration are inseparable in BOTH directions: an
//     undeclared capability gate is the forbidden "required gate with no
//     producer", a declaration without the gate listed is drift
func (p *Policy) validateCapabilityTiers() error {
	if p.Scope == "company" {
		if len(p.Capabilities) > 0 {
			return fmt.Errorf("policy %s: the company baseline carries capability declarations; only a project overlay may declare capabilities", p.ID)
		}
		for _, entry := range p.CapabilityGates {
			if _, known := gateKinds[entry.GateID]; !known {
				return fmt.Errorf("policy %s: capability gate %q is outside the frozen enum", p.ID, entry.GateID)
			}
			if !producerKindAllowed(entry.GateID, entry.ProducerKind) {
				return fmt.Errorf("policy %s: producer kind %q is not allowed for gate %q", p.ID, entry.ProducerKind, entry.GateID)
			}
		}
		if !capabilityCatalogEqual(p.CapabilityGates) {
			return fmt.Errorf("policy %s: company capability_gates must be exactly the frozen %d-entry catalog", p.ID, len(capabilityCatalog))
		}
		return nil
	}
	if len(p.CapabilityGates) > 0 {
		return fmt.Errorf("policy %s: capability_gates belongs to the company baseline; a %s overlay inherits the catalog at resolution", p.ID, p.Scope)
	}

	declared := map[string]bool{}
	for _, declaration := range p.Capabilities {
		gate, known := gateByCapabilityKey[declaration.Capability]
		if !known {
			return fmt.Errorf("policy %s: capability %q is outside the frozen catalog", p.ID, declaration.Capability)
		}
		if declaration.Producer.Repo == "" || declaration.Producer.Job == "" {
			return fmt.Errorf("policy %s: capability %q must anchor its producer (repo and job) — declaring a capability means declaring its producer", p.ID, declaration.Capability)
		}
		if declared[declaration.Capability] {
			return fmt.Errorf("policy %s: capability %q is declared twice", p.ID, declaration.Capability)
		}
		declared[declaration.Capability] = true
		if !slices.Contains(p.RequiredGates, gate) {
			return fmt.Errorf("policy %s: capability %q is declared but its gate %q is not in required_gates", p.ID, declaration.Capability, gate)
		}
	}
	for _, gate := range p.RequiredGates {
		key, cataloged := capabilityKeyByGate[gate]
		if cataloged && !declared[key] {
			return fmt.Errorf("policy %s: capability gate %q is required without declaring capability %q — an undeclared capability gate has no guaranteed producer", p.ID, gate, key)
		}
	}
	return nil
}

// capabilityCatalogEqual compares a capability_gates list against the
// frozen catalog as a SET (document order only affects the digest, not
// validity).
func capabilityCatalogEqual(entries []CapabilityGate) bool {
	if len(entries) != len(capabilityCatalog) {
		return false
	}
	remaining := make([]CapabilityGate, len(capabilityCatalog))
	copy(remaining, capabilityCatalog)
	for _, entry := range entries {
		match := -1
		for index, candidate := range remaining {
			if candidate == entry {
				match = index
				break
			}
		}
		if match < 0 {
			return false
		}
		remaining = append(remaining[:match], remaining[match+1:]...)
	}
	return true
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
		"required_gates": true, "capability_gates": true, "capabilities": true,
		"coverage": true, "security": true,
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
