package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runtime is the container runtime binary (docker or podman — both honor
// the same hardening flags; podman is rootless by design).
type Runtime struct {
	binary string
}

// DetectRuntime finds an installed OCI runtime, preferring podman
// (rootless by design) over docker.
func DetectRuntime() (*Runtime, error) {
	for _, candidate := range []string{"podman", "docker"} {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return &Runtime{binary: path}, nil
		}
	}
	return nil, fmt.Errorf("sandbox: no OCI runtime found (podman or docker required)")
}

// Available reports whether a usable runtime is present; tests use this
// to skip on machines without one.
func Available() bool {
	_, err := DetectRuntime()
	return err == nil
}

// Run executes the spec in a fully hardened container. The image digest
// is resolved by the runtime itself (pull by digest); a wrong digest
// simply fails to resolve.
func (r *Runtime) Run(ctx context.Context, spec ContainerSpec) (ExitResult, error) {
	if err := spec.Validate(); err != nil {
		return ExitResult{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	args := []string{
		"run", "--rm",
		"--network", "none", // default no network
		"--cap-drop", "ALL", // no Linux capabilities
		"--security-opt", "no-new-privileges", // no setuid/setgid escape
		"--read-only",                              // immutable root filesystem
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=64m", // scratch without suid
		"--user", "65534:65534", // nobody — never root
		"--pids-limit", fmt.Sprintf("%d", spec.PIDsLimit),
		"--memory", fmt.Sprintf("%dm", spec.MemoryMB),
		"--cpus", fmt.Sprintf("%.2f", float64(spec.CPUMillis)/1000.0),
		"--stop-timeout", "5",
		// Exactly one writable mount: the allocated workspace.
		"--volume", spec.WorkspaceHostPath + ":" + spec.WorkDir + ":rw",
		"--workdir", spec.WorkDir,
	}
	for key, value := range spec.Env {
		args = append(args, "--env", key+"="+value)
	}
	// The digest-pinned image and the profile's exact argv.
	args = append(args, spec.ImageDigest)
	args = append(args, spec.Argv...)

	started := time.Now()
	// The binary comes from exec.LookPath (never request data); every
	// argument is server-derived here — argv arrives only from a
	// validated, digest-pinned Command Profile via Validate().
	command := exec.CommandContext(runCtx, r.binary, args...) //nolint:gosec // arguments are server-derived and validated above
	var output boundedBuffer
	output.limit = spec.OutputLimitBytes
	command.Stdout = &output
	command.Stderr = &output
	runErr := command.Run()
	duration := time.Since(started)

	result := ExitResult{
		DurationMs: duration.Milliseconds(),
		Output:     output.String(),
		Truncated:  output.truncated,
	}
	if runCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		return result, nil
	}
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("sandbox: runtime invocation failed: %w", runErr)
	}
	return result, nil
}

// boundedBuffer caps captured output, keeping a truncation marker.
type boundedBuffer struct {
	limit       int64
	accumulated int64
	truncated   bool
	buffer      bytes.Buffer
}

func (b *boundedBuffer) Write(chunk []byte) (int, error) {
	b.accumulated += int64(len(chunk))
	if b.truncated {
		return len(chunk), nil
	}
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.truncated = true
		return len(chunk), nil
	}
	if int64(len(chunk)) > remaining {
		b.buffer.Write(chunk[:remaining])
		b.truncated = true
		return len(chunk), nil
	}
	b.buffer.Write(chunk)
	return len(chunk), nil
}

func (b *boundedBuffer) String() string {
	text := b.buffer.String()
	if b.truncated {
		return text + "\n[sandbox: output truncated at limit]"
	}
	return text
}

// CommandDigestReport returns the exact runtime invocation for audit
// records without the spec's environment values.
func CommandDigestReport(spec ContainerSpec) string {
	return strings.Join(append([]string{
		"run --rm --network none --cap-drop ALL",
		"--security-opt no-new-privileges --read-only --user 65534:65534",
		fmt.Sprintf("--pids-limit %d --memory %dm --cpus %dm",
			spec.PIDsLimit, spec.MemoryMB, spec.CPUMillis),
		fmt.Sprintf("--volume %s:%s:rw", spec.WorkspaceHostPath, spec.WorkDir),
		spec.ImageDigest,
	}, spec.Argv...), " ")
}
