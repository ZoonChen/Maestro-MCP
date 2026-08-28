package m0_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	_ "modernc.org/sqlite"
)

var (
	repositoryRoot string
	maestroBinary  string
)

func TestMain(m *testing.M) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "cannot resolve integration test location")
		os.Exit(1)
	}
	repositoryRoot = filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	if configuredBinary := os.Getenv("MAESTRO_BINARY"); configuredBinary != "" {
		resolvedBinary, err := filepath.Abs(configuredBinary)
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolve MAESTRO_BINARY: %v\n", err)
			os.Exit(1)
		}
		maestroBinary = resolvedBinary
		if info, statErr := os.Stat(maestroBinary); statErr != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			fmt.Fprintf(os.Stderr, "MAESTRO_BINARY is not an executable regular file: %s\n", maestroBinary)
			os.Exit(1)
		}
		os.Exit(m.Run())
	}

	buildDirectory, err := os.MkdirTemp("", "maestro-m0-binary-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create build directory: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(buildDirectory)

	maestroBinary = filepath.Join(buildDirectory, "maestro")
	build := exec.Command("go", "build", "-trimpath", "-o", maestroBinary, "./cmd/maestro")
	build.Dir = repositoryRoot
	buildOutput, err := build.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build real Maestro binary: %v\n%s", err, buildOutput)
		_ = os.RemoveAll(buildDirectory)
		os.Exit(1)
	}

	exitCode := m.Run()
	if err := os.RemoveAll(buildDirectory); err != nil {
		fmt.Fprintf(os.Stderr, "remove build directory: %v\n", err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

func TestCLIContractsAndSchemaMigration(t *testing.T) {
	t.Parallel()

	var version struct {
		Version            string `json:"version"`
		SchemaVersion      int    `json:"schema_version"`
		MinProtocolVersion string `json:"min_protocol_version"`
		MaxProtocolVersion string `json:"max_protocol_version"`
	}
	output := runBinary(t, "version", "--json")
	if err := json.Unmarshal(output, &version); err != nil {
		t.Fatalf("decode version JSON: %v\n%s", err, output)
	}
	if version.Version == "" || version.SchemaVersion < 2 {
		t.Fatalf("incomplete version contract: %+v", version)
	}
	if version.MinProtocolVersion == "" || version.MaxProtocolVersion == "" {
		t.Fatalf("missing MCP compatibility range: %+v", version)
	}

	databasePath := filepath.Join(t.TempDir(), "maestro.db")
	migrationOutput := runBinary(t, "migrate", "up", "--db", databasePath)
	if !bytes.Contains(migrationOutput, []byte(fmt.Sprintf(
		"migration plan current_schema=0 target_schema=%d",
		version.SchemaVersion,
	))) {
		t.Fatalf("migration plan was not printed before execution: %s", migrationOutput)
	}
	if !bytes.Contains(migrationOutput, []byte("migration complete schema_version=")) {
		t.Fatalf("unexpected migration output: %s", migrationOutput)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var physicalSchemaVersion int
	if err := database.QueryRow("PRAGMA user_version").Scan(&physicalSchemaVersion); err != nil {
		t.Fatalf("read physical schema version: %v", err)
	}
	if physicalSchemaVersion != version.SchemaVersion {
		t.Fatalf("binary/schema migration drift: version=%d physical=%d", version.SchemaVersion, physicalSchemaVersion)
	}

	var doctor struct {
		Status        string `json:"status"`
		Database      string `json:"database"`
		SchemaVersion int    `json:"schema_version"`
		RemoteWrite   bool   `json:"remote_write"`
	}
	doctorOutput := runBinary(t, "doctor", "--db", databasePath, "--json")
	if err := json.Unmarshal(doctorOutput, &doctor); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, doctorOutput)
	}
	if doctor.Status != "ok" || doctor.Database != "ok" {
		t.Fatalf("doctor did not report healthy dependencies: %+v", doctor)
	}
	if doctor.SchemaVersion != version.SchemaVersion {
		t.Fatalf("schema version drift: version=%d doctor=%d", version.SchemaVersion, doctor.SchemaVersion)
	}
	if doctor.RemoteWrite {
		t.Fatal("remote writes must be disabled by default in M0")
	}
}

func TestRealCLIRuntimesRequireExplicitMigrationForExistingDatabase(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "version-gate.db")
	runBinary(t, "migrate", "up", "--db", databasePath)

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	var currentVersion int
	if err := database.QueryRow("PRAGMA user_version").Scan(&currentVersion); err != nil {
		_ = database.Close()
		t.Fatalf("read current schema version: %v", err)
	}
	oldVersion := currentVersion - 1
	if oldVersion < 1 {
		_ = database.Close()
		t.Fatalf("test requires a migratable existing schema, current=%d", currentVersion)
	}
	if _, err := database.Exec(fmt.Sprintf("PRAGMA user_version = %d", oldVersion)); err != nil {
		_ = database.Close()
		t.Fatalf("mark database as old schema: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close old-schema database: %v", err)
	}

	commands := [][]string{
		{"server", "--db", databasePath, "--http", reserveAddress(t)},
		{"runner", "--db", databasePath, "--runner-id", "version-gate"},
	}
	for _, arguments := range commands {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, maestroBinary, arguments...)
		command.Dir = repositoryRoot
		command.Env = append(
			os.Environ(),
			"MAESTRO_AUTH_TOKEN=version-gate-token",
			"MAESTRO_REMOTE_WRITE=false",
		)
		command.Stdin = strings.NewReader("")
		output, runErr := command.CombinedOutput()
		cancel()
		var exitError *exec.ExitError
		if !errors.As(runErr, &exitError) || exitError.ExitCode() != 6 {
			t.Fatalf("maestro %s must exit 6 for old schema: %v\n%s", strings.Join(arguments, " "), runErr, output)
		}
		if !bytes.Contains(output, []byte("code=VERSION_INCOMPATIBLE")) ||
			!bytes.Contains(output, []byte("run maestro migrate up")) {
			t.Fatalf("maestro %s returned an unstable version error:\n%s", strings.Join(arguments, " "), output)
		}

		database, err = sql.Open("sqlite", databasePath)
		if err != nil {
			t.Fatalf("reopen rejected runtime database: %v", err)
		}
		var versionAfterRuntime int
		if err := database.QueryRow("PRAGMA user_version").Scan(&versionAfterRuntime); err != nil {
			_ = database.Close()
			t.Fatalf("read schema after rejected runtime: %v", err)
		}
		if err := database.Close(); err != nil {
			t.Fatalf("close rejected runtime database: %v", err)
		}
		if versionAfterRuntime != oldVersion {
			t.Fatalf("runtime silently migrated schema: before=%d after=%d", oldVersion, versionAfterRuntime)
		}
	}

	runBinary(t, "migrate", "up", "--db", databasePath)
	database, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open explicitly migrated database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var migratedVersion int
	if err := database.QueryRow("PRAGMA user_version").Scan(&migratedVersion); err != nil {
		t.Fatalf("read explicit migration result: %v", err)
	}
	if migratedVersion != currentVersion {
		t.Fatalf("explicit migration did not reach current schema: got=%d want=%d", migratedVersion, currentVersion)
	}
}

