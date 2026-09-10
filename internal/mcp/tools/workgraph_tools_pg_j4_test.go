package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

// J4 (CR-1 terminal state), PG-gated: the asset review→approve walk
// runs against the real asset ledger for BOTH functional planes —
// qa_owner and technical_lead each walk one asset through the real
// handlers, the coarse asset.approve grant serves both, and the
// production session-role derivation (the stdio delegated context)
// still cannot release.

func newJ4ToolsFixture(t *testing.T) (*Services, *ToolGuard, string) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	index := strings.LastIndex(dsn, "/")
	require.GreaterOrEqual(t, index, 0, "MAESTRO_TEST_POSTGRES_DSN has no database path")
	const dbName = "maestro_tools_j4_test"
	admin, err := store.OpenPostgres(context.Background(), dsn)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), fmt.Sprintf("CREATE DATABASE %s", dbName))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		_ = admin.Close()
	})

	db, err := store.OpenPostgres(context.Background(), dsn[:index+1]+dbName)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7500-0000-7000-8000-000000000021', 'j4 tools team')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ('018f7500-0000-7000-8000-000000000022', '018f7500-0000-7000-8000-000000000021', 'j4-tools', 'J4 Tools', 'active')`)
	require.NoError(t, err)

	const projectID = "018f7500-0000-7000-8000-000000000022"
	services := &Services{
		Binding: &TransportBinding{
			ProjectID: projectID,
			SessionID: "owner-session",
			WorkerID:  "owner-worker",
		},
		Assets: pg.Assets(),
	}
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	guard, err := NewToolGuard(policy)
	require.NoError(t, err)
	return services, guard, projectID
}

func j4RegisterRequest(assetID, title, key string) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"asset_id":        assetID,
		"asset_type":      "prd",
		"title":           title,
		"sensitivity":     "internal",
		"source_digest":   "sha256:" + strings.Repeat("b7", 32),
		"content_ref":     "assets/" + assetID + "/v1.md",
		"idempotency_key": key,
	}
	return req
}

func j4TransitionRequest(assetID, key string) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"asset_id":        assetID,
		"version":         float64(1),
		"idempotency_key": key,
	}
	return req
}

func j4Review(t *testing.T, services *Services, req mcp.CallToolRequest) {
	t.Helper()
	result, err := handleAssetTransition(ctxBG(), req, services, "review",
		func(ctx context.Context, assetID string, version int, actor string) (*store.Asset, error) {
			return services.Assets.ReviewAsset(ctx, assetID, version, actor)
		})
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
}

func j4Approve(t *testing.T, services *Services, assetID string, req mcp.CallToolRequest) *store.Asset {
	t.Helper()
	result, err := handleAssetTransition(ctxBG(), req, services, "approve",
		func(ctx context.Context, assetID string, version int, actor string) (*store.Asset, error) {
			return services.Assets.ApproveAsset(ctx, assetID, version, actor)
		})
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	approved, err := services.Assets.GetAsset(ctxBG(), assetID, 1)
	require.NoError(t, err)
	return approved
}

// j4FunctionalPrincipal is the guard-side authority for one function
// over a viewer membership (the J1 dual-track: the MCP session carries
// the actor identity, the functional grant carries the permission).
func j4FunctionalPrincipal(projectID, function string) *model.PrincipalContext {
	return &model.PrincipalContext{
		PrincipalID:        "user:" + function,
		ProjectMemberships: map[string]string{projectID: "viewer"},
		FunctionalRoles:    []string{function},
	}
}

