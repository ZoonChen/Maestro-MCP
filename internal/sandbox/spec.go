// Package sandbox executes server-authorized work in a hardened,
// digest-pinned OCI container (M1-RUN-001 / SEC-RUNNER-SECURITY): network
// disabled by default, every Linux capability dropped, no-new-privileges,
// read-only root filesystem, non-root UID, PID/cgroup/memory limits, and
// exactly ONE writable mount — the allocated workspace. Nothing from the
// host (sockets, SSH, cloud credentials, HOME) is visible inside.
//
// Declared egress (P5a): a profile may declare an allowlist of mirror
// domains (zero-trust-validation §"无默认网络, profile 明确声明"). The
// allowlist mode keeps deny-by-default — the job container lands on an
// internal network with no external route, and the ONLY egress path is a
// per-execution filtering proxy that forwards CONNECT requests to the
// declared hosts and denies everything else.
package sandbox

import (
	"regexp"
	"time"
)

// Network modes. Empty defaults to none at execution time; allowlist
// requires AllowHosts.
const (
	NetworkNone      = "none"
	NetworkAllowlist = "allowlist"
)

// ContainerSpec is one fully constrained execution. Argv comes only from a
// server-approved versioned Command Profile — never from an agent.
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
	// NetworkMode selects the egress policy: NetworkNone (default) or
	// NetworkAllowlist (declared mirror domains through a filtering
	// proxy; no direct route exists).
	NetworkMode string
	// AllowHosts carries the declared egress domains for allowlist mode
	// (hostname or dot-prefixed subdomain match, mirror-style).
	AllowHosts []string
	// ExecutionID names the per-execution egress resources (network and
	// proxy container). Required for allowlist mode.
	ExecutionID string
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
	switch s.NetworkMode {
	case "", NetworkNone:
		if len(s.AllowHosts) != 0 {
			return errSpec("network none cannot carry allow_hosts")
		}
	case NetworkAllowlist:
		if len(s.AllowHosts) == 0 || len(s.AllowHosts) > 16 {
			return errSpec("allowlist mode requires 1..16 allow_hosts")
		}
		for _, host := range s.AllowHosts {
			if !allowedHostPattern.MatchString(host) {
				return errSpec("allow_host must be a hostname or dot-prefixed domain: " + host)
			}
		}
		if !executionIDPattern.MatchString(s.ExecutionID) {
			return errSpec("allowlist mode requires a valid execution id for egress resources")
		}
	default:
		return errSpec("network mode must be none or allowlist")
	}
	return nil
}

func containsDigest(ref string) bool {
	// name@sha256:<64 hex> — cheap structural check; the runtime verifies
	// that the digest actually resolves at pull time.
	for i := 0; i+14 <= len(ref); i++ {
		if ref[i:i+8] == "@sha256:" && len(ref)-i-8 == 64 {
			return true
		}
	}
	return false
}

// allowedHostPattern accepts a lowercase hostname (mirror domains) or a
// dot-prefixed domain that subdomain-matches at the proxy. Ports, schemes,
// wildcards and IPv6 literals are deliberately out of scope: mirrors are
// plain https hostnames.
var allowedHostPattern = regexp.MustCompile(`^(\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// executionIDPattern accepts the server-issued execution identifiers used
// to name per-execution egress resources (docker object names).
var executionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

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