func TestRealCLIRejectsCorruptCurrentSchemaWithoutRepair(t *testing.T) {
	var versionInfo struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(runBinary(t, "version", "--json"), &versionInfo); err != nil {
		t.Fatalf("decode schema version: %v", err)
	}
	if versionInfo.SchemaVersion < 1 {
		t.Fatalf("invalid current schema version: %d", versionInfo.SchemaVersion)
	}

	cases := []struct {
		name    string
		corrupt string
		forged  bool
	}{
		{name: "forged current empty database", forged: true},
		{name: "missing required table", corrupt: `DROP TABLE runtime_state`},
		{name: "missing upgraded column", corrupt: `ALTER TABLE tasks DROP COLUMN lease_expires_at`},
		{name: "missing required unique index", corrupt: `DROP INDEX idx_task_leases_one_active`},
		{
			name: "same-name no-op security trigger",
			corrupt: `DROP TRIGGER trg_tasks_m0_no_done_insert;
				CREATE TRIGGER trg_tasks_m0_no_done_insert BEFORE INSERT ON tasks BEGIN SELECT 1; END;`,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			databasePath := filepath.Join(t.TempDir(), "corrupt-current.db")
			if !test.forged {
				runBinary(t, "migrate", "up", "--db", databasePath)
			}
			database, err := sql.Open("sqlite", databasePath)
			if err != nil {
				t.Fatalf("open corruption target: %v", err)
			}
			if test.forged {
				_, err = database.Exec(fmt.Sprintf("PRAGMA user_version = %d", versionInfo.SchemaVersion))
			} else {
				_, err = database.Exec(`INSERT INTO projects(id,name,workspace_path)
					VALUES('integrity-marker','preserve-me','/integrity-marker')`)
				if err == nil {
					_, err = database.Exec(test.corrupt)
				}
			}
			if err != nil {
				_ = database.Close()
				t.Fatalf("corrupt current schema: %v", err)
			}
			if err := database.Close(); err != nil {
				t.Fatalf("close corruption target: %v", err)
			}

			before := schemaAndMarkerSnapshot(t, databasePath)
			commands := [][]string{
				{"server", "--db", databasePath, "--http", reserveAddress(t)},
				{"runner", "--db", databasePath, "--runner-id", "integrity-gate"},
				{"doctor", "--db", databasePath, "--json"},
			}
			for _, arguments := range commands {
				output, exitCode := runBinaryExpectExit(t, arguments...)
				if exitCode != 6 || !bytes.Contains(output, []byte("code=SCHEMA_INTEGRITY_FAILED")) ||
					!bytes.Contains(output, []byte("restore the database from a verified backup")) {
					t.Fatalf("maestro %s did not fail closed for corrupt schema (exit=%d):\n%s",
						strings.Join(arguments, " "), exitCode, output)
				}
				if after := schemaAndMarkerSnapshot(t, databasePath); after != before {
					t.Fatalf("maestro %s changed corrupt database while rejecting it", strings.Join(arguments, " "))
				}
			}

			output, exitCode := runBinaryExpectExit(t, "migrate", "up", "--db", databasePath)
			if exitCode != 5 || !bytes.Contains(output, []byte("code=MIGRATION_FAILED")) ||
				!bytes.Contains(output, []byte(fmt.Sprintf("migration plan current_schema=%d target_schema=%d", versionInfo.SchemaVersion, versionInfo.SchemaVersion))) ||
				!bytes.Contains(output, []byte("refusing automatic repair")) ||
				!bytes.Contains(output, []byte("verified backup")) {
				t.Fatalf("explicit migration accepted corrupt current schema (exit=%d):\n%s", exitCode, output)
			}
			if after := schemaAndMarkerSnapshot(t, databasePath); after != before {
				t.Fatal("rejected explicit migration changed corrupt database")
			}
		})
	}
}

func runBinaryExpectExit(t *testing.T, arguments ...string) ([]byte, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, maestroBinary, arguments...)
	command.Dir = repositoryRoot
	command.Env = append(
		os.Environ(),
		"MAESTRO_AUTH_TOKEN=integrity-gate-token",
		"MAESTRO_REMOTE_WRITE=false",
	)
	command.Stdin = strings.NewReader("")
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		if err == nil {
			t.Fatalf("maestro %s unexpectedly succeeded:\n%s", strings.Join(arguments, " "), output)
		}
		t.Fatalf("maestro %s did not return a process exit code: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return output, exitError.ExitCode()
}

func schemaAndMarkerSnapshot(t *testing.T, databasePath string) string {
	t.Helper()
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open schema snapshot: %v", err)
	}
	defer database.Close()

	var snapshot strings.Builder
	var version int
	if err := database.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read snapshot schema version: %v", err)
	}
	fmt.Fprintf(&snapshot, "version=%d\n", version)
	rows, err := database.Query(`SELECT type, name, tbl_name, sql FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read snapshot catalog: %v", err)
	}
	for rows.Next() {
		var objectType, name, tableName, definition string
		if err := rows.Scan(&objectType, &name, &tableName, &definition); err != nil {
			_ = rows.Close()
			t.Fatalf("scan snapshot catalog: %v", err)
		}
		fmt.Fprintf(&snapshot, "%s|%s|%s|%s\n", objectType, name, tableName, definition)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate snapshot catalog: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close snapshot catalog: %v", err)
	}

	var projectsTable int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE type='table' AND name='projects'`).Scan(&projectsTable); err != nil {
		t.Fatalf("check snapshot projects table: %v", err)
	}
	if projectsTable == 1 {
		var markerCount int
		if err := database.QueryRow(`SELECT COUNT(*) FROM projects
			WHERE id='integrity-marker' AND name='preserve-me'`).Scan(&markerCount); err != nil {
			t.Fatalf("read snapshot marker: %v", err)
		}
		fmt.Fprintf(&snapshot, "marker=%d\n", markerCount)
	}
	return snapshot.String()
}

func TestRealMigrateRefusesDatabaseOwnedByLiveServer(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "live-migration-lock.db")
	runtime := startRuntime(t, databasePath, false)
	defer runtime.stop(t)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open live server database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var versionBefore int
	if err := database.QueryRow("PRAGMA user_version").Scan(&versionBefore); err != nil {
		_ = database.Close()
		t.Fatalf("read schema before rejected migration: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, maestroBinary, "migrate", "up", "--db", databasePath)
	command.Dir = repositoryRoot
	command.Env = append(os.Environ(), "MAESTRO_REMOTE_WRITE=false")
	output, runErr := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(runErr, &exitError) || exitError.ExitCode() != 5 {
		t.Fatalf("migration against live server must exit 5: %v\n%s", runErr, output)
	}
	if !bytes.Contains(output, []byte("code=MIGRATION_FAILED")) ||
		!bytes.Contains(output, []byte("another Maestro server owns this database")) {
		_ = database.Close()
		t.Fatalf("migration did not report runtime lock conflict:\n%s", output)
	}
	var versionAfter int
	if err := database.QueryRow("PRAGMA user_version").Scan(&versionAfter); err != nil {
		_ = database.Close()
		t.Fatalf("read schema after rejected migration: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close live server database: %v", err)
	}
	if versionAfter != versionBefore {
		t.Fatalf("rejected migration changed schema: before=%d after=%d", versionBefore, versionAfter)
	}
	assertStatus(t, http.MethodGet, runtime.baseURL+"/readyz", "", "", http.StatusOK)

	runtime.stop(t)
	runBinary(t, "migrate", "up", "--db", databasePath)
}

func TestRealRuntimeLockCanonicalizesParentSymlinkAlias(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.MkdirAll(realDirectory, 0o700); err != nil {
		t.Fatalf("create real database directory: %v", err)
	}
	aliasDirectory := filepath.Join(root, "alias")
	if err := os.Symlink(realDirectory, aliasDirectory); err != nil {
		t.Skipf("create database directory alias: %v", err)
	}
	realDatabase := filepath.Join(realDirectory, "maestro.db")
	aliasDatabase := filepath.Join(aliasDirectory, "maestro.db")
	runtime := startRuntime(t, realDatabase, false)
	defer runtime.stop(t)

	output, exitCode := runBinaryExpectExit(
		t,
		"server", "--db", aliasDatabase, "--http", reserveAddress(t),
	)
	if exitCode != 3 || !bytes.Contains(output, []byte("another Maestro server owns this database")) {
		t.Fatalf("server acquired duplicate ownership through parent symlink (exit=%d):\n%s", exitCode, output)
	}
	output, exitCode = runBinaryExpectExit(t, "migrate", "up", "--db", aliasDatabase)
	if exitCode != 5 || !bytes.Contains(output, []byte("another Maestro server owns this database")) {
		t.Fatalf("migration bypassed live lock through parent symlink (exit=%d):\n%s", exitCode, output)
	}
	assertStatus(t, http.MethodGet, runtime.baseURL+"/readyz", "", "", http.StatusOK)

	runtime.stop(t)
	runBinary(t, "migrate", "up", "--db", aliasDatabase)
}

func TestRealStdioMCPInitializeAndListTools(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "runner.db")
	runBinary(t, "migrate", "up", "--db", databasePath)
	stdioProcess, err := startManagedStdioMCPClient(
		maestroBinary,
		append(os.Environ(), "MAESTRO_REMOTE_WRITE=false"),
		"runner", "--db", databasePath, "--runner-id", "m0-test-runner",
	)
	if err != nil {
		t.Fatalf("start real stdio Runner: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := stdioProcess.Close(); closeErr != nil {
			t.Errorf("close stdio MCP client: %v", closeErr)
		}
	})
	mcpClient := stdioProcess.Client

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	initializeMCP(t, ctx, mcpClient)
	assertMCPToolCatalog(t, ctx, mcpClient)
	if err := mcpClient.Ping(ctx); err != nil {
		t.Fatalf("real stdio MCP ping: %v", err)
	}
}

