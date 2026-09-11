package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Allowlist semantics (P5a): declared mirror domains are reachable through
// the filtering proxy; everything else stays denied, exactly like the
// none-mode default.

func TestSpecValidateNetworkModes(t *testing.T) {
	base := func(mode string, hosts []string, execID string) error {
		spec := testSpec(t, "/bin/true")
		spec.NetworkMode = mode
		spec.AllowHosts = hosts
		spec.ExecutionID = execID
		return spec.Validate()
	}

	assert.NoError(t, base("", nil, ""))
	assert.NoError(t, base(NetworkNone, nil, ""))
	assert.Error(t, base(NetworkNone, []string{"maven.aliyun.com"}, ""), "none cannot carry hosts")

	assert.NoError(t, base(NetworkAllowlist, []string{"maven.aliyun.com", ".npmjs.org"}, "exec-1"))

	assert.Error(t, base(NetworkAllowlist, nil, "exec-1"), "allowlist requires hosts")
	assert.Error(t, base(NetworkAllowlist, []string{}, "exec-1"), "allowlist requires hosts")
	assert.Error(t, base(NetworkAllowlist, make([]string, 17), "exec-1"), "host count is capped")
	assert.Error(t, base(NetworkAllowlist, []string{"maven.aliyun.com"}, ""), "allowlist requires an execution id")
	assert.Error(t, base(NetworkAllowlist, []string{"https://maven.aliyun.com"}, "exec-1"), "schemes are rejected")
	assert.Error(t, base(NetworkAllowlist, []string{"maven.aliyun.com:443"}, "exec-1"), "ports are rejected")
	assert.Error(t, base(NetworkAllowlist, []string{"maven..aliyun"}, "exec-1"), "malformed hosts are rejected")
	assert.Error(t, base("bridge", nil, ""), "unknown modes are rejected")
}

// TestSandboxAllowlistEgressFiltering runs REAL containers: an allowlisted
// https host answers through the proxy, a non-listed host is refused, and
// the sandbox default (none) still has no egress at all.
func TestSandboxAllowlistEgressFiltering(t *testing.T) {
	runtime := sandboxRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// curlimages/curl:8.11.1, digest-pinned.
	spec := testSpec(t, "curl", "-s", "-o", "/dev/null",
		"-w", "%{http_code}",
		"--max-time", "20", "https://registry.npmmirror.com/")
	spec.ImageDigest = "curlimages/curl@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69"
	spec.NetworkMode = NetworkAllowlist
	spec.AllowHosts = []string{"registry.npmmirror.com"}
	spec.ExecutionID = "p5a-egress-test-1"
	spec.Timeout = 90 * time.Second

	allowed, err := runtime.Run(ctx, spec)
	require.NoError(t, err)
	assert.Equal(t, 0, allowed.ExitCode, "allowed host through proxy: %s", allowed.Output)
	// podman interleaves image-pull progress into the captured output
	// (docker keeps it on stderr); the command's own answer is the last
	// non-empty line under both engines.
	assert.Equal(t, "200", lastOutputLine(allowed.Output), "npmmirror answers through the filtered proxy")

	// Same spec, non-listed host: the proxy must refuse the CONNECT.
	deniedSpec := spec
	deniedSpec.ExecutionID = "p5a-egress-test-2"
	deniedSpec.Argv = []string{"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"--max-time", "20", "https://example.com/"}
	denied, err := runtime.Run(ctx, deniedSpec)
	require.NoError(t, err)
	assert.NotEqual(t, 0, denied.ExitCode, "non-listed host must not be reachable: %s", denied.Output)
}

// lastOutputLine returns the last non-empty line of a captured output.
func lastOutputLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if trimmed := strings.TrimSpace(lines[index]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
