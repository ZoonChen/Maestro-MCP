package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplicationHealthAuthAndMCPTransport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a, err := New(context.Background(), Options{
		DBPath:           filepath.Join(t.TempDir(), "maestro.db"),
		MaintenanceOwner: true,
		AuthToken:        "test-token",
		RemoteWrite:      false,
		DataGCInterval:   time.Hour,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })

	// Handler-level test servers do not call ServeHTTP, so explicitly model the
	// point immediately after a listener has been bound.
	a.ready.Store(true)
	server := httptest.NewServer(a.Handler())
	defer server.Close()

	for _, path := range []string{"/livez", "/readyz"} {
		response, requestErr := http.Get(server.URL + path)
		require.NoError(t, requestErr)
		assert.Equal(t, http.StatusOK, response.StatusCode, path)
		response.Body.Close()
	}

	response, err := http.Get(server.URL + "/dashboard")
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	response.Body.Close()

	initialize := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "m0-http-smoke",
				"version": "1.0.0",
			},
		},
	}

	response = postMCP(t, server.URL+"/mcp", initialize, "", "")
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	response.Body.Close()

	response = postMCP(t, server.URL+"/mcp", initialize, "wrong-token", "")
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	response.Body.Close()

	response = postMCP(t, server.URL+"/mcp", initialize, "test-token", "")
	require.Equal(t, http.StatusOK, response.StatusCode)
	sessionID := response.Header.Get("Mcp-Session-Id")
	require.NotEmpty(t, sessionID)
	var initializeResponse struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&initializeResponse))
	response.Body.Close()
	assert.Equal(t, "2.0", initializeResponse.JSONRPC)
	assert.Equal(t, 1, initializeResponse.ID)
	assert.Equal(t, "2025-11-25", initializeResponse.Result.ProtocolVersion)

	listTools := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	}
	response = postMCP(t, server.URL+"/mcp", listTools, "test-token", sessionID)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var toolsResponse struct {
		ID     int `json:"id"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&toolsResponse))
	response.Body.Close()
	require.NotEmpty(t, toolsResponse.Result.Tools)
	for _, tool := range toolsResponse.Result.Tools {
		assert.NotEqual(t, "merge_task", tool.Name)
	}
}

func TestApplicationCloseStopsTrackedBackgroundWork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a, err := New(context.Background(), Options{
		DBPath:               filepath.Join(t.TempDir(), "maestro.db"),
		MaintenanceOwner:     true,
		AuthToken:            "test-token",
		StaleScannerInterval: 5 * time.Millisecond,
		DataGCInterval:       5 * time.Millisecond,
	})
	require.NoError(t, err)
	a.ready.Store(true)

	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Application.Close did not stop tracked background work")
	}
	assert.Error(t, a.Ready(context.Background()))
	require.NoError(t, a.Close(), "Close must be idempotent")
}

func TestBeginDrainImmediatelyStopsNewWritesButKeepsLiveness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a, err := New(context.Background(), Options{
		DBPath: filepath.Join(t.TempDir(), "maestro.db"), MaintenanceOwner: true,
		AuthToken: "test-token", RemoteWrite: true, DataGCInterval: time.Hour,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	a.ready.Store(true)
	server := httptest.NewServer(a.Handler())
	defer server.Close()

	a.BeginDrain()
	livez, err := http.Get(server.URL + "/livez")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, livez.StatusCode)
	livez.Body.Close()
	readyz, err := http.Get(server.URL + "/readyz")
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, readyz.StatusCode)
	readyz.Body.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/projects", strings.NewReader(`{}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	var payload map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))
	assert.Equal(t, "RUNTIME_DRAINING", payload["error_code"])
	assert.NotEmpty(t, payload["correlation_id"])
}

func TestRuntimeLockCanonicalizesAliasesAndRejectsDatabaseSymlink(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(realDirectory, 0o700))
	aliasDirectory := filepath.Join(root, "alias")
	if err := os.Symlink(realDirectory, aliasDirectory); err != nil {
		t.Skipf("create directory symlink: %v", err)
	}

	realDatabase := filepath.Join(realDirectory, "maestro.db")
	first, err := AcquireRuntimeLock(realDatabase)
	require.NoError(t, err)
	require.NotNil(t, first)

	workingDirectory, err := os.Getwd()
	require.NoError(t, err)
	relativeDatabase, err := filepath.Rel(workingDirectory, realDatabase)
	require.NoError(t, err)
	_, err = AcquireRuntimeLock(relativeDatabase)
	require.ErrorContains(t, err, "another Maestro server owns this database")

	aliasDatabase := filepath.Join(aliasDirectory, "maestro.db")
	_, err = AcquireRuntimeLock(aliasDatabase)
	require.ErrorContains(t, err, "another Maestro server owns this database")
	require.NoError(t, first.Close())

	afterRelease, err := AcquireRuntimeLock(aliasDatabase)
	require.NoError(t, err)
	require.NotNil(t, afterRelease)
	require.NoError(t, afterRelease.Close())

	realTarget := filepath.Join(realDirectory, "target.db")
	require.NoError(t, os.WriteFile(realTarget, nil, 0o600))
	databaseSymlink := filepath.Join(realDirectory, "database-link.db")
	require.NoError(t, os.Symlink(realTarget, databaseSymlink))
	_, err = AcquireRuntimeLock(databaseSymlink)
	require.ErrorContains(t, err, "database path must not be a symbolic link")

	lockSymlinkDatabase := filepath.Join(realDirectory, "lock-link.db")
	lockTarget := filepath.Join(realDirectory, "lock-target")
	require.NoError(t, os.WriteFile(lockTarget, nil, 0o600))
	require.NoError(t, os.Symlink(lockTarget, lockSymlinkDatabase+".runtime.lock"))
	_, err = AcquireRuntimeLock(lockSymlinkDatabase)
	require.ErrorContains(t, err, "runtime lock path must not be a symbolic link")
}

func postMCP(t *testing.T, url string, payload any, token, sessionID string) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		request.Header.Set("Mcp-Session-Id", sessionID)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	if response.StatusCode >= 500 {
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("MCP endpoint returned %s: %s", response.Status, data)
	}
	return response
}
