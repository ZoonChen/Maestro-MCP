// Package gitbroker is the runner-host Git broker (M2-GIT-001): the
// ONLY component with source-push authority. It pushes exclusively to
// generated maestro task branches under an expected-SHA lease, refuses
// the configured default/protected branch outright, and never exposes
// merge or arbitrary-ref operations — platform-side protection is the
// second wall (GitLab protected branches), this is the first.
package gitbroker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// TaskBranchPrefix is the frozen task-branch namespace.
const TaskBranchPrefix = "maestro"

var (
	projectKeyRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	taskIDRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	shaRe        = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// Broker errors are stable conditions callers map to user guidance.
var (
	ErrRefused          = errors.New("gitbroker: operation refused")
	ErrLeaseMismatch    = errors.New("gitbroker: remote branch moved, lease failed")
	ErrRemoteUnresolved = errors.New("gitbroker: remote branch not found")
)

// Credential carries the member credential for one remote.
type Credential struct {
	Username string
	Password string
}

// CredentialSource resolves push credentials from the host's storage
// (OS keychain in production; the test source reads the environment).
type CredentialSource interface {
	Credential(ctx context.Context, remoteURL string) (Credential, error)
}

// EnvCredentialSource reads MAESTRO_GIT_* variables — wiring and tests.
type EnvCredentialSource struct{}

// Credential implements CredentialSource.
func (EnvCredentialSource) Credential(_ context.Context, _ string) (Credential, error) {
	return Credential{
		Username: os.Getenv("MAESTRO_GIT_USERNAME"),
		Password: os.Getenv("MAESTRO_GIT_PASSWORD"),
	}, nil
}

// Broker is the task-branch push authority.
type Broker struct {
	Credentials CredentialSource
	// AllowedSchemes constrains remotes (default https/ssh/git; tests
	// add file for local fixtures).
	AllowedSchemes map[string]bool
}

// NewBroker builds the production broker.
func NewBroker(credentials CredentialSource) *Broker {
	return &Broker{
		Credentials:    credentials,
		AllowedSchemes: map[string]bool{"https": true, "ssh": true, "git": true},
	}
}

// TaskBranchName builds maestro/<project-key>/<task-id>.
func TaskBranchName(projectKey, taskID string) (string, error) {
	if !projectKeyRe.MatchString(projectKey) {
		return "", fmt.Errorf("%w: project key %q must match ^[a-z][a-z0-9-]{0,62}$", ErrRefused, projectKey)
	}
	if !taskIDRe.MatchString(taskID) {
		return "", fmt.Errorf("%w: task id %q is malformed", ErrRefused, taskID)
	}
	return TaskBranchPrefix + "/" + projectKey + "/" + taskID, nil
}

// ValidateRemote enforces the remote allowlist.
func (b *Broker) ValidateRemote(remoteURL string) error {
	parsed, err := url.Parse(remoteURL)
	if err != nil {
		return fmt.Errorf("%w: remote is unparseable", ErrRefused)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "ssh" {
		scheme = "ssh"
	}
	// scp-like syntax (git@host:path) parses as scheme "git@host:path"
	// with Opaque set — treat a non-empty Opaque with a colon as ssh.
	if parsed.Scheme != "" && strings.Contains(parsed.Scheme, "@") {
		scheme = "ssh"
	}
	if !b.AllowedSchemes[scheme] {
		return fmt.Errorf("%w: remote scheme %q is not allowed", ErrRefused, parsed.Scheme)
	}
	return nil
}

// ResolveBranchSHA reads a remote branch head without a checkout.
func (b *Broker) ResolveBranchSHA(ctx context.Context, remoteURL, branch string) (string, bool, error) {
	if err := b.ValidateRemote(remoteURL); err != nil {
		return "", false, err
	}
	out, err := b.runGit(ctx, "", remoteURL, "ls-remote", remoteURL, "refs/heads/"+branch)
	if err != nil {
		return "", false, fmt.Errorf("gitbroker: ls-remote: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", false, nil
	}
	sha, _, _ := strings.Cut(line, "\t")
	if !shaRe.MatchString(sha) {
		return "", false, fmt.Errorf("gitbroker: ls-remote returned a malformed SHA")
	}
	return sha, true, nil
}

// PushRequest is one task-branch push.
type PushRequest struct {
	WorktreePath string // committed worktree whose HEAD is pushed
	RemoteURL    string
	ProjectKey   string
	TaskID       string
	// ExpectedBase is the SHA the remote branch must still point at
	// (empty allows only a brand-new branch).
	ExpectedBase string
	// DefaultBranch is the integration mapping's target branch: the
	// broker refuses to touch it even by misconfiguration.
	DefaultBranch string
}

// PushResult records the ref update.
type PushResult struct {
	Branch string
	OldSHA string
	NewSHA string
}

// PushTaskBranch pushes the worktree HEAD to the generated task branch
// under the lease. Refusals: malformed names, disallowed remotes, the
// configured default branch, a moved remote (lease), or an unexpected
// post-push state.
func (b *Broker) PushTaskBranch(ctx context.Context, req PushRequest) (*PushResult, error) {
	branch, err := TaskBranchName(req.ProjectKey, req.TaskID)
	if err != nil {
		return nil, err
	}
	if err := b.ValidateRemote(req.RemoteURL); err != nil {
		return nil, err
	}
	if req.DefaultBranch != "" && branch == req.DefaultBranch {
		// Structurally impossible for maestro/* names, checked anyway:
		// the guard must not depend on the generator staying correct.
		return nil, fmt.Errorf("%w: refusing to push the default branch %q", ErrRefused, req.DefaultBranch)
	}
	if strings.HasPrefix(req.DefaultBranch, "refs/heads/") && "refs/heads/"+branch == req.DefaultBranch {
		return nil, fmt.Errorf("%w: refusing to push the default branch", ErrRefused)
	}

	newSHA, err := b.worktreeHead(ctx, req.WorktreePath)
	if err != nil {
		return nil, err
	}
	oldSHA, exists, err := b.ResolveBranchSHA(ctx, req.RemoteURL, branch)
	if err != nil {
		return nil, err
	}
	switch {
	case exists && req.ExpectedBase == "":
		return nil, fmt.Errorf("%w: branch %s exists but no lease was given", ErrRefused, branch)
	case exists && oldSHA != req.ExpectedBase:
		return nil, fmt.Errorf("%w: branch %s is at %s, expected %s", ErrLeaseMismatch, branch, short(oldSHA), short(req.ExpectedBase))
	case !exists && req.ExpectedBase != "":
		return nil, fmt.Errorf("%w: branch %s vanished, lease expected %s", ErrLeaseMismatch, branch, short(req.ExpectedBase))
	}

	pushArgs := []string{"push", "--force-with-lease=refs/heads/" + branch + ":" + oldSHA,
		req.RemoteURL, newSHA + ":refs/heads/" + branch}
	if !exists {
		// A new branch needs no lease (the empty lease above already
		// asserts non-existence atomically on the server side).
		pushArgs = []string{"push", "--force-with-lease=refs/heads/" + branch + ":",
			req.RemoteURL, newSHA + ":refs/heads/" + branch}
	}
	if _, err := b.runGit(ctx, req.WorktreePath, req.RemoteURL, pushArgs...); err != nil {
		return nil, fmt.Errorf("gitbroker: push: %w", err)
	}

	confirmed, exists, err := b.ResolveBranchSHA(ctx, req.RemoteURL, branch)
	if err != nil {
		return nil, err
	}
	if !exists || confirmed != newSHA {
		return nil, fmt.Errorf("gitbroker: post-push verification failed for %s", branch)
	}
	return &PushResult{Branch: branch, OldSHA: oldSHA, NewSHA: confirmed}, nil
}

// worktreeHead resolves the committed HEAD of the local worktree.
func (b *Broker) worktreeHead(ctx context.Context, worktreePath string) (string, error) {
	out, err := b.runGit(ctx, worktreePath, "", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("gitbroker: worktree HEAD: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if !shaRe.MatchString(sha) {
		return "", fmt.Errorf("gitbroker: worktree HEAD is not a commit SHA")
	}
	return sha, nil
}

// runGit executes git with a bounded window, bounded output and a
// scrubbed environment; credentials ride an askpass helper resolved
// FOR THE REMOTE so they never appear in argv or the environment dump
// of a ps listing.
func (b *Broker) runGit(ctx context.Context, dir, remoteURL string, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	askpass, cleanup, askpassErr := b.askpassHelper(ctx, remoteURL)
	if askpassErr != nil {
		return nil, askpassErr
	}
	defer cleanup()

	cmd := exec.CommandContext(runCtx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.TempDir(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=" + askpass,
		"GIT_CONFIG_NOSYSTEM=1",
	}
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	if len(out) > 4<<20 {
		return nil, errors.New("gitbroker: output exceeded the bound")
	}
	return out, nil
}

// askpassHelper materializes the credential as a one-shot helper
// script; removed on completion. When no credential source is set the
// helper answers nothing (keyless remotes like file fixtures).
func (b *Broker) askpassHelper(ctx context.Context, remoteURL string) (string, func(), error) {
	if b.Credentials == nil {
		return "/bin/false", func() {}, nil
	}
	credential, err := b.Credentials.Credential(ctx, remoteURL)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "maestro-gitbroker-*")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "askpass")
	script := "#!/bin/sh\ncase \"$1\" in *Username*) printf %s ;; *) printf %s ;; esac\n"
	replaced := fmt.Sprintf(script, shellQuote(credential.Username), shellQuote(credential.Password))
	//nolint:gosec // GIT_ASKPASS must be executable; owner-only 0700
	if err := os.WriteFile(path, []byte(replaced), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return path, func() { _ = os.RemoveAll(dir) }, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
