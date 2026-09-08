package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// PG-gated M4-OBS-001 export surface: the frozen audit-export wire
// over the immutable audit_events table — uuid event ids stable across
// exports, chain digests, the redaction record, RBAC on audit.export
// and the tamper-check endpoint.

const (
	obsTeamID    = "018f7e00-0000-7000-8000-00000000a101"
	obsProjectID = "018f7e00-0000-7000-8000-00000000a102"
)

func TestAuditExportEndpoints(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
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
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'obs handler') ON CONFLICT DO NOTHING`, obsTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'obs-h', 'OBS-H', 'active') ON CONFLICT DO NOTHING`, obsProjectID, obsTeamID)
	require.NoError(t, err)
	var firstSeq int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COALESCE(max(id), 0) FROM audit_events WHERE project_id = $1`, obsProjectID).Scan(&firstSeq))
	if firstSeq == 0 {
		for index := 1; index <= 3; index++ {
			_, err = db.ExecContext(ctx, `INSERT INTO audit_events
				(actor_principal, project_id, action, resource_type, resource_id, decision, correlation_id)
				VALUES ($1, $2, $3, 'work_item', $4, $5, $6)`,
				"obs-user-"+string(rune('0'+index)), obsProjectID, "work_item.read",
				"obs-w-"+string(rune('0'+index)),
				[]string{"allow", "deny", "allow"}[index-1], "obs-corr-"+string(rune('0'+index)))
			require.NoError(t, err)
		}
		require.NoError(t, db.QueryRowContext(ctx, `SELECT min(id) FROM audit_events WHERE project_id = $1`, obsProjectID).Scan(&firstSeq))
	}
	lastSeq := firstSeq + 2

	obsHandler := NewObservabilityHandler(pg.Observability())
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	idp := newHandlerTestIdP(t)
	resolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"plat-1": {obsProjectID: "platform_admin"},
		"dev-1":  {obsProjectID: "developer"},
	}}
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, resolver)

	router := gin.New()
	router.Use(mw.Authenticate)
	RegisterControlPlane(router, ControlPlaneOptions{
		Identity: mw, Observability: obsHandler, Scope: pg.Instances(),
	})
	platTK, devTK := idp.signedToken(t, "plat-1"), idp.signedToken(t, "dev-1")

	request := func(token, method, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	exportPath := "/api/v3/projects/" + obsProjectID + "/audit-export?from_seq=" +
		strconv.FormatInt(firstSeq, 10) + "&to_seq=" + strconv.FormatInt(lastSeq, 10)

	// The export wire conforms to the frozen schema's shape.
	rec := request(platTK, http.MethodGet, exportPath, nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var export map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &export))
	assert.Equal(t, "3.0", export["schema_version"])
	assert.NotEmpty(t, export["export_id"])
	_, err = uuid.Parse(export["export_id"].(string))
	require.NoError(t, err)
	entries := export["entries"].([]any)
	require.Len(t, entries, 3)
	chainDigest := export["chain_digest"].(string)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, chainDigest)

	var entryDigests []string
	var eventIDs []string
	for index, raw := range entries {
		entry := raw.(map[string]any)
		_, err = uuid.Parse(entry["event_id"].(string))
		require.NoError(t, err, "event_id must be a uuid")
		_, err = time.Parse(time.RFC3339Nano, entry["occurred_at"].(string))
		require.NoError(t, err, "occurred_at must be RFC3339")
		digest := entry["entry_digest"].(string)
		assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)
		entryDigests = append(entryDigests, digest)
		eventIDs = append(eventIDs, entry["event_id"].(string))
		if index == 0 {
			assert.NotContains(t, entry, "prev_digest", "the first entry has no predecessor")
		} else {
			assert.Equal(t, entryDigests[index-1], entry["prev_digest"])
		}
		assert.NotContains(t, entry, "token_hash")
	}
	redaction := export["redaction"].(map[string]any)
	assert.Equal(t, "audit-export-v3", redaction["policy_version"])
	assert.Contains(t, redaction["fields_masked"], "token_hash")
	assert.Contains(t, redaction["fields_masked"], "reason")

	// Event ids are deterministic across exports; export ids are not.
	rec = request(platTK, http.MethodGet, exportPath, nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var second map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &second))
	secondEntries := second["entries"].([]any)
	for index := range entries {
		assert.Equal(t, eventIDs[index], secondEntries[index].(map[string]any)["event_id"],
			"the same row always exports the same uuid")
	}
	assert.NotEqual(t, export["export_id"], second["export_id"])

	// The tamper check: claimed digests from the export verify; a
	// forged digest is a 409 mismatch.
	verifyBody := func(claimed []string) string {
		payload := map[string]any{"from_seq": firstSeq, "to_seq": lastSeq, "claimed_digests": claimed}
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		return string(encoded)
	}
	headers := map[string]string{"If-Match": chainDigest, "Idempotency-Key": "obs-verify-1", "Content-Type": "application/json"}
	rec = request(platTK, http.MethodPost, "/api/v3/projects/"+obsProjectID+"/audit-export/verify",
		headers, verifyBody(entryDigests))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"verified":true`)

	forged := append([]string{}, entryDigests...)
	forged[1] = "sha256:" + strings.Repeat("0", 64)
	rec = request(platTK, http.MethodPost, "/api/v3/projects/"+obsProjectID+"/audit-export/verify",
		headers, verifyBody(forged))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "AUDIT_CHAIN_MISMATCH")

	// Precondition and idempotency headers are required by the frozen
	// contract.
	rec = request(platTK, http.MethodPost, "/api/v3/projects/"+obsProjectID+"/audit-export/verify",
		map[string]string{"Idempotency-Key": "k"}, verifyBody(entryDigests))
	assert.Equal(t, http.StatusPreconditionRequired, rec.Code)
	rec = request(platTK, http.MethodPost, "/api/v3/projects/"+obsProjectID+"/audit-export/verify",
		map[string]string{"If-Match": chainDigest}, verifyBody(entryDigests))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Range validation and RBAC negatives.
	rec = request(platTK, http.MethodGet, "/api/v3/projects/"+obsProjectID+"/audit-export?from_seq=5&to_seq=1", nil, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = request(platTK, http.MethodGet, "/api/v3/projects/"+obsProjectID+"/audit-export?from_seq=0&to_seq=1", nil, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = request(devTK, http.MethodGet, exportPath, nil, "")
	assert.Equal(t, http.StatusForbidden, rec.Code, "developer lacks audit.export")
	rec = request(platTK, http.MethodGet, "/api/v3/projects/018f7e00-0000-7000-8000-00000000ffff/audit-export?from_seq=1&to_seq=2", nil, "")
	assert.Equal(t, http.StatusNotFound, rec.Code, "unknown scopes hide as 404")
}
