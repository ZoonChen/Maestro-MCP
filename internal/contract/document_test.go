package contract

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const validDocument = `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1.0.0"},
  "paths": {
    "/a": {"get": {"responses": {"200": {"description": "ok"}}}}
  }
}`

func TestParseDocumentAcceptsValidDocument(t *testing.T) {
	doc, err := ParseDocument([]byte(validDocument))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.OpenAPI != "3.0.3" {
		t.Fatalf("version = %q", doc.OpenAPI)
	}
	if len(doc.CanonicalHash) != 7+64 || doc.CanonicalHash[:7] != "sha256:" {
		t.Fatalf("hash shape = %q", doc.CanonicalHash)
	}
}

func TestParseDocumentFailsClosed(t *testing.T) {
	cases := map[string]string{
		"not json":          `{`,
		"empty":             ``,
		"missing openapi":   `{"paths": {}}`,
		"swagger 2.0":       `{"swagger": "2.0", "paths": {}}`,
		"openapi 4.0":       `{"openapi": "4.0.0", "paths": {}}`,
		"missing paths":     `{"openapi": "3.0.3"}`,
		"path without dash": `{"openapi": "3.0.3", "paths": {"orders": {}}}`,
		"path item object":  `{"openapi": "3.0.3", "paths": {"/a": 7}}`,
		"operation object":  `{"openapi": "3.0.3", "paths": {"/a": {"get": 7}}}`,
	}
	for name, payload := range cases {
		if _, err := ParseDocument([]byte(payload)); err == nil {
			t.Fatalf("%s: expected fail-closed error", name)
		}
	}
}

func TestCanonicalizeStableAcrossKeyOrderAndWhitespace(t *testing.T) {
	reordered := `{
      "paths": {
        "/a": { "get": { "responses": { "200": { "description": "ok" } } } }
      },
      "openapi": "3.0.3",
      "info": {"version": "1.0.0", "title": "t"}
    }`
	first, err := Canonicalize([]byte(validDocument))
	if err != nil {
		t.Fatalf("canonicalize first: %v", err)
	}
	second, err := Canonicalize([]byte(reordered))
	if err != nil {
		t.Fatalf("canonicalize second: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("canonical forms differ:\n%s\n%s", first, second)
	}
}

func TestCanonicalizePreservesNumericLexemes(t *testing.T) {
	first, err := Canonicalize([]byte(`{"a": 1.0}`))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := Canonicalize([]byte(`{"a": 1}`))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(first) == string(second) {
		t.Fatalf("numeric lexemes 1.0 and 1 must stay distinct")
	}
}

func TestGoldenFixtures(t *testing.T) {
	const fixtureRoot = "../../tests/fixtures/openapi-golden"
	entries, err := os.ReadDir(fixtureRoot)
	if err != nil {
		t.Fatalf("read fixture root: %v", err)
	}
	ran := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		ran++
		name := entry.Name()
		load := func(file string) []byte {
			data, err := os.ReadFile(fixtureRoot + "/" + name + "/" + file)
			if err != nil {
				t.Fatalf("%s: read %s: %v", name, file, err)
			}
			return data
		}
		expectedRaw := load("expected.json")
		expected := struct {
			Compatible        bool     `json:"compatible"`
			BreakingLocations []string `json:"breaking_locations"`
		}{}
		if err := json.Unmarshal(expectedRaw, &expected); err != nil {
			t.Fatalf("%s: expected.json: %v", name, err)
		}
		oldDoc, err := ParseDocument(load("base.json"))
		if err != nil {
			t.Fatalf("%s: parse base: %v", name, err)
		}
		newDoc, err := ParseDocument(load("variant.json"))
		if err != nil {
			t.Fatalf("%s: parse variant: %v", name, err)
		}
		result := Diff(oldDoc, newDoc)
		if result.Ruleset != RulesetVersion {
			t.Fatalf("%s: ruleset = %q", name, result.Ruleset)
		}
		if result.Compatible != expected.Compatible {
			t.Fatalf("%s: compatible = %v, want %v (changes: %+v)", name, result.Compatible, expected.Compatible, result.Changes)
		}
		for _, fragment := range expected.BreakingLocations {
			found := false
			for _, change := range result.Changes {
				if change.Breaking && containsFragment(change.Location, fragment) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s: breaking location %q not reported (changes: %+v)", name, fragment, result.Changes)
			}
		}
		if expected.Compatible {
			for _, change := range result.Changes {
				if change.Breaking {
					t.Fatalf("%s: unexpected breaking change %+v", name, change)
				}
			}
		}
	}
	if ran == 0 {
		t.Fatalf("no golden fixture cases found")
	}
}

func containsFragment(location, fragment string) bool {
	return strings.Contains(location, fragment)
}
