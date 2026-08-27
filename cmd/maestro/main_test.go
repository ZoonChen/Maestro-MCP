package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionAndUsageExitCodes(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(context.Background(), []string{"version", "--json"}, streams{
		out: &stdout,
		err: &stderr,
	})
	require.Equal(t, exitOK, code)
	var info versionInfo
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &info))
	assert.NotEmpty(t, info.Version)
	assert.NotEmpty(t, info.GoVersion)
	assert.Greater(t, info.SchemaVersion, 0)
	assert.Empty(t, stderr.String())

	for _, command := range [][]string{
		{"server", "--help"},
		{"runner", "--help"},
		{"migrate", "--help"},
		{"doctor", "--help"},
		{"version", "--help"},
	} {
		stdout.Reset()
		stderr.Reset()
		assert.Equal(t, exitOK, execute(context.Background(), command, streams{out: &stdout, err: &stderr}), command)
	}

	stdout.Reset()
	stderr.Reset()
	code = execute(context.Background(), []string{"does-not-exist"}, streams{
		out: &stdout,
		err: &stderr,
	})
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "code=USAGE_ERROR")
	assert.Contains(t, stderr.String(), "correlation_id=")
}

func TestInvalidOriginAndDatabaseConfigurationNeverEchoesValues(t *testing.T) {
	t.Run("origin from config file", func(t *testing.T) {
		t.Setenv("MAESTRO_ALLOWED_ORIGINS", "")
		canary := "m0-config-origin-stderr-canary"
		configPath := filepath.Join(t.TempDir(), "maestro.yaml")
		require.NoError(t, os.WriteFile(configPath, []byte(
			`allowed_origins: ["https://user:`+canary+`@example.test?token=also-secret"]`+"\n",
		), 0o600))
		assertConfigFailureDoesNotEcho(t, []string{"doctor", "--config", configPath}, canary)
	})

	t.Run("origin from environment", func(t *testing.T) {
		canary := "m0-env-origin-stderr-canary.example"
		t.Setenv("MAESTRO_ALLOWED_ORIGINS", "https://"+canary+",https://"+canary)
		assertConfigFailureDoesNotEcho(t, []string{"doctor"}, canary)
	})

	t.Run("database URI from config file", func(t *testing.T) {
		canary := "m0-config-db-stderr-canary"
		configPath := filepath.Join(t.TempDir(), "maestro.yaml")
		require.NoError(t, os.WriteFile(configPath, []byte(
			`db_path: "file:`+canary+`.db?mode=memory&cache=shared"`+"\n",
		), 0o600))
		assertConfigFailureDoesNotEcho(t, []string{"doctor", "--config", configPath}, canary)
	})

	t.Run("database URI from command line", func(t *testing.T) {
		canary := "m0-cli-db-stderr-canary"
		assertConfigFailureDoesNotEcho(t, []string{"doctor", "--db", "file:" + canary + ".db?mode=ro"}, canary)
	})
}

func assertConfigFailureDoesNotEcho(t *testing.T, command []string, canary string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(context.Background(), command, streams{out: &stdout, err: &stderr})
	require.Equal(t, exitUsage, code, stderr.String())
	assert.Contains(t, stderr.String(), "code=CONFIG_INVALID")
	assert.Contains(t, stderr.String(), "correlation_id=")
	assert.NotContains(t, stderr.String(), canary)
	assert.NotContains(t, stdout.String(), canary)
}

func TestMigrateUpAndDoctor(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "maestro.db")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(context.Background(), []string{"migrate", "up", "--db", dbPath}, streams{
		out: &stdout,
		err: &stderr,
	})
	require.Equal(t, exitOK, code, stderr.String())
	assert.Contains(t, stdout.String(), fmt.Sprintf(
		"migration plan current_schema=0 target_schema=%d",
		store.CurrentSchemaVersion(),
	))
	assert.Contains(t, stdout.String(), "migration complete")

	stdout.Reset()
	stderr.Reset()
	code = execute(context.Background(), []string{"doctor", "--db", dbPath, "--json"}, streams{
		out: &stdout,
		err: &stderr,
	})
	require.Equal(t, exitOK, code, stderr.String())
	var report doctorReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.Equal(t, "ok", report.Status)
	assert.Equal(t, "ok", report.Database)
	assert.False(t, report.RemoteWrite)
}

