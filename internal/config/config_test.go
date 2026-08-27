package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfigSecurityIsFailClosed(t *testing.T) {
	cfg := DefaultConfig()
	assert.Empty(t, cfg.AuthToken)
	assert.Empty(t, cfg.AllowedOrigins)
	assert.False(t, cfg.RemoteWrite)
	assert.False(t, cfg.Validation.AllowHostExecution)
	assert.Empty(t, cfg.Validation.CommandProfiles)
	testExecution, err := cfg.TestExecutionConfig()
	require.NoError(t, err)
	assert.Nil(t, testExecution.Profiles)
	assert.Empty(t, testExecution.PolicyVersion)
	assert.Empty(t, testExecution.PolicyDigest)
}

func TestLoadSecurityOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.yaml")
	content := []byte("allowed_origins: [\"https://one.example\", \"https://two.example\"]\nremote_write: true\n")
	require.NoError(t, os.WriteFile(path, content, 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.AuthToken)
	assert.Equal(t, []string{"https://one.example", "https://two.example"}, cfg.AllowedOrigins)
	assert.True(t, cfg.RemoteWrite)
}

func TestAllowedOriginErrorsNeverEchoConfiguredValues(t *testing.T) {
	tests := []struct {
		name    string
		origins []string
		canary  string
	}{
		{
			name:    "credential and query",
			origins: []string{"https://user:m0-config-origin-canary@example.test?token=also-secret"},
			canary:  "m0-config-origin-canary",
		},
		{
			name:    "duplicate",
			origins: []string{"https://m0-duplicate-origin-canary.example", "https://m0-duplicate-origin-canary.example"},
			canary:  "m0-duplicate-origin-canary",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.AllowedOrigins = test.origins
			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "allowed_origins[")
			assert.NotContains(t, err.Error(), test.canary)
		})
	}
}

func TestMalformedAllowedOriginsYAMLNeverEchoesScalar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.yaml")
	canary := "m0-origin-yaml-type-canary"
	require.NoError(t, os.WriteFile(path, []byte("allowed_origins: "+canary+"\n"), 0o600))
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configuration schema")
	assert.NotContains(t, err.Error(), canary)
}

func TestAllowedOriginEnvironmentParsingErrorNeverEchoesValue(t *testing.T) {
	originCanary := "m0-env-origin-duplicate-canary.example"
	t.Setenv("MAESTRO_ALLOWED_ORIGINS", "https://"+originCanary+",https://"+originCanary)
	cfg := DefaultConfig()
	err := ApplyEnvOverrides(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "origin list item 1 is duplicated")
	assert.NotContains(t, err.Error(), originCanary)
}

func TestDBPathRejectsSQLiteURIAndQueryWithoutEchoingValue(t *testing.T) {
	tests := []string{
		"file:m0-db-uri-canary.db?mode=memory&cache=shared",
		"FILE:m0-db-uri-uppercase-canary.db?mode=ro",
		"m0-db-query-canary.db?mode=ro",
		"m0-db-fragment-canary.db#fragment",
	}
	for _, path := range tests {
		cfg := DefaultConfig()
		cfg.DBPath = path
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "db_path must be a filesystem path")
		assert.NotContains(t, err.Error(), "m0-db-")
	}

	cfg := DefaultConfig()
	cfg.DBPath = ":memory:"
	require.NoError(t, cfg.Validate())
}

func TestLoadRejectsPlaintextAuthTokenWithoutEchoingSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.yaml")
	secret := "do-not-echo-this-secret"
	require.NoError(t, os.WriteFile(path, []byte("auth_token: "+secret+"\n"), 0o600))

	_, err := Load(path)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

func TestLoadInvalidRemoteWriteFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.yaml")
	require.NoError(t, os.WriteFile(path, []byte("remote_write: sometimes\n"), 0o600))

	cfg, err := Load(path)
	require.Error(t, err)
	assert.False(t, cfg.RemoteWrite)
}

func TestLoadRejectsUnknownDuplicateAndInvalidScalar(t *testing.T) {
	tests := map[string]string{
		"unknown field":   "unknown_setting: true\n",
		"duplicate field": "http_addr: 127.0.0.1:8080\nhttp_addr: 127.0.0.1:8081\n",
		"invalid integer": "max_connections: many\n",
		"second document": "remote_write: false\n---\nremote_write: true\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "maestro.yaml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			_, err := Load(path)
			require.Error(t, err)
		})
	}
}

