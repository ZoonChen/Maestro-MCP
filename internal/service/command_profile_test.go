package service

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

func TestCommandProfileRegistryRequiresExactDigest(t *testing.T) {
	profile := testCommandProfile(t, "success")
	digest, err := profile.Digest()
	require.NoError(t, err)
	registry, err := NewCommandProfileRegistry([]CommandProfile{profile})
	require.NoError(t, err)

	resolved, err := registry.Resolve(profile.ID, profile.Version, digest)
	require.NoError(t, err)
	assert.Equal(t, profile.Argv, resolved.Argv)
	_, err = registry.Resolve(profile.ID, profile.Version, "sha256:"+strings.Repeat("0", 64))
	require.Error(t, err)
	_, err = registry.Resolve("unknown", profile.Version, digest)
	require.Error(t, err)
}

func TestCommandProfileRejectsShellAndSecretEnvironment(t *testing.T) {
	profile := testCommandProfile(t, "success")
	profile.Argv = []string{"sh", "-c", "echo compromised"}
	_, err := NewCommandProfileRegistry([]CommandProfile{profile})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell interpreters are forbidden")

	profile = testCommandProfile(t, "success")
	profile.Environment["API_TOKEN"] = "secret"
	_, err = NewCommandProfileRegistry([]CommandProfile{profile})
	require.Error(t, err)
}

func TestCommandProfileValidationRejectsEveryUntrustedCapability(t *testing.T) {
	base := testCommandProfile(t, "success")
	mutations := []struct {
		name   string
		mutate func(*CommandProfile)
	}{
		{name: "bad id", mutate: func(p *CommandProfile) { p.ID = "X" }},
		{name: "bad version", mutate: func(p *CommandProfile) { p.Version = "latest" }},
		{name: "mutable image", mutate: func(p *CommandProfile) { p.ImageDigest = "latest" }},
		{name: "empty argv", mutate: func(p *CommandProfile) { p.Argv = nil }},
		{name: "too many argv", mutate: func(p *CommandProfile) { p.Argv = make([]string, 33); p.Argv[0] = "go" }},
		{name: "empty executable", mutate: func(p *CommandProfile) { p.Argv[0] = "" }},
		{name: "nul arg", mutate: func(p *CommandProfile) { p.Argv = []string{"go", "bad\x00arg"} }},
		{name: "long arg", mutate: func(p *CommandProfile) { p.Argv = []string{"go", strings.Repeat("x", 1025)} }},
		{name: "unsafe workdir", mutate: func(p *CommandProfile) { p.WorkingDirectory = "../outside" }},
		{name: "network", mutate: func(p *CommandProfile) { p.Network.Mode = "egress" }},
		{name: "host allowlist", mutate: func(p *CommandProfile) { p.Network.AllowHosts = []string{"example.com"} }},
		{name: "cpu bounds", mutate: func(p *CommandProfile) { p.Resources.CPUMillis = 0 }},
		{name: "memory bounds", mutate: func(p *CommandProfile) { p.Resources.MemoryMB = 64 }},
		{name: "disk bounds", mutate: func(p *CommandProfile) { p.Resources.DiskMB = 64 }},
		{name: "pid bounds", mutate: func(p *CommandProfile) { p.Resources.PIDs = 1 }},
		{name: "output bounds", mutate: func(p *CommandProfile) { p.OutputLimitBytes = 1 }},
		{name: "timeout bounds", mutate: func(p *CommandProfile) { p.TimeoutSeconds = 0 }},
		{name: "bad env name", mutate: func(p *CommandProfile) { p.Environment["lower"] = "x" }},
		{name: "secret env", mutate: func(p *CommandProfile) { p.Environment["PASSWORD"] = "x" }},
		{name: "nul env", mutate: func(p *CommandProfile) { p.Environment["SAFE"] = "bad\x00value" }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			profile := base
			profile.Argv = append([]string(nil), base.Argv...)
			profile.Environment = cloneStringMap(base.Environment)
			tt.mutate(&profile)
			_, err := NewCommandProfileRegistry([]CommandProfile{profile})
			require.Error(t, err)
		})
	}
	_, err := NewCommandProfileRegistry([]CommandProfile{base, base})
	require.Error(t, err)
	var registry *CommandProfileRegistry
	_, err = registry.Resolve(base.ID, base.Version, "")
	require.Error(t, err)
}

func TestExecuteCommandProfileBoundedOutputAndTimeout(t *testing.T) {
	worktree := t.TempDir()

	outputProfile := testCommandProfile(t, "output")
	result, err := executeCommandProfile(context.Background(), outputProfile, worktree, TestExecutionConfig{
		AllowHostExecution: true,
		MaxOutputBytes:     1024,
	})
	require.Error(t, err)
	assert.True(t, result.Truncated)
	assert.LessOrEqual(t, len(result.Output), 1024)

	timeoutProfile := testCommandProfile(t, "sleep")
	result, err = executeCommandProfile(context.Background(), timeoutProfile, worktree, TestExecutionConfig{
		AllowHostExecution: true,
		DefaultTimeout:     50 * time.Millisecond,
	})
	require.Error(t, err)
	assert.True(t, result.TimedOut)
}

