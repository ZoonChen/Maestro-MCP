package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/sandbox"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
)

// The executor adapter's contract tests: digest-pinned profile
// resolution, honest outcome mapping, heartbeat continuity and the
// fail-closed refusals — all against a fake sandbox.

type fakeSandbox struct {
	mu       sync.Mutex
	specs    []sandbox.ContainerSpec
	result   sandbox.ExitResult
	runErr   error
	blockFor time.Duration
}

func (f *fakeSandbox) Run(ctx context.Context, spec sandbox.ContainerSpec) (sandbox.ExitResult, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	blockFor := f.blockFor
	f.mu.Unlock()
	if blockFor > 0 {
		select {
		case <-ctx.Done():
			return sandbox.ExitResult{ExitCode: -1}, ctx.Err()
		case <-time.After(blockFor):
		}
	}
	return f.result, f.runErr
}

func testProfile() service.CommandProfile {
	return service.CommandProfile{
		ID: "go-build", Version: "1.0.0",
		ImageDigest:      "sha256:" + strings.Repeat("a", 64),
		Argv:             []string{"make", "build"},
		WorkingDirectory: ".",
		Network:          service.CommandProfileNetwork{Mode: "none"},
		Resources:        service.CommandProfileResources{CPUMillis: 2000, MemoryMB: 2048, DiskMB: 4096, PIDs: 256},
		OutputLimitBytes: 65536, TimeoutSeconds: 120,
	}
}

func newTestExecutor(t *testing.T, box *fakeSandbox, profile service.CommandProfile, probe func(context.Context, string) (string, error)) (*SandboxExecutor, *Lease) {
	t.Helper()
	registry, err := service.NewCommandProfileRegistry([]service.CommandProfile{profile})
	require.NoError(t, err)
	digest, err := profile.Digest()
	require.NoError(t, err)
	lease := &Lease{
		ID: "lease-1", Version: 4, Epoch: 2, ExecutionID: "exec-1",
		WorkItemID: "wi-1", WorkspaceGeneration: 1,
		CommandProfiles: []CommandProfileRef{{ID: profile.ID, Version: profile.Version, Digest: digest}},
	}
	return &SandboxExecutor{
		Profiles:      registry,
		Sandbox:       box,
		WorkspaceRoot: t.TempDir(),
		CommitProbe:   probe,
	}, lease
}

func TestSandboxExecutorCompletedWithCommit(t *testing.T) {
	box := &fakeSandbox{result: sandbox.ExitResult{ExitCode: 0, Output: "build ok"}}
	executor, lease := newTestExecutor(t, box, testProfile(),
		func(context.Context, string) (string, error) { return "0123456789abcdef0123456789abcdef01234567", nil })

	completion, err := executor.Execute(context.Background(), lease, func() error { return nil })
	require.NoError(t, err)
	assert.Equal(t, OutcomeCompleted, completion.Outcome)
	require.NotNil(t, completion.CommitSHA)
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", *completion.CommitSHA)
	assert.Equal(t, int64(4), completion.LeaseVersion)

	require.Len(t, box.specs, 1)
	spec := box.specs[0]
	assert.Equal(t, testProfile().ImageDigest, spec.ImageDigest)
	assert.Equal(t, []string{"make", "build"}, spec.Argv)
	assert.Equal(t, 2048, spec.MemoryMB)
	assert.Equal(t, 256, spec.PIDsLimit)
	// The workspace is per-execution under the configured root.
	assert.Contains(t, spec.WorkspaceHostPath, "exec-1")
}

func TestSandboxExecutorRefusesDigestMismatch(t *testing.T) {
	box := &fakeSandbox{}
	executor, lease := newTestExecutor(t, box, testProfile(), nil)
	// Tamper the reference digest: the profile content no longer matches.
	lease.CommandProfiles[0].Digest = "sha256:" + strings.Repeat("b", 64)

	completion, err := executor.Execute(context.Background(), lease, func() error { return nil })
	require.NoError(t, err)
	assert.Equal(t, OutcomeFailed, completion.Outcome)
	assert.Contains(t, completion.Summary, "digest mismatch")
	assert.Empty(t, box.specs, "a mismatched profile must never start a container")
}

