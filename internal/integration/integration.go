// Package integration implements the M3-INT-001 manifest core: the
// exact revision combination that pins one IntegrationRun. The frozen
// E2E rules hold here — E2E-RULE-001 (any SHA/digest change creates a
// NEW run), E2E-RULE-003 (passed is valid only for the exact
// combination) — because the manifest hash IS the combination
// identity. The package is pure; persistence lives in internal/store.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RepositoryRevision is one repository's exact pin (the frozen wire
// shape: mapping id, sha, optional contract hash).
type RepositoryRevision struct {
	RepositoryMappingID string
	SHA                 string
	ContractHash        string
}

// Manifest is the full run pin: the revision set plus the suite,
// environment and policy identities (the E2E wire manifest, required
// fields per the frozen schema).
type Manifest struct {
	Revisions          []RepositoryRevision
	ContractHash       string // top-level combined contract hash
	SuiteVersion       string
	EnvironmentProfile string
	FixtureVersion     string
	PolicyVersion      string
	TTL                time.Duration
}

// Validate enforces the frozen manifest rules: at least two distinct
// repositories (a cross-repository run by definition), each with a
// valid SHA, no duplicate repository mappings, and a TTL within the
// 15-minute to 24-hour window.
func (m *Manifest) Validate() error {
	if len(m.Revisions) < 2 {
		return fmt.Errorf("integration manifest: at least two repositories are required")
	}
	seen := map[string]bool{}
	for _, revision := range m.Revisions {
		if revision.RepositoryMappingID == "" {
			return fmt.Errorf("integration manifest: repository mapping id is required")
		}
		if seen[revision.RepositoryMappingID] {
			return fmt.Errorf("integration manifest: duplicate repository %s", revision.RepositoryMappingID)
		}
		seen[revision.RepositoryMappingID] = true
		if len(revision.SHA) < 7 || len(revision.SHA) > 64 || !isHex(revision.SHA) {
			return fmt.Errorf("integration manifest: repository %s has a malformed SHA", revision.RepositoryMappingID)
		}
	}
	if m.TTL < 15*time.Minute {
		return fmt.Errorf("integration manifest: TTL %s is below the 15-minute floor", m.TTL)
	}
	if m.TTL > 24*time.Hour {
		return fmt.Errorf("integration manifest: TTL %s is above the 24-hour ceiling", m.TTL)
	}
	return nil
}

// CombinationDigest derives the run identity: a canonical JSON encoding
// of the sorted revision set and the suite/environment/policy pins.
// Key order and repository ORDER in the input do not matter; any
// VALUE change produces a different digest (E2E-RULE-001).
func (m *Manifest) CombinationDigest() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	sorted := append([]RepositoryRevision(nil), m.Revisions...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].RepositoryMappingID < sorted[j].RepositoryMappingID
	})
	type flat struct {
		Repository string `json:"repository"`
		SHA        string `json:"sha"`
		Contract   string `json:"contract,omitempty"`
	}
	canonical := struct {
		Repositories []flat `json:"repositories"`
		ContractHash string `json:"contract_hash"`
		Suite        string `json:"suite_version"`
		Environment  string `json:"environment_profile"`
		Fixture      string `json:"fixture_version"`
		Policy       string `json:"policy_version"`
		TTLSeconds   int    `json:"ttl_seconds"`
	}{}
	for _, revision := range sorted {
		canonical.Repositories = append(canonical.Repositories, flat{
			Repository: revision.RepositoryMappingID,
			SHA:        revision.SHA,
			Contract:   revision.ContractHash,
		})
	}
	canonical.ContractHash = m.ContractHash
	canonical.Suite = m.SuiteVersion
	canonical.Environment = m.EnvironmentProfile
	canonical.Fixture = m.FixtureVersion
	canonical.Policy = m.PolicyVersion
	canonical.TTLSeconds = int(m.TTL.Seconds())

	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("integration manifest: canonical encode: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func isHex(value string) bool {
	for _, ch := range strings.ToLower(value) {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
