//go:build p5a

package runner_test

// P5a-4 real-execution harness: drives the production SandboxExecutor and
// the rootless OCI sandbox with the pilot's phase-1 Command Profiles
// (deploy/gitlab/peixun/command-profiles.yaml) against REAL clones of the
// two pilot repositories. This proves the whole diagnostic-execution
// slice — pinned images, declared-egress allowlist, proxy env, junit
// artifacts — end to end on a machine with:
//
//	docker; the GitLab sandbox up (make gitlab-up && make gitlab-provision
//	&& deploy/gitlab/peixun-provision.sh); P5A_PILOT_GITLAB_PAT=<root PAT>.
//
// Run:  go test -tags p5a ./internal/runner/ -run TestP5aPhaseOneProfiles -v -count=1
// It is excluded from CI (no p5a tag there); the sandbox egress filter
// itself is covered in CI by internal/sandbox/egress_test.go.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/config"
	"github.com/ZoonChen/Maestro-MCP/internal/runner"
	"github.com/ZoonChen/Maestro-MCP/internal/sandbox"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"gopkg.in/yaml.v3"
)

type p5aProfileFile struct {
	Validation struct {
		CommandProfiles []config.ValidationCommandProfile `yaml:"command_profiles"`
	} `yaml:"validation"`
}

func p5aLoadProfiles(t *testing.T) (*service.CommandProfileRegistry, map[string]service.CommandProfile) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "gitlab", "peixun", "command-profiles.yaml"))
	require.NoError(t, err, "command-profiles.yaml is part of the P5a slice")
	var file p5aProfileFile
	require.NoError(t, yaml.Unmarshal(raw, &file))
	require.NotEmpty(t, file.Validation.CommandProfiles)
	profiles := make([]service.CommandProfile, 0, len(file.Validation.CommandProfiles))
	for _, p := range file.Validation.CommandProfiles {
		profiles = append(profiles, service.CommandProfile{
			ID:               p.ID,
			Version:          p.Version,
			ImageDigest:      p.ImageDigest,
			Argv:             p.Argv,
			WorkingDirectory: p.WorkingDirectory,
			Network:          service.CommandProfileNetwork{Mode: p.Network.Mode, AllowHosts: p.Network.AllowHosts},
			Resources: service.CommandProfileResources{
				CPUMillis: p.Resources.CPUMillis, MemoryMB: p.Resources.MemoryMB,
				DiskMB: p.Resources.DiskMB, PIDs: p.Resources.PIDs,
			},
			OutputLimitBytes: p.OutputLimitBytes,
			TimeoutSeconds:   p.TimeoutSeconds,
			Environment:      p.Environment,
		})
	}
	registry, err := service.NewCommandProfileRegistry(profiles)
	require.NoError(t, err, "the committed profile file must pass registry validation")
	byID := make(map[string]service.CommandProfile, len(profiles))
	for _, profile := range profiles {
		byID[profile.ID] = profile
	}
	return registry, byID
}

func p5aCloneRepo(t *testing.T, url, pat, dest string) {
	t.Helper()
	require.NoError(t, exec.Command("git", "clone", "-q", "--depth", "1",
		"http://root:"+pat+"@127.0.0.1:8181/"+url+".git", dest).Run())
}

func TestP5aPhaseOneProfiles(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("no OCI runtime on this machine")
	}
	pat := os.Getenv("P5A_PILOT_GITLAB_PAT")
	if pat == "" {
		t.Skip("P5A_PILOT_GITLAB_PAT not set (sandbox GitLab root PAT)")
	}
	registry, byID := p5aLoadProfiles(t)
	runtime, err := sandbox.DetectRuntime()
	require.NoError(t, err)

	root := t.TempDir()

	// The executor mounts WorkspaceRoot/<ExecutionID> as the one writable
	// workspace, so each case's repo clone must sit exactly there.
	// profile id -> repo label -> repo url -> junit artifact (relative).
	cases := []struct {
		profile  string
		repo     string
		repoURL  string
		artifact string
	}{
		{"maven-build", "backend", "peixun/peixun-backend", "ci-smoke/target/surefire-reports"},
		{"npm-build", "web", "peixun/peixun-web", "ci-smoke/junit.xml"},
		{"playwright-e2e", "web", "peixun/peixun-web", "ci-e2e/junit.xml"},
	}

	for _, tc := range cases {
		t.Run(tc.profile+"@"+tc.repo, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			profile := byID[tc.profile]
			require.NotNil(t, profile, "profile %s missing from the committed file", tc.profile)
			digest, err := profile.Digest()
			require.NoError(t, err)

			executionID := "p5a-" + tc.profile + "-" + tc.repo
			workspace := filepath.Join(root, executionID)
			p5aCloneRepo(t, tc.repoURL, pat, workspace)
			executor := &runner.SandboxExecutor{
				Profiles:      registry,
				Sandbox:       runtime,
				WorkspaceRoot: root,
			}
			lease := &runner.Lease{
				ID: "p5a-" + tc.profile, Version: 1,
				ExecutionID:         executionID,
				WorkspaceGeneration: 1,
				CommandProfiles: []runner.CommandProfileRef{
					{ID: profile.ID, Version: profile.Version, Digest: digest},
				},
			}
			completion, execErr := executor.Execute(ctx, lease, func() error { return nil })
			require.NoError(t, execErr)
			assert.Equal(t, runner.OutcomeCompleted, completion.Outcome,
				"real profile execution must complete: %s", completion.Summary)
			t.Logf("summary tail: %s", tail(completion.Summary, 400))
			if _, statErr := os.Stat(filepath.Join(workspace, tc.artifact)); statErr != nil {
				t.Errorf("expected junit artifact %s after run: %v", tc.artifact, statErr)
			}
		})
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