func TestApplyEnvOverridesIsAtomicAndFailsClosed(t *testing.T) {
	t.Setenv("MAESTRO_REMOTE_WRITE", "sometimes")
	cfg := DefaultConfig()
	original := *cfg
	require.Error(t, ApplyEnvOverrides(cfg))
	assert.Equal(t, original, *cfg)
}

func TestDefaultListenAddressIsLoopback(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, "127.0.0.1:8080", cfg.HTTPAddr)
}

func TestLoadTrustedValidationProfilesThroughRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.yaml")
	content := `
http_addr: "127.0.0.1:9090"
remote_write: false
validation:
  allow_host_execution: true
  default_timeout_sec: 60
  max_output_bytes: 32768
  policy_version: "3.0.0-m0"
  policy_digest: "sha256:` + strings.Repeat("a", 64) + `"
  command_profiles:
    - id: "go-m0-test"
      version: "3.0.0"
      image_digest: "sha256:` + strings.Repeat("b", 64) + `"
      argv: ["go", "test", "./..."]
      working_directory: "."
      network: {mode: "none", allow_hosts: []}
      resources: {cpu_millis: 2000, memory_mb: 2048, disk_mb: 4096, pids: 256}
      output_limit_bytes: 32768
      timeout_seconds: 60
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	testExecution, err := cfg.TestExecutionConfig()
	require.NoError(t, err)
	require.NotNil(t, testExecution.Profiles)
	assert.True(t, testExecution.AllowHostExecution)
	assert.Equal(t, "3.0.0-m0", testExecution.PolicyVersion)
	assert.Equal(t, 60, int(testExecution.DefaultTimeout.Seconds()))
	assert.Equal(t, 32768, testExecution.MaxOutputBytes)

	reports, err := cfg.ValidationProfileReports()
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "go-m0-test@3.0.0", reports[0].Ref)
	_, err = testExecution.Profiles.Resolve("go-m0-test", "3.0.0", reports[0].Digest)
	require.NoError(t, err)
}

func TestValidationHostExecutionSecurityInvariants(t *testing.T) {
	valid := DefaultConfig()
	valid.Validation.AllowHostExecution = true
	valid.Validation.PolicyVersion = "3.0.0-m0"
	valid.Validation.PolicyDigest = "sha256:" + strings.Repeat("a", 64)
	valid.Validation.CommandProfiles = []ValidationCommandProfile{validTestProfile()}
	require.NoError(t, valid.Validate())

	tests := map[string]func(*Config){
		"remote writes":   func(cfg *Config) { cfg.RemoteWrite = true },
		"wildcard listen": func(cfg *Config) { cfg.HTTPAddr = "0.0.0.0:8080" },
		"hostname listen": func(cfg *Config) { cfg.HTTPAddr = "localhost:8080" },
		"empty profiles":  func(cfg *Config) { cfg.Validation.CommandProfiles = nil },
		"missing policy":  func(cfg *Config) { cfg.Validation.PolicyDigest = "" },
		"shell profile":   func(cfg *Config) { cfg.Validation.CommandProfiles[0].Argv = []string{"sh", "-c", "go test ./..."} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := *valid
			cfg.Validation = valid.Validation
			cfg.Validation.CommandProfiles = append([]ValidationCommandProfile(nil), valid.Validation.CommandProfiles...)
			mutate(&cfg)
			require.Error(t, cfg.Validate())
		})
	}
}

func TestEnvCannotTurnRemoteWriteOnForHostExecutor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Validation.AllowHostExecution = true
	cfg.Validation.PolicyVersion = "3.0.0-m0"
	cfg.Validation.PolicyDigest = "sha256:" + strings.Repeat("a", 64)
	cfg.Validation.CommandProfiles = []ValidationCommandProfile{validTestProfile()}
	require.NoError(t, cfg.Validate())
	t.Setenv("MAESTRO_REMOTE_WRITE", "true")
	require.Error(t, ApplyEnvOverrides(cfg))
	assert.False(t, cfg.RemoteWrite)
}

func validTestProfile() ValidationCommandProfile {
	return ValidationCommandProfile{
		ID:               "go-m0-test",
		Version:          "3.0.0",
		ImageDigest:      "sha256:" + strings.Repeat("b", 64),
		Argv:             []string{"go", "test", "./..."},
		WorkingDirectory: ".",
		Network:          ValidationCommandProfileNetwork{Mode: "none", AllowHosts: []string{}},
		Resources: ValidationCommandProfileResources{
			CPUMillis: 2000,
			MemoryMB:  2048,
			DiskMB:    4096,
			PIDs:      256,
		},
		OutputLimitBytes: 32768,
		TimeoutSeconds:   60,
	}
}