func TestServerAndRunnerRejectNonCurrentExistingDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "maestro.db")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	require.Equal(t, exitOK, execute(context.Background(), []string{"migrate", "up", "--db", dbPath}, streams{
		out: &stdout,
		err: &stderr,
	}), stderr.String())

	database, err := store.NewSQLiteDB(dbPath)
	require.NoError(t, err)
	_, err = database.DB().Exec(`PRAGMA user_version = 4`)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	for _, command := range [][]string{
		{"server", "--db", dbPath, "--http", "127.0.0.1:0"},
		{"runner", "--db", dbPath, "--runner-id", "version-check"},
	} {
		stdout.Reset()
		stderr.Reset()
		code := execute(context.Background(), command, streams{
			in:  strings.NewReader(""),
			out: &stdout,
			err: &stderr,
		})
		require.Equal(t, exitIncompatible, code, "%v: %s", command, stderr.String())
		assert.Contains(t, stderr.String(), "code=VERSION_INCOMPATIBLE")
		assert.Contains(t, stderr.String(), "run maestro migrate up")
		assert.Empty(t, stdout.String(), "incompatible runtime must not start a transport")

		database, err = store.NewSQLiteDB(dbPath)
		require.NoError(t, err)
		var version int
		require.NoError(t, database.DB().QueryRow("PRAGMA user_version").Scan(&version))
		assert.Equal(t, 4, version, "runtime command must not apply pending migration")
		require.NoError(t, database.Close())
	}

	stdout.Reset()
	stderr.Reset()
	require.Equal(t, exitOK, execute(context.Background(), []string{"migrate", "up", "--db", dbPath}, streams{
		out: &stdout,
		err: &stderr,
	}), stderr.String())
	assert.Contains(t, stdout.String(), "migration complete schema_version=")
}

func TestCommandsRejectCorruptCurrentSchemaWithoutRepair(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "corrupt-current.db")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	require.Equal(t, exitOK, execute(context.Background(), []string{"migrate", "up", "--db", dbPath}, streams{
		out: &stdout,
		err: &stderr,
	}), stderr.String())

	database, err := store.NewSQLiteDB(dbPath)
	require.NoError(t, err)
	_, err = database.DB().Exec(`INSERT INTO projects(id,name,workspace_path)
		VALUES('integrity-marker','preserve-me','/integrity-marker')`)
	require.NoError(t, err)
	_, err = database.DB().Exec(`DROP TRIGGER trg_validation_runs_append_only_update`)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	for _, command := range [][]string{
		{"server", "--db", dbPath, "--http", "127.0.0.1:0"},
		{"runner", "--db", dbPath, "--runner-id", "integrity-check"},
		{"doctor", "--db", dbPath, "--json"},
	} {
		stdout.Reset()
		stderr.Reset()
		code := execute(context.Background(), command, streams{
			in:  strings.NewReader(""),
			out: &stdout,
			err: &stderr,
		})
		require.Equal(t, exitIncompatible, code, "%v: %s", command, stderr.String())
		assert.Contains(t, stderr.String(), "code=SCHEMA_INTEGRITY_FAILED")
		assert.Contains(t, stderr.String(), "restore the database from a verified backup")
		assert.Empty(t, stdout.String(), "corrupt schema must not start or report healthy")
		assertCorruptSchemaPreserved(t, dbPath)
	}

	stdout.Reset()
	stderr.Reset()
	code := execute(context.Background(), []string{"migrate", "up", "--db", dbPath}, streams{
		out: &stdout,
		err: &stderr,
	})
	require.Equal(t, exitMigration, code, stderr.String())
	assert.Contains(t, stderr.String(), "code=MIGRATION_FAILED")
	assert.Contains(t, stderr.String(), "refusing automatic repair")
	assert.Contains(t, stderr.String(), "verified backup")
	assert.Contains(t, stdout.String(), fmt.Sprintf(
		"migration plan current_schema=%d target_schema=%d",
		store.CurrentSchemaVersion(),
		store.CurrentSchemaVersion(),
	))
	assert.NotContains(t, stdout.String(), "migration complete")
	assertCorruptSchemaPreserved(t, dbPath)
}

