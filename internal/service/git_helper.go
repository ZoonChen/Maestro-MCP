package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxGitOutputBytes = int64(4 << 20)

var (
	gitSHARe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	taskIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func getBaseCommit(ctx context.Context, workspacePath string) (string, error) {
	workspace, err := canonicalExistingDir(workspacePath)
	if err != nil {
		return "", fmt.Errorf("getBaseCommit: %w", err)
	}
	out, err := runGitOutput(ctx, workspace, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("getBaseCommit: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if !gitSHARe.MatchString(sha) {
		return "", fmt.Errorf("getBaseCommit: git returned invalid commit SHA")
	}
	return sha, nil
}

func getHeadCommit(ctx context.Context, worktreePath string) (string, error) {
	out, err := runGitOutput(ctx, worktreePath, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !gitSHARe.MatchString(sha) {
		return "", fmt.Errorf("git returned invalid HEAD SHA")
	}
	return sha, nil
}

// verifyWorktreeRepository validates path containment, repository identity and
// the exact immutable baseline commit before any validation command runs.
func verifyWorktreeRepository(ctx context.Context, workspacePath, worktreePath, baseCommit string) (canonicalWorktree, sourceCommit string, err error) {
	if !gitSHARe.MatchString(baseCommit) {
		return "", "", fmt.Errorf("baseline SHA is missing or invalid")
	}
	canonicalWorktree, err = validateWorktreeLocation(workspacePath, worktreePath)
	if err != nil {
		return "", "", err
	}

	topOut, err := runGitOutput(ctx, canonicalWorktree, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("resolve worktree repository: %w", err)
	}
	repositoryRoot, err := canonicalExistingDir(strings.TrimSpace(string(topOut)))
	if err != nil || repositoryRoot != canonicalWorktree {
		return "", "", fmt.Errorf("worktree repository root does not match stored path")
	}

	resolvedBase, err := runGitOutput(ctx, canonicalWorktree, "rev-parse", "--verify", baseCommit+"^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("baseline commit unavailable: %w", err)
	}
	if strings.TrimSpace(string(resolvedBase)) != baseCommit {
		return "", "", fmt.Errorf("baseline SHA does not resolve exactly")
	}

	sourceCommit, err = getHeadCommit(ctx, canonicalWorktree)
	if err != nil {
		return "", "", fmt.Errorf("source SHA unavailable: %w", err)
	}
	if _, err := runGitOutput(ctx, canonicalWorktree, "merge-base", "--is-ancestor", baseCommit, sourceCommit); err != nil {
		return "", "", fmt.Errorf("baseline is not an ancestor of source SHA: %w", err)
	}
	return canonicalWorktree, sourceCommit, nil
}

func createWorktree(ctx context.Context, workspacePath, taskID string) (string, error) {
	if !taskIDRe.MatchString(taskID) {
		return "", fmt.Errorf("createWorktree: unsafe task id")
	}
	workspace, err := canonicalExistingDir(workspacePath)
	if err != nil {
		return "", fmt.Errorf("createWorktree: %w", err)
	}
	baseCommit, err := getBaseCommit(ctx, workspace)
	if err != nil {
		return "", err
	}
	branchName := "task/" + taskID
	relativeParent := filepath.Join(".maestro", "worktrees")
	if _, err := resolvePathWithinRoot(workspace, relativeParent, false); err != nil {
		return "", fmt.Errorf("createWorktree: unsafe worktree parent: %w", err)
	}
	parent := filepath.Join(workspace, relativeParent)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("createWorktree: create parent: %w", err)
	}
	if _, err := resolvePathWithinRoot(workspace, relativeParent, true); err != nil {
		return "", fmt.Errorf("createWorktree: unsafe worktree parent: %w", err)
	}
	worktreePath := filepath.Join(parent, taskID)
	if _, err := os.Lstat(worktreePath); err == nil || !os.IsNotExist(err) {
		return "", fmt.Errorf("createWorktree: target already exists or cannot be inspected")
	}

	if _, err := runGitOutput(ctx, workspace, "worktree", "add", "-b", branchName, worktreePath, baseCommit); err != nil {
		return "", fmt.Errorf("createWorktree: %w", err)
	}
	canonical, err := validateWorktreeLocation(workspace, worktreePath)
	if err != nil {
		return "", fmt.Errorf("createWorktree: created worktree failed containment check: %w", err)
	}
	return canonical, nil
}

func removeWorktree(ctx context.Context, workspacePath, worktreePath string) error {
	workspace, err := canonicalExistingDir(workspacePath)
	if err != nil {
		return fmt.Errorf("removeWorktree: %w", err)
	}
	canonical, err := validateWorktreeLocation(workspace, worktreePath)
	if err != nil {
		return fmt.Errorf("removeWorktree: %w", err)
	}
	if _, err := runGitOutput(ctx, workspace, "worktree", "remove", "--force", canonical); err != nil {
		return fmt.Errorf("removeWorktree: %w", err)
	}
	return nil
}

func deleteBranch(ctx context.Context, workspacePath, branchName string) error {
	if !strings.HasPrefix(branchName, "task/") || !taskIDRe.MatchString(strings.TrimPrefix(branchName, "task/")) {
		return fmt.Errorf("deleteBranch: unsafe branch name")
	}
	if _, err := runGitOutput(ctx, workspacePath, "branch", "-D", branchName); err != nil {
		return fmt.Errorf("deleteBranch %s: %w", branchName, err)
	}
	return nil
}

// getChangedFiles returns committed/staged/unstaged changes relative to the
// exact baseline plus untracked files. Every Git error and malformed path is a
// hard error; no subcommand may silently collapse to an empty diff.
func getChangedFiles(ctx context.Context, worktreePath, baseCommit string) ([]string, error) {
	if !gitSHARe.MatchString(baseCommit) {
		return nil, fmt.Errorf("getChangedFiles: invalid baseline SHA")
	}
	seen := make(map[string]struct{})
	result := make([]string, 0)
	addNULTerminated := func(data []byte, producer string) error {
		if len(data) == 0 {
			return nil
		}
		if data[len(data)-1] != 0 {
			return fmt.Errorf("%s returned malformed non-NUL-terminated paths", producer)
		}
		for _, raw := range strings.Split(string(data[:len(data)-1]), "\x00") {
			if raw == "" || !utf8.ValidString(raw) {
				return fmt.Errorf("%s returned an empty or invalid UTF-8 path", producer)
			}
			normalized, err := normalizeRepositoryPath(raw, false)
			if err != nil {
				return fmt.Errorf("%s returned unsafe path %q: %w", producer, raw, err)
			}
			if _, exists := seen[normalized]; !exists {
				seen[normalized] = struct{}{}
				result = append(result, normalized)
			}
		}
		return nil
	}

	diff, err := runGitOutput(ctx, worktreePath, "diff", "--no-ext-diff", "--name-only", "--no-renames", "-z", baseCommit, "--")
	if err != nil {
		return nil, fmt.Errorf("getChangedFiles: diff failed: %w", err)
	}
	if err := addNULTerminated(diff, "git diff"); err != nil {
		return nil, fmt.Errorf("getChangedFiles: %w", err)
	}
	untracked, err := runGitOutput(ctx, worktreePath, "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return nil, fmt.Errorf("getChangedFiles: untracked scan failed: %w", err)
	}
	if err := addNULTerminated(untracked, "git ls-files"); err != nil {
		return nil, fmt.Errorf("getChangedFiles: %w", err)
	}
	return result, nil
}

func runGitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	gitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	safeArgs := append([]string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "credential.helper=",
		"-c", "protocol.file.allow=never",
	}, args...)
	cmd := exec.CommandContext(gitCtx, "git", safeArgs...)
	cmd.Dir = dir
	cmd.Env = append(gitSafeEnvironment(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	output := newBoundedOutput(maxGitOutputBytes)
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if output.Truncated() {
		return nil, fmt.Errorf("git output exceeded %d bytes", maxGitOutputBytes)
	}
	if gitCtx.Err() != nil {
		return nil, fmt.Errorf("git command timeout/cancelled: %w", gitCtx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, sanitizeDiagnostic(output.String()))
	}
	return output.Bytes(), nil
}

func gitSafeEnvironment() []string {
	env := []string{"LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if pathValue := os.Getenv("PATH"); pathValue != "" {
		env = append(env, "PATH="+pathValue)
	}
	return env
}
