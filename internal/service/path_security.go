package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxValidationFileBytes  = int64(10 << 20)
	maxValidationTotalBytes = int64(100 << 20)
)

// canonicalExistingDir resolves an existing directory to an absolute canonical
// path. Callers compare canonical paths rather than trusting stored path text.
func canonicalExistingDir(dir string) (string, error) {
	if dir == "" || strings.ContainsRune(dir, '\x00') {
		return "", fmt.Errorf("path is empty or contains NUL")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve directory symlinks: %w", err)
	}
	return filepath.Clean(real), nil
}

// validateRelativePath rejects paths controlled by a task/profile unless they
// are canonical, relative, NUL-free and traversal-free.
func validateRelativePath(relative string, allowDot bool) (string, error) {
	if relative == "" || strings.ContainsRune(relative, '\x00') {
		return "", fmt.Errorf("relative path is empty or contains NUL")
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	relative = filepath.FromSlash(relative)
	clean := filepath.Clean(relative)
	if clean != relative {
		return "", fmt.Errorf("path must be canonical and may not contain dot or duplicate components")
	}
	if clean == "." {
		if allowDot {
			return clean, nil
		}
		return "", fmt.Errorf("path must identify a file or directory")
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal is not allowed")
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("path is not canonical")
		}
	}
	return clean, nil
}

// resolvePathWithinRoot resolves a task-controlled relative path beneath root
// and rejects every existing symlink component. Missing final components may be
// accepted for deleted/untracked-diff handling when requireExisting is false.
func resolvePathWithinRoot(root, relative string, requireExisting bool) (string, error) {
	canonicalRoot, err := canonicalExistingDir(root)
	if err != nil {
		return "", fmt.Errorf("canonical root: %w", err)
	}
	clean, err := validateRelativePath(relative, true)
	if err != nil {
		return "", err
	}
	if clean == "." {
		return canonicalRoot, nil
	}

	candidate := filepath.Join(canonicalRoot, clean)
	if !pathWithinRoot(canonicalRoot, candidate) {
		return "", fmt.Errorf("resolved path escapes root")
	}

	current := canonicalRoot
	parts := strings.Split(clean, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if os.IsNotExist(statErr) && !requireExisting {
				break
			}
			return "", fmt.Errorf("lstat path component: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink path component is not allowed")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("non-directory path component")
		}
	}

	if requireExisting {
		real, evalErr := filepath.EvalSymlinks(candidate)
		if evalErr != nil {
			return "", fmt.Errorf("resolve path: %w", evalErr)
		}
		if !pathWithinRoot(canonicalRoot, real) {
			return "", fmt.Errorf("resolved path escapes root")
		}
	}
	return filepath.Clean(candidate), nil
}

func pathWithinRoot(root, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// validateWorktreeLocation proves the stored worktree is inside the project's
// reserved .maestro/worktrees subtree after symlink resolution.
func validateWorktreeLocation(workspacePath, worktreePath string) (string, error) {
	workspace, err := canonicalExistingDir(workspacePath)
	if err != nil {
		return "", fmt.Errorf("workspace invalid: %w", err)
	}
	worktree, err := canonicalExistingDir(worktreePath)
	if err != nil {
		return "", fmt.Errorf("worktree invalid: %w", err)
	}
	reserved := filepath.Join(workspace, ".maestro", "worktrees")
	if !pathWithinRoot(reserved, worktree) || worktree == reserved {
		return "", fmt.Errorf("worktree is outside reserved workspace subtree")
	}
	return worktree, nil
}