func assertCorruptSchemaPreserved(t *testing.T, dbPath string) {
	t.Helper()
	database, err := store.NewSQLiteDB(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	var triggerCount int
	require.NoError(t, database.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'trigger' AND name = 'trg_validation_runs_append_only_update'`).Scan(&triggerCount))
	assert.Zero(t, triggerCount)
	var marker string
	require.NoError(t, database.DB().QueryRow(`SELECT name FROM projects
		WHERE id = 'integrity-marker'`).Scan(&marker))
	assert.Equal(t, "preserve-me", marker)
}

func TestRunnerUsesRealStdioMCPWithoutStdoutLogs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "maestro.db")
	var migrationStdout bytes.Buffer
	var migrationStderr bytes.Buffer
	require.Equal(t, exitOK, execute(context.Background(), []string{"migrate", "up", "--db", dbPath}, streams{
		out: &migrationStdout, err: &migrationStderr,
	}), migrationStderr.String())
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"m0-stdio-smoke","version":"1.0.0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n") + "\n"
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(context.Background(), []string{"runner", "--db", dbPath, "--runner-id", "test-runner"}, streams{
		in:  strings.NewReader(input),
		out: &stdout,
		err: &stderr,
	})
	require.Equal(t, exitOK, code, stderr.String())

	responses := make(map[int]map[string]any)
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	for scanner.Scan() {
		var response map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &response), "stdout must contain MCP JSON only: %s", scanner.Text())
		id, ok := response["id"].(float64)
		if ok {
			responses[int(id)] = response
		}
	}
	require.NoError(t, scanner.Err())
	require.Contains(t, responses, 1, "initialize response missing")
	require.Contains(t, responses, 2, "tools/list response missing")
	result, ok := responses[2]["result"].(map[string]any)
	require.True(t, ok)
	tools, ok := result["tools"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, tools)
}

func TestRunnerCannotBootstrapEmptyDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "runner-empty.db")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(context.Background(), []string{"runner", "--db", dbPath, "--runner-id", "non-owner"}, streams{
		in: strings.NewReader(""), out: &stdout, err: &stderr,
	})
	require.Equal(t, exitIncompatible, code, stderr.String())
	assert.Contains(t, stderr.String(), "code=VERSION_INCOMPATIBLE")
	assert.Contains(t, stderr.String(), "run maestro migrate up")
	assert.Empty(t, stdout.String())

	database, err := store.NewSQLiteDB(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, database.Close()) }()
	var version, objects int
	require.NoError(t, database.DB().QueryRow("PRAGMA user_version").Scan(&version))
	require.Zero(t, version)
	require.NoError(t, database.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL`).Scan(&objects))
	require.Zero(t, objects, "non-owner Runner must not create schema objects")
}

func TestDoctorReportsOnlyValidationProfileIdentity(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "maestro.db")
	configPath := filepath.Join(directory, "maestro.yaml")
	configText := `
db_path: "` + dbPath + `"
http_addr: "127.0.0.1:8080"
remote_write: false
validation:
  allow_host_execution: true
  default_timeout_sec: 30
  max_output_bytes: 8192
  policy_version: "3.0.0-m0"
  policy_digest: "sha256:` + strings.Repeat("a", 64) + `"
  command_profiles:
    - id: "go-m0-test"
      version: "3.0.0"
      image_digest: "sha256:` + strings.Repeat("b", 64) + `"
      argv: ["go", "test", "./..."]
      working_directory: "."
      network: {mode: "none", allow_hosts: []}
      resources: {cpu_millis: 1000, memory_mb: 512, disk_mb: 1024, pids: 64}
      output_limit_bytes: 8192
      timeout_seconds: 30
      environment: {M0_MARKER: "non-secret-but-not-diagnostic"}
`
	require.NoError(t, os.WriteFile(configPath, []byte(configText), 0o600))

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(context.Background(), []string{"migrate", "up", "--config", configPath}, streams{out: &stdout, err: &stderr})
	require.Equal(t, exitOK, code, stderr.String())
	stdout.Reset()
	stderr.Reset()
	code = execute(context.Background(), []string{"doctor", "--config", configPath, "--json"}, streams{out: &stdout, err: &stderr})
	require.Equal(t, exitOK, code, stderr.String())

	var report doctorReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.True(t, report.HostExecution)
	assert.Equal(t, "3.0.0-m0", report.PolicyVersion)
	require.Len(t, report.ValidationProfiles, 1)
	assert.Equal(t, "go-m0-test@3.0.0", report.ValidationProfiles[0].Ref)
	assert.True(t, strings.HasPrefix(report.ValidationProfiles[0].Digest, "sha256:"))
	assert.NotContains(t, stdout.String(), "go test")
	assert.NotContains(t, stdout.String(), "M0_MARKER")
	assert.NotContains(t, stdout.String(), "non-secret-but-not-diagnostic")

	stdout.Reset()
	stderr.Reset()
	code = execute(context.Background(), []string{"runner", "--config", configPath, "--runner-id", "host-warning"}, streams{
		in: strings.NewReader(""), out: &stdout, err: &stderr,
	})
	require.Equal(t, exitOK, code, stderr.String())
	assert.Contains(t, stderr.String(), "HOST_EXECUTION_ENABLED")
	assert.Contains(t, stderr.String(), "SECURITY WARNING")
	assert.NotContains(t, stderr.String(), "go test")
	assert.NotContains(t, stderr.String(), "M0_MARKER")
	assert.Empty(t, stdout.String())
}
