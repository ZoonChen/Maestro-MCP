package service

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// getBaseCommit returns the current HEAD commit hash of the repository at workspacePath.
func getBaseCommit(ctx context.Context, workspacePath string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = workspacePath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("getBaseCommit: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// createWorktree creates a git worktree for the given task at a path derived from
// the workspace path and task ID. Returns the worktree path.
func createWorktree(ctx context.Context, workspacePath, taskID string) (string, error) {
	branchName := fmt.Sprintf("task/%s", taskID)
	worktreePath := fmt.Sprintf("%s/.maestro/worktrees/%s", workspacePath, taskID)

	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "-b", branchName, worktreePath, "HEAD")
	cmd.Dir = workspacePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("createWorktree: %w: %s", err, string(out))
	}
	return worktreePath, nil
}

// removeWorktree forcefully removes a git worktree at the given path.
func removeWorktree(ctx context.Context, workspacePath, worktreePath string) error {
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = workspacePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("removeWorktree: %w: %s", err, string(out))
	}
	return nil
}

// deleteBranch deletes a git branch by name. Used during GC to clean up
// task/<id> branches after the worktree is removed.
func deleteBranch(ctx context.Context, workspacePath, branchName string) error {
	cmd := exec.CommandContext(ctx, "git", "branch", "-D", branchName)
	cmd.Dir = workspacePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deleteBranch %s: %w: %s", branchName, err, string(out))
	}
	return nil
}

// getChangedFiles returns the list of files changed in the worktree relative to baseCommit.
// It captures: committed changes vs base, staged but uncommitted, unstaged working tree,
// and untracked files.
func getChangedFiles(ctx context.Context, worktreePath, baseCommit string) ([]string, error) {
	seen := make(map[string]bool)
	var result []string
	addFiles := func(data []byte) {
		for _, f := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			f = strings.TrimSpace(f)
			if f != "" && !seen[f] {
				seen[f] = true
				result = append(result, f)
			}
		}
	}

	// 1. Committed changes relative to base commit.
	cmd1 := exec.CommandContext(ctx, "git", "diff", "--name-only", baseCommit)
	cmd1.Dir = worktreePath
	if out, err := cmd1.Output(); err == nil {
		addFiles(out)
	}

	// 2. Staged but uncommitted changes.
	cmd2 := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only")
	cmd2.Dir = worktreePath
	if out, err := cmd2.Output(); err == nil {
		addFiles(out)
	}

	// 3. Unstaged working tree changes.
	cmd3 := exec.CommandContext(ctx, "git", "diff", "--name-only")
	cmd3.Dir = worktreePath
	if out, err := cmd3.Output(); err == nil {
		addFiles(out)
	}

	// 4. Untracked files.
	cmd4 := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard")
	cmd4.Dir = worktreePath
	if out, err := cmd4.Output(); err == nil {
		addFiles(out)
	}

	return result, nil
}

// mergeWorktree merges the task branch into the main branch in the main workspace.
// Returns the merge commit hash on success, or indicates a conflict.
// If conflict is true, the merge was aborted due to conflicts.
func mergeWorktree(ctx context.Context, workspacePath, taskID string) (mergeCommit string, conflict bool, err error) {
	branchName := fmt.Sprintf("task/%s", taskID)

	// Execute git merge with a timeout.
	mergeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(mergeCtx, "git", "merge", branchName)
	cmd.Dir = workspacePath
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)
		// Check if it's a merge conflict.
		if strings.Contains(outputStr, "CONFLICT") {
			// Abort the merge to clean up.
			abortCmd := exec.CommandContext(mergeCtx, "git", "merge", "--abort")
			abortCmd.Dir = workspacePath
			_ = abortCmd.Run()
			return "", true, nil
		}
		return "", false, fmt.Errorf("mergeWorktree: %w: %s", err, outputStr)
	}

	// Get the merge commit hash.
	hashCmd := exec.CommandContext(mergeCtx, "git", "rev-parse", "HEAD")
	hashCmd.Dir = workspacePath
	hashOut, hashErr := hashCmd.Output()
	if hashErr != nil {
		return "", false, fmt.Errorf("mergeWorktree: get HEAD after merge: %w", hashErr)
	}

	return strings.TrimSpace(string(hashOut)), false, nil
}
