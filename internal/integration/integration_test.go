package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseManifest() Manifest {
	return Manifest{
		Revisions: []RepositoryRevision{
			{RepositoryMappingID: "m-web", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{RepositoryMappingID: "m-api", SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
		ContractHash:       "sha256:" + "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1",
		SuiteVersion:       "suite-2",
		EnvironmentProfile: "staging-1",
		FixtureVersion:     "fx-3",
		PolicyVersion:      "pol-1",
		TTL:                time.Hour,
	}
}

func TestManifestValidateFailClosed(t *testing.T) {
	base := baseManifest()
	single := baseManifest()
	single.Revisions = single.Revisions[:1]
	require.Error(t, (&single).Validate(), "cross-repository means at least two")

	dup := baseManifest()
	dup.Revisions = append(dup.Revisions, RepositoryRevision{RepositoryMappingID: "m-web", SHA: "cccccccccccccccccccccccccccccccccccccccc"})
	require.Error(t, (&dup).Validate(), "role uniqueness")

	badSHA := baseManifest()
	badSHA.Revisions[0].SHA = "not-hex!"
	require.Error(t, (&badSHA).Validate())

	for name, ttl := range map[string]time.Duration{
		"below floor":   14 * time.Minute,
		"above ceiling": 25 * time.Hour,
	} {
		manifest := baseManifest()
		manifest.TTL = ttl
		require.Error(t, (&manifest).Validate(), name)
	}
	require.NoError(t, (&base).Validate())
	// (base used below for digest identity)
}

func TestCombinationDigestIdentity(t *testing.T) {
	base := baseManifest()

	// Repository order does not matter.
	reordered := baseManifest()
	reordered.Revisions = []RepositoryRevision{base.Revisions[1], base.Revisions[0]}
	a, err := base.CombinationDigest()
	require.NoError(t, err)
	b, err := reordered.CombinationDigest()
	require.NoError(t, err)
	assert.Equal(t, a, b, "combination is a set, not a sequence")

	// ANY value change forks the identity (E2E-RULE-001).
	for name, mutate := range map[string]func(*Manifest){
		"repository sha": func(m *Manifest) { m.Revisions[0].SHA = "dddddddddddddddddddddddddddddddddddddddd" },
		"contract hash": func(m *Manifest) {
			m.ContractHash = "sha256:" + "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"
		},
		"suite version": func(m *Manifest) { m.SuiteVersion = "suite-3" },
		"environment":   func(m *Manifest) { m.EnvironmentProfile = "staging-2" },
		"fixture":       func(m *Manifest) { m.FixtureVersion = "fx-4" },
		"policy":        func(m *Manifest) { m.PolicyVersion = "pol-2" },
		"ttl":           func(m *Manifest) { m.TTL = 2 * time.Hour },
		"extra repo": func(m *Manifest) {
			m.Revisions = append(m.Revisions, RepositoryRevision{RepositoryMappingID: "m-db", SHA: "ffffffffffffffffffffffffffffffffffffffff"})
		},
	} {
		variant := baseManifest()
		mutate(&variant)
		v, err := variant.CombinationDigest()
		require.NoError(t, err)
		assert.NotEqual(t, a, v, name)
	}
}
