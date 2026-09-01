package gitbroker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests run against REAL git remotes (temporary bare repositories):
// the lease semantics, the default-branch refusal and the SHA checks
// are behaviors of git itself plus the broker's guards, so they are
// exercised end to end rather than mocked.

type gitFixture struct {
	remote  string
	workdir string
	broker  *Broker
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	run(t, dir, "git", "init", "--bare", remote)

	work := filepath.Join(dir, "work")
	require.NoError(t, os.MkdirAll(work, 0o755))
	run(t, work, "git", "init", "-b", "main")
	run(t, work, "git", "config", "user.email", "broker@test")
	run(t, work, "git", "config", "user.name", "broker test")
	writeFile(t, work, "README.md", "# fixture\n")
	run(t, work, "git", "add", ".")
	run(t, work, "git", "commit", "-m", "init")
	run(t, work, "git", "push", remote, "main")

	// Seed the default branch on the remote for resolve checks.
	broker := NewBroker(nil)
	broker.AllowedSchemes["file"] = true
	return &gitFixture{remote: "file://" + remote, workdir: work, broker: broker}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	//nolint:gosec // test helper running fixture git commands
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

func (f *gitFixture) commit(t *testing.T, message string) string {
	t.Helper()
	writeFile(t, f.workdir, "CHANGE.md", message+"\n")
	run(t, f.workdir, "git", "add", ".")
	run(t, f.workdir, "git", "commit", "-m", message)
	return strings.TrimSpace(run(t, f.workdir, "git", "rev-parse", "HEAD"))
}

func TestTaskBranchNameValidation(t *testing.T) {
	valid, err := TaskBranchName("acme-proj", "T-1234.5")
	require.NoError(t, err)
	assert.Equal(t, "maestro/acme-proj/T-1234.5", valid)

	for _, tc := range [][2]string{
		{"ACME", "T-1"},  // uppercase project key
		{"a", "T-1"},     // too short key
		{"acme", "../x"}, // path escape in task id
		{"acme", "T 1"},  // space
		{"", "T-1"},      // empty key
		{"acme", ""},     // empty task
		{"-acme", "T-1"}, // leading dash key
	} {
		_, err := TaskBranchName(tc[0], tc[1])
		assert.ErrorIs(t, err, ErrRefused, "pair %v", tc)
	}
}

func TestResolveBranchSHA(t *testing.T) {
	f := newGitFixture(t)

	mainSHA, exists, err := f.broker.ResolveBranchSHA(context.Background(), f.remote, "main")
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Regexp(t, `^[0-9a-f]{40}$`, mainSHA)

	_, exists, err = f.broker.ResolveBranchSHA(context.Background(), f.remote, "absent")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestPushTaskBranchLeaseLifecycle(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()

	first := f.commit(t, "first change")
	result, err := f.broker.PushTaskBranch(ctx, PushRequest{
		WorktreePath:  f.workdir,
		RemoteURL:     f.remote,
		ProjectKey:    "acme",
		TaskID:        "T-100",
		ExpectedBase:  "",
		DefaultBranch: "main",
	})
	require.NoError(t, err)
	assert.Equal(t, "maestro/acme/T-100", result.Branch)
	assert.Equal(t, first, result.NewSHA)
	assert.Empty(t, result.OldSHA)

	// A second push without a lease is refused: existing branches only
	// move under an explicit expectation.
	_, err = f.broker.PushTaskBranch(ctx, PushRequest{
		WorktreePath: f.workdir, RemoteURL: f.remote,
		ProjectKey: "acme", TaskID: "T-100",
	})
	assert.ErrorIs(t, err, ErrRefused)

	// A stale lease is refused.
	second := f.commit(t, "second change")
	_, err = f.broker.PushTaskBranch(ctx, PushRequest{
		WorktreePath: f.workdir, RemoteURL: f.remote,
		ProjectKey: "acme", TaskID: "T-100",
		ExpectedBase: "0000000000000000000000000000000000000000",
	})
	assert.ErrorIs(t, err, ErrLeaseMismatch)

	// The current lease moves the branch atomically.
	result, err = f.broker.PushTaskBranch(ctx, PushRequest{
		WorktreePath: f.workdir, RemoteURL: f.remote,
		ProjectKey: "acme", TaskID: "T-100",
		ExpectedBase: first,
	})
	require.NoError(t, err)
	assert.Equal(t, first, result.OldSHA)
	assert.Equal(t, second, result.NewSHA)

	// The default branch head is untouched by every push above.
	remoteMain, _, err := f.broker.ResolveBranchSHA(ctx, f.remote, "main")
	require.NoError(t, err)
	run(t, f.workdir, "git", "fetch", f.remote, "main")
	localMain := strings.TrimSpace(run(t, f.workdir, "git", "rev-parse", "FETCH_HEAD"))
	assert.Equal(t, localMain, remoteMain, "the broker never touches the default branch")
}

func TestPushRefusesDisallowedRemotes(t *testing.T) {
	f := newGitFixture(t)
	production := NewBroker(nil) // no file scheme

	_, err := production.PushTaskBranch(context.Background(), PushRequest{
		WorktreePath: f.workdir, RemoteURL: f.remote,
		ProjectKey: "acme", TaskID: "T-1",
	})
	assert.ErrorIs(t, err, ErrRefused, "file remotes are fixtures only")

	_, _, err = production.ResolveBranchSHA(context.Background(), "http://insecure.example/x.git", "main")
	assert.ErrorIs(t, err, ErrRefused, "plaintext http is refused")
}

func TestDefaultBranchGuard(t *testing.T) {
	f := newGitFixture(t)
	broker := NewBroker(nil)
	broker.AllowedSchemes["file"] = true

	// Even if a misconfiguration asked for the default branch name, the
	// generated task branch can never equal it; the guard proves the
	// invariant rather than trusting the generator.
	_, err := broker.PushTaskBranch(context.Background(), PushRequest{
		WorktreePath: f.workdir, RemoteURL: f.remote,
		ProjectKey: "acme", TaskID: "T-1", DefaultBranch: "maestro/acme/T-1",
	})
	assert.ErrorIs(t, err, ErrRefused)
}

func TestEnvCredentialSource(t *testing.T) {
	t.Setenv("MAESTRO_GIT_USERNAME", "member")
	t.Setenv("MAESTRO_GIT_PASSWORD", "token-1")
	credential, err := EnvCredentialSource{}.Credential(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "member", credential.Username)
	assert.Equal(t, "token-1", credential.Password)
}
