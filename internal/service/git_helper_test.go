package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetChangedFilesFailsClosedAndIncludesUntracked(t *testing.T) {
	workspace, base := createTestGitRepository(t)
	worktree, err := createWorktree(context.Background(), workspace, "T-validation")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(worktree, "src", "main.go"), []byte("package main\n// changed\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "src", "new.go"), []byte("package main\n"), 0o600))

	files, err := getChangedFiles(context.Background(), worktree, base)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"src/main.go", "src/new.go"}, files)

	_, err = getChangedFiles(context.Background(), worktree, "not-a-sha")
	require.Error(t, err)
}

func TestVerifyWorktreeRepositoryRejectsOutsideAndBadSHA(t *testing.T) {
	workspace, base := createTestGitRepository(t)
	outside, _ := createTestGitRepository(t)

	_, _, err := verifyWorktreeRepository(context.Background(), workspace, outside, base)
	require.Error(t, err)

	worktree, err := createWorktree(context.Background(), workspace, "T-sha")
	require.NoError(t, err)
	_, _, err = verifyWorktreeRepository(context.Background(), workspace, worktree, "abc123")
	require.Error(t, err)
}

func TestCreateWorktreeRejectsSymlinkedReservedDirectory(t *testing.T) {
	workspace, _ := createTestGitRepository(t)
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(workspace, ".maestro")))

	_, err := createWorktree(context.Background(), workspace, "T-escape")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe worktree parent")
}

func TestWorktreeRemovalAndBranchDeletionUseValidatedTargets(t *testing.T) {
	workspace, _ := createTestGitRepository(t)
	worktree, err := createWorktree(context.Background(), workspace, "T-cleanup")
	require.NoError(t, err)

	require.Error(t, deleteBranch(context.Background(), workspace, "main"))
	require.Error(t, deleteBranch(context.Background(), workspace, "task/../escape"))
	require.Error(t, removeWorktree(context.Background(), workspace, t.TempDir()))
	require.NoError(t, removeWorktree(context.Background(), workspace, worktree))
	require.NoError(t, deleteBranch(context.Background(), workspace, "task/T-cleanup"))
}

func TestGitHelpersRejectMissingRepositoriesAndUnsafeTaskIDs(t *testing.T) {
	ctx := context.Background()
	notRepository := t.TempDir()
	_, err := getBaseCommit(ctx, notRepository)
	require.Error(t, err)
	_, err = getHeadCommit(ctx, notRepository)
	require.Error(t, err)
	_, err = createWorktree(ctx, notRepository, "T-safe")
	require.Error(t, err)
	_, err = createWorktree(ctx, notRepository, "../escape")
	require.Error(t, err)
	require.Error(t, removeWorktree(ctx, filepath.Join(notRepository, "missing"), notRepository))
}

func createTestGitRepository(t *testing.T) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	runTestGit(t, workspace, "init", "--quiet")
	runTestGit(t, workspace, "config", "user.email", "maestro-test@example.invalid")
	runTestGit(t, workspace, "config", "user.name", "Maestro Test")
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "src", "main.go"), []byte("package main\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("coverage.out\n"), 0o600))
	runTestGit(t, workspace, "add", "--", "src/main.go", ".gitignore")
	runTestGit(t, workspace, "commit", "--quiet", "-m", "initial")
	base := string(runTestGit(t, workspace, "rev-parse", "HEAD"))
	for len(base) > 0 && (base[len(base)-1] == '\n' || base[len(base)-1] == '\r') {
		base = base[:len(base)-1]
	}
	return workspace, base
}

func runTestGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
	return out
}