func TestRealRunnerCannotBootstrapEmptyDatabase(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "runner-empty.db")
	output, exitCode := runBinaryExpectExit(
		t, "runner", "--db", databasePath, "--runner-id", "non-owner",
	)
	if exitCode != 6 || !bytes.Contains(output, []byte("code=VERSION_INCOMPATIBLE")) {
		t.Fatalf("empty-database Runner did not fail closed (exit=%d):\n%s", exitCode, output)
	}
	database, err := sql.Open("sqlite", databasePath)
	requireNoError(t, err, "open Runner-created empty database")
	defer database.Close()
	var version, objects int
	requireNoError(t, database.QueryRow("PRAGMA user_version").Scan(&version), "read empty Runner version")
	requireNoError(t, database.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL`).Scan(&objects), "read empty Runner catalog")
	if version != 0 || objects != 0 {
		t.Fatalf("non-owner Runner mutated empty database: version=%d objects=%d", version, objects)
	}
}

func TestSIGTERMRejectsWriteOnAlreadyAcceptedConnection(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "drain.db")
	process := startRuntime(t, databasePath, true)
	t.Cleanup(func() {
		if process.stopped {
			return
		}
		process.stopped = true
		_ = process.command.Process.Kill()
		<-process.done
	})

	address := strings.TrimPrefix(process.baseURL, "http://")
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	requireNoError(t, err, "open connection before drain")
	defer connection.Close()
	_, err = fmt.Fprintf(connection,
		"POST /api/v1/projects HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 2\r\n",
		address, process.token,
	)
	requireNoError(t, err, "write partial pre-drain request")

	requireNoError(t, process.command.Process.Signal(syscall.SIGTERM), "signal real server")
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(process.stderr.String(), "lifecycle=DRAINING") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(process.stderr.String(), "lifecycle=DRAINING") {
		t.Fatalf("real server did not enter drain before timeout: %s", process.stderr.String())
	}
	_, err = io.WriteString(connection, "\r\n{}")
	requireNoError(t, err, "complete request after drain")
	requireNoError(t, connection.SetReadDeadline(time.Now().Add(3*time.Second)), "bound drain response")
	request, err := http.NewRequest(http.MethodPost, process.baseURL+"/api/v1/projects", nil)
	requireNoError(t, err, "construct drain response request")
	response, err := http.ReadResponse(bufio.NewReader(connection), request)
	requireNoError(t, err, "read drain response on accepted connection")
	body, err := io.ReadAll(response.Body)
	requireNoError(t, err, "read drain response body")
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable ||
		!bytes.Contains(body, []byte(`"error_code":"RUNTIME_DRAINING"`)) ||
		!bytes.Contains(body, []byte(`"correlation_id":"`)) {
		t.Fatalf("post-signal write did not fail with stable drain error: status=%d body=%s",
			response.StatusCode, body)
	}

	process.stopped = true
	select {
	case waitErr := <-process.done:
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) || exitError.ExitCode() != 143 {
			t.Fatalf("SIGTERM drain must exit 143, got %v: %s", waitErr, process.stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = process.command.Process.Kill()
		<-process.done
		t.Fatalf("real server did not complete SIGTERM drain: %s", process.stderr.String())
	}
}

func TestRawStdioStdoutContainsOnlyJSONRPC(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "raw-stdio.db")
	runBinary(t, "migrate", "up", "--db", databasePath)
	tests := []struct {
		name  string
		input string
		ids   []int
	}{
		{name: "empty stdin has empty stdout"},
		{
			name: "initialize and tools list are strict JSON lines",
			input: strings.Join([]string{
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw-m0","version":"1.0.0"}}}`,
				`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
				`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
			}, "\n") + "\n",
			ids: []int{1, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, maestroBinary,
				"runner", "--db", databasePath, "--runner-id", "raw-stdio-runner")
			command.Dir = repositoryRoot
			command.Env = environmentWithOverrides(map[string]string{
				"GIN_MODE":             "debug",
				"MAESTRO_REMOTE_WRITE": "false",
			})
			command.Stdin = strings.NewReader(tt.input)
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			if err := command.Run(); err != nil {
				t.Fatalf("raw stdio Runner failed: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
			}

			trimmed := strings.TrimSpace(stdout.String())
			if len(tt.ids) == 0 {
				if trimmed != "" {
					t.Fatalf("empty stdio session wrote non-protocol stdout:\n%s", stdout.String())
				}
				return
			}

			lines := strings.Split(trimmed, "\n")
			if len(lines) != len(tt.ids) {
				t.Fatalf("stdio stdout line count=%d, want %d; every line must be one JSON-RPC response:\n%s",
					len(lines), len(tt.ids), stdout.String())
			}
			for index, line := range lines {
				var response struct {
					JSONRPC string `json:"jsonrpc"`
					ID      int    `json:"id"`
				}
				if err := json.Unmarshal([]byte(line), &response); err != nil {
					t.Fatalf("stdio stdout line %d is not JSON-RPC: %v\n%s", index+1, err, line)
				}
				if response.JSONRPC != "2.0" || response.ID != tt.ids[index] {
					t.Fatalf("unexpected stdio response line %d: %+v", index+1, response)
				}
			}
		})
	}
}

func TestRealHTTPRuntimeSecurityMCPAndRestart(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "server.db")
	first := startRuntime(t, databasePath, false)

	assertStatus(t, http.MethodGet, first.baseURL+"/livez", "", "", http.StatusOK)
	assertStatus(t, http.MethodGet, first.baseURL+"/readyz", "", "", http.StatusOK)
	assertStatus(t, http.MethodGet, first.baseURL+"/dashboard", "", "", http.StatusUnauthorized)
	assertStatus(t, http.MethodGet, first.baseURL+"/dashboard", "Bearer incorrect", "", http.StatusUnauthorized)
	assertStatus(t, http.MethodGet, first.baseURL+"/dashboard", "Bearer "+first.token, "", http.StatusOK)
	assertStatus(t, http.MethodPost, first.baseURL+"/api/v1/projects", "Bearer "+first.token, `{}`, http.StatusForbidden)
	assertStatusWithOrigin(
		t,
		http.MethodGet,
		first.baseURL+"/dashboard",
		"Bearer "+first.token,
		"http://127.0.0.1.attacker.invalid",
		http.StatusForbidden,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	httpMCP, err := client.NewStreamableHttpClient(
		first.baseURL+"/mcp",
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + first.token}),
	)
	if err != nil {
		cancel()
		first.stop(t)
		t.Fatalf("create Streamable HTTP MCP client: %v", err)
	}
	if err := httpMCP.Start(ctx); err != nil {
		cancel()
		first.stop(t)
		t.Fatalf("start Streamable HTTP MCP client: %v", err)
	}
	initializeMCP(t, ctx, httpMCP)
	assertMCPToolCatalog(t, ctx, httpMCP)
	if err := httpMCP.Close(); err != nil {
		t.Errorf("close Streamable HTTP MCP client: %v", err)
	}
	cancel()

	// HTTP tools/call is a mutation boundary even when the selected tool might
	// later reject its own payload. The transport feature flag must stop it
	// before protocol dispatch.
	assertStatus(
		t,
		http.MethodPost,
		first.baseURL+"/mcp",
		"Bearer "+first.token,
		`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"create_project","arguments":{}}}`,
		http.StatusForbidden,
	)

	first.stop(t)

	// A second real process must open and migrate the same database cleanly.
	second := startRuntime(t, databasePath, false)
	assertStatus(t, http.MethodGet, second.baseURL+"/readyz", "", "", http.StatusOK)
	second.stop(t)
}

