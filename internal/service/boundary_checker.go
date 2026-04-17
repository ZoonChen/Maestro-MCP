package service

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// BoundaryCheckResult contains the results of a boundary compliance check.
type BoundaryCheckResult struct {
	OK         bool     `json:"ok"`
	Violations []string `json:"violations,omitempty"`
}

// checkBoundaries verifies that all changed files are within allowed directories
// and do not match any forbidden glob patterns.
//
// Design note: when allowedDirs is empty (or null), all files are treated as
// allowed. This is intentional — a task without allowed_directories is
// unrestricted, which is the default for tasks that don't need directory-level
// isolation. If you want to block ALL file changes, set allowed_directories to
// a non-existent directory like ["__none__"].
//
// Hard-coded protections: .git/ and .maestro/ directories are ALWAYS forbidden,
// regardless of allowed_directories or forbidden_patterns settings.
func checkBoundaries(changedFiles []string, allowedDirsJSON, forbiddenPatternsJSON string) BoundaryCheckResult {
	// Parse allowed directories.
	var allowedDirs []string
	if allowedDirsJSON != "" {
		if err := json.Unmarshal([]byte(allowedDirsJSON), &allowedDirs); err != nil {
			slog.Error("checkBoundaries: invalid allowed_directories JSON", "allowed_dirs_json", allowedDirsJSON, "error", err)
		}
	}

	// Parse forbidden patterns.
	var forbiddenPatterns []string
	if forbiddenPatternsJSON != "" {
		if err := json.Unmarshal([]byte(forbiddenPatternsJSON), &forbiddenPatterns); err != nil {
			slog.Error("checkBoundaries: invalid forbidden_patterns JSON", "forbidden_patterns_json", forbiddenPatternsJSON, "error", err)
		}
	}

	result := BoundaryCheckResult{OK: true}

	for _, file := range changedFiles {
		// Normalize path separators.
		normalizedFile := filepath.ToSlash(file)

		// Symlink protection: detect if the file path contains symlink components.
		// Git tracks symlinks as files — a symlink inside an allowed directory could
		// point outside the boundary. We check the normalized path for symlink markers.
		// Note: full EvalSymlinks requires a real filesystem; the file paths come from
		// git diff, so we rely on path analysis as a defense-in-depth measure.
		if containsSymlinkIndicator(file) {
			result.OK = false
			result.Violations = append(result.Violations,
				"file "+file+" appears to be or contain a symlink (not allowed)")
			continue
		}

		// Hard-coded system directory protection: .git/ and .maestro/ are always forbidden.
		if isSystemPath(normalizedFile) {
			result.OK = false
			result.Violations = append(result.Violations,
				"file "+file+" is in a protected system directory (.git or .maestro)")
			continue
		}

		// Check if file is within at least one allowed directory.
		allowed := false
		for _, dir := range allowedDirs {
			normalizedDir := strings.TrimSuffix(filepath.ToSlash(dir), "/") + "/"
			if strings.HasPrefix(normalizedFile, normalizedDir) || normalizedFile == strings.TrimSuffix(normalizedDir, "/") {
				allowed = true
				break
			}
		}
		if !allowed && len(allowedDirs) > 0 {
			result.OK = false
			result.Violations = append(result.Violations,
				"file "+file+" is outside allowed directories")
			continue
		}

		// Check if file matches any forbidden pattern.
		// Dual matching: first try full path, then try basename (for backwards compatibility).
		for _, pattern := range forbiddenPatterns {
			matched := false
			// Full path match (for patterns containing "/")
			if strings.Contains(pattern, "/") {
				if m, err := filepath.Match(pattern, normalizedFile); err == nil && m {
					matched = true
				}
			}
			// Basename match (for simple filename glob patterns)
			if !matched {
				if m, err := filepath.Match(pattern, filepath.Base(file)); err == nil && m {
					matched = true
				}
			}
			if matched {
				result.OK = false
				result.Violations = append(result.Violations,
					"file "+file+" matches forbidden pattern "+pattern)
				break
			}
		}
	}

	return result
}

// isSystemPath checks if a normalized file path is inside a protected system directory.
// These directories are always forbidden regardless of task configuration.
func isSystemPath(normalizedPath string) bool {
	// Check .git/ directory
	if strings.HasPrefix(normalizedPath, ".git/") || normalizedPath == ".git" {
		return true
	}
	// Check .maestro/ directory
	if strings.HasPrefix(normalizedPath, ".maestro/") || normalizedPath == ".maestro" {
		return true
	}
	return false
}

// containsSymlinkIndicator checks if a file path is or traverses through a symlink.
// It resolves the real path of the file and compares it to the given path.
// If they differ, the path contains a symlink component.
func containsSymlinkIndicator(file string) bool {
	// Best-effort: try to evaluate symlinks on the real filesystem.
	// If the file doesn't exist (e.g., testing), we can't check, so skip.
	realPath, err := filepath.EvalSymlinks(file)
	if err != nil {
		// File doesn't exist on disk — can't verify. Allow through.
		return false
	}
	// If the resolved path differs from the original, a symlink was involved.
	normalizedOrig := filepath.Clean(file)
	normalizedReal := filepath.Clean(realPath)
	return normalizedOrig != normalizedReal
}

// isSymlinkPath checks if the given path is itself a symlink on the filesystem.
func isSymlinkPath(file string) bool { //nolint:unused
	info, err := os.Lstat(file)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}
