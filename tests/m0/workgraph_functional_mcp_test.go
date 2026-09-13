package m0_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/app"
	"github.com/ZoonChen/Maestro-MCP/internal/handler"
	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	maestrotools "github.com/ZoonChen/Maestro-MCP/internal/mcp/tools"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// W5-1: functional principals reach asset_review / asset_approve over
// the REAL MCP Streamable HTTP protocol with REAL bearer verification
// (docs/testing/mcp-test-guide.md: no REST substitution). The first-run
// gap was that the MCP guard synthesized the principal from the session
// task role only, so no functional authority could ever ride an MCP
// call; the identity bridge now authorizes /mcp calls with the
// server-resolved principal (memberships + functional roles) through
// the same frozen policy REST uses.
//
// The walk also carries the W5-2 multi-sign release-note contract end
// to end over MCP: the product_owner signoff alone leaves the version
// reviewed, the technical_lead signoff completes the pair and flips it.

// w5IdP is a minimal but honest OIDC issuer: discovery, JWKS and ES256
// tokens in the RFC 7515 raw R||S wire form (the same shape the
// identity package verifies).
type w5IdP struct {
	server *httptest.Server
	key    *ecdsa.PrivateKey
	kid    string
	issuer string
}

func newW5IdP(t *testing.T) *w5IdP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	idp := &w5IdP{key: key, kid: "w5-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, idp.issuer, idp.issuer+"/certs")
	})
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Uncompressed point 0x04 || X || Y sliced into the JWKS
		// coordinates (PublicKey.X/.Y are deprecated since Go 1.26).
		point, pointErr := idp.key.PublicKey.Bytes()
		if pointErr != nil {
			http.Error(w, "point encode failed", http.StatusInternalServerError)
			return
		}
		x := base64.RawURLEncoding.EncodeToString(point[1:33])
		y := base64.RawURLEncoding.EncodeToString(point[33:65])
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"EC","crv":"P-256","use":"sig","kid":%q,"x":%q,"y":%q}]}`,
			idp.kid, x, y)
	})
	idp.server = httptest.NewTLSServer(mux)
	idp.issuer = idp.server.URL
	t.Cleanup(idp.server.Close)
	return idp
}

// Client returns an HTTP client that trusts the issuer's test CA (the
// verifier runs inside the composed application).
func (idp *w5IdP) Client() *http.Client {
	return idp.server.Client()
}

func (idp *w5IdP) token(t *testing.T, subject string) string {
	t.Helper()
	now := time.Now().UTC()
	claims := map[string]any{
		"iss": idp.issuer, "sub": subject,
		"aud": []string{"maestro-console"},
		"exp": now.Add(30 * time.Minute).Unix(), "nbf": now.Add(-1 * time.Minute).Unix(),
	}
	header := map[string]any{"alg": "ES256", "kid": idp.kid}
	encode := func(value any) string {
		payload, err := json.Marshal(value)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(payload)
	}
	signed := encode(header) + "." + encode(claims)
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, idp.key, digest[:])
	require.NoError(t, err)
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	s.FillBytes(raw[32:])
	return signed + "." + base64.RawURLEncoding.EncodeToString(raw)
}

// startW5App composes the REAL application (the same composition root
// cmd/maestro wires): the full gin router chain with the OIDC identity
// mount, the armed MCP guard and the J2c tool stores. The runner-path
// parts of the brief stay covered by the real binary via stdio above;
// the macOS host ignores SSL_CERT_FILE, so the TLS IdP is trusted by
// handing the verifier the issuer's own client — the exact trust the
// containerized production deployment gets from its CA mount.
func startW5App(t *testing.T, pgStore *store.PostgresStore, idp *w5IdP) *httptest.Server {
	t.Helper()
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	verifier, err := identity.NewTokenVerifier(idp.issuer, "maestro-console", idp.Client())
	require.NoError(t, err)
	middleware := handler.NewOIDCMiddleware(policy, verifier, identity.NewStoreResolver(pgStore.Identities()))
	guard, err := maestrotools.NewToolGuard(policy)
	require.NoError(t, err)

	runtimeApp, err := app.New(context.Background(), app.Options{
		DBPath: filepath.Join(t.TempDir(), "w5-app.db"),
		// A scratch test deployment: the engine-wide remote-write guard
		// must let the authenticated mutating MCP calls through (the
		// production stack arms this via MAESTRO_REMOTE_WRITE=true).
		MaintenanceOwner: true,
		RemoteWrite:      true,
		Identity:         middleware.IdentityMount(),
		MCPGuard:         guard,
		MCPWorkGraph:     pgStore.WorkGraph(),
		MCPAssets:        pgStore.Assets(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtimeApp.Close() })

	server := httptest.NewServer(runtimeApp.Handler())
	t.Cleanup(server.Close)
	return server
}

// w5MCPClient connects one identity-bound MCP Streamable HTTP client.
func w5MCPClient(t *testing.T, baseURL, token string) *client.Client {
	t.Helper()
	mcpClient, err := client.NewStreamableHttpClient(baseURL+"/mcp",
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	require.NoError(t, mcpClient.Start(ctx))
	t.Cleanup(func() { _ = mcpClient.Close() })
	// The initialize handshake rides the same authenticated transport.
	initializeMCP(t, ctx, mcpClient)
	return mcpClient
}

func TestFunctionalPrincipalAssetApprovalRealStreamableMCP(t *testing.T) {
	pgStore, testDSN, configPath := j2cTestDatabase(t)
	ctx := context.Background()

	// Seed: one team/project; the registering owner is a delegated stdio
	// runner session, the approvers are identity principals.
	const teamID = "018f6100-0000-7000-8000-0000000000d1"
	const projectID = "018f6200-0000-7000-8000-0000000000d1"
	seed := pgStore.DB()
	_, err := seed.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'team-w5')`, teamID)
	require.NoError(t, err)
	_, err = seed.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'w5-mcp', 'W5 MCP surface project', 'active')`,
		projectID, teamID)
	require.NoError(t, err)

	idp := newW5IdP(t)
	productUser, err := pgStore.Identities().GetOrCreateUser(ctx, idp.issuer, "w5-product", "")
	require.NoError(t, err)
	techUser, err := pgStore.Identities().GetOrCreateUser(ctx, idp.issuer, "w5-technical", "")
	require.NoError(t, err)
	plainUser, err := pgStore.Identities().GetOrCreateUser(ctx, idp.issuer, "w5-plain", "")
	require.NoError(t, err)
	for _, userID := range []string{productUser.ID, techUser.ID, plainUser.ID} {
		_, err = seed.ExecContext(ctx,
			`INSERT INTO memberships (team_id, user_id, role) VALUES ($1, $2, 'developer')`, teamID, userID)
		require.NoError(t, err)
	}
	require.NoError(t, pgStore.Identities().GrantFunctionalRole(ctx, &store.FunctionalPrincipal{
		UserID: productUser.ID, Function: "product_owner",
		SourceRef: "W5-1 test grant: product plane over MCP",
	}))
	require.NoError(t, pgStore.Identities().GrantFunctionalRole(ctx, &store.FunctionalPrincipal{
		UserID: techUser.ID, Function: "technical_lead",
		SourceRef: "W5-1 test grant: technical plane over MCP",
	}))

	// The delegated runner session registers the release-note asset (the
	// owner stays a session principal, so separation of duties holds).
	runner := j2cStartRunner(t, testDSN, configPath, "w5-owner", projectID)
	digest := "sha256:" + strings.Repeat("e5", 32)
	result, text := callRealTool(t, ctx, runner, "asset_register", map[string]any{
		"asset_id": "ART-release-note-601", "asset_type": "release-note",
		"title":           "W5 首演放行单（真 Streamable HTTP 协议）",
		"sensitivity":     "internal",
		"source_digest":   digest,
		"content_ref":     "assets/ART-release-note-601/release-note.md",
		"summary":         `{"decision":"路线 B 主线","evidence":"AB 双轮实测"}`,
		"idempotency_key": "w5-real-register-0001",
	})
	require.False(t, result.IsError, text)
	assert.Contains(t, text, `"required_approver_roles":["product_owner","technical_lead"]`)

	server := startW5App(t, pgStore, idp)
	baseURL := server.URL
	techClient := w5MCPClient(t, baseURL, idp.token(t, "w5-technical"))

	// The technical_lead functional plane reviews over real MCP.
	result, text = callRealTool(t, ctx, techClient, "asset_review", map[string]any{
		"asset_id": "ART-release-note-601", "version": float64(1), "idempotency_key": "w5-real-review-0001",
	})
	require.False(t, result.IsError, text)
	assert.Contains(t, text, `"status":"reviewed"`)

	// W5-2 over MCP: the product_owner signature alone does NOT release
	// the release-note version; the partial signoff ledger is visible.
	productClient := w5MCPClient(t, baseURL, idp.token(t, "w5-product"))
	result, text = callRealTool(t, ctx, productClient, "asset_approve", map[string]any{
		"asset_id": "ART-release-note-601", "version": float64(1), "idempotency_key": "w5-real-approve-0001",
	})
	require.False(t, result.IsError, text)
	assert.Contains(t, text, `"status":"reviewed"`, "one signature must not release a multi-sign asset")
	assert.Contains(t, text, `"role":"product_owner"`)
	assert.Contains(t, text, `"principal":"user:`+productUser.ID+`"`)

	// The completing technical_lead signoff flips the version approved.
	result, text = callRealTool(t, ctx, techClient, "asset_approve", map[string]any{
		"asset_id": "ART-release-note-601", "version": float64(1), "idempotency_key": "w5-real-approve-0002",
	})
	require.False(t, result.IsError, text)
	assert.Contains(t, text, `"status":"approved"`, "both required roles signed: the flip lands")

	// The audit chain binds the walk: the reviewers are the functional
	// USER principals, not any session actor.
	var reviewers string
	err = seed.QueryRowContext(ctx, `
		SELECT string_agg(DISTINCT actor_principal, ',' ORDER BY actor_principal) FROM audit_events
		WHERE action IN ('asset.reviewed', 'asset.approved', 'asset.approval.recorded')
		  AND resource_id = 'ART-release-note-601@1'`).Scan(&reviewers)
	require.NoError(t, err)
	assert.Contains(t, reviewers, "user:"+techUser.ID)
	assert.Contains(t, reviewers, "user:"+productUser.ID)

	// Fail closed: a developer membership WITHOUT functional roles never
	// reaches the review plane over MCP.
	plainClient := w5MCPClient(t, baseURL, idp.token(t, "w5-plain"))
	result, text = callRealTool(t, ctx, plainClient, "asset_review", map[string]any{
		"asset_id": "ART-release-note-601", "version": float64(1), "idempotency_key": "w5-real-plain-0001",
	})
	require.True(t, result.IsError, "a plain developer must not reach the functional review plane")
	assert.Contains(t, text, "FORBIDDEN")

	// Unauthenticated /mcp is refused before the protocol layer: the
	// initialize REQUEST itself carries no credential, so the identity
	// layer rejects it (the transport connects lazily, the handshake
	// must be what fails).
	anonymous, err := client.NewStreamableHttpClient(baseURL + "/mcp")
	require.NoError(t, err)
	anonymousCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, anonymous.Start(anonymousCtx))
	request := mcp.InitializeRequest{}
	request.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = mcp.Implementation{Name: "maestro-w5-anonymous", Version: "1.0.0"}
	_, err = anonymous.Initialize(anonymousCtx, request)
	require.Error(t, err, "an unauthenticated MCP initialize must be refused")
	assert.Contains(t, err.Error(), "401")
}
