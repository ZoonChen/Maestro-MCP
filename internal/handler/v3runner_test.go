package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated integration tests for the v3 Runner lifecycle endpoints: the
// full enrollment flow against the real PostgreSQL registry, plus the
// device-token gate and the admin authorization matrix.

type v3Fixture struct {
	router     *gin.Engine
	registry   *store.PostgresStore
	tokens     *identity.DeviceTokenMinter
	code       string
	adminToken string
}

func newV3Fixture(t *testing.T) *v3Fixture {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	// Package tests run concurrently with other packages' PG suites on the
	// same server; a dedicated database per suite avoids cross-package
	// schema-reset deadlocks.
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_handler_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_handler_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_handler_test WITH (FORCE)`)
		_ = admin.Close()
	})

	db, err := store.OpenPostgres(context.Background(), testDatabaseDSN(t))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)

	registry, err := store.NewPostgresStore(db)
	require.NoError(t, err)
	tokens, err := identity.NewDeviceTokenMinter("test-secret-0123456789abcdef0123456789", time.Now)
	require.NoError(t, err)
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)

	ctx := context.Background()
	owner, err := registry.Identities().GetOrCreateUser(ctx, "https://idp.example", "admin-1", "Admin One")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO teams (id, name) VALUES ('018f2000-0000-7000-8000-000000000001', 'v3 fixture team')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ('018f2000-0000-7000-8000-000000000002', '018f2000-0000-7000-8000-000000000001', 'v3-fixture', 'V3 Fixture', 'active')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO memberships (team_id, user_id, role) VALUES ('018f2000-0000-7000-8000-000000000001', $1, 'project_admin')`, owner.ID)
	require.NoError(t, err)

	code, codeHash, err := identity.NewEnrollmentCode()
	require.NoError(t, err)
	require.NoError(t, registry.RunnerRegistry().CreateEnrollment(ctx, &model.RunnerEnrollment{
		ProjectID: "018f2000-0000-7000-8000-000000000002", CodeHash: codeHash,
		ExpiresAt: time.Now().Add(enrollmentCodeTTL).UTC().Format(time.RFC3339),
		CreatedBy: owner.ID,
	}))

	idp := newHandlerTestIdP(t)
	adminToken := idp.signedToken(t, "admin-1")
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	adminResolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"admin-1": {"018f2000-0000-7000-8000-000000000002": "project_admin"},
	}}
	adminMW := NewOIDCMiddleware(policy, verifier, adminResolver)

	router := SetupRouter(nil, nil, nil, nil, nil, nil, nil, nil, nil, "unused",
		RouterOptions{Identity: adminMW.IdentityMount(), RemoteWrite: true})
	RegisterRunnerV3(router, RunnerV3Options{Registry: registry, Tokens: tokens, Policy: policy, Admin: adminMW})
	return &v3Fixture{router: router, registry: registry, tokens: tokens, code: code, adminToken: adminToken}
}

