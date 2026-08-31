package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfigKeepsSQLiteBaseline(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, DatabaseDriverSQLite, cfg.DatabaseDriver())
	assert.False(t, cfg.PostgresEnabled())

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, DatabaseDriverSQLite, cfg.DatabaseDriver())
}

func TestDatabaseSectionDriverContract(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		driver  string
		wantErr string
	}{
		{
			name:    "explicit sqlite driver",
			yaml:    "database:\n  driver: sqlite\n",
			driver:  DatabaseDriverSQLite,
			wantErr: "",
		},
		{
			name:    "postgres requires a dsn source",
			yaml:    "database:\n  driver: postgres\n",
			wantErr: "dsn_secret_ref",
		},
		{
			name:    "postgres with secret ref",
			yaml:    "database:\n  driver: postgres\n  dsn_secret_ref: docker-secret://maestro-postgres-dsn\n  max_open_connections: 20\n",
			driver:  DatabaseDriverPostgres,
			wantErr: "",
		},
		{
			name:    "dsn presence implies postgres",
			yaml:    "database:\n  dsn_secret_ref: docker-secret://maestro-postgres-dsn\n",
			driver:  DatabaseDriverPostgres,
			wantErr: "",
		},
		{
			name:    "unknown driver fails closed",
			yaml:    "database:\n  driver: mysql\n",
			wantErr: "sqlite or postgres",
		},
		{
			name:    "pool size below the machine schema minimum",
			yaml:    "database:\n  driver: postgres\n  dsn_secret_ref: docker-secret://maestro-postgres-dsn\n  max_open_connections: 1\n",
			wantErr: "between 2 and 100",
		},
		{
			name:    "sqlite path rejects uri forms",
			yaml:    "database:\n  driver: sqlite\n  sqlite_path: \"file:data/x?mode=ro\"\n",
			wantErr: "sqlite_path",
		},
		{
			name:    "plaintext dsn is not a file field",
			yaml:    "database:\n  driver: postgres\n  dsn: postgres://localhost/maestro\n",
			wantErr: "configuration schema",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "maestro.yaml")
			require.NoError(t, os.WriteFile(path, []byte(test.yaml), 0o600))
			cfg, err := Load(path)
			if err != nil {
				// Decode-level rejections (unknown fields) still fail here.
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			// Env-absent full validation, as loadRuntimeConfig runs it.
			err = cfg.Validate()
			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.driver, cfg.DatabaseDriver())
		})
	}
}

