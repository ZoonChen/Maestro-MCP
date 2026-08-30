// Package sandbox executes server-authorized work in a hardened,
// digest-pinned OCI container (M1-RUN-001 / SEC-RUNNER-SECURITY): network
// disabled, every Linux capability dropped, no-new-privileges, read-only
// root filesystem, non-root UID, PID/cgroup/memory limits, and exactly
// ONE writable mount — the allocated workspace. Nothing from the host
// (sockets, SSH, cloud credentials, HOME) is visible inside.
package sandbox

import (
	"time"
)

// ContainerSpec is one fully constrained execution. Argv comes only from
// a server-approved versioned Command Profile — never from an agent.
type ContainerSpec struct {
	// ImageDigest is the immutable image reference (name@sha256:...).
	ImageDigest string
	// Argv is the profile's exact argument vector.
	Argv []string
	// WorkDir inside the container (typically /workspace).
	WorkDir string
	// Env is the minimal allowlist the profile declares; secrets and host
	// variables are structurally absent.
	Env map[string]string
	// WorkspaceHostPath is the ONE bind mount, writable, at WorkDir.
	WorkspaceHostPath string
	// Resource hard limits from the profile.
	MemoryMB  int
	CPUMillis int
	PIDsLimit int
	// Timeout bounds the whole execution.
	Timeout time.Duration
	// OutputLimitBytes caps captured stdout+stderr.
	OutputLimitBytes int64
}

// ExitResult reports the bounded outcome.
type ExitResult struct {
	ExitCode   int
	Output     string
	Truncated  bool
	TimedOut   bool
	DurationMs int64
}

// Validate enforces the hardening invariants: a spec that weakens any
// control is rejected before a container exists.
func (s *ContainerSpec) Validate() error {
	if s.ImageDigest == "" || !containsDigest(s.ImageDigest) {
		return errSpec("image must be pinned by digest")
	}
	if len(s.Argv) == 0 {
		return errSpec("argv must come from an approved command profile")
	}
	if s.WorkspaceHostPath == "" || s.WorkDir == "" {
		return errSpec("exactly one workspace mount is required")
	}
	if s.MemoryMB < 32 || s.MemoryMB > 16384 {
		return errSpec("memory limit must be 32..16384 MB")
	}
	if s.CPUMillis < 100 || s.CPUMillis > 16000 {
		return errSpec("cpu limit must be 100..16000 millis")
	}
	if s.PIDsLimit < 16 || s.PIDsLimit > 512 {
		return errSpec("pids limit must be 16..512")
	}
	if s.Timeout <= 0 || s.Timeout > 30*time.Minute {
		return errSpec("timeout must be 1s..30m")
	}
	if s.OutputLimitBytes < 1024 || s.OutputLimitBytes > 32<<20 {
		return errSpec("output limit must be 1KiB..32MiB")
	}
	for key := range s.Env {
		if key == "" || containsAny(key, ' ', '=') {
			return errSpec("environment keys must be plain identifiers")
		}
	}
	return nil
}

func containsDigest(ref string) bool {
	// name@sha256:<64 hex> — cheap structural check; the runtime verifies
	// the digest actually resolves at pull time.
	for i := 0; i+14 <= len(ref); i++ {
		if ref[i:i+8] == "@sha256:" && len(ref)-i-8 == 64 {
			return true
		}
	}
	return false
}

func containsAny(value string, needles ...rune) bool {
	for _, r := range value {
		for _, n := range needles {
			if r == n {
				return true
			}
		}
	}
	return false
}

type specError string

func errSpec(msg string) error { return specError("sandbox: " + msg) }

func (e specError) Error() string { return string(e) }
