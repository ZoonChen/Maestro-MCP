package handler

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated end-to-end tests for the frozen ingestGitLabWebhook operation:
// the full verify -> persist -> dedup -> dispatch chain against the real
// webhook_inbox/webhook_deliveries/outbox_events tables.

const (
	whInstanceID = "018f6500-0000-7000-8000-000000000001"
	whProjectID  = "018f6500-0000-7000-8000-000000000002"
	whGitlabProj = 4242
	whSecretEnv  = "MAESTRO_WEBHOOK_SECRET_TEST"
	whPayloadKey = "e2e-payload-key"
)

type whFixture struct {
	router     *gin.Engine
	db         *sql.DB
	pg         *store.PostgresStore
	ingestor   *webhook.Ingestor
	dispatcher *webhook.Dispatcher
	cipher     *webhook.PayloadCipher
}

func newWebhookFixture(t *testing.T) *whFixture {
	t.Helper()
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
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f6500-0000-7000-8000-000000000003', 'webhook fixture team')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f6500-0000-7000-8000-000000000003', 'wh-fixture', 'WH Fixture', 'active')`, whProjectID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ($1, 'https://gitlab.example.com', 'fixture instance', 'env:MAESTRO_GITLAB_BOT_TEST', $2)`,
		whInstanceID, "env:"+whSecretEnv)
	require.NoError(t, err)

	t.Setenv(whSecretEnv, "fixture-shared-token")

	cipher, err := webhook.NewPayloadCipher(whPayloadKey)
	require.NoError(t, err)
	webhookStore := pg.Webhooks()
	ingestor := &webhook.Ingestor{Store: webhookStore, Secrets: webhook.EnvSecretResolver{}, Cipher: cipher}
	dispatcher := &webhook.Dispatcher{
		Store: webhookStore, Cipher: cipher,
		MaxAttempts: 3, BaseBackoff: 50_000_000, MaxBackoff: 1_000_000_000,
	}

	router := gin.New()
	router.Use(MaxBodySize(1 << 20))
	RegisterGitLabWebhookIngest(router, GitLabWebhookOptions{Ingestor: ingestor})
	return &whFixture{router: router, db: db, pg: pg, ingestor: ingestor, dispatcher: dispatcher, cipher: cipher}
}

func (f *whFixture) seedMapping(t *testing.T, webhookUUID string) {
	t.Helper()
	var registered any
	if webhookUUID != "" {
		registered = webhookUUID
	}
	_, err := f.db.Exec(`
		INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, webhook_uuid)
		VALUES ($1, $2, $3, $4)`, whInstanceID, whGitlabProj, whProjectID, registered)
	require.NoError(t, err)
}

func (f *whFixture) post(t *testing.T, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/"+whInstanceID, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		if value == "" {
			continue
		}
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

func validHeaders() map[string]string {
	return map[string]string{
		"X-Gitlab-Token":        "fixture-shared-token",
		"X-Gitlab-Event":        "Merge Request Hook",
		"X-Gitlab-Event-UUID":   "evt-e2e-1",
		"X-Gitlab-Webhook-UUID": "hook-uuid-1",
	}
}

func mrBody() string {
	return `{"object_kind":"merge_request","project":{"id":` + itoa(whGitlabProj) + `},"object_attributes":{"iid":7,"action":"open"}}`
}

func itoa(v int) string { return strconv.Itoa(v) }

