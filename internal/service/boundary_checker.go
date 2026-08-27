package service

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// BoundaryCheckResult contains the deterministic boundary decision. Policy
// parsing/path-integrity errors are represented as violations and never pass.
type BoundaryCheckResult struct {
	OK         bool     `json:"ok"`
	ErrorCode  string   `json:"error_code,omitempty"`
	Violations []string `json:"violations,omitempty"`
}

// checkBoundaries performs lexical checks and is retained for focused unit
// tests. Validation must call checkBoundariesInWorktree so filesystem symlinks
// are also checked relative to the actual worktree.
func checkBoundaries(changedFiles []string, allowedDirsJSON, forbiddenPatternsJSON string) BoundaryCheckResult {
	return checkBoundariesInWorktree("", changedFiles, allowedDirsJSON, forbiddenPatternsJSON)
}

func checkBoundariesInWorktree(worktreeRoot string, changedFiles []string, allowedDirsJSON, forbiddenPatternsJSON string) BoundaryCheckResult {
	allowedDirs, err := parseAllowedDirectories(allowedDirsJSON)
	if err != nil {
		return BoundaryCheckResult{OK: false, ErrorCode: "POLICY_INVALID", Violations: []string{err.Error()}}
	}
	forbiddenPatterns, err := parseForbiddenPatterns(forbiddenPatternsJSON)
	if err != nil {
		return BoundaryCheckResult{OK: false, ErrorCode: "POLICY_INVALID", Violations: []string{err.Error()}}
	}

	result := BoundaryCheckResult{OK: true}
	for _, file := range changedFiles {
		normalizedFile, pathErr := normalizeRepositoryPath(file, false)
		if pathErr != nil {
			result.addViolation("file %q has an unsafe path: %v", file, pathErr)
			continue
		}
		if isSystemPath(normalizedFile) {
			result.addViolation("file %s is in a protected system directory (.git or .maestro)", normalizedFile)
			continue
		}
		if worktreeRoot != "" {
			if _, resolveErr := resolvePathWithinRoot(worktreeRoot, normalizedFile, false); resolveErr != nil {
				result.addViolation("file %s failed canonical/symlink validation: %v", normalizedFile, resolveErr)
				continue
			}
		}

		allowed := false
		for _, dir := range allowedDirs {
			if normalizedFile == dir || strings.HasPrefix(normalizedFile, dir+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			result.addViolation("file %s is outside allowed directories", normalizedFile)
			continue
		}

		for _, pattern := range forbiddenPatterns {
			var matched bool
			if strings.HasSuffix(pattern, "/") {
				prefix := strings.TrimSuffix(pattern, "/")
				matched = normalizedFile == prefix || strings.HasPrefix(normalizedFile, prefix+"/")
			} else if strings.Contains(pattern, "/") {
				matched, _ = path.Match(pattern, normalizedFile)
			} else {
				matched, _ = path.Match(pattern, path.Base(normalizedFile))
			}
			if matched {
				result.addViolation("file %s matches forbidden pattern %s", normalizedFile, pattern)
				break
			}
		}
	}
	return result
}

func (r *BoundaryCheckResult) addViolation(format string, args ...any) {
	r.OK = false
	if r.ErrorCode == "" {
		r.ErrorCode = "BOUNDARY_VIOLATION"
	}
	r.Violations = append(r.Violations, fmt.Sprintf(format, args...))
}

func parseAllowedDirectories(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("allowed_directories evidence is missing")
	}
	var dirs []string
	if err := json.Unmarshal([]byte(raw), &dirs); err != nil {
		return nil, fmt.Errorf("allowed_directories is invalid JSON: %w", err)
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("allowed_directories must contain at least one directory")
	}

	seen := make(map[string]struct{}, len(dirs))
	normalized := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		clean, err := normalizeRepositoryPath(dir, true)
		if err != nil {
			return nil, fmt.Errorf("allowed directory %q is unsafe: %w", dir, err)
		}
		if clean == "." || isSystemPath(clean) {
			return nil, fmt.Errorf("allowed directory %q is too broad or protected", dir)
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		normalized = append(normalized, clean)
	}
	return normalized, nil
}

func parseForbiddenPatterns(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "null" {
		return []string{}, nil
	}
	var patterns []string
	if err := json.Unmarshal([]byte(raw), &patterns); err != nil {
		return nil, fmt.Errorf("forbidden_patterns is invalid JSON: %w", err)
	}
	for _, pattern := range patterns {
		if pattern == "" || strings.ContainsRune(pattern, '\x00') || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "..") {
			return nil, fmt.Errorf("forbidden pattern %q is unsafe", pattern)
		}
		candidate := strings.TrimSuffix(pattern, "/")
		if _, err := path.Match(candidate, candidate); err != nil {
			return nil, fmt.Errorf("forbidden pattern %q is invalid: %w", pattern, err)
		}
	}
	return patterns, nil
}

func normalizeRepositoryPath(value string, directory bool) (string, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	value = strings.TrimSuffix(value, "/")
	if value == "" || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("path must be non-empty, relative and NUL-free")
	}
	clean := path.Clean(value)
	if clean == "." && directory {
		return clean, nil
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path traversal is not allowed")
	}
	if clean != value {
		return "", fmt.Errorf("path must be canonical")
	}
	return clean, nil
}

func isSystemPath(normalizedPath string) bool {
	return normalizedPath == ".git" || strings.HasPrefix(normalizedPath, ".git/") ||
		normalizedPath == ".maestro" || strings.HasPrefix(normalizedPath, ".maestro/")
}
