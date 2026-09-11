package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runDocker executes one docker CLI invocation and returns its combined
// output. Arguments are server-derived (execution id, validated hosts,
// pinned digests) — never request data.
func runDocker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // server-derived arguments
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runDockerQuiet(ctx context.Context, args ...string) error {
	_, err := runDocker(ctx, args...)
	return err
}

// Per-execution egress for allowlist profiles (P5a). The job container has
// no external route: it lands on an internal docker network whose only
// other member is a filtering proxy. The proxy is dual-homed (the internal
// network plus the default bridge) and forwards CONNECT requests to the
// profile's declared hosts, denying everything else. Every resource is
// named after the execution id and torn down after the run.
//
// The proxy image is digest-pinned like the profile images themselves:
// ubuntu/squid:6.6-24.04_edge. Bump the pin deliberately (intranet
// upgrades follow the same discipline as the GitLab stack).

// EgressProxyImage is the digest-pinned filtering proxy.
const EgressProxyImage = "ubuntu/squid@sha256:8a3baed477e2c282ab8aa5edad442f69873246964f225c5c2ae8364b6610963c"

// egressAlias is the in-network DNS name the job's proxy environment
// points at; docker's embedded DNS resolves it on the internal network.
const egressAlias = "egress"

// egressResources couples the per-execution proxy handles with its
// teardown.
type egressResources struct {
	network      string
	proxyName    string
	confDir      string
	proxyEnvHook func(map[string]string)
}

// provisionEgress stands up one internal network plus one filtering
// proxy for this execution. hosts is the validated allowlist.
func (r *Runtime) provisionEgress(ctx context.Context, executionID string, hosts []string) (*egressResources, error) {
	network := "maestro-egress-" + executionID
	proxyName := "maestro-egress-proxy-" + executionID

	if out, err := runDocker(ctx, "network", "create", "--internal", network); err != nil {
		return nil, fmt.Errorf("sandbox: egress network create failed: %s: %w", strings.TrimSpace(out), err)
	}

	confDir, err := os.MkdirTemp("", "maestro-egress-")
	if err != nil {
		_ = runDockerQuiet(ctx, "network", "rm", network)
		return nil, fmt.Errorf("sandbox: egress conf dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "squid.conf"), []byte(squidConf(hosts)), 0o600); err != nil {
		_ = os.RemoveAll(confDir)
		_ = runDockerQuiet(ctx, "network", "rm", network)
		return nil, fmt.Errorf("sandbox: egress conf write: %w", err)
	}

	if out, err := runDocker(ctx, "run", "-d", "--rm",
		"--name", proxyName,
		"--network", network,
		"--network-alias", egressAlias,
		"-v", filepath.Join(confDir, "squid.conf")+":/etc/squid/squid.conf:ro",
		EgressProxyImage); err != nil {
		_ = os.RemoveAll(confDir)
		_ = runDockerQuiet(ctx, "network", "rm", network)
		return nil, fmt.Errorf("sandbox: egress proxy start failed: %s: %w", strings.TrimSpace(out), err)
	}
	// The internet foot: attach the proxy to the default bridge so the
	// job network itself stays internal-only.
	if out, err := runDocker(ctx, "network", "connect", "bridge", proxyName); err != nil {
		r.teardownEgress(context.WithoutCancel(ctx), &egressResources{network: network, proxyName: proxyName, confDir: confDir})
		return nil, fmt.Errorf("sandbox: egress proxy bridge attach failed: %s: %w", strings.TrimSpace(out), err)
	}
	// Wait for the proxy listener: squid starts in two phases (swap dir
	// bootstrap, then serve); a job started in between sees connection
	// refused. Bash's /dev/tcp probe needs nothing beyond the image.
	ready := false
	for i := 0; i < 40 && !ready; i++ {
		probeErr := runDockerQuiet(ctx, "exec", proxyName, "bash",
			"-c", "(echo > /dev/tcp/127.0.0.1/3128) 2>/dev/null")
		ready = probeErr == nil
		if !ready {
			select {
			case <-ctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	if !ready {
		r.teardownEgress(context.WithoutCancel(ctx), &egressResources{network: network, proxyName: proxyName, confDir: confDir})
		return nil, fmt.Errorf("sandbox: egress proxy did not become ready")
	}

	return &egressResources{
		network:   network,
		proxyName: proxyName,
		confDir:   confDir,
		proxyEnvHook: func(env map[string]string) {
			proxy := "http://" + egressAlias + ":3128"
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
				env[key] = proxy
			}
			env["NO_PROXY"] = "localhost,127.0.0.1"
			env["no_proxy"] = "localhost,127.0.0.1"
		},
	}, nil
}

// teardownEgress removes the proxy container, the internal network and the
// generated conf. Failures are swallowed on purpose: teardown must never
// mask the run outcome, and leftovers are visible in docker for debugging.
func (r *Runtime) teardownEgress(ctx context.Context, e *egressResources) {
	if e == nil {
		return
	}
	_ = runDockerQuiet(ctx, "rm", "-f", e.proxyName)
	_ = runDockerQuiet(ctx, "network", "rm", e.network)
	_ = os.RemoveAll(e.confDir)
}

// squidConf renders the deny-by-default filter: only http(s) to the
// declared hosts on ports 80/443 is forwarded; caching is off (the
// workspace is the only writable surface). All ACLs are defined here —
// the distro defaults are deliberately not inherited.
func squidConf(hosts []string) string {
	var b strings.Builder
	b.WriteString("http_port 3128\n")
	b.WriteString("acl allowed dstdomain")
	for _, host := range hosts {
		b.WriteString(" ")
		b.WriteString(host)
	}
	b.WriteString("\nacl Safe_ports port 443\n")
	b.WriteString("acl Safe_ports port 80\n")
	b.WriteString("acl CONNECT method CONNECT\n")
	b.WriteString("http_access deny !Safe_ports\n")
	b.WriteString("http_access allow allowed\n")
	b.WriteString("http_access deny all\n")
	b.WriteString("cache deny all\n")
	return b.String()
}
