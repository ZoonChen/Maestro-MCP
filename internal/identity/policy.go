// Package identity implements the M1 authorization core (M1-AUTH-001,
// ADR-003): the server-side PrincipalContext and the unified
// authorize(principal, action, resource) policy decision point backed by
// the frozen permission matrix. Default deny; deny always overrides allow;
// protected actions are refused for Maestro principals outright.
package identity

import (
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/docs/specs/rbac"
	"gopkg.in/yaml.v3"
)

// Policy is the parsed, immutable permission matrix.
type Policy struct {
	Version        string
	DefaultEffect  string
	Roles          map[string]Grants
	Service        map[string]Grants
	Bootstrap      map[string]Grants
	FunctionalRole map[string]Grants
	Protected      ProtectedActions
	Delegation     DelegationRules
}

// Grants is one principal class's allow/deny permission sets.
type Grants struct {
	Allow map[string]struct{}
	Deny  map[string]struct{}
}

// ProtectedActions are actions Maestro principals can never take.
type ProtectedActions struct {
	FinalMergeExecutor    string   `yaml:"executor"`
	FinalMergeAllowed     bool     `yaml:"maestro_allowed"`
	NonWaivablePrinciples []string `yaml:"non_waivable"`
}

// DelegationRules encode the frozen agent-restriction flags.
type DelegationRules struct {
	AgentIntersection    string `yaml:"agent_effective_permissions"`
	ServiceInheritHuman  bool   `yaml:"service_accounts_inherit_human_roles"`
	AgentDefineCommand   bool   `yaml:"agent_may_define_command_network_or_secret"`
	AgentSelfReviewWaive bool   `yaml:"agent_may_self_review_waive_or_merge"`
}

type grantsYAML struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
	// Conditions are dynamic predicates (membership, subject identity,
	// rate limits) enforced by the calling surface with request context;
	// the static evaluator keeps them parsed for traceability.
	Conditions map[string]yaml.Node `yaml:"conditions"`
}

type policyYAML struct {
	Version             string                `yaml:"version"`
	DefaultEffect       string                `yaml:"default_effect"`
	Roles               map[string]grantsYAML `yaml:"roles"`
	FunctionalApprovers map[string]grantsYAML `yaml:"functional_approvers"`
	ServiceIdentities   map[string]grantsYAML `yaml:"service_identities"`
	BootstrapIdentities map[string]grantsYAML `yaml:"bootstrap_identities"`
	Delegation          DelegationRules       `yaml:"delegation"`
	ProtectedActions    protectedYAML         `yaml:"protected_actions"`
}

type protectedYAML struct {
	FinalMerge  finalMergeYAML `yaml:"final_merge"`
	NonWaivable []string       `yaml:"non_waivable"`
}

type finalMergeYAML struct {
	Executor       string `yaml:"executor"`
	MaestroAllowed bool   `yaml:"maestro_allowed"`
}

// LoadPolicy parses and validates the permission matrix. Structural drift
// (unknown default effect, empty roles) fails closed.
func LoadPolicy(raw []byte) (*Policy, error) {
	var parsed policyYAML
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("identity: parse permission matrix: %w", err)
	}
	if parsed.Version != "3.0" {
		return nil, fmt.Errorf("identity: unsupported permission matrix version %q", parsed.Version)
	}
	if parsed.DefaultEffect != "deny" {
		return nil, fmt.Errorf("identity: default_effect must be deny, got %q", parsed.DefaultEffect)
	}
	policy := &Policy{
		Version:        parsed.Version,
		DefaultEffect:  parsed.DefaultEffect,
		Roles:          grantsMap(parsed.Roles),
		FunctionalRole: grantsMap(parsed.FunctionalApprovers),
		Service:        grantsMap(parsed.ServiceIdentities),
		Bootstrap:      grantsMap(parsed.BootstrapIdentities),
		Delegation:     parsed.Delegation,
		Protected: ProtectedActions{
			FinalMergeExecutor:    parsed.ProtectedActions.FinalMerge.Executor,
			FinalMergeAllowed:     parsed.ProtectedActions.FinalMerge.MaestroAllowed,
			NonWaivablePrinciples: parsed.ProtectedActions.NonWaivable,
		},
	}
	if len(policy.Roles) == 0 {
		return nil, fmt.Errorf("identity: permission matrix has no roles")
	}
	return policy, nil
}

// EmbeddedPolicy loads the frozen matrix compiled into the binary.
func EmbeddedPolicy() (*Policy, error) {
	return LoadPolicy(rbacspec.PermissionsYAML)
}

func grantsMap(raw map[string]grantsYAML) map[string]Grants {
	converted := make(map[string]Grants, len(raw))
	for name, grants := range raw {
		converted[name] = Grants{
			Allow: setOf(grants.Allow),
			Deny:  setOf(grants.Deny),
		}
	}
	return converted
}

func setOf(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}
