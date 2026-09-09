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

// UI-2 (task brief E): GET /api/v3/projects/:pid/work-items/:wid/waivers
// lists the waiver lifecycle for the HITL queue through the real
// authorize tree and the real PostgreSQL quality store.

func TestWorkItemWaiversList(t *testing.T) {
	f := newQualityFixture(t)
	gate := seedVerdictWithGate(t, f)

	waiverPath := "/api/v3/projects/" + qProjectID + "/gates/" + gate.GateID + "/waivers"
	body := fmt.Sprintf(`{"source_sha": %q, "merge_request_iid": 7, "check": %q,
		"reason": "queue list test waiver ticket-901", "expires_at": %q}`,
		gate.SourceSHA, gate.Check, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))
	created := f.request(t, f.adminTK, http.MethodPost, waiverPath,
		map[string]string{"If-Match": `"1"`, "Idempotency-Key": "wl1"}, body)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	t.Run("developer with quality.read sees the queued waiver and its remaining window", func(t *testing.T) {
		response := f.request(t, f.devTK, http.MethodGet,
			"/api/v3/projects/"+qProjectID+"/work-items/"+qWorkItemID+"/waivers", nil, "")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), `"requested"`)
		assert.Contains(t, response.Body.String(), gate.GateID)
		assert.Contains(t, response.Body.String(), `"remaining_seconds"`)
		assert.Contains(t, response.Body.String(), `"expires_at"`)
		// The remaining window is a live countdown, not a static flag.
		assert.NotContains(t, response.Body.String(), `"remaining_seconds":0`)
	})

	t.Run("revoked waivers report a closed window", func(t *testing.T) {
		waivers, err := f.pg.Quality().ListWaiversForWorkItem(context.Background(), qProjectID, qWorkItemID)
		require.NoError(t, err)
		require.Len(t, waivers, 1)
		revoked := f.request(t, f.adminTK, http.MethodPost,
			"/api/v3/projects/"+qProjectID+"/waivers/"+waivers[0].ID+"/revoke",
			map[string]string{"If-Match": `"1"`, "Idempotency-Key": "wl2"},
			`{"reason": "queue list test revocation"}`)
		require.Equal(t, http.StatusOK, revoked.Code, revoked.Body.String())

		response := f.request(t, f.viewerTK, http.MethodGet,
			"/api/v3/projects/"+qProjectID+"/work-items/"+qWorkItemID+"/waivers", nil, "")
		require.Equal(t, http.StatusOK, response.Code)
		assert.Contains(t, response.Body.String(), `"revoked"`)
		assert.Contains(t, response.Body.String(), `"remaining_seconds":0`)
	})

	t.Run("unknown work item hides as 404", func(t *testing.T) {
		response := f.request(t, f.devTK, http.MethodGet,
			"/api/v3/projects/"+qProjectID+"/work-items/018f7500-0000-7000-8000-00000000dead/waivers", nil, "")
		assert.Equal(t, http.StatusNotFound, response.Code)
	})
}