func TestSandboxExecutorRefusesNetworkProfiles(t *testing.T) {
	// The registry itself rejects non-none modes (validated at approval
	// time); the executor keeps its own guard for defense in depth. Reach
	// it directly with a registry-shaped profile whose network mode the
	// schema cannot carry.
	networked := testProfile()
	networked.ID = "net-profile"
	networked.Network = service.CommandProfileNetwork{Mode: "bridge"}
	registry, err := service.NewCommandProfileRegistry([]service.CommandProfile{networked})
	if err == nil {
		// Registry accepted it (future schema relaxation): the executor
		// guard must still refuse.
		digest, digestErr := networked.Digest()
		require.NoError(t, digestErr)
		box := &fakeSandbox{}
		executor := &SandboxExecutor{
			Profiles: registry, Sandbox: box, WorkspaceRoot: t.TempDir(),
			CommitProbe: func(context.Context, string) (string, error) { return "", nil },
		}
		lease := &Lease{ID: "l", Version: 1, ExecutionID: "e", WorkspaceGeneration: 1,
			CommandProfiles: []CommandProfileRef{{ID: networked.ID, Version: networked.Version, Digest: digest}}}
		completion, execErr := executor.Execute(context.Background(), lease, func() error { return nil })
		require.NoError(t, execErr)
		assert.Equal(t, OutcomeFailed, completion.Outcome)
		assert.Contains(t, completion.Summary, "network mode")
		assert.Empty(t, box.specs)
		return
	}
	// Registry refused: the guard is proven at approval time.
	assert.Contains(t, err.Error(), "network.mode=none")
}

func TestSandboxExecutorMapsFailureAndTimeout(t *testing.T) {
	failing := testProfile()
	failing.ID = "fail-profile"
	box := &fakeSandbox{result: sandbox.ExitResult{ExitCode: 2, Output: "make: error 2"}}
	executor, lease := newTestExecutor(t, box, failing, nil)
	completion, err := executor.Execute(context.Background(), lease, func() error { return nil })
	require.NoError(t, err)
	assert.Equal(t, OutcomeFailed, completion.Outcome)
	assert.Nil(t, completion.CommitSHA)
	assert.Contains(t, completion.Summary, "exit 2")

	timed := testProfile()
	timed.ID = "timeout-profile"
	timedBox := &fakeSandbox{result: sandbox.ExitResult{ExitCode: -1, TimedOut: true}}
	executor2, lease2 := newTestExecutor(t, timedBox, timed, nil)
	completion2, err := executor2.Execute(context.Background(), lease2, func() error { return nil })
	require.NoError(t, err)
	assert.Equal(t, OutcomeFailed, completion2.Outcome)
	assert.Contains(t, completion2.Summary, "timeout")
}

func TestSandboxExecutorSuccessWithoutCommitIsHonestFailure(t *testing.T) {
	box := &fakeSandbox{result: sandbox.ExitResult{ExitCode: 0}}
	executor, lease := newTestExecutor(t, box, testProfile(),
		func(context.Context, string) (string, error) { return "", errors.New("no HEAD") })

	completion, err := executor.Execute(context.Background(), lease, func() error { return nil })
	require.NoError(t, err)
	assert.Equal(t, OutcomeFailed, completion.Outcome)
	assert.Contains(t, completion.Summary, "no commit")
}

func TestSandboxExecutorHeartbeatsDuringLongRuns(t *testing.T) {
	box := &fakeSandbox{
		result:   sandbox.ExitResult{ExitCode: 0},
		blockFor: 70 * time.Millisecond, // spans multiple fast heartbeats
	}
	executor := &SandboxExecutor{}
	registry, err := service.NewCommandProfileRegistry([]service.CommandProfile{testProfile()})
	require.NoError(t, err)
	digest, _ := testProfile().Digest()
	lease := &Lease{
		ID: "lease-1", Version: 1, ExecutionID: "exec-1", WorkspaceGeneration: 1,
		CommandProfiles: []CommandProfileRef{{ID: "go-build", Version: "1.0.0", Digest: digest}},
	}
	executor.Profiles = registry
	executor.Sandbox = box
	executor.WorkspaceRoot = t.TempDir()
	executor.CommitProbe = func(context.Context, string) (string, error) {
		return "0123456789abcdef0123456789abcdef01234567", nil
	}

	heartbeats := 0
	executor.heartbeatInterval = 20 * time.Millisecond

	completion, err := executor.Execute(context.Background(), lease, func() error {
		heartbeats++
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, OutcomeCompleted, completion.Outcome)
	assert.GreaterOrEqual(t, heartbeats, 2, "heartbeats must tick while the sandbox runs")

	// A heartbeat rejection mid-flight converts the outcome to an honest
	// failure even if the sandbox succeeded.
	executor.CommitProbe = func(context.Context, string) (string, error) { return "sha", nil }
	box.mu.Lock()
	box.blockFor = 50 * time.Millisecond
	box.mu.Unlock()
	executor.heartbeatInterval = 10 * time.Millisecond
	completion, err = executor.Execute(context.Background(), lease, func() error {
		return errors.New("LEASE_VERSION_MISMATCH")
	})
	require.NoError(t, err)
	assert.Equal(t, OutcomeFailed, completion.Outcome)
	assert.Contains(t, completion.Summary, "heartbeat rejected")
}
