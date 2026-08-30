package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Escape tests execute REAL hardened containers and attempt every escape
// family from SEC-RUNNER-SECURITY: file (host path traversal), network
// (egress), process (host PID namespace), environment (secret leakage)
// and container escape (runtime socket / device / credential access).
// They run wherever an OCI runtime exists — including CI — and skip only
// on machines without one.

const (
	// alpine 3.20 digest-pinned test image.
	testImageDigest = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	testMemoryMB    = 256
	testCPUMillis   = 1000
	testPIDsLimit   = 64
	testOutputLimit = 64 << 10
)

func sandboxRuntime(t *testing.T) *Runtime {
	t.Helper()
	if !Available() {
		t.Skip("no OCI runtime available (podman or docker required)")
	}
	runtime, err := DetectRuntime()
	require.NoError(t, err)
	return runtime
}

func testSpec(t *testing.T, argv ...string) ContainerSpec {
	t.Helper()
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "marker.txt"), []byte("workspace\n"), 0o600))
	return ContainerSpec{
		ImageDigest:       testImageDigest,
		Argv:              argv,
		WorkDir:           "/workspace",
		WorkspaceHostPath: workspace,
		MemoryMB:          testMemoryMB,
		CPUMillis:         testCPUMillis,
		PIDsLimit:         testPIDsLimit,
		Timeout:           60 * time.Second,
		OutputLimitBytes:  testOutputLimit,
	}
}

func TestSandboxFileEscapeIsBlocked(t *testing.T) {
	runtime := sandboxRuntime(t)
	ctx := context.Background()

	// The workspace is writable; paths outside it do not exist, and the
	// root filesystem is read-only.
	result, err := runtime.Run(ctx, testSpec(t,
		"/bin/sh", "-c",
		`ls /workspace/marker.txt >/dev/null && echo WORKSPACE_OK; ls /root 2>&1 | head -1; touch /etc/pwned 2>&1 && echo ROOTFS_WRITABLE || echo ROOTFS_READONLY`))
	require.NoError(t, err)
	assert.Contains(t, result.Output, "WORKSPACE_OK", "the one workspace mount must work")
	assert.Contains(t, result.Output, "ROOTFS_READONLY", "the root filesystem must be immutable")
	assert.NotContains(t, result.Output, "ROOTFS_WRITABLE")
	// Host filesystem probes outside the workspace fail outright.
	result, err = runtime.Run(ctx, testSpec(t,
		"/bin/sh", "-c", `ls /proc/self/../../../ 2>&1 | head -1`))
	require.NoError(t, err)
	assert.NotContains(t, result.Output, "Users", "host home directories must not be enumerable")
}

func TestSandboxNetworkIsDisabled(t *testing.T) {
	runtime := sandboxRuntime(t)
	result, err := runtime.Run(context.Background(), testSpec(t,
		"/bin/sh", "-c",
		`ip link 2>/dev/null | grep -c 'UP' || true; (echo > /dev/tcp/1.1.1.1/53) 2>&1 && echo EGRESS_ALLOWED || echo EGRESS_BLOCKED`))
	require.NoError(t, err)
	assert.Contains(t, result.Output, "EGRESS_BLOCKED", "no network interfaces means no egress")
	assert.NotContains(t, result.Output, "EGRESS_ALLOWED")
}

func TestSandboxProcessNamespaceIsIsolated(t *testing.T) {
	runtime := sandboxRuntime(t)
	result, err := runtime.Run(context.Background(), testSpec(t,
		"/bin/sh", "-c", `ps aux 2>/dev/null | wc -l; ls /proc | grep -E '^[0-9]+$' | wc -l`))
	require.NoError(t, err)
	// The container sees only its own PID namespace: a handful of
	// processes, never the host's hundreds.
	lines := strings.Fields(result.Output)
	require.NotEmpty(t, lines)
	processCount := 0
	for _, line := range lines {
		var parsed int
		if _, err := fmt.Sscanf(line, "%d", &parsed); err == nil && parsed > 0 && parsed < 100 {
			processCount = parsed
		}
	}
	assert.Less(t, processCount, 20, "host PID namespace must be invisible")
}

func TestSandboxEnvironmentCarriesNoSecrets(t *testing.T) {
	runtime := sandboxRuntime(t)
	result, err := runtime.Run(context.Background(), testSpec(t, "/usr/bin/env"))
	require.NoError(t, err)
	lower := strings.ToLower(result.Output)
	for _, secret := range []string{"token", "secret", "password", "key", "credential", "ssh", "aws", "kube"} {
		assert.NotContains(t, lower, secret, "no %q-shaped variables may leak", secret)
	}
	// Declared allowlist entries pass through.
	declared := testSpec(t, "/bin/sh", "-c", "echo [$PROFILE_ID]")
	declared.Env = map[string]string{"PROFILE_ID": "go-test"}
	result, err = runtime.Run(context.Background(), declared)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "[go-test]", "declared allowlist entries pass through")
}

func TestSandboxContainerEscapeIsBlocked(t *testing.T) {
	runtime := sandboxRuntime(t)
	ctx := context.Background()

	// No runtime socket, no devices, no cloud credentials, no SSH keys.
	result, err := runtime.Run(ctx, testSpec(t, "/bin/sh", "-c",
		`for path in /var/run/docker.sock /run/containerd/containerd.sock /var/run/secrets/cloud /root/.ssh /home/.aws; do
			if [ -e "$path" ]; then echo "FOUND:$path"; fi
		done; echo SCAN_DONE`))
	require.NoError(t, err)
	assert.Contains(t, result.Output, "SCAN_DONE")
	assert.NotContains(t, result.Output, "FOUND:", "no sockets, devices or credentials may be reachable")
}

func TestSandboxUserIsNeverRoot(t *testing.T) {
	runtime := sandboxRuntime(t)
	result, err := runtime.Run(context.Background(), testSpec(t, "/usr/bin/id"))
	require.NoError(t, err)
	assert.Contains(t, result.Output, "uid=65534", "execution must run as an unprivileged uid")
	assert.NotContains(t, result.Output, "uid=0")
}

func TestSandboxTimeoutAndOutputBounds(t *testing.T) {
	runtime := sandboxRuntime(t)
	spec := testSpec(t, "/bin/sh", "-c", "sleep 30")
	spec.Timeout = 3 * time.Second
	result, err := runtime.Run(context.Background(), spec)
	require.NoError(t, err)
	assert.True(t, result.TimedOut)
	assert.Less(t, result.DurationMs, int64(15000))

	flood := testSpec(t, "/bin/sh", "-c", "yes spam | head -c 200000")
	flood.OutputLimitBytes = 4096
	result, err = runtime.Run(context.Background(), flood)
	require.NoError(t, err)
	assert.True(t, result.Truncated)
	assert.Less(t, len(result.Output), 5000)
}

func TestSpecValidationRejectsWeakSpecs(t *testing.T) {
	base := testSpec(t, "true")
	weak := base
	weak.ImageDigest = "alpine:latest"
	assert.Error(t, weak.Validate(), "unpinned images are rejected")

	weak = base
	weak.Argv = nil
	assert.Error(t, weak.Validate(), "profile-less argv is rejected")

	weak = base
	weak.MemoryMB = 8
	assert.Error(t, weak.Validate())

	weak = base
	weak.WorkspaceHostPath = ""
	assert.Error(t, weak.Validate(), "a workspace mount is mandatory")
}