func TestWebhookIngestEndToEnd(t *testing.T) {
	f := newWebhookFixture(t)

	t.Run("verified delivery persists and stays exactly once", func(t *testing.T) {
		response := f.post(t, validHeaders(), mrBody())
		require.Equal(t, http.StatusAccepted, response.Code)
		assert.Contains(t, response.Body.String(), "EVENT_PERSISTED")

		var status string
		var attempts int
		var sealed []byte
		require.NoError(t, f.db.QueryRow(`
			SELECT status, attempts, raw_body_encrypted FROM webhook_inbox
			WHERE gitlab_instance_id = $1 AND external_event_id = 'evt-e2e-1'`, whInstanceID).
			Scan(&status, &attempts, &sealed))
		assert.Equal(t, "received", status)
		assert.Zero(t, attempts)
		assert.NotContains(t, string(sealed), "object_kind", "raw body is stored encrypted")

		var outcome string
		var tokenVerified bool
		require.NoError(t, f.db.QueryRow(`
			SELECT outcome, token_verified FROM webhook_deliveries
			WHERE gitlab_instance_id = $1 AND external_event_id = 'evt-e2e-1'`, whInstanceID).
			Scan(&outcome, &tokenVerified))
		assert.Equal(t, "accepted", outcome)
		assert.True(t, tokenVerified)

		// Exact re-delivery: idempotent 2xx, no state regression.
		response = f.post(t, validHeaders(), mrBody())
		require.Equal(t, http.StatusAccepted, response.Code)
		assert.Contains(t, response.Body.String(), "EVENT_DUPLICATE")

		var inboxRows, deliveryRows int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM webhook_inbox`).Scan(&inboxRows))
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM webhook_deliveries`).Scan(&deliveryRows))
		assert.Equal(t, 1, inboxRows, "dedup keeps exactly one inbox row")
		assert.Equal(t, 2, deliveryRows, "every delivery is audited, including duplicates")
		require.NoError(t, f.db.QueryRow(`
			SELECT status, attempts FROM webhook_inbox WHERE external_event_id = 'evt-e2e-1'`).
			Scan(&status, &attempts))
		assert.Equal(t, "received", status, "a re-delivery never regresses inbox state")
		assert.Zero(t, attempts)
	})

	t.Run("forged token has no business effect", func(t *testing.T) {
		headers := validHeaders()
		headers["X-Gitlab-Token"] = "forged"
		headers["X-Gitlab-Event-UUID"] = "evt-forged"
		response := f.post(t, headers, mrBody())
		require.Equal(t, http.StatusUnauthorized, response.Code)

		var inboxRows int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM webhook_inbox WHERE external_event_id = 'evt-forged'`).Scan(&inboxRows))
		assert.Zero(t, inboxRows)

		var reason string
		require.NoError(t, f.db.QueryRow(`
			SELECT reject_reason FROM webhook_deliveries WHERE external_event_id = 'evt-forged'`).
			Scan(&reason))
		assert.Equal(t, "TOKEN_MISMATCH", reason)
	})

	t.Run("uncontracted event kind is archived only", func(t *testing.T) {
		headers := validHeaders()
		headers["X-Gitlab-Event"] = "Note Hook"
		headers["X-Gitlab-Event-UUID"] = "evt-note"
		response := f.post(t, headers, mrBody())
		require.Equal(t, http.StatusAccepted, response.Code)

		var inboxRows int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM webhook_inbox WHERE external_event_id = 'evt-note'`).Scan(&inboxRows))
		assert.Zero(t, inboxRows, "archived events never enter the inbox")
		var reason string
		require.NoError(t, f.db.QueryRow(`
			SELECT reject_reason FROM webhook_deliveries WHERE external_event_id = 'evt-note'`).Scan(&reason))
		assert.Equal(t, "UNSUPPORTED_EVENT_KIND", reason)
	})

	t.Run("payload without project is quarantined", func(t *testing.T) {
		headers := validHeaders()
		headers["X-Gitlab-Event-UUID"] = "evt-no-project"
		response := f.post(t, headers, `{"object_kind":"merge_request","object_attributes":{"iid":9}}`)
		require.Equal(t, http.StatusAccepted, response.Code)
		assert.Contains(t, response.Body.String(), "EVENT_QUARANTINED")

		var status, reason string
		require.NoError(t, f.db.QueryRow(`
			SELECT i.status, d.reject_reason FROM webhook_inbox i
			JOIN webhook_deliveries d ON d.inbox_id = i.id
			WHERE i.external_event_id = 'evt-no-project'`).Scan(&status, &reason))
		assert.Equal(t, "dead_letter", status)
		assert.Equal(t, "PAYLOAD_PROJECT_MISSING", reason)
	})

	t.Run("missing uuid falls back to the composite key", func(t *testing.T) {
		headers := validHeaders()
		headers["X-Gitlab-Event-UUID"] = ""
		response := f.post(t, headers, mrBody())
		require.Equal(t, http.StatusAccepted, response.Code)

		var keys int
		require.NoError(t, f.db.QueryRow(`
			SELECT count(*) FROM webhook_inbox WHERE external_event_id LIKE 'compat:%'`).Scan(&keys))
		assert.Equal(t, 1, keys)
	})

	t.Run("oversized body is rejected by the transport limit", func(t *testing.T) {
		headers := validHeaders()
		headers["X-Gitlab-Event-UUID"] = "evt-huge"
		response := f.post(t, headers, `{"project_id":`+itoa(whGitlabProj)+`,"pad":"`+strings.Repeat("x", 1<<20)+`"}`)
		require.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	})
}