func TestLiveServerOwnsRecoveryWhileStdioRunnerSharesStateSafely(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "shared-runtime.db")
	server := startRuntime(t, databasePath, true)
	defer server.stop(t)

	projectID := createRuntimeProject(t, server, t.TempDir())
	requestJSON(t, http.MethodPost, server.baseURL+"/api/v1/projects/"+projectID+"/sessions", "Bearer "+server.token,
		`{"id":"live-session","role":"backend","client_type":"rest-api","capacity":1}`, http.StatusOK, nil)

	database, err := sql.Open("sqlite", databasePath)
	requireNoError(t, err, "open shared runtime database")
	t.Cleanup(func() { _ = database.Close() })
	var status string
	requireNoError(t, database.QueryRow(`SELECT status FROM agent_sessions
		WHERE project_id=? AND COALESCE(external_id,id)='live-session'`, projectID).Scan(&status), "read live Session")
	if status != "online" {
		t.Fatalf("server Session must start online, got %s", status)
	}

	// EOF is a clean stdio Runner shutdown. Constructing its local MCP graph may
	// open the same SQLite file, but it is not the maintenance owner and must not
	// infer that the still-running server crashed.
	runnerCtx, cancelRunner := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRunner()
	runner := exec.CommandContext(runnerCtx, maestroBinary,
		"runner", "--db", databasePath, "--runner-id", "shared-state-runner")
	runner.Dir = repositoryRoot
	runner.Env = append(os.Environ(), "MAESTRO_REMOTE_WRITE=false")
	runner.Stdin = strings.NewReader("")
	if output, runErr := runner.CombinedOutput(); runErr != nil {
		t.Fatalf("stdio Runner sharing live server state failed: %v\n%s", runErr, output)
	}
	requireNoError(t, database.QueryRow(`SELECT status FROM agent_sessions
		WHERE project_id=? AND COALESCE(external_id,id)='live-session'`, projectID).Scan(&status), "read Session after Runner")
	if status != "online" {
		t.Fatalf("stdio Runner recovered live server Session: got %s", status)
	}

	// A second maintenance owner is more dangerous than a stdio reader and must
	// fail before recovery touches durable state.
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSecond()
	second := exec.CommandContext(secondCtx, maestroBinary,
		"server", "--db", databasePath, "--http", reserveAddress(t))
	second.Dir = repositoryRoot
	second.Env = append(os.Environ(), "MAESTRO_AUTH_TOKEN=second-owner", "MAESTRO_REMOTE_WRITE=false")
	secondOutput, secondErr := second.CombinedOutput()
	if secondErr == nil {
		t.Fatalf("second server unexpectedly acquired recovery ownership: %s", secondOutput)
	}
	var exitError *exec.ExitError
	if !errors.As(secondErr, &exitError) || exitError.ExitCode() != 3 ||
		!bytes.Contains(secondOutput, []byte("another Maestro server owns this database")) {
		t.Fatalf("second server did not fail with stable ownership error: %v\n%s", secondErr, secondOutput)
	}
	requireNoError(t, database.QueryRow(`SELECT status FROM agent_sessions
		WHERE project_id=? AND COALESCE(external_id,id)='live-session'`, projectID).Scan(&status), "read Session after rejected server")
	if status != "online" {
		t.Fatalf("rejected server mutated live Session: got %s", status)
	}
}

func TestRealBinaryRestartRecoversPersistedExecution(t *testing.T) {
	workspacePath := createValidationRepository(t)
	databasePath := filepath.Join(t.TempDir(), "recovery.db")
	first := startRuntime(t, databasePath, true)
	defer first.stop(t)

	projectID := createRuntimeProject(t, first, workspacePath)
	featureID := createRuntimeFeature(t, first, projectID)
	requestJSON(t, http.MethodPost, first.baseURL+"/api/v1/projects/"+projectID+"/sessions", "Bearer "+first.token,
		`{"id":"m0-recovery-session","role":"backend","client_type":"rest-api","capacity":5}`, http.StatusOK, nil)
	taskID := createRuntimeTask(t, first, projectID, featureID, `{}`, "restart recovery")

	database, err := sql.Open("sqlite", databasePath)
	requireNoError(t, err, "open recovery database")
	t.Cleanup(func() { _ = database.Close() })
	_, _ = database.Exec("PRAGMA busy_timeout = 5000")
	worktreePath := claimRuntimeTask(t, first, database, projectID, taskID, "m0-recovery-session", "m0-recovery-worker")
	if worktreePath == "" {
		t.Fatal("claimed execution has no durable worktree")
	}
	var originalTaskVersion int64
	var activeLeaseID string
	requireNoError(t, database.QueryRow(`SELECT version, active_lease_id FROM tasks
		WHERE project_id=? AND id=? AND status='executing'`, projectID, taskID).
		Scan(&originalTaskVersion, &activeLeaseID), "read persisted execution before restart")
	if activeLeaseID == "" {
		t.Fatal("executing task has no active Lease before restart")
	}

	first.stop(t)
	second := startRuntime(t, databasePath, false)
	defer second.stop(t)
	assertStatus(t, http.MethodGet, second.baseURL+"/readyz", "", "", http.StatusOK)

	var recoveredStatus string
	var recoveredVersion int64
	var recoveredLease sql.NullString
	requireNoError(t, database.QueryRow(`SELECT status, version, active_lease_id FROM tasks
		WHERE project_id=? AND id=?`, projectID, taskID).
		Scan(&recoveredStatus, &recoveredVersion, &recoveredLease), "read recovered task")
	if recoveredStatus != "needs_human" || recoveredVersion != originalTaskVersion+1 || recoveredLease.Valid {
		t.Fatalf("restart did not fail closed: status=%s version=%d original_version=%d lease=%+v",
			recoveredStatus, recoveredVersion, originalTaskVersion, recoveredLease)
	}

	var leaseStatus, workerStatus, sessionStatus, worktreeStatus string
	var workerTask sql.NullString
	requireNoError(t, database.QueryRow(`SELECT status FROM task_leases
		WHERE project_id=? AND id=?`, projectID, activeLeaseID).Scan(&leaseStatus), "read recovered Lease")
	requireNoError(t, database.QueryRow(`SELECT status, current_task_id FROM agent_workers
		WHERE project_id=? AND id='m0-recovery-worker'`, projectID).Scan(&workerStatus, &workerTask), "read recovered Worker")
	requireNoError(t, database.QueryRow(`SELECT status FROM agent_sessions
		WHERE project_id=? AND COALESCE(external_id,id)='m0-recovery-session'`, projectID).Scan(&sessionStatus), "read recovered Session")
	requireNoError(t, database.QueryRow(`SELECT status FROM worktrees
		WHERE project_id=? AND task_id=?`, projectID, taskID).Scan(&worktreeStatus), "read recovered worktree")
	if leaseStatus != "expired" || workerStatus != "lost" || workerTask.Valid ||
		sessionStatus != "offline" || worktreeStatus != "quarantined" {
		t.Fatalf("incomplete restart reconciliation: lease=%s worker=%s worker_task=%+v session=%s worktree=%s",
			leaseStatus, workerStatus, workerTask, sessionStatus, worktreeStatus)
	}
	var recoveryAudit int
	requireNoError(t, database.QueryRow(`SELECT COUNT(*) FROM audit_log
		WHERE action='runtime.recovery' AND result='ALLOWED' AND detail LIKE '%"interrupted_tasks":1%'`).
		Scan(&recoveryAudit), "read recovery audit")
	if recoveryAudit != 1 {
		t.Fatalf("expected one audited non-empty recovery, got %d", recoveryAudit)
	}

	// A second restart must not transition or version the already reconciled
	// Task again.
	second.stop(t)
	third := startRuntime(t, databasePath, false)
	defer third.stop(t)
	var stableStatus string
	var stableVersion int64
	requireNoError(t, database.QueryRow(`SELECT status, version FROM tasks
		WHERE project_id=? AND id=?`, projectID, taskID).Scan(&stableStatus, &stableVersion), "read idempotently recovered task")
	if stableStatus != recoveredStatus || stableVersion != recoveredVersion {
		t.Fatalf("repeated recovery changed stable state: first=%s@%d second=%s@%d",
			recoveredStatus, recoveredVersion, stableStatus, stableVersion)
	}
}

