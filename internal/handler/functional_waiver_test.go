package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// J1 (G-α / UI-4): a principal holding an ACTIVE functional grant over
// a viewer membership really reaches waiver.approve through the frozen
// authorize tree — and the transition's audit row distinguishes the
// functional authority from project roles. PG-gated end to end.

func TestFunctionalApproverApprovesWaiver(t *testing.T) {
	f := newQualityFixture(t)
	gate := seedVerdictWithGate(t, f)

	// The project admin (a project role) requests the waiver; the
	// functional approver (viewer + security_owner) approves it.
	waiverPath := "/api/v3/projects/" + qProjectID + "/gates/" + gate.GateID + "/waivers"
	body := fmt.Sprintf(`{"source_sha": %q, "merge_request_iid": 7, "check": %q,
		"reason": "functional approval e2e ticket-910", "expires_at": %q}`,
		gate.SourceSHA, gate.Check, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))
	created := f.request(t, f.adminTK, http.MethodPost, waiverPath,
		map[string]string{"If-Match": `"1"`, "Idempotency-Key": "fa1"}, body)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	waivers, err := f.pg.Quality().ListWaiversForWorkItem(context.Background(), qProjectID, qWorkItemID)
	require.NoError(t, err)
	require.Len(t, waivers, 1)
	waiver := waivers[0]
	approvePath := "/api/v3/projects/" + qProjectID + "/waivers/" + waiver.ID + "/approve"

	t.Run("the functional principal reaches a real 200", func(t *testing.T) {
		response := f.request(t, f.secTK, http.MethodPost, approvePath,
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "fa2"},
			`{"reason": "independent security review complete"}`)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), `"approved"`)
		assert.Contains(t, response.Body.String(), f.secPID)
	})

	t.Run("the audit row distinguishes the functional subject", func(t *testing.T) {
		var action, decision, reason, policyVersion string
		var actor string
		err := f.db.QueryRowContext(context.Background(), `
			SELECT actor_principal, action, decision, reason, COALESCE(policy_version, '')
			FROM audit_events
			WHERE action = 'waiver.approve' AND resource_id = $1`, waiver.ID).
			Scan(&actor, &action, &decision, &reason, &policyVersion)
		require.NoError(t, err)
		assert.Equal(t, f.secPID, actor, "the audit subject is the human principal")
		assert.Equal(t, "waiver.approve", action)
		assert.Equal(t, "allow", decision)
		assert.Contains(t, reason, "authority=functional:security_owner", reason)
		assert.Contains(t, reason, "independent security review complete")
		assert.Equal(t, "3.0", policyVersion, "the frozen matrix version rides the row")
	})

	t.Run("double approval conflicts without a second audit row", func(t *testing.T) {
		response := f.request(t, f.secTK, http.MethodPost, approvePath,
			map[string]string{"If-Match": `"2"`, "Idempotency-Key": "fa3"},
			`{"reason": "second attempt should conflict"}`)
		assert.Equal(t, http.StatusConflict, response.Code)

		var rows int
		require.NoError(t, f.db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM audit_events WHERE action = 'waiver.approve' AND resource_id = $1`,
			waiver.ID).Scan(&rows))
		assert.Equal(t, 1, rows, "a denied transition never writes an allow row")
	})
}

func TestFunctionalApproverDoesNotOverreachProjectPermissions(t *testing.T) {
	f := newQualityFixture(t)

	// The functional grant confers ONLY the frozen functional
	// permissions: security_owner over a viewer membership still cannot
	// strengthen a quality policy (project_admin's
	// project_policy.strengthen) or create work items.
	strengthen := f.request(t, f.secTK, http.MethodPut,
		"/api/v3/projects/"+qProjectID+"/quality-policy",
		map[string]string{"If-None-Match": "*", "Idempotency-Key": "fo1"},
		strengtheningOverlayJSON("acme", "3.0.0", true))
	assert.Equal(t, http.StatusForbidden, strengthen.Code)

	// A project role never approves: the pre-J1 fail-closed decision
	// stays for principals without an active functional grant.
	gate := seedVerdictWithGate(t, f)
	waiverPath := "/api/v3/projects/" + qProjectID + "/gates/" + gate.GateID + "/waivers"
	body := fmt.Sprintf(`{"source_sha": %q, "merge_request_iid": 7, "check": %q,
		"reason": "no-overreach waiver ticket-911", "expires_at": %q}`,
		gate.SourceSHA, gate.Check, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))
	created := f.request(t, f.adminTK, http.MethodPost, waiverPath,
		map[string]string{"If-Match": `"1"`, "Idempotency-Key": "fo2"}, body)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	waivers, err := f.pg.Quality().ListWaiversForWorkItem(context.Background(), qProjectID, qWorkItemID)
	require.NoError(t, err)
	var waiverID string
	for _, row := range waivers {
		if row.Reason == "no-overreach waiver ticket-911" {
			waiverID = row.ID
		}
	}
	require.NotEmpty(t, waiverID)

	admin := f.request(t, f.adminTK, http.MethodPost,
		"/api/v3/projects/"+qProjectID+"/waivers/"+waiverID+"/approve",
		map[string]string{"If-Match": `"1"`, "Idempotency-Key": "fo3"},
		`{"reason": "project admin must not approve"}`)
	assert.Equal(t, http.StatusForbidden, admin.Code)

	viewer := f.request(t, f.viewerTK, http.MethodPost,
		"/api/v3/projects/"+qProjectID+"/waivers/"+waiverID+"/approve",
		map[string]string{"If-Match": `"1"`, "Idempotency-Key": "fo4"},
		`{"reason": "plain viewer must not approve"}`)
	assert.Equal(t, http.StatusForbidden, viewer.Code)
}

func TestWaiverRevocationAuditsProjectAuthority(t *testing.T) {
	f := newQualityFixture(t)
	gate := seedVerdictWithGate(t, f)

	waiverPath := "/api/v3/projects/" + qProjectID + "/gates/" + gate.GateID + "/waivers"
	body := fmt.Sprintf(`{"source_sha": %q, "merge_request_iid": 7, "check": %q,
		"reason": "revoke audit waiver ticket-912", "expires_at": %q}`,
		gate.SourceSHA, gate.Check, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))
	created := f.request(t, f.adminTK, http.MethodPost, waiverPath,
		map[string]string{"If-Match": `"1"`, "Idempotency-Key": "ra1"}, body)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	waivers, err := f.pg.Quality().ListWaiversForWorkItem(context.Background(), qProjectID, qWorkItemID)
	require.NoError(t, err)
	require.Len(t, waivers, 1)

	revoked := f.request(t, f.adminTK, http.MethodPost,
		"/api/v3/projects/"+qProjectID+"/waivers/"+waivers[0].ID+"/revoke",
		map[string]string{"If-Match": `"1"`, "Idempotency-Key": "ra2"},
		`{"reason": "superseded by a policy fix"}`)
	require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())

	var reason, policyVersion string
	var actor string
	err = f.db.QueryRowContext(context.Background(), `
		SELECT actor_principal, reason, COALESCE(policy_version, '')
		FROM audit_events
		WHERE action = 'waiver.revoke' AND resource_id = $1`, waivers[0].ID).
		Scan(&actor, &reason, &policyVersion)
	require.NoError(t, err)
	assert.Equal(t, f.adminPID, actor)
	assert.Contains(t, reason, "authority=project:project_admin", reason)
	assert.Equal(t, "3.0", policyVersion)
}
