package evidence

import (
	"embed"
	"fmt"
)

// companyPolicyJSON is the embedded company baseline (the
// permissions.yaml precedent: the deployment authority ships with the
// binary, drift fails closed at startup, and rotation happens through a
// release — never through a request).
//
//go:embed company_policy.json
var companyPolicyFS embed.FS

const companyPolicyAsset = "company_policy.json"

// CompanyPolicy loads and validates the embedded company baseline.
func CompanyPolicy() (*Policy, error) {
	raw, err := companyPolicyFS.ReadFile(companyPolicyAsset)
	if err != nil {
		return nil, fmt.Errorf("company policy: %w", err)
	}
	policy, err := ParsePolicy(raw)
	if err != nil {
		return nil, fmt.Errorf("company policy: %w", err)
	}
	if policy.Scope != "company" {
		return nil, fmt.Errorf("company policy: embedded asset has scope %q", policy.Scope)
	}
	return policy, nil
}