func TestFunctionalOwnersWalkAssetReviewApprovePG(t *testing.T) {
	services, guard, projectID := newJ4ToolsFixture(t)

	// The coarse grants serve both functions: review and approve allow
	// for qa_owner AND technical_lead over the same asset plane.
	for _, function := range []string{"qa_owner", "technical_lead"} {
		for _, tool := range []string{"asset_review", "asset_approve"} {
			decision, err := guard.Authorize(context.Background(), tool, j4FunctionalPrincipal(projectID, function), projectID)
			require.NoError(t, err)
			assert.True(t, decision.Allow, "%s over %s: %v", tool, function, decision.Reasons)
		}
	}

	// qa_owner walks asset one end to end.
	registered := mcp.CallToolRequest{}
	registered.Params.Arguments = j4RegisterRequest("ART-prd-101", "J4 走查制品一", "j4-pg-register-000000001").Params.Arguments
	result, err := handleAssetRegister(ctxBG(), registered, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))

	// The registering session may not review its own asset (SoD); the
	// functional session continues the walk.
	selfReview := j4TransitionRequest("ART-prd-101", "j4-pg-selfreview-00001")
	denied, err := handleAssetTransition(ctxBG(), selfReview, services, "review",
		func(ctx context.Context, assetID string, version int, actor string) (*store.Asset, error) {
			return services.Assets.ReviewAsset(ctx, assetID, version, actor)
		})
	require.NoError(t, err)
	assert.True(t, denied.IsError, "the registering session must not review")
	assert.Contains(t, mcpResultText(denied), "separation of duties")

	services.Binding.SessionID = "qa-owner-session"
	j4Review(t, services, j4TransitionRequest("ART-prd-101", "j4-pg-qa-review-000001"))
	approved := j4Approve(t, services, "ART-prd-101", j4TransitionRequest("ART-prd-101", "j4-pg-qa-approve-0001"))
	assert.Equal(t, store.AssetStatusApproved, approved.Status)
	assert.NotEmpty(t, approved.ReviewedAt)
	assert.NotEmpty(t, approved.ApprovedAt)

	// technical_lead walks asset two end to end.
	services.Binding.SessionID = "owner-session-2"
	second := mcp.CallToolRequest{}
	second.Params.Arguments = j4RegisterRequest("ART-prd-102", "J4 走查制品二", "j4-pg-register-000000002").Params.Arguments
	result, err = handleAssetRegister(ctxBG(), second, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))

	services.Binding.SessionID = "tl-owner-session"
	j4Review(t, services, j4TransitionRequest("ART-prd-102", "j4-pg-tl-review-000001"))
	approved = j4Approve(t, services, "ART-prd-102", j4TransitionRequest("ART-prd-102", "j4-pg-tl-approve-0001"))
	assert.Equal(t, store.AssetStatusApproved, approved.Status)

	// The ledger reflects both approved versions through the read tool.
	query := mcp.CallToolRequest{}
	query.Params.Arguments = map[string]any{"status": "approved"}
	result, err = handleAssetQuery(ctxBG(), query, services)
	require.NoError(t, err)
	require.False(t, result.IsError, mcpResultText(result))
	assert.Contains(t, mcpResultText(result), "ART-prd-101")
	assert.Contains(t, mcpResultText(result), "ART-prd-102")
}

func TestProductionSessionRolesNeverReleaseAssetsPG(t *testing.T) {
	services, guard, projectID := newJ4ToolsFixture(t)

	// The production stdio derivation: backend/frontend/devops sessions
	// authorize as developer, verifier as verifier, coordinator as
	// coordinator. Register/propose stay reachable; the release never is.
	for _, sessionRole := range []string{"backend", "frontend", "devops"} {
		decision, err := guard.Authorize(context.Background(), "asset_register",
			guard.Principal(services.Binding, sessionRole), projectID)
		require.NoError(t, err)
		assert.True(t, decision.Allow, "%s registers: %v", sessionRole, decision.Reasons)

		decision, err = guard.Authorize(context.Background(), "asset_approve",
			guard.Principal(services.Binding, sessionRole), projectID)
		require.NoError(t, err)
		assert.False(t, decision.Allow, "%s sessions never release assets", sessionRole)
	}
	decision, err := guard.Authorize(context.Background(), "asset_approve",
		guard.Principal(services.Binding, "coordinator"), projectID)
	require.NoError(t, err)
	assert.False(t, decision.Allow, "coordinator sessions never release assets")

	// And an unknown task role authorizes nothing.
	decision, err = guard.Authorize(context.Background(), "asset_query",
		guard.Principal(services.Binding, "intruder"), projectID)
	require.NoError(t, err)
	assert.False(t, decision.Allow)
}
