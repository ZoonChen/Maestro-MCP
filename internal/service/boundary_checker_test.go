package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckBoundaries(t *testing.T) {
	tests := []struct {
		name                 string
		changedFiles         []string
		allowedDirs          []string
		forbiddenPatterns    []string
		wantOK               bool
		wantViolationCount   int
		wantViolationSubstrs []string // substrings expected in violations
	}{
		{
			name:               "files within allowed directories",
			changedFiles:       []string{"src/main.go", "src/util/helper.go"},
			allowedDirs:        []string{"src/"},
			forbiddenPatterns:  nil,
			wantOK:             true,
			wantViolationCount: 0,
		},
		{
			name:                 "file outside allowed directories",
			changedFiles:         []string{"src/main.go", "vendor/lib.go"},
			allowedDirs:          []string{"src/"},
			forbiddenPatterns:    nil,
			wantOK:               false,
			wantViolationCount:   1,
			wantViolationSubstrs: []string{"vendor/lib.go", "outside allowed directories"},
		},
		{
			name:                 "file matching forbidden pattern",
			changedFiles:         []string{"src/config.secret"},
			allowedDirs:          []string{"src/"},
			forbiddenPatterns:    []string{"*.secret"},
			wantOK:               false,
			wantViolationCount:   1,
			wantViolationSubstrs: []string{"config.secret", "forbidden pattern", "*.secret"},
		},
		{
			name:               "empty changed files list",
			changedFiles:       []string{},
			allowedDirs:        []string{"src/"},
			forbiddenPatterns:  []string{"*.secret"},
			wantOK:             true,
			wantViolationCount: 0,
		},
		{
			name:               "nil changed files list",
			changedFiles:       nil,
			allowedDirs:        []string{"src/"},
			forbiddenPatterns:  []string{"*.secret"},
			wantOK:             true,
			wantViolationCount: 0,
		},
		{
			name:               "empty forbidden patterns allows all files in allowed dirs",
			changedFiles:       []string{"src/main.go", "src/config.secret"},
			allowedDirs:        []string{"src/"},
			forbiddenPatterns:  []string{},
			wantOK:             true,
			wantViolationCount: 0,
		},
		{
			name: "multiple violations mixed out_of_bounds and forbidden_pattern",
			changedFiles: []string{
				"src/main.go",
				"vendor/lib.go",
				"src/config.secret",
			},
			allowedDirs:        []string{"src/"},
			forbiddenPatterns:  []string{"*.secret"},
			wantOK:             false,
			wantViolationCount: 2,
			wantViolationSubstrs: []string{
				"vendor/lib.go",
				"outside allowed directories",
				"config.secret",
				"forbidden pattern",
			},
		},
		{
			name:               "nested directory paths within allowed parent",
			changedFiles:       []string{"src/pkg/util/helper.go", "src/pkg/main.go"},
			allowedDirs:        []string{"src/pkg/"},
			forbiddenPatterns:  nil,
			wantOK:             true,
			wantViolationCount: 0,
		},
		{
			name:                 "glob pattern matches file base name",
			changedFiles:         []string{"src/app.env"},
			allowedDirs:          []string{"src/"},
			forbiddenPatterns:    []string{"*.env"},
			wantOK:               false,
			wantViolationCount:   1,
			wantViolationSubstrs: []string{"app.env", "forbidden pattern", "*.env"},
		},
		{
			name:               "file exactly matches allowed directory without trailing slash",
			changedFiles:       []string{"src"},
			allowedDirs:        []string{"src/"},
			forbiddenPatterns:  nil,
			wantOK:             true,
			wantViolationCount: 0,
		},
		{
			name:                 "missing allowed dirs fails closed",
			changedFiles:         []string{"foo/bar.go", "baz/qux.go"},
			allowedDirs:          nil,
			forbiddenPatterns:    nil,
			wantOK:               false,
			wantViolationCount:   1,
			wantViolationSubstrs: []string{"allowed_directories evidence is missing"},
		},
		{
			name:                 "empty allowed dirs JSON fails closed",
			changedFiles:         []string{"foo/bar.go"},
			allowedDirs:          []string{},
			forbiddenPatterns:    nil,
			wantOK:               false,
			wantViolationCount:   1,
			wantViolationSubstrs: []string{"must contain at least one directory"},
		},
		{
			name:                 "forbidden pattern on file inside allowed dir still triggers",
			changedFiles:         []string{"src/.env"},
			allowedDirs:          []string{"src/"},
			forbiddenPatterns:    []string{".env"},
			wantOK:               false,
			wantViolationCount:   1,
			wantViolationSubstrs: []string{".env", "forbidden pattern"},
		},
		{
			name:               "multiple allowed dirs file in second dir",
			changedFiles:       []string{"internal/service/handler.go"},
			allowedDirs:        []string{"src/", "internal/"},
			forbiddenPatterns:  nil,
			wantOK:             true,
			wantViolationCount: 0,
		},
		{
			name:                 "file in neither of multiple allowed dirs",
			changedFiles:         []string{"cmd/main.go"},
			allowedDirs:          []string{"src/", "internal/"},
			forbiddenPatterns:    nil,
			wantOK:               false,
			wantViolationCount:   1,
			wantViolationSubstrs: []string{"cmd/main.go", "outside allowed directories"},
		},
		{
			name:               "forbidden pattern does not match unrelated file",
			changedFiles:       []string{"src/main.go"},
			allowedDirs:        []string{"src/"},
			forbiddenPatterns:  []string{"*.secret"},
			wantOK:             true,
			wantViolationCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowedDirsJSON := marshalJSON(t, tt.allowedDirs)
			forbiddenPatternsJSON := marshalJSON(t, tt.forbiddenPatterns)

			got := checkBoundaries(tt.changedFiles, allowedDirsJSON, forbiddenPatternsJSON)

			if got.OK != tt.wantOK {
				t.Errorf("checkBoundaries() OK = %v, want %v", got.OK, tt.wantOK)
			}
			if len(got.Violations) != tt.wantViolationCount {
				t.Errorf("checkBoundaries() violation count = %d, want %d (violations: %v)",
					len(got.Violations), tt.wantViolationCount, got.Violations)
			}
			for _, substr := range tt.wantViolationSubstrs {
				found := false
				for _, v := range got.Violations {
					if contains(v, substr) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("checkBoundaries() expected violation containing %q, got violations: %v",
						substr, got.Violations)
				}
			}
		})
	}
}

