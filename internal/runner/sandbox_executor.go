package runner

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/sandbox"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
)

// SandboxExecutor adapts the hardened OCI sandbox to the daemon's
// Executor contract (M1-RUN-001): leases carry only digest-pinned
// Command Profile REFERENCES — the profile content comes from the local
// trusted registry and MUST match the reference digest exactly, or the
// execution refuses to run (fail closed, reported honestly).
type SandboxExecutor struct {
	// heartbeatInterval is the cadence; tests tighten it. Defaults to the
	// frozen 15s window.
	heartbeatInterval time.Duration
	// Profiles resolves approved, versioned command profiles.
	Profiles *service.CommandProfileRegistry
	// Sandbox runs one hardened container.
	Sandbox SandboxRunner
	// WorkspaceRoot hosts one directory per execution.
	WorkspaceRoot string
	// CommitProbe derives the produced commit for completed work
	// (workspace git HEAD). Nil uses the default git probe.
	CommitProbe func(ctx context.Context, workspacePath string) (string, error)
}

// SandboxRunner is the sandbox execution boundary (implemented by
// *sandbox.Runtime; fakes serve tests).
type SandboxRunner interface {
	Run(ctx context.Context, spec sandbox.ContainerSpec) (sandbox.ExitResult, error)
}

// Execute runs the lease's referenced profile in the sandbox and maps
// the bounded outcome onto the frozen completion contract.
func (e *SandboxExecutor) Execute(ctx context.Context, lease *Lease, heartbeat func() error) (ExecutionCompletion, error) {
	if e.heartbeatInterval <= 0 {
		e.heartbeatInterval = HeartbeatInterval
	}
	if len(lease.CommandProfiles) == 0 {
		return e.failed(lease, "lease carries no command profile reference"), nil
	}
	reference := lease.CommandProfiles[0]

	profile, err := e.Profiles.Resolve(reference.ID, reference.Version, reference.Digest)
	if err != nil {
		return e.failed(lease, fmt.Sprintf("command profile %s@%s unavailable: %v", reference.ID, reference.Version, err)), nil
	}

	workspace := filepath.Join(e.WorkspaceRoot, lease.ExecutionID)
	spec := sandbox.ContainerSpec{
		ImageDigest:       profile.ImageDigest,
		Argv:              append([]string(nil), profile.Argv...),
		WorkDir:           "/" + strings.Trim(profile.WorkingDirectory, "/"),
		Env:               cloneEnv(profile.Environment),
		WorkspaceHostPath: workspace,
		MemoryMB:          profile.Resources.MemoryMB,
		CPUMillis:         profile.Resources.CPUMillis,
		PIDsLimit:         profile.Resources.PIDs,
		Timeout:           time.Duration(profile.TimeoutSeconds) * time.Second,
		OutputLimitBytes:  profile.OutputLimitBytes,
	}
	if spec.WorkDir == "/" || profile.WorkingDirectory == "" {
		spec.WorkDir = "/workspace"
	}
	// The M1 sandbox is default-no-network; a profile declaring a network
	// mode other than "none" cannot be honored yet and must not silently
	// degrade — refuse.
	if profile.Network.Mode != "" && profile.Network.Mode != "none" {
		return e.failed(lease, fmt.Sprintf("profile network mode %q is not supported by the M1 sandbox", profile.Network.Mode)), nil
	}

	// Heartbeats tick for the whole execution so the lease stays live
	// exactly as long as the work does.
	heartbeatCtx, stopHeartbeats := context.WithCancel(ctx)
	var heartbeatWG sync.WaitGroup
	heartbeatFailures := make(chan error, 1)
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		ticker := time.NewTicker(e.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if err := heartbeat(); err != nil {
					select {
					case heartbeatFailures <- err:
					default:
					}
					return
				}
			}
		}
	}()
	result, runErr := e.Sandbox.Run(ctx, spec)
	stopHeartbeats()
	heartbeatWG.Wait()

	close(heartbeatFailures)
	if hbErr := <-heartbeatFailures; hbErr != nil {
		// The lease died mid-flight: the outcome is the server's call, but
		// reporting a success here would be a lie.
		return e.failed(lease, fmt.Sprintf("lease heartbeat rejected: %v", hbErr)), nil
	}
	if runErr != nil {
		return ExecutionCompletion{}, runErr
	}

	if result.TimedOut {
		return e.failed(lease, "execution exceeded the profile timeout"), nil
	}
	if result.ExitCode != 0 {
		return ExecutionCompletion{
			LeaseID:              lease.ID,
			LeaseVersion:         lease.Version,
			ConnectionGeneration: "",
			WorkspaceGeneration:  lease.WorkspaceGeneration,
			Outcome:              OutcomeFailed,
			Summary:              truncate("exit "+fmt.Sprint(result.ExitCode)+": "+firstLines(result.Output), 8000),
		}, nil
	}

	commit, probeErr := e.probeCommit(ctx, workspace)
	if probeErr != nil {
		// A run without a produced commit cannot be "completed" under the
		// frozen contract — the honest outcome is a failure the server can
		// triage, never a fabricated success.
		return e.failed(lease, "execution succeeded but produced no commit: "+probeErr.Error()), nil //nolint:nilerr // deliberate honest-failure mapping
	}
	return ExecutionCompletion{
		LeaseID:              lease.ID,
		LeaseVersion:         lease.Version,
		ConnectionGeneration: "",
		WorkspaceGeneration:  lease.WorkspaceGeneration,
		Outcome:              OutcomeCompleted,
		CommitSHA:            &commit,
		Summary:              truncate(firstLines(result.Output), 8000),
	}, nil
}

func (e *SandboxExecutor) probeCommit(ctx context.Context, workspace string) (string, error) {
	if e.CommitProbe != nil {
		return e.CommitProbe(ctx, workspace)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, "git", "-C", workspace, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("workspace has no commit: %w", err)
	}
	commit := strings.TrimSpace(string(out))
	if commit == "" {
		return "", fmt.Errorf("workspace HEAD is empty")
	}
	return commit, nil
}

func (e *SandboxExecutor) failed(lease *Lease, reason string) ExecutionCompletion {
	return ExecutionCompletion{
		LeaseID:             lease.ID,
		LeaseVersion:        lease.Version,
		WorkspaceGeneration: lease.WorkspaceGeneration,
		Outcome:             OutcomeFailed,
		Summary:             truncate(reason, 8000),
	}
}

func cloneEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(env))
	for key, value := range env {
		cloned[key] = value
	}
	return cloned
}

func firstLines(output string) string {
	lines := strings.SplitN(output, "\n", 5)
	return strings.Join(lines, "\n")
}