func TestWebhookDispatchEndToEnd(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedMapping(t, "")

	headers := validHeaders()
	require.Equal(t, http.StatusAccepted, f.post(t, headers, mrBody()).Code)

	outcome, err := f.dispatcher.DispatchOne(context.Background(), "e2e-worker")
	require.NoError(t, err)
	assert.Equal(t, "applied", outcome)

	// The frozen envelope reached the outbox with the inbox identity.
	var eventType, subject, projectID string
	require.NoError(t, f.db.QueryRow(`
		SELECT event_type, subject, project_id FROM outbox_events WHERE event_id = (
			SELECT id FROM webhook_inbox WHERE external_event_id = 'evt-e2e-1')`).
		Scan(&eventType, &subject, &projectID))
	assert.Equal(t, "gitlab.webhook.received", eventType)
	assert.Contains(t, subject, "evt-e2e-1")
	assert.Equal(t, whProjectID, projectID)

	var status string
	require.NoError(t, f.db.QueryRow(`
		SELECT status FROM webhook_inbox WHERE external_event_id = 'evt-e2e-1'`).Scan(&status))
	assert.Equal(t, "processed", status)

	// Drained inbox: a second dispatch has nothing to claim.
	outcome, err = f.dispatcher.DispatchOne(context.Background(), "e2e-worker")
	require.NoError(t, err)
	assert.Equal(t, "empty", outcome)
}

func TestWebhookUnmappedProjectDeadLetters(t *testing.T) {
	f := newWebhookFixture(t) // no mapping seeded

	headers := validHeaders()
	require.Equal(t, http.StatusAccepted, f.post(t, headers, mrBody()).Code)

	outcome, err := f.dispatcher.DispatchOne(context.Background(), "e2e-worker")
	require.NoError(t, err)
	assert.Equal(t, "dead_letter", outcome)

	var status, reason string
	require.NoError(t, f.db.QueryRow(`
		SELECT i.status, d.reject_reason FROM webhook_inbox i
		JOIN webhook_deliveries d ON d.inbox_id = i.id
		WHERE i.external_event_id = 'evt-e2e-1' AND d.outcome = 'dead_letter'`).Scan(&status, &reason))
	assert.Equal(t, "dead_letter", status)
	assert.Equal(t, "UNMAPPED_PROJECT", reason)

	// Replay after the operator maps the project: the SAME event identity
	// applies through the normal path, exactly once.
	f.seedMapping(t, "")
	replayed, err := f.pg.Webhooks().ReplayDeadLetter(context.Background(), func() string {
		var id string
		require.NoError(t, f.db.QueryRow(`
			SELECT id FROM webhook_inbox WHERE external_event_id = 'evt-e2e-1'`).Scan(&id))
		return id
	}())
	require.NoError(t, err)
	require.True(t, replayed)

	outcome, err = f.dispatcher.DispatchOne(context.Background(), "e2e-worker")
	require.NoError(t, err)
	assert.Equal(t, "applied", outcome)

	var envelopes int
	require.NoError(t, f.db.QueryRow(`
		SELECT count(*) FROM outbox_events WHERE event_type = 'gitlab.webhook.received'`).Scan(&envelopes))
	assert.Equal(t, 1, envelopes, "replay must not duplicate the envelope")

	var replayStatus string
	require.NoError(t, f.db.QueryRow(`
		SELECT status FROM webhook_inbox WHERE external_event_id = 'evt-e2e-1'`).Scan(&replayStatus))
	assert.Equal(t, "processed", replayStatus)
}

func TestWebhookHookIdentityMismatch(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedMapping(t, "registered-hook-uuid")

	headers := validHeaders()
	headers["X-Gitlab-Webhook-UUID"] = "some-other-hook"
	response := f.post(t, headers, mrBody())
	require.Equal(t, http.StatusUnauthorized, response.Code)

	var inboxRows int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM webhook_inbox`).Scan(&inboxRows))
	assert.Zero(t, inboxRows)

	// The registered identity passes.
	headers["X-Gitlab-Webhook-UUID"] = "registered-hook-uuid"
	response = f.post(t, headers, mrBody())
	require.Equal(t, http.StatusAccepted, response.Code)
}

func TestWebhookSuspendedInstance(t *testing.T) {
	f := newWebhookFixture(t)
	_, err := f.db.Exec(`UPDATE gitlab_instances SET status = 'suspended' WHERE id = $1`, whInstanceID)
	require.NoError(t, err)

	response := f.post(t, validHeaders(), mrBody())
	require.Equal(t, http.StatusServiceUnavailable, response.Code)

	var inboxRows int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM webhook_inbox`).Scan(&inboxRows))
	assert.Zero(t, inboxRows)
}

func TestWebhookReceiverIsolatedFromBearerTree(t *testing.T) {
	// The receiver must not require a bearer principal even when the OIDC
	// middleware is mounted engine-wide: it self-gates on the shared token.
	f := newWebhookFixture(t)
	request := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/"+whInstanceID, bytes.NewReader([]byte(mrBody())))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range validHeaders() {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	require.Equal(t, http.StatusAccepted, response.Code, "no Authorization header is required")
}