func TestRealWebSocketProjectEvent(t *testing.T) {
	workspacePath := t.TempDir()
	databasePath := filepath.Join(t.TempDir(), "websocket.db")
	runtime := startRuntime(t, databasePath, true)
	defer runtime.stop(t)

	projectPayload := fmt.Sprintf(`{"name":"M0 WebSocket","workspace_path":%q}`, workspacePath)
	var projectResponse struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	requestJSON(
		t,
		http.MethodPost,
		runtime.baseURL+"/api/v1/projects",
		"Bearer "+runtime.token,
		projectPayload,
		http.StatusOK,
		&projectResponse,
	)
	if projectResponse.Data.ID == "" {
		t.Fatal("project creation did not return an ID")
	}

	websocketURL := "ws" + strings.TrimPrefix(runtime.baseURL, "http") +
		"/api/v1/projects/" + projectResponse.Data.ID + "/ws"
	headers := http.Header{
		"Authorization": []string{"Bearer " + runtime.token},
		"Origin":        []string{runtime.baseURL},
	}
	connection, response, err := websocket.DefaultDialer.Dial(websocketURL, headers)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("upgrade authenticated WebSocket (status=%d): %v", status, err)
	}
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set WebSocket deadline: %v", err)
	}

	requestJSON(
		t,
		http.MethodPost,
		runtime.baseURL+"/api/v1/projects/"+projectResponse.Data.ID+"/features",
		"Bearer "+runtime.token,
		`{"title":"WebSocket event","description":"M0 smoke"}`,
		http.StatusOK,
		nil,
	)
	_, message, err := connection.ReadMessage()
	if err != nil {
		t.Fatalf("read WebSocket project event: %v", err)
	}
	var event struct {
		Type      string `json:"type"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(message, &event); err != nil {
		t.Fatalf("decode WebSocket event: %v\n%s", err, message)
	}
	if event.Type != "feature.created" || event.ProjectID != projectResponse.Data.ID {
		t.Fatalf("unexpected WebSocket event: %+v", event)
	}
}

func TestRealBinaryZeroTrustValidationSuccessAndFailure(t *testing.T) {
	workspacePath := createValidationRepository(t)
	databasePath := filepath.Join(t.TempDir(), "validation.db")
	configurationDirectory := t.TempDir()
	runnerConfigPath := filepath.Join(configurationDirectory, "runner.yaml")
	serverConfigPath := filepath.Join(configurationDirectory, "server.yaml")

	goExecutable, err := exec.LookPath("go")
	requireNoError(t, err, "find Go executable")
	goExecutable, err = filepath.EvalSymlinks(goExecutable)
	requireNoError(t, err, "resolve Go executable")
	policyDigest := "sha256:" + strings.Repeat("c", 64)
	runnerConfig := fmt.Sprintf(`
db_path: %q
http_addr: "127.0.0.1:8080"
remote_write: false
validation:
  allow_host_execution: true
  default_timeout_sec: 45
  max_output_bytes: 65536
  policy_version: "3.0.0-m0"
  policy_digest: %q
  command_profiles:
    - id: "go-m0-test"
      version: "3.0.0"
      image_digest: "sha256:%s"
      argv: [%q, "test", "-coverprofile=coverage.out", "./..."]
      working_directory: "."
      network: {mode: "none", allow_hosts: []}
      resources: {cpu_millis: 2000, memory_mb: 2048, disk_mb: 4096, pids: 256}
      output_limit_bytes: 65536
      timeout_seconds: 45
`, databasePath, policyDigest, strings.Repeat("b", 64), goExecutable)
	requireNoError(t, os.WriteFile(runnerConfigPath, []byte(runnerConfig), 0o600), "write Runner config")
	serverConfig := fmt.Sprintf("db_path: %q\nhttp_addr: \"127.0.0.1:8080\"\nremote_write: true\n", databasePath)
	requireNoError(t, os.WriteFile(serverConfigPath, []byte(serverConfig), 0o600), "write server config")
	runBinary(t, "migrate", "up", "--config", runnerConfigPath)

	// The HTTP process can mutate orchestration state but has no host execution
	// profiles. Only the already-running local stdio Runner owns that authority.
	httpRuntime := startRuntimeWithConfig(t, serverConfigPath, true)
	defer httpRuntime.stop(t)

	var doctor struct {
		HostExecution      bool `json:"host_execution"`
		RemoteWrite        bool `json:"remote_write"`
		ValidationProfiles []struct {
			Ref    string `json:"ref"`
			Digest string `json:"digest"`
		} `json:"validation_profiles"`
	}
	doctorOutput := runBinary(t, "doctor", "--config", runnerConfigPath, "--json")
	requireNoError(t, json.Unmarshal(doctorOutput, &doctor), "decode Runner doctor report")
	if !doctor.HostExecution || doctor.RemoteWrite || len(doctor.ValidationProfiles) != 1 {
		t.Fatalf("unsafe or incomplete Runner validation report: %+v", doctor)
	}
	profileDigest := doctor.ValidationProfiles[0].Digest

	projectID := createRuntimeProject(t, httpRuntime, workspacePath)
	featureID := createRuntimeFeature(t, httpRuntime, projectID)
	const validationRunnerSession = "m0-validation-runner-session"
	const validationRunnerWorker = "m0-validation-runner-worker"
	requestJSON(t, http.MethodPost, httpRuntime.baseURL+"/api/v1/projects/"+projectID+"/sessions", "Bearer "+httpRuntime.token,
		fmt.Sprintf(`{"id":%q,"role":"backend","client_type":"rest-api","capacity":5}`, validationRunnerSession), http.StatusOK, nil)
	// The stdio Runner starts with its server-side delegated context: the
	// --project binding (and derived session/worker identity) can never be
	// supplied or overridden through tool arguments.
	runnerProcess, err := startManagedStdioMCPClient(
		maestroBinary,
		append(os.Environ(), "MAESTRO_REMOTE_WRITE=false"),
		"runner", "--config", runnerConfigPath, "--runner-id", "m0-validation-runner", "--project", projectID,
	)
	requireNoError(t, err, "start persistent real stdio Runner")
	t.Cleanup(func() {
		if closeErr := runnerProcess.Close(); closeErr != nil {
			t.Errorf("close validation Runner: %v", closeErr)
		}
	})
	runner := runnerProcess.Client
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	initializeMCP(t, ctx, runner)
	testRequirementsBytes, err := json.Marshal(map[string]any{
		"profile_id": "go-m0-test", "profile_version": "3.0.0", "profile_digest": profileDigest,
		"coverage_format": "go-cover", "coverage_path": "coverage.out", "min_coverage": 80,
	})
	requireNoError(t, err, "encode test requirements")

	database, err := sql.Open("sqlite", databasePath)
	requireNoError(t, err, "open validation database")
	t.Cleanup(func() { _ = database.Close() })
	_, _ = database.Exec("PRAGMA busy_timeout = 5000")

	successTaskID := createRuntimeTask(t, httpRuntime, projectID, featureID, string(testRequirementsBytes), "validation success")
	var claimQueueVersion int64
	requireNoError(t, database.QueryRow(`SELECT COALESCE((SELECT version FROM project_queue_versions
		WHERE project_id=?), 0)`, projectID).Scan(&claimQueueVersion), "read queue token before claim")
	claimResult, claimText := callRealTool(t, ctx, runner, "get_next_task", map[string]any{
		"idempotency_key": "m0-real-claim-0000000001", "queue_version": claimQueueVersion,
	})
	if claimResult.IsError || !strings.Contains(claimText, successTaskID) {
		t.Fatalf("real MCP claim did not return the queued task: is_error=%t content=%s", claimResult.IsError, claimText)
	}
	var successWorktree string
	requireNoError(t, database.QueryRow(`SELECT worktree_path FROM worktrees
		WHERE project_id=? AND task_id=? AND status='active'`, projectID, successTaskID).
		Scan(&successWorktree), "read MCP-claimed worktree")
	var leaseID, leaseExpiry string
	var leaseVersion int64
	requireNoError(t, database.QueryRow(`SELECT id, version, expires_at FROM task_leases
		WHERE project_id=? AND task_id=? AND status='active'`, projectID, successTaskID).
		Scan(&leaseID, &leaseVersion, &leaseExpiry), "read active Lease before MCP heartbeat")
	heartbeatArguments := map[string]any{
		"work_item_id": successTaskID,
		"lease_id":     leaseID, "lease_version": leaseVersion,
		"idempotency_key": "m0-real-heartbeat-0001",
	}
	heartbeatResult, heartbeatText := callRealTool(t, ctx, runner, "heartbeat_task", heartbeatArguments)
	if heartbeatResult.IsError {
		t.Fatalf("real MCP heartbeat returned an error: %s", heartbeatText)
	}
	var heartbeatPayload struct {
		WorkItemID   string `json:"work_item_id"`
		LeaseID      string `json:"lease_id"`
		LeaseVersion int64  `json:"lease_version"`
		ExpiresAt    string `json:"expires_at"`
	}
	requireNoError(t, json.Unmarshal([]byte(heartbeatText), &heartbeatPayload), "decode real MCP heartbeat")
	if heartbeatPayload.WorkItemID != successTaskID || heartbeatPayload.LeaseID != leaseID ||
		heartbeatPayload.LeaseVersion != leaseVersion+1 || heartbeatPayload.ExpiresAt == "" {
		t.Fatalf("incomplete MCP heartbeat result: %+v", heartbeatPayload)
	}
	var storedLeaseVersion int64
	var storedLeaseExpiry string
	requireNoError(t, database.QueryRow(`SELECT version, expires_at FROM task_leases
		WHERE project_id=? AND id=?`, projectID, leaseID).
		Scan(&storedLeaseVersion, &storedLeaseExpiry), "read renewed Lease")
	if storedLeaseVersion != heartbeatPayload.LeaseVersion || storedLeaseExpiry != heartbeatPayload.ExpiresAt {
		t.Fatalf("heartbeat result/database mismatch: result=%+v stored_version=%d stored_expiry=%s original_expiry=%s",
			heartbeatPayload, storedLeaseVersion, storedLeaseExpiry, leaseExpiry)
	}

	// A transport retry with the same idempotency key and original CAS version
	// must return the original result without renewing a second time.
	replayedHeartbeat, replayedText := callRealTool(t, ctx, runner, "heartbeat_task", heartbeatArguments)
	if replayedHeartbeat.IsError || replayedText != heartbeatText {
		t.Fatalf("real MCP heartbeat replay changed its result: is_error=%t first=%s replay=%s",
			replayedHeartbeat.IsError, heartbeatText, replayedText)
	}
	requireNoError(t, database.QueryRow(`SELECT version FROM task_leases
		WHERE project_id=? AND id=?`, projectID, leaseID).Scan(&storedLeaseVersion), "read replayed Lease")
	if storedLeaseVersion != leaseVersion+1 {
		t.Fatalf("idempotent heartbeat renewed twice: version=%d", storedLeaseVersion)
	}

	staleArguments := map[string]any{}
	for key, value := range heartbeatArguments {
		staleArguments[key] = value
	}
	staleArguments["idempotency_key"] = "m0-real-heartbeat-stale-0002"
	staleHeartbeat, staleText := callRealTool(t, ctx, runner, "heartbeat_task", staleArguments)
	if !staleHeartbeat.IsError || !strings.Contains(staleText, `"code":"LEASE_VERSION_MISMATCH"`) {
		t.Fatalf("stale MCP heartbeat did not fail with a stable CAS error: is_error=%t content=%s",
			staleHeartbeat.IsError, staleText)
	}

	requireNoError(t, os.WriteFile(filepath.Join(successWorktree, "src", "subtract.go"), []byte("package sample\n\nfunc Subtract(a, b int) int { return a - b }\n"), 0o600), "write successful change")
	requireNoError(t, os.WriteFile(filepath.Join(successWorktree, "src", "subtract_test.go"), []byte("package sample\n\nimport \"testing\"\n\nfunc TestSubtract(t *testing.T) { if Subtract(5, 3) != 2 { t.Fatal(\"bad difference\") } }\n"), 0o600), "write successful test")
	runGit(t, successWorktree, "add", "src/subtract.go", "src/subtract_test.go")
	runGit(t, successWorktree, "-c", "user.name=M0 Test", "-c", "user.email=m0@example.invalid", "commit", "-m", "successful validation change")

	var renewedLeaseVersion int64
	requireNoError(t, database.QueryRow(`SELECT version FROM task_leases
		WHERE project_id=? AND id=?`, projectID, leaseID).Scan(&renewedLeaseVersion), "read lease version before submit")
	successCommit := gitRevParse(t, successWorktree)
	var dbgAssigned, dbgWorker sql.NullString
	_ = database.QueryRow(`SELECT assigned_session_id, assigned_worker_id FROM tasks WHERE project_id=? AND id=?`, projectID, successTaskID).Scan(&dbgAssigned, &dbgWorker)
	var dbgLeaseSession string
	_ = database.QueryRow(`SELECT session_id FROM task_leases WHERE id=?`, leaseID).Scan(&dbgLeaseSession)
	var dbgPhys, dbgExt string
	_ = database.QueryRow(`SELECT id, COALESCE(external_id,'') FROM agent_sessions WHERE project_id=? AND COALESCE(external_id,id)=?`, projectID, validationRunnerSession).Scan(&dbgPhys, &dbgExt)
	t.Logf("DBG lease.session=%q sess.phys=%q ext=%q", dbgLeaseSession, dbgPhys, dbgExt)
	result, resultText := callRealTool(t, ctx, runner, "submit_task_result", map[string]any{
		"work_item_id": successTaskID, "lease_id": leaseID, "lease_version": renewedLeaseVersion,
		"commit_sha": successCommit, "evidence_refs": []string{"local://validation"},
		"summary": "real binary validation", "idempotency_key": "m0-real-submit-0000000001",
	})
	if result.IsError {
		t.Fatalf("successful real-binary validation returned an MCP error: %s", resultText)
	}
	var successStatus, successResult, successError, evidenceDigest, sourceCommit, policyVersion, profileRef string
	requireNoError(t, database.QueryRow(`SELECT t.status, v.result, COALESCE(v.error_code,''), v.evidence_digest, v.source_commit, v.policy_version, v.profile_ref
		FROM tasks t JOIN validation_runs v ON v.project_id=t.project_id AND v.task_id=t.id
		WHERE t.project_id=? AND t.id=? ORDER BY v.attempt DESC LIMIT 1`, projectID, successTaskID).Scan(
		&successStatus, &successResult, &successError, &evidenceDigest, &sourceCommit, &policyVersion, &profileRef,
	), "read successful immutable evidence")
	if successStatus != "validating" || successResult != "passed" || successError != "" ||
		!strings.HasPrefix(evidenceDigest, "sha256:") || len(sourceCommit) != 40 ||
		policyVersion != "3.0.0-m0" || !strings.HasPrefix(profileRef, "go-m0-test@3.0.0@sha256:") {
		t.Fatalf("incomplete successful validation evidence: status=%s result=%s error=%s digest=%s source=%s policy=%s profile=%s",
			successStatus, successResult, successError, evidenceDigest, sourceCommit, policyVersion, profileRef)
	}
	if _, err := database.Exec(`UPDATE validation_runs SET result='failed' WHERE project_id=? AND task_id=?`, projectID, successTaskID); err == nil {
		t.Fatal("validation Evidence UPDATE unexpectedly bypassed append-only trigger")
	}

	failureTaskID := createRuntimeTask(t, httpRuntime, projectID, featureID, string(testRequirementsBytes), "validation failure")
	failureWorktree := claimRuntimeTask(t, httpRuntime, database, projectID, failureTaskID, validationRunnerSession, validationRunnerWorker)
	failingTest := []byte("package sample\n\nimport \"testing\"\n\nfunc TestIntentionalFailure(t *testing.T) { t.Fatal(\"intentional M0 failure\") }\n")
	requireNoError(t, os.WriteFile(filepath.Join(failureWorktree, "src", "failure_test.go"), failingTest, 0o600), "write failing test")
	runGit(t, failureWorktree, "add", "src/failure_test.go")
	runGit(t, failureWorktree, "-c", "user.name=M0 Test", "-c", "user.email=m0@example.invalid", "commit", "-m", "failing validation change")

	var failureLeaseID string
	var failureLeaseVersion int64
	requireNoError(t, database.QueryRow(`SELECT id, version FROM task_leases
		WHERE project_id=? AND task_id=? AND status='active'`, projectID, failureTaskID).
		Scan(&failureLeaseID, &failureLeaseVersion), "read failure task lease")
	failureCommit := gitRevParse(t, failureWorktree)
	result, resultText = callRealTool(t, ctx, runner, "submit_task_result", map[string]any{
		"work_item_id": failureTaskID, "lease_id": failureLeaseID, "lease_version": failureLeaseVersion,
		"commit_sha": failureCommit, "evidence_refs": []string{"local://validation"},
		"idempotency_key": "m0-real-submit-fail-00002",
	})
	if !result.IsError || !strings.Contains(resultText, "TEST_FAILED") {
		t.Fatalf("failing real-binary validation did not fail closed: is_error=%t content=%s", result.IsError, resultText)
	}
	var failureStatus, failureResult, failureCode, failureDigest string
	requireNoError(t, database.QueryRow(`SELECT t.status, v.result, COALESCE(v.error_code,''), v.evidence_digest
		FROM tasks t JOIN validation_runs v ON v.project_id=t.project_id AND v.task_id=t.id
		WHERE t.project_id=? AND t.id=? ORDER BY v.attempt DESC LIMIT 1`, projectID, failureTaskID).Scan(
		&failureStatus, &failureResult, &failureCode, &failureDigest,
	), "read failed immutable evidence")
	if failureStatus != "failed" || failureResult != "failed" || failureCode != "TEST_FAILED" || !strings.HasPrefix(failureDigest, "sha256:") {
		t.Fatalf("failure did not remain fail-closed: status=%s result=%s code=%s digest=%s", failureStatus, failureResult, failureCode, failureDigest)
	}
}

// managedStdioMCPClient owns the real Runner process separately from the MCP
// transport. mcp-go's process-owning Stdio.Close closes the child's stderr pipe
// before waiting for the child. The Runner deliberately writes lifecycle logs to
// stderr, so that close order can deliver SIGPIPE while the child is performing an
// otherwise graceful EOF shutdown. Keeping stderr drained until cmd.Wait proves
// that the real process exited successfully and also prevents pipe or zombie leaks.
type managedStdioMCPClient struct {
	*client.Client

	command      *exec.Cmd
	stdout       io.ReadCloser
	stderr       io.ReadCloser
	stderrDone   chan error
	stderrBuffer bytes.Buffer

	closeOnce sync.Once
	closeErr  error
}

type nonClosingReadCloser struct {
	io.Reader
}

func (nonClosingReadCloser) Close() error { return nil }

func startManagedStdioMCPClient(command string, environment []string, arguments ...string) (*managedStdioMCPClient, error) {
	child := exec.Command(command, arguments...)
	if len(environment) > 0 {
		child.Env = environment
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create Runner stdin: %w", err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("create Runner stdout: %w", err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("create Runner stderr: %w", err)
	}
	if err := child.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start Runner process: %w", err)
	}

	managed := &managedStdioMCPClient{
		command:    child,
		stdout:     stdout,
		stderr:     stderr,
		stderrDone: make(chan error, 1),
	}
	go func() {
		_, copyErr := io.Copy(&managed.stderrBuffer, stderr)
		managed.stderrDone <- copyErr
	}()

	stdioTransport := transport.NewIO(stdout, stdin, nonClosingReadCloser{Reader: stderr})
	managed.Client = client.NewClient(stdioTransport)
	if err := managed.Start(context.Background()); err != nil {
		_ = stdin.Close()
		_ = child.Process.Kill()
		_ = child.Wait()
		_ = stdout.Close()
		_ = stderr.Close()
		<-managed.stderrDone
		return nil, fmt.Errorf("start Runner MCP transport: %w", err)
	}
	return managed, nil
}

func (managed *managedStdioMCPClient) Close() error {
	managed.closeOnce.Do(func() {
		// Client.Close closes only stdin here: stderr is intentionally wrapped by
		// nonClosingReadCloser and stays drainable until the process has exited.
		managed.closeErr = managed.Client.Close()

		waitDone := make(chan error, 1)
		go func() { waitDone <- managed.command.Wait() }()

		var waitErr error
		select {
		case waitErr = <-waitDone:
		case <-time.After(5 * time.Second):
			managed.closeErr = errors.Join(managed.closeErr, errors.New("Runner did not exit after graceful stdin EOF"))
			if signalErr := managed.command.Process.Signal(syscall.SIGTERM); signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
				managed.closeErr = errors.Join(managed.closeErr, fmt.Errorf("signal Runner after graceful shutdown timeout: %w", signalErr))
			}
			select {
			case waitErr = <-waitDone:
			case <-time.After(2 * time.Second):
				if killErr := managed.command.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					managed.closeErr = errors.Join(managed.closeErr, fmt.Errorf("kill unresponsive Runner: %w", killErr))
				}
				select {
				case waitErr = <-waitDone:
				case <-time.After(2 * time.Second):
					managed.closeErr = errors.Join(managed.closeErr, errors.New("Runner process was not reaped after kill"))
				}
			}
		}
		if waitErr != nil {
			managed.closeErr = errors.Join(managed.closeErr, fmt.Errorf("Runner process exit: %w", waitErr))
		}

		_ = managed.stdout.Close()
		stderrDrained := false
		select {
		case copyErr := <-managed.stderrDone:
			stderrDrained = true
			if copyErr != nil && !errors.Is(copyErr, os.ErrClosed) {
				managed.closeErr = errors.Join(managed.closeErr, fmt.Errorf("drain Runner stderr: %w", copyErr))
			}
		case <-time.After(2 * time.Second):
			_ = managed.stderr.Close()
			select {
			case copyErr := <-managed.stderrDone:
				stderrDrained = true
				if copyErr != nil && !errors.Is(copyErr, os.ErrClosed) {
					managed.closeErr = errors.Join(managed.closeErr, fmt.Errorf("drain Runner stderr after close: %w", copyErr))
				}
			case <-time.After(2 * time.Second):
				managed.closeErr = errors.Join(managed.closeErr, errors.New("Runner stderr reader did not terminate"))
			}
		}
		_ = managed.stderr.Close()

		if managed.closeErr != nil && stderrDrained && managed.stderrBuffer.Len() > 0 {
			managed.closeErr = fmt.Errorf("%w; Runner stderr=%q", managed.closeErr, managed.stderrBuffer.String())
		}
	})
	return managed.closeErr
}

func initializeMCP(t *testing.T, ctx context.Context, mcpClient *client.Client) {
	t.Helper()
	request := mcp.InitializeRequest{}
	request.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = mcp.Implementation{Name: "maestro-m0-test", Version: "1.0.0"}
	request.Params.Capabilities = mcp.ClientCapabilities{}
	result, err := mcpClient.Initialize(ctx, request)
	if err != nil {
		t.Fatalf("MCP initialize: %v", err)
	}
	if result.ServerInfo.Name == "" || result.ProtocolVersion == "" {
		t.Fatalf("incomplete MCP initialize result: %+v", result)
	}
}

func assertMCPToolCatalog(t *testing.T, ctx context.Context, mcpClient *client.Client) {
	t.Helper()
	result, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("MCP tools/list: %v", err)
	}
	if len(result.Tools) == 0 {
		t.Fatal("MCP tools/list returned an empty catalog")
	}
	for _, tool := range result.Tools {
		switch tool.Name {
		case "merge_task":
			t.Fatal("merge_task must not be exposed by the M0 MCP server")
		case "claim_batch":
			t.Fatal("claim_batch must not bypass the one-active-execution Worker invariant")
		case "release_worker":
			t.Fatal("release_worker must not bypass Lease cancellation and recovery")
		}
	}
	if !slices.ContainsFunc(result.Tools, func(tool mcp.Tool) bool { return tool.Name == "heartbeat_task" }) {
		t.Fatal("heartbeat_task must be present so work longer than one Lease window remains authorized")
	}
}

func runBinary(t *testing.T, arguments ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, maestroBinary, arguments...)
	command.Dir = repositoryRoot
	command.Env = append(os.Environ(), "MAESTRO_REMOTE_WRITE=false")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("maestro %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return output
}

func requireNoError(t *testing.T, err error, action string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

func createValidationRepository(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	requireNoError(t, os.MkdirAll(filepath.Join(workspace, "src"), 0o700), "create source directory")
	files := map[string]string{
		"go.mod":          "module example.invalid/maestro-m0-validation\n\ngo 1.25\n",
		".gitignore":      "coverage.out\n.maestro/\n",
		"src/add.go":      "package sample\n\nfunc Add(a, b int) int { return a + b }\n",
		"src/add_test.go": "package sample\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(2, 3) != 5 { t.Fatal(\"bad sum\") } }\n",
	}
	for name, content := range files {
		requireNoError(t, os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600), "write "+name)
	}
	runGit(t, workspace, "init", "-q")
	runGit(t, workspace, "add", ".")
	runGit(t, workspace, "-c", "user.name=M0 Test", "-c", "user.email=m0@example.invalid", "commit", "-q", "-m", "baseline")
	return workspace
}

func createRuntimeProject(t *testing.T, process *runtimeProcess, workspacePath string) string {
	t.Helper()
	requestBody, err := json.Marshal(map[string]any{"name": "M0 Validation", "workspace_path": workspacePath})
	requireNoError(t, err, "encode project")
	var response struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	requestJSON(t, http.MethodPost, process.baseURL+"/api/v1/projects", "Bearer "+process.token, string(requestBody), http.StatusOK, &response)
	if response.Data.ID == "" {
		t.Fatal("real HTTP project creation returned no ID")
	}
	return response.Data.ID
}

func createRuntimeFeature(t *testing.T, process *runtimeProcess, projectID string) string {
	t.Helper()
	var response struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	requestJSON(t, http.MethodPost, process.baseURL+"/api/v1/projects/"+projectID+"/features", "Bearer "+process.token,
		`{"title":"M0 validation feature","description":"real binary evidence"}`, http.StatusOK, &response)
	if response.Data.ID == "" {
		t.Fatal("real HTTP feature creation returned no ID")
	}
	return response.Data.ID
}

func createRuntimeTask(t *testing.T, process *runtimeProcess, projectID, featureID, requirements, title string) string {
	t.Helper()
	requestBody, err := json.Marshal(map[string]any{
		"feature_id": featureID, "title": title, "description": "exercise fixed M0 validation pipeline", "role": "backend",
		"allowed_directories": `["src"]`, "forbidden_patterns": `[]`, "test_requirements": requirements,
	})
	requireNoError(t, err, "encode task")
	var response struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	requestJSON(t, http.MethodPost, process.baseURL+"/api/v1/projects/"+projectID+"/tasks", "Bearer "+process.token,
		string(requestBody), http.StatusOK, &response)
	if response.Data.ID == "" {
		t.Fatal("real HTTP task creation returned no ID")
	}
	return response.Data.ID
}

func claimRuntimeTask(t *testing.T, process *runtimeProcess, database *sql.DB, projectID, taskID, sessionID, workerID string) string {
	t.Helper()
	requestBody, err := json.Marshal(map[string]string{"session_id": sessionID, "worker_id": workerID})
	requireNoError(t, err, "encode task claim")
	requestJSON(t, http.MethodPost, process.baseURL+"/api/v1/projects/"+projectID+"/tasks/"+taskID+"/claim", "Bearer "+process.token,
		string(requestBody), http.StatusOK, nil)
	var worktreePath string
	requireNoError(t, database.QueryRow(`SELECT worktree_path FROM worktrees WHERE project_id=? AND task_id=? AND status='active'`, projectID, taskID).Scan(&worktreePath), "read claimed worktree")
	return worktreePath
}

func callRealTool(t *testing.T, ctx context.Context, mcpClient *client.Client, name string, arguments map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	request := mcp.CallToolRequest{}
	request.Params.Name = name
	request.Params.Arguments = arguments
	result, err := mcpClient.CallTool(ctx, request)
	requireNoError(t, err, "call real MCP tool "+name)
	var content strings.Builder
	for _, item := range result.Content {
		switch typed := item.(type) {
		case mcp.TextContent:
			content.WriteString(typed.Text)
		case *mcp.TextContent:
			content.WriteString(typed.Text)
		}
	}
	return result, content.String()
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type runtimeProcess struct {
	command *exec.Cmd
	stderr  lockedBuffer
	done    chan error
	baseURL string
	token   string
	stopped bool
}

func startRuntime(t *testing.T, databasePath string, remoteWrite bool) *runtimeProcess {
	t.Helper()
	return startRuntimeProcess(t, []string{"server", "--db", databasePath}, remoteWrite)
}

func startRuntimeWithConfig(t *testing.T, configPath string, remoteWrite bool) *runtimeProcess {
	t.Helper()
	return startRuntimeProcess(t, []string{"server", "--config", configPath}, remoteWrite)
}

func startRuntimeProcess(t *testing.T, baseArguments []string, remoteWrite bool) *runtimeProcess {
	t.Helper()
	address := reserveAddress(t)
	process := &runtimeProcess{
		baseURL: "http://" + address,
		token:   "m0-integration-token",
	}
	arguments := append(append([]string(nil), baseArguments...), "--http", address, "--shutdown-timeout", "5s")
	process.command = exec.Command(maestroBinary, arguments...)
	process.command.Dir = repositoryRoot
	process.command.Env = append(
		os.Environ(),
		"MAESTRO_AUTH_TOKEN="+process.token,
		fmt.Sprintf("MAESTRO_REMOTE_WRITE=%t", remoteWrite),
	)
	process.command.Stdout = io.Discard
	process.command.Stderr = &process.stderr
	if err := process.command.Start(); err != nil {
		t.Fatalf("start real Maestro server: %v", err)
	}
	process.done = make(chan error, 1)
	go func() { process.done <- process.command.Wait() }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := (&http.Client{Timeout: 500 * time.Millisecond}).Get(process.baseURL + "/readyz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return process
			}
		}
		select {
		case err := <-process.done:
			t.Fatalf("Maestro server exited before readiness (%v): %s", err, process.stderr.String())
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = process.command.Process.Kill()
	<-process.done
	t.Fatalf("Maestro server did not become ready: %s", process.stderr.String())
	return nil
}

func (process *runtimeProcess) stop(t *testing.T) {
	t.Helper()
	if process.stopped {
		return
	}
	process.stopped = true
	if err := process.command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal Maestro server: %v", err)
	}
	select {
	case err := <-process.done:
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 130 {
			t.Fatalf("graceful shutdown must exit 130 after SIGINT, got %v: %s", err, process.stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = process.command.Process.Kill()
		<-process.done
		t.Fatalf("Maestro server did not drain within 10s: %s", process.stderr.String())
	}
}

func reserveAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve HTTP address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release HTTP address: %v", err)
	}
	return address
}

func assertStatus(t *testing.T, method, url, authorization, body string, expected int) {
	t.Helper()
	assertStatusWithOrigin(t, method, url, authorization, "", expected, body)
}

func assertStatusWithOrigin(t *testing.T, method, url, authorization, origin string, expected int, body ...string) {
	t.Helper()
	requestBody := ""
	if len(body) > 0 {
		requestBody = body[0]
	}
	request, err := http.NewRequest(method, url, strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("create HTTP request: %v", err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if requestBody != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode != expected {
		t.Fatalf("%s %s: got %d, want %d; body=%s", method, url, response.StatusCode, expected, responseBody)
	}
}

func requestJSON(t *testing.T, method, url, authorization, body string, expected int, target any) {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create HTTP request: %v", err)
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, url, err)
	}
	if response.StatusCode != expected {
		t.Fatalf("%s %s: got %d, want %d; body=%s", method, url, response.StatusCode, expected, responseBody)
	}
	if target != nil {
		if err := json.Unmarshal(responseBody, target); err != nil {
			t.Fatalf("decode %s %s response: %v\n%s", method, url, err, responseBody)
		}
	}
}

func environmentWithOverrides(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[key]; !overridden {
			environment = append(environment, entry)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func gitRevParse(t *testing.T, worktree string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	command.Dir = worktree
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD in %s: %v", worktree, err)
	}
	return strings.TrimSpace(string(output))
}