func (f *v3Fixture) adminRequest(method, path string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	if method == http.MethodPost {
		body.WriteString("{}")
	}
	request := httptest.NewRequest(method, path, &body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.adminToken)
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

func enrollBody(code string) map[string]any {
	return map[string]any{
		"enrollment_code":   code,
		"device_public_key": "device-key-1",
		"display_name":      "runner-v3-test",
		"capabilities":      []string{"rootless_oci", "no_new_privileges", "resource_limits"},
	}
}

func postJSONWithKey(t *testing.T, router *gin.Engine, path string, body any, token, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func postJSON(t *testing.T, router *gin.Engine, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestRunnerV3EnrollmentLifecycle(t *testing.T) {
	fixture := newV3Fixture(t)

	response := postJSON(t, fixture.router, "/api/v3/runners/enroll", enrollBody(fixture.code), "")
	require.Equal(t, http.StatusCreated, response.Code, "body: %s", response.Body.String())
	var credential struct {
		RunnerID    string `json:"runner_id"`
		State       string `json:"state"`
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &credential))
	assert.Equal(t, model.RunnerStatusPendingApproval, credential.State)

	// Single-use: the same code never enrolls again.
	duplicate := postJSON(t, fixture.router, "/api/v3/runners/enroll", enrollBody(fixture.code), "")
	assert.Equal(t, http.StatusConflict, duplicate.Code)

	// A wrong code is rejected.
	wrong := postJSON(t, fixture.router, "/api/v3/runners/enroll", enrollBody("not-the-code-at-all"), "")
	assert.Equal(t, http.StatusBadRequest, wrong.Code)

	// The device token answers /me with the pending state.
	me := httptest.NewRequest(http.MethodGet, "/api/v3/runners/me", nil)
	me.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	meResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(meResponse, me)
	require.Equal(t, http.StatusOK, meResponse.Code, "body: %s", meResponse.Body.String())
	var state struct {
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(meResponse.Body.Bytes(), &state))
	assert.Equal(t, model.RunnerStatusPendingApproval, state.State)

	// Claiming is live dispatch now: a pending-approval runner is
	// forbidden (403) even before admin approval; no-work comes after.
	claim := postJSONWithKey(t, fixture.router, "/api/v3/runner-leases/claim",
		map[string]any{"protocol_version": "3.0", "connection_generation": "gen",
			"capabilities": []string{"a", "b", "c"}, "wait_seconds": 5},
		credential.AccessToken, "daemon-test-claim-000001-q0")
	assert.Equal(t, http.StatusForbidden, claim.Code, "body: %s", claim.Body.String())

	// A project admin approves.
	approved := fixture.adminRequest(http.MethodPost, "/api/v3/runners/"+credential.RunnerID+"/approve")
	require.Equal(t, http.StatusOK, approved.Code, "body: %s", approved.Body.String())

	// Revocation is terminal and immediately gates the device token (410).
	revoked := fixture.adminRequest(http.MethodPost, "/api/v3/runners/"+credential.RunnerID+"/revoke")
	require.Equal(t, http.StatusOK, revoked.Code, "body: %s", revoked.Body.String())
	meAfter := httptest.NewRequest(http.MethodGet, "/api/v3/runners/me", nil)
	meAfter.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	meAfterResponse := httptest.NewRecorder()
	fixture.router.ServeHTTP(meAfterResponse, meAfter)
	assert.Equal(t, http.StatusGone, meAfterResponse.Code, "revoked device must be 410 Gone")

	// Unknown runners hide as 404 for admins.
	missing := fixture.adminRequest(http.MethodPost, "/api/v3/runners/018f1000-0000-7000-8000-00000000dead/approve")
	assert.Equal(t, http.StatusNotFound, missing.Code)

}

func TestRunnerV3EnrollmentRejectsIncompleteBodies(t *testing.T) {
	fixture := newV3Fixture(t)
	for name, mutate := range map[string]func(map[string]any){
		"no code":       func(b map[string]any) { b["enrollment_code"] = "" },
		"no key":        func(b map[string]any) { b["device_public_key"] = "" },
		"no name":       func(b map[string]any) { b["display_name"] = "" },
		"two caps only": func(b map[string]any) { b["capabilities"] = []string{"a", "b"} },
	} {
		t.Run(name, func(t *testing.T) {
			body := enrollBody(fixture.code)
			mutate(body)
			response := postJSON(t, fixture.router, "/api/v3/runners/enroll", body, "")
			assert.Equal(t, http.StatusBadRequest, response.Code)
		})
	}
}

func TestRunnerV3AdminRequiresAuthentication(t *testing.T) {
	fixture := newV3Fixture(t)
	response := postJSON(t, fixture.router, "/api/v3/runners/some-id/approve", map[string]any{}, "")
	assert.Equal(t, http.StatusUnauthorized, response.Code)
}

// testDatabaseDSN rewrites the suite DSN's database name.
func testDatabaseDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	index := strings.LastIndex(dsn, "/")
	if index < 0 {
		t.Fatal("MAESTRO_TEST_POSTGRES_DSN has no database path")
	}
	return dsn[:index+1] + "maestro_handler_test"
}