func TestDatabaseEnvironmentOverrides(t *testing.T) {
	cfg := DefaultConfig()
	t.Setenv("MAESTRO_DATABASE_DSN", "postgres://maestro:secret@localhost:5432/maestro")
	require.NoError(t, ApplyEnvOverrides(cfg))
	assert.Equal(t, DatabaseDriverPostgres, cfg.DatabaseDriver())
	assert.True(t, cfg.PostgresEnabled())

	t.Setenv("MAESTRO_DB_DRIVER", "bogus")
	err := ApplyEnvOverrides(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sqlite or postgres")
}

func TestOIDCSectionContract(t *testing.T) {
	valid := "oidc:\n  issuer: https://idp.internal.example.com\n  client_id: maestro-control-plane\n  client_secret_ref: docker-secret://maestro-oidc-client-secret\n  audience: maestro\n"
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "valid section", yaml: valid, wantErr: ""},
		{name: "missing section is allowed", yaml: "log_level: info\n", wantErr: ""},
		{name: "plain http issuer", yaml: "oidc:\n  issuer: http://idp.internal.example.com\n  client_id: c\n  client_secret_ref: r\n  audience: maestro\n", wantErr: "HTTPS"},
		{name: "missing client id", yaml: "oidc:\n  issuer: https://idp.internal.example.com\n  client_secret_ref: r\n  audience: maestro\n", wantErr: "client_id"},
		{name: "missing secret ref", yaml: "oidc:\n  issuer: https://idp.internal.example.com\n  client_id: c\n  audience: maestro\n", wantErr: "client_secret_ref"},
		{name: "missing audience", yaml: "oidc:\n  issuer: https://idp.internal.example.com\n  client_id: c\n  client_secret_ref: r\n", wantErr: "audience"},
		{name: "non standard token ttl", yaml: "oidc:\n  issuer: https://idp.internal.example.com\n  client_id: c\n  client_secret_ref: r\n  audience: maestro\n  access_token_ttl_seconds: 3600\n", wantErr: "900"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "maestro.yaml")
			require.NoError(t, os.WriteFile(path, []byte(test.yaml), 0o600))
			cfg, err := Load(path)
			require.NoError(t, err, "Load is decode-only; the full gate is Validate")
			err = cfg.Validate()
			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestOIDCEnvironmentMaterializesSectionFailClosed(t *testing.T) {
	cfg := DefaultConfig()
	t.Setenv("MAESTRO_OIDC_ISSUER", "https://idp.internal.example.com")
	err := ApplyEnvOverrides(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client_id")
	assert.Nil(t, cfg.OIDC, "invalid override must not mutate the original config")
}

func TestOIDCEnvironmentBuildsCompleteSection(t *testing.T) {
	cfg := DefaultConfig()
	t.Setenv("MAESTRO_OIDC_ISSUER", "https://idp.internal.example.com")
	t.Setenv("MAESTRO_OIDC_CLIENT_ID", "maestro-control-plane")
	t.Setenv("MAESTRO_OIDC_CLIENT_SECRET_REF", "docker-secret://maestro-oidc-client-secret")
	t.Setenv("MAESTRO_OIDC_AUDIENCE", "maestro")
	t.Setenv("MAESTRO_OIDC_CLIENT_SECRET", "env-resolved-secret")
	require.NoError(t, ApplyEnvOverrides(cfg))
	require.NotNil(t, cfg.OIDC)
	assert.Equal(t, "maestro", cfg.OIDC.Audience)
	assert.Equal(t, "env-resolved-secret", cfg.OIDC.ClientSecret)
}

func TestRunnerSectionFrozenWindows(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "defaults match the frozen windows", yaml: "log_level: info\n", wantErr: ""},
		{name: "heartbeat is fixed", yaml: "runner:\n  heartbeat_seconds: 30\n", wantErr: "15"},
		{name: "suspect is fixed", yaml: "runner:\n  suspect_seconds: 60\n", wantErr: "45"},
		{name: "offline is fixed", yaml: "runner:\n  offline_seconds: 120\n", wantErr: "90"},
		{name: "enrollment ttl is fixed", yaml: "runner:\n  enrollment_ttl_seconds: 300\n", wantErr: "600"},
		{name: "lease ttl below minimum", yaml: "runner:\n  lease_ttl_seconds: 10\n", wantErr: "between 30 and 600"},
		{name: "lease ttl above maximum", yaml: "runner:\n  lease_ttl_seconds: 900\n", wantErr: "between 30 and 600"},
		{name: "long poll above maximum", yaml: "runner:\n  long_poll_seconds: 60\n", wantErr: "between 1 and 30"},
		{name: "operator tuned lease ttl", yaml: "runner:\n  lease_ttl_seconds: 120\n  long_poll_seconds: 20\n", wantErr: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "maestro.yaml")
			require.NoError(t, os.WriteFile(path, []byte(test.yaml), 0o600))
			cfg, err := Load(path)
			require.NoError(t, err, "Load is decode-only; the full gate is Validate")
			err = cfg.Validate()
			if test.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestHostExecutionRequiresSQLiteDriver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maestro.yaml")
	yaml := "database:\n  driver: postgres\n  dsn_secret_ref: docker-secret://maestro-postgres-dsn\nvalidation:\n  allow_host_execution: true\n  policy_version: m1-contract-test\n  policy_digest: \"sha256:0000000000000000000000000000000000000000000000000000000000000000\"\n  command_profiles:\n    - id: go-m1-contract-test\n      version: \"1.0.0\"\n      image_digest: \"sha256:1111111111111111111111111111111111111111111111111111111111111111\"\n      argv: [\"go\", \"test\", \"./...\"]\n      working_directory: .\n      network: {mode: none, allow_hosts: []}\n      resources: {cpu_millis: 2000, memory_mb: 2048, disk_mb: 4096, pids: 256}\n      output_limit_bytes: 32768\n      timeout_seconds: 60\n"
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
	cfg, err := Load(path)
	require.NoError(t, err, "Load is decode-only")
	err = cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires the sqlite driver")
}