func TestExecuteCommandProfileDisabledByDefault(t *testing.T) {
	profile := testCommandProfile(t, "success")
	_, err := executeCommandProfile(context.Background(), profile, t.TempDir(), TestExecutionConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disabled")
}

func TestExecuteCommandProfileRejectsUnsafeRuntimePaths(t *testing.T) {
	profile := testCommandProfile(t, "success")
	profile.WorkingDirectory = "missing"
	_, err := executeCommandProfile(context.Background(), profile, t.TempDir(), TestExecutionConfig{AllowHostExecution: true})
	require.Error(t, err)

	profile = testCommandProfile(t, "success")
	profile.Argv[0] = filepath.Join(t.TempDir(), "missing")
	_, err = executeCommandProfile(context.Background(), profile, t.TempDir(), TestExecutionConfig{AllowHostExecution: true})
	require.Error(t, err)

	_, err = resolveApprovedExecutable("relative/tool", safeExecutableSearchPath())
	require.Error(t, err)
	_, err = resolveApprovedExecutable("definitely-not-a-maestro-executable", safeExecutableSearchPath())
	require.Error(t, err)

	nonExecutable := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(nonExecutable, []byte("binary"), 0o600))
	_, err = validateExecutable(nonExecutable)
	require.Error(t, err)
	symlink := filepath.Join(t.TempDir(), "tool-link")
	require.NoError(t, os.Symlink(os.Args[0], symlink))
	_, err = validateExecutable(symlink)
	require.Error(t, err)
}

func TestCommandProfileHelperProcess(t *testing.T) {
	if os.Getenv("MAESTRO_HELPER_PROCESS") != "true" {
		return
	}
	mode := os.Getenv("MAESTRO_HELPER_MODE")
	switch mode {
	case "output":
		fmt.Print(strings.Repeat("x", 8192))
	case "sleep":
		time.Sleep(5 * time.Second)
	case "sleep-coverage":
		time.Sleep(3500 * time.Millisecond)
		if err := os.WriteFile("coverage.out", []byte("mode: set\nmain.go:1.1,2.1 1 1\n"), 0o600); err != nil {
			os.Exit(9)
		}
	case "coverage":
		if err := os.WriteFile("coverage.out", []byte("mode: set\nmain.go:1.1,2.1 1 1\n"), 0o600); err != nil {
			os.Exit(9)
		}
	case "secret-coverage":
		canaries := testDiagnosticSecretCanaries()
		fmt.Printf("AWS_ACCESS_KEY_ID=%s\n", canaries.awsAccessKey)
		fmt.Printf("GITHUB_TOKEN=%s\n", canaries.github)
		fmt.Printf("gitlab=%s\n", canaries.gitlab)
		fmt.Printf("jwt=%s\n", canaries.jwt)
		fmt.Printf("clone %s\n", canaries.credentialURL)
		fmt.Printf("%s\n%s\n%s\n", canaries.pemBegin, canaries.pemBody, canaries.pemEnd)
		if err := os.WriteFile("coverage.out", []byte("mode: set\nmain.go:1.1,2.1 1 1\n"), 0o600); err != nil {
			os.Exit(9)
		}
	case "failure":
		os.Exit(7)
	}
	os.Exit(0)
}

type diagnosticSecretCanaries struct {
	awsAccessKey  string
	github        string
	gitlab        string
	jwt           string
	basic         string
	credentialURL string
	pemBegin      string
	pemBody       string
	pemEnd        string
}

func testDiagnosticSecretCanaries() diagnosticSecretCanaries {
	return diagnosticSecretCanaries{
		awsAccessKey:  "AKIA" + strings.Repeat("Q", 16),
		github:        "ghp_" + strings.Repeat("g", 36),
		gitlab:        "glpat-" + strings.Repeat("L", 24),
		jwt:           "eyJ" + strings.Repeat("a", 12) + "." + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12),
		basic:         strings.Repeat("Q", 24) + "==",
		credentialURL: "https://build-user:" + "url-password" + "@example.invalid/repository.git",
		pemBegin:      "-----BEGIN " + "PRIVATE KEY-----",
		pemBody:       strings.Repeat("M", 64),
		pemEnd:        "-----END " + "PRIVATE KEY-----",
	}
}

func (c diagnosticSecretCanaries) values() []string {
	return []string{
		c.awsAccessKey, c.github, c.gitlab, c.jwt, c.basic,
		"build-user:url-password", c.pemBody,
	}
}

func testCommandProfile(t *testing.T, mode string) CommandProfile {
	t.Helper()
	return CommandProfile{
		ID:               "go-unit",
		Version:          "3.0.0",
		ImageDigest:      "sha256:" + strings.Repeat("b", 64),
		Argv:             []string{os.Args[0], "-test.run=TestCommandProfileHelperProcess"},
		WorkingDirectory: ".",
		Network:          CommandProfileNetwork{Mode: "none", AllowHosts: []string{}},
		Resources: CommandProfileResources{
			CPUMillis: 1000,
			MemoryMB:  512,
			DiskMB:    1024,
			PIDs:      64,
		},
		OutputLimitBytes: 1024,
		TimeoutSeconds:   10,
		Environment: map[string]string{
			"MAESTRO_HELPER_PROCESS": "true",
			"MAESTRO_HELPER_MODE":    mode,
		},
	}
}