// marshalJSON is a test helper that marshals a value to JSON string.
// Returns empty string for nil and empty slices to match production behavior.
func marshalJSON(t *testing.T, v interface{}) string {
	t.Helper()
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal test data: %v", err)
	}
	s := string(b)
	// Match production behavior: empty JSON arrays are treated as "no constraint".
	if s == "null" || s == "[]" {
		// For allowed dirs and forbidden patterns, empty arrays should produce
		// empty JSON arrays so the function still parses them (but len > 0 check
		// will see 0 items). However, to truly test "empty allowed dirs means
		// unrestricted", we pass "[]" which is valid JSON and will parse to
		// an empty slice.
		//
		// For nil slices (Go nil), we return "" to simulate the case where
		// the JSON column in the DB is empty/null.
		if s == "null" {
			return ""
		}
		return s
	}
	return s
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestCheckBoundariesRejectsTraversalAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	requireDir := filepath.Join(root, "src")
	if err := os.MkdirAll(requireDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(requireDir, "escape")); err != nil {
		t.Fatal(err)
	}

	result := checkBoundariesInWorktree(root, []string{"src/escape/secret.txt"}, `["src"]`, `[]`)
	if result.OK || result.ErrorCode != "BOUNDARY_VIOLATION" {
		t.Fatalf("expected symlink boundary violation, got %+v", result)
	}

	result = checkBoundaries([]string{"src/../../secret.txt"}, `["src"]`, `[]`)
	if result.OK {
		t.Fatalf("expected traversal boundary violation, got %+v", result)
	}
}

func TestCheckBoundariesRejectsInvalidPolicyJSON(t *testing.T) {
	result := checkBoundaries([]string{"src/main.go"}, `{`, `[]`)
	if result.OK || result.ErrorCode != "POLICY_INVALID" {
		t.Fatalf("expected fail-closed policy error, got %+v", result)
	}

	result = checkBoundaries([]string{"src/main.go"}, `["src"]`, `["["]`)
	if result.OK || result.ErrorCode != "POLICY_INVALID" {
		t.Fatalf("expected invalid glob policy error, got %+v", result)
	}
}
