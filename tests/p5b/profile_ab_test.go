//go:build p5b

package p5b_test

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

// TestP5bProfileAB runs the phase-1 maven-build Command Profile against
// BOTH D1 routes — peixun-backend (RuoYi 底座, route B) and playedu-eval
// (PlayEdu, route A) — same profile, same sandbox, same network: the
// build-chain A/B the detailed-design pins. Diagnostic Evidence only;
// the authoritative word stays with the GitLab pipelines.

type p5bProfileFile struct {
	Validation struct {
		CommandProfiles []config.ValidationCommandProfile `yaml:"command_profiles"`
	} `yaml:"validation"`
}

func TestP5bProfileAB(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("no OCI runtime on this machine")
	}
	pat := os.Getenv("P5B_PILOT_GITLAB_PAT")
	if pat == "" {
		t.Skip("P5B_PILOT_GITLAB_PAT not set (sandbox GitLab root PAT)")
	}
	raw, err := os.ReadFile(filepath.Join(p5bRepositoryRoot, "deploy", "gitlab", "peixun", "command-profiles.yaml"))
	require.NoError(t, err)
	var file p5bProfileFile
	require.NoError(t, yaml.Unmarshal(raw, &file))
	require.NotEmpty(t, file.Validation.CommandProfiles)
	profiles := make([]service.CommandProfile, 0, len(file.Validation.CommandProfiles))
	for _, p := range file.Validation.CommandProfiles {
		profiles = append(profiles, service.CommandProfile{
			ID: p.ID, Version: p.Version, ImageDigest: p.ImageDigest, Argv: p.Argv,
			WorkingDirectory: p.WorkingDirectory,
			Network:          service.CommandProfileNetwork{Mode: p.Network.Mode, AllowHosts: p.Network.AllowHosts},
			Resources: service.CommandProfileResources{
				CPUMillis: p.Resources.CPUMillis, MemoryMB: p.Resources.MemoryMB,
				DiskMB: p.Resources.DiskMB, PIDs: p.Resources.PIDs,
			},
			OutputLimitBytes: p.OutputLimitBytes, TimeoutSeconds: p.TimeoutSeconds,
			Environment: p.Environment,
		})
	}
	registry, err := service.NewCommandProfileRegistry(profiles)
	require.NoError(t, err)
	byID := map[string]service.CommandProfile{}
	for _, profile := range profiles {
		byID[profile.ID] = profile
	}
	profile := byID["maven-build"]
	require.NotNil(t, profile, "maven-build profile missing from the committed file")
	digest, err := profile.Digest()
	require.NoError(t, err)

	runtime, err := sandbox.DetectRuntime()
	require.NoError(t, err)
	root := t.TempDir()

	ev := p5bNewEvidence(t)
	ab := map[string]any{}
	for _, route := range []struct {
		label, repo string
		rounds      int
	}{{"routeB-ruoyi", "peixun/peixun-backend", 2}, {"routeA-playedu", "peixun/playedu-eval", 2}} {
		for round := 1; round <= route.rounds; round++ {
			t.Run(route.label+"-round"+string(rune('0'+round)), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				executionID := "p5b-ab-" + route.label + "-r" + string(rune('0'+round))
				workspace := filepath.Join(root, executionID)
				require.NoError(t, exec.Command("git", "clone", "-q", "--depth", "1",
					"http://root:"+pat+"@127.0.0.1:8181/"+route.repo+".git", workspace).Run())
				executor := &runner.SandboxExecutor{
					Profiles: registry, Sandbox: runtime, WorkspaceRoot: root,
				}
				lease := &runner.Lease{
					ID: executionID, Version: 1, ExecutionID: executionID, WorkspaceGeneration: 1,
					CommandProfiles: []runner.CommandProfileRef{
						{ID: profile.ID, Version: profile.Version, Digest: digest},
					},
				}
				started := time.Now()
				completion, execErr := executor.Execute(ctx, lease, func() error { return nil })
				require.NoError(t, execErr)
				elapsed := time.Since(started).Seconds()
				assert.Equal(t, runner.OutcomeCompleted, completion.Outcome,
					"real profile execution must complete: %s", completion.Summary)
				_, statErr := os.Stat(filepath.Join(workspace, "ci-smoke/target/surefire-reports"))
				require.NoError(t, statErr, "junit reports must exist after the run")
				ab[executionID] = map[string]any{
					"repo": route.repo, "profile": "maven-build@1.0.0",
					"outcome": string(completion.Outcome), "seconds": elapsed,
				}
				t.Logf("%s: %.1fs outcome=%s", executionID, elapsed, completion.Outcome)
				ev.record(t, "profile_ab", ab)
			})
		}
	}
}
