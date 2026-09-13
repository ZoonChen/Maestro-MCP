package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// UI-1 (task brief E): POST /api/v3/webhooks/dead-letters/:inbox_id/replay
// rides the runbook §8/§9 dual-person replay contract (ReplayDeadLetter,
// landed with #88) through the real authorize tree and the real
// PostgreSQL webhook store.

const (
	dlqInstanceID = "018f7500-0000-7000-8000-000000000201"
	dlqInboxID    = "018f7500-0000-7000-8000-000000000202"
)

func seedDeadLetter(t *testing.T, f *qualityFixture, inboxID, status string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `
		INSERT INTO gitlab_instances (id, base_url, display_name, status, bot_credential_ref, webhook_secret_ref)
		VALUES ($1, 'https://gitlab.dlq.example', 'DLQ E2E', 'active', 'ref:bot', 'ref:hook')
		ON CONFLICT (id) DO NOTHING`, dlqInstanceID)
	require.NoError(t, err)
	_, err = f.db.ExecContext(context.Background(), `
		INSERT INTO webhook_inbox (id, gitlab_instance_id, external_event_id, event_kind, payload_digest, status)
		VALUES ($1, $2, $3, 'push', $4, $5)
		ON CONFLICT (gitlab_instance_id, external_event_id) DO UPDATE SET status = EXCLUDED.status`,
		inboxID, dlqInstanceID, "evt-"+inboxID, "sha256:"+repeatChar('a', 64), status)
	require.NoError(t, err)
}

func repeatChar(char byte, count int) string {
	runes := make([]byte, count)
	for index := range runes {
		runes[index] = char
	}
	return string(runes)
}

func TestDeadLetterReplayEndpoint(t *testing.T) {
	f := newQualityFixture(t)
	seedDeadLetter(t, f, dlqInboxID, "dead_letter")
	replayPath := "/api/v3/webhooks/dead-letters/" + dlqInboxID + "/replay"
	requestedBy := "ops-colleague-7"
	substantive := "runbook drill: delivery retried after the TLS rotation fix landed"

	// W5-6 / OPS-1 terminal state: the frozen permission is
	// webhook.deadletter.replay on the operations_owner functional plane
	// (admin-1 above); a plain developer is denied, and so is a
	// project_admin without the operations grant (gitlab.reconcile no
	// longer reaches this route).
	t.Run("developer without webhook.deadletter.replay is denied", func(t *testing.T) {
		response := f.request(t, f.devTK, http.MethodPost, replayPath, nil,
			`{"requested_by": "`+requestedBy+`", "reason": "`+substantive+`"}`)
		assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	})

	t.Run("self-approval is refused before the store", func(t *testing.T) {
		response := f.request(t, f.adminTK, http.MethodPost, replayPath, nil,
			`{"requested_by": "`+f.adminPID+`", "reason": "`+substantive+`"}`)
		assert.Equal(t, http.StatusForbidden, response.Code)
		assert.Contains(t, response.Body.String(), "SEPARATION_OF_DUTIES")
	})

	t.Run("a body naming another approver is refused", func(t *testing.T) {
		response := f.request(t, f.adminTK, http.MethodPost, replayPath, nil,
			`{"requested_by": "`+requestedBy+`", "approved_by": "someone-else", "reason": "`+substantive+`"}`)
		assert.Equal(t, http.StatusForbidden, response.Code)
		assert.Contains(t, response.Body.String(), "APPROVER_IDENTITY_MISMATCH")
	})

	t.Run("thin reason is rejected by the store contract", func(t *testing.T) {
		response := f.request(t, f.adminTK, http.MethodPost, replayPath, nil,
			`{"requested_by": "`+requestedBy+`", "reason": "too short"}`)
		assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
		assert.Contains(t, response.Body.String(), "REPLAY_APPROVAL_INVALID")
	})

	t.Run("dual-person replay requeues, audits and collapses on repeat", func(t *testing.T) {
		response := f.request(t, f.adminTK, http.MethodPost, replayPath, nil,
			`{"requested_by": "`+requestedBy+`", "reason": "`+substantive+`"}`)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), `"replayed":true`)

		var status string
		err := f.db.QueryRowContext(context.Background(),
			`SELECT status FROM webhook_inbox WHERE id = $1`, dlqInboxID).Scan(&status)
		require.NoError(t, err)
		assert.Equal(t, "received", status, "the quarantined row must be back in the queue under its original identity")

		var auditCount int
		var actor, reason string
		err = f.db.QueryRowContext(context.Background(), `
			SELECT count(*), min(actor_principal), min(reason) FROM audit_events
			WHERE action = 'webhook.dead_letter.replayed' AND resource_id = $1`, dlqInboxID).
			Scan(&auditCount, &actor, &reason)
		require.NoError(t, err)
		assert.Equal(t, 1, auditCount, "exactly one atomic replay audit row")
		assert.Equal(t, f.adminPID, actor, "the audited approver is the authenticated principal")
		assert.Contains(t, reason, requestedBy)

		// The row is no longer quarantined: replay collapses to a 404.
		again := f.request(t, f.adminTK, http.MethodPost, replayPath, nil,
			`{"requested_by": "`+requestedBy+`", "reason": "`+substantive+`"}`)
		assert.Equal(t, http.StatusNotFound, again.Code)
	})

	t.Run("unknown inbox id hides as 404", func(t *testing.T) {
		response := f.request(t, f.adminTK, http.MethodPost,
			"/api/v3/webhooks/dead-letters/018f7500-0000-7000-8000-00000000dead/replay", nil,
			`{"requested_by": "`+requestedBy+`", "reason": "`+substantive+`"}`)
		assert.Equal(t, http.StatusNotFound, response.Code)
	})
}
