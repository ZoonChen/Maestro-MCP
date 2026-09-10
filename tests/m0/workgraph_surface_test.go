package m0_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// J2c work-graph/asset MCP surface against the REAL stdio JSON-RPC
// protocol (docs/testing/mcp-test-guide.md: no REST substitution) on the
// PostgreSQL control plane. Two real runners carry the separation of
// duties: the registering session owns the asset, the second session
// reviews and releases it. Skipped without MAESTRO_TEST_POSTGRES_DSN so
// `make smoke`-style runs stay green on SQLite-only environments.

func j2cPostgresEnv(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	return dsn
}

// j2cTestDatabase provisions a dedicated database (admin DSN -> created
// db) and applies the full migration chain the same way the operator
// does (the real binary's migrate up), never touching shared state.
func j2cTestDatabase(t *testing.T) (*store.PostgresStore, string, string) {
	t.Helper()
	adminDSN := j2cPostgresEnv(t)
	base := adminDSN[:strings.LastIndex(adminDSN, "/")+1]
	admin, err := store.OpenPostgres(context.Background(), adminDSN)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_m0_j2c_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_m0_j2c_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_m0_j2c_test WITH (FORCE)`)
		_ = admin.Close()
	})
	testDSN := base + "maestro_m0_j2c_test"

	// The runtime core always carries its SQLite local baseline; the
	// PostgreSQL control plane composes on top (database.driver), so the
	// runner config carries both and each chain is migrated through the
	// real binary the way an operator would.
	localDB := filepath.Join(t.TempDir(), "j2c-local.db")
	runBinary(t, "migrate", "up", "--db", localDB)
	configPath := filepath.Join(t.TempDir(), "j2c-runner.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(
		"db_path: %q\nhttp_addr: \"127.0.0.1:0\"\nremote_write: false\ndatabase:\n  driver: postgres\n", localDB)), 0o600))
	runBinaryEnv(t, append(os.Environ(), "MAESTRO_DATABASE_DSN="+testDSN), "migrate", "up", "--config", configPath)

	db, err := store.OpenPostgres(context.Background(), testDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	pgStore, err := store.NewPostgresStore(db)
	require.NoError(t, err)
	return pgStore, testDSN, configPath
}

func runBinaryEnv(t *testing.T, environment []string, arguments ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, maestroBinary, arguments...)
	command.Env = environment
	output, err := command.CombinedOutput()
	requireNoError(t, err, fmt.Sprintf("run maestro %s: %s", strings.Join(arguments, " "), string(output)))
}

// j2cStartRunner boots one real stdio runner against the test database.
func j2cStartRunner(t *testing.T, testDSN, configPath, runnerID, projectID string) *client.Client {
	t.Helper()
	process, err := startManagedStdioMCPClient(
		maestroBinary,
		append(os.Environ(), "MAESTRO_DATABASE_DSN="+testDSN, "MAESTRO_REMOTE_WRITE=false"),
		"runner", "--config", configPath, "--runner-id", runnerID, "--project", projectID,
	)
	requireNoError(t, err, "start "+runnerID)
	t.Cleanup(func() {
		if closeErr := process.Close(); closeErr != nil {
			t.Errorf("close %s: %v", runnerID, closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	t.Cleanup(cancel)
	initializeMCP(t, ctx, process.Client)
	return process.Client
}

func TestWorkGraphAssetToolsRealMCPProtocol(t *testing.T) {
	pgStore, testDSN, configPath := j2cTestDatabase(t)
	ctx := context.Background()

	// Seed: project, one active WorkPattern and one plan with a root
	// work package (quorum join) — all through the real store surface.
	const teamID = "018f6100-0000-7000-8000-0000000000c1"
	const projectID = "018f6200-0000-7000-8000-0000000000c1"
	seed := pgStore.DB()
	_, err := seed.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'team-j2c')`, teamID)
	require.NoError(t, err)
	_, err = seed.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'j2c-surface', 'J2c surface project', 'active')`,
		projectID, teamID)
	require.NoError(t, err)

	graph := pgStore.WorkGraph()
	pattern, err := graph.CreateWorkPattern(ctx, store.WorkPattern{
		ProjectID: projectID, Name: "slice-by-capability", Version: 1, Status: "active",
		Body: []byte(`{"template":"slice-by-capability","fanout":"capability"}`), CreatedBy: "seed",
	})
	require.NoError(t, err)

	plan, err := graph.CreateWorkPlan(ctx, store.CreateWorkPlanInput{
		ProjectID: projectID, Title: "J2c protocol plan", HumanCode: "MST-WP-00042",
		RootSpec: []byte(`{"title":"试点根包","success_threshold":{"kind":"quorum","k":2}}`), Actor: "seed",
	})
	require.NoError(t, err)

	owner := j2cStartRunner(t, testDSN, configPath, "j2c-owner", projectID)
	reviewer := j2cStartRunner(t, testDSN, configPath, "j2c-reviewer", projectID)

	t.Run("graph query returns tree status and join policy", func(t *testing.T) {
		result, text := callRealTool(t, ctx, owner, "worktree_graph_query", map[string]any{"plan_id": plan.ID})
		require.False(t, result.IsError, text)
		var payload struct {
			Plan struct {
				ID           string `json:"id"`
				GraphVersion int64  `json:"graph_version"`
				Status       string `json:"status"`
			} `json:"plan"`
			Nodes []struct {
				ID       string `json:"id"`
				Status   string `json:"status"`
				Depth    int    `json:"depth"`
				NodeType string `json:"node_type"`
			} `json:"nodes"`
			NodeSpecs []struct {
				NodeID           string `json:"node_id"`
				SuccessThreshold *struct {
					Kind string `json:"kind"`
					K    int    `json:"k"`
				} `json:"success_threshold"`
			} `json:"node_specs"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &payload))
		assert.Equal(t, plan.ID, payload.Plan.ID)
		require.Len(t, payload.Nodes, 1) // the seeded root package
		assert.Equal(t, "work_package", payload.Nodes[0].NodeType)
		require.Len(t, payload.NodeSpecs, 1)
		require.NotNil(t, payload.NodeSpecs[0].SuccessThreshold)
		assert.Equal(t, "quorum", payload.NodeSpecs[0].SuccessThreshold.Kind)
		assert.Equal(t, 2, payload.NodeSpecs[0].SuccessThreshold.K)
	})

	t.Run("decomposition proposal applies behind the graph CAS", func(t *testing.T) {
		result, text := callRealTool(t, ctx, owner, "decomposition_propose", map[string]any{
			"plan_id":                plan.ID,
			"expected_graph_version": float64(plan.GraphVersion),
			"idempotency_key":        "j2c-real-propose-0001",
			"work_pattern":           map[string]any{"pattern_id": pattern.ID, "version": 1},
			"nodes": []map[string]any{{
				"local_id": "api", "parent": "ext:" + plan.RootNodeID, "node_type": "work_item",
				"slot_key": "api.contract", "human_code": "MST-WI-00421",
				"spec": map[string]any{
					"title": "API 契约切片", "repo": "peixun-java",
					"baseline_sha":        "0123456789012345678901234567890123456789",
					"workspace_paths":     []string{"services/api"},
					"owning_capability":   "backend.contract",
					"budget_units":        float64(100),
					"acceptance_criteria": []string{"API 契约通过 OpenAPI 机检"},
				},
			}},
		})
		require.False(t, result.IsError, text)
		var record struct {
			Status     string          `json:"status"`
			Violations json.RawMessage `json:"violations"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &record))
		assert.Equal(t, "applied", record.Status)
		assert.Equal(t, "null", string(record.Violations))

		// A stale CAS token replays as a conflict, never a silent apply.
		stale := map[string]any{
			"plan_id": plan.ID, "expected_graph_version": float64(1),
			"idempotency_key": "j2c-real-propose-0002",
			"work_pattern":    map[string]any{"pattern_id": pattern.ID, "version": 1},
			"nodes": []map[string]any{{
				"local_id": "web", "parent": "ext:" + plan.RootNodeID, "node_type": "work_item",
				"slot_key": "web.contract", "human_code": "MST-WI-00422",
				"spec": map[string]any{
					"title": "Web 契约切片", "repo": "peixun-vue",
					"baseline_sha":        "0123456789012345678901234567890123456789",
					"workspace_paths":     []string{"web/src"},
					"owning_capability":   "frontend.contract",
					"acceptance_criteria": []string{"页面通过契约快照测试"},
				},
			}},
		}
		result, text = callRealTool(t, ctx, owner, "decomposition_propose", stale)
		require.True(t, result.IsError, "stale CAS must fail closed")
		assert.Contains(t, text, "GRAPH_VERSION_MISMATCH")

		// The violation channel: a proposal node without its owning
		// capability is REJECTED with the stable code, not applied.
		bad := map[string]any{
			"plan_id": plan.ID, "expected_graph_version": float64(plan.GraphVersion + 1),
			"idempotency_key": "j2c-real-propose-0003",
			"work_pattern":    map[string]any{"pattern_id": pattern.ID, "version": 1},
			"nodes": []map[string]any{{
				"local_id": "ops", "parent": "ext:" + plan.RootNodeID, "node_type": "work_item",
				"slot_key": "ops.contract", "human_code": "MST-WI-00423",
				"spec": map[string]any{
					"title": "Ops 切片", "repo": "peixun-java",
					"baseline_sha":        "0123456789012345678901234567890123456789",
					"workspace_paths":     []string{"ops"},
					"acceptance_criteria": []string{"运维手册过机检"},
				},
			}},
		}
		result, text = callRealTool(t, ctx, owner, "decomposition_propose", bad)
		require.False(t, result.IsError, text) // rejection is a decided outcome, not a transport error
		require.NoError(t, json.Unmarshal([]byte(text), &record))
		assert.Equal(t, "rejected", record.Status)
		assert.Contains(t, string(record.Violations), "WGP-RS-009")
	})

	t.Run("asset ledger lifecycle across two runners", func(t *testing.T) {
		digest := "sha256:" + strings.Repeat("ab", 32)
		registerArgs := map[string]any{
			"asset_id":        "ART-blueprint-042",
			"asset_type":      "blueprint",
			"title":           "试点蓝图（真协议）",
			"sensitivity":     "internal",
			"source_digest":   digest,
			"content_ref":     "assets/ART-blueprint-042/blueprint.md",
			"idempotency_key": "j2c-real-register-0001",
		}
		result, text := callRealTool(t, ctx, owner, "asset_register", registerArgs)
		require.False(t, result.IsError, text)
		var registered struct {
			Version        int    `json:"version"`
			Status         string `json:"status"`
			OwnerPrincipal string `json:"owner_principal"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &registered))
		assert.Equal(t, 1, registered.Version)
		assert.Equal(t, "session:j2c-owner-session", registered.OwnerPrincipal)

		// The owning session may neither review nor release (separation).
		result, text = callRealTool(t, ctx, owner, "asset_review", map[string]any{
			"asset_id": "ART-blueprint-042", "version": float64(1), "idempotency_key": "j2c-real-review-0001",
		})
		require.True(t, result.IsError, "owner self-review must fail closed")
		assert.Contains(t, text, "FORBIDDEN")

		// The second runner reviews and releases.
		result, text = callRealTool(t, ctx, reviewer, "asset_review", map[string]any{
			"asset_id": "ART-blueprint-042", "version": float64(1), "idempotency_key": "j2c-real-review-0002",
		})
		require.False(t, result.IsError, text)
		result, text = callRealTool(t, ctx, reviewer, "asset_approve", map[string]any{
			"asset_id": "ART-blueprint-042", "version": float64(1), "idempotency_key": "j2c-real-approve-0002",
		})
		require.False(t, result.IsError, text)
		var approved struct {
			Status string `json:"status"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &approved))
		assert.Equal(t, "approved", approved.Status)

		// The ledger query returns the chain, sensitivity-filterable.
		result, text = callRealTool(t, ctx, reviewer, "asset_query", map[string]any{"asset_id": "ART-blueprint-042"})
		require.False(t, result.IsError, text)
		assert.Contains(t, text, `"status":"approved"`)
	})
}

// TestWorkGraphAssetToolsSQLiteHonestDegradation runs against the m0
// SQLite binary: the six tools are listed and answer with explicit
// boundary states, never fabricated data or silent success.
func TestWorkGraphAssetToolsSQLiteHonestDegradation(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "j2c-sqlite.db")
	configPath := filepath.Join(t.TempDir(), "j2c-sqlite.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("db_path: %q\nhttp_addr: \"127.0.0.1:0\"\nremote_write: false\n", databasePath)), 0o600))
	runBinary(t, "migrate", "up", "--config", configPath)

	process, err := startManagedStdioMCPClient(
		maestroBinary,
		append(os.Environ(), "MAESTRO_REMOTE_WRITE=false"),
		"runner", "--config", configPath, "--runner-id", "j2c-sqlite", "--project", "project-j2c-sqlite",
	)
	requireNoError(t, err, "start SQLite runner")
	t.Cleanup(func() {
		if closeErr := process.Close(); closeErr != nil {
			t.Errorf("close SQLite runner: %v", closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	initializeMCP(t, ctx, process.Client)
	assertMCPToolCatalog(t, ctx, process.Client)

	result, text := callRealTool(t, ctx, process.Client, "worktree_graph_query", map[string]any{"plan_id": "plan-any"})
	require.False(t, result.IsError, text)
	assert.Contains(t, text, `"available":false`)

	result, text = callRealTool(t, ctx, process.Client, "decomposition_propose", map[string]any{
		"plan_id": "plan-any", "expected_graph_version": float64(1),
		"idempotency_key": "j2c-sqlite-propose-0001",
		"work_pattern":    map[string]any{"pattern_id": "pattern-any", "version": 1},
		"nodes": []map[string]any{{
			"local_id": "api", "parent": "ext:root", "node_type": "work_item",
			"slot_key": "api", "human_code": "MST-WI-00001",
			"spec": map[string]any{"title": "sqlite degradation"},
		}},
	})
	require.True(t, result.IsError, "mutating tools must fail closed without the store")
	assert.Contains(t, text, "OPERATION_DISABLED")

	result, text = callRealTool(t, ctx, process.Client, "asset_query", map[string]any{})
	require.False(t, result.IsError, text)
	assert.Contains(t, text, `"available":false`)
}
