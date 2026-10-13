package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W9 (F64/F66) claim-face acceptance through the real /api/v3 tree: the
// rehearsal's one 500 (a display name riding runner_id into a uuid
// column) is a 400, the concurrent targeted race answers 409
// CLAIM_TARGET_HELD naming the holder with zero 500s, and every
// successful claim leaves its work_item.claimed audit row plus
// work_item.state.changed outbox event.

// TestW9ClaimNonUUIDRunnerIs400 replays F-G2-1 verbatim: the first
// rehearsal claim attempt sent the runner's display name as runner_id and
// drew 500 INTERNAL_ERROR from the uuid-column binding check. The shape
// fence rejects it before any query, on every execution-lifecycle face.
func TestW9ClaimNonUUIDRunnerIs400(t *testing.T) {
	f := newW8TargetedFixture(t)
	claimPath := "/api/v3/projects/" + w7ProjectID + "/work-items/claim"
	before := w7QueueVersion(t, f)

	for _, body := range []string{
		// The rehearsal's exact shape: targeted claim, display name.
		fmt.Sprintf(`{"runner_id":"s2a-claim-runner","connection_generation":"w9-gen","work_item_id":%q}`, w7TailItem),
		// The untargeted face carries the same fence.
		`{"runner_id":"s2a-claim-runner","connection_generation":"w9-gen"}`,
	} {
		reply := w7Request(t, f, f.devTK, http.MethodPost, claimPath, body, "")
		require.Equal(t, http.StatusBadRequest, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "INVALID_PARAMETER")
	}

	var leases int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM leases`).Scan(&leases))
	assert.Zero(t, leases, "the shape refusal happens before any side effect")
	assert.Equal(t, before, w7QueueVersion(t, f), "the shape refusal does not move the queue token")

	// The complete and release faces share the runner_id uuid column.
	completePath := "/api/v3/projects/" + w7ProjectID + "/executions/018f7900-0000-7000-8000-000000000041/complete"
	reply := w7Request(t, f, f.devTK, http.MethodPost, completePath,
		`{"runner_id":"s2a-claim-runner","connection_generation":"w9-gen","outcome":"completed","commit_sha":"`+
			"0123456789abcdef0123456789abcdef01234567"+`"}`, "")
	require.Equal(t, http.StatusBadRequest, reply.Code, reply.Body.String())
	assert.Contains(t, reply.Body.String(), "INVALID_PARAMETER")

	releasePath := "/api/v3/projects/" + w7ProjectID + "/executions/018f7900-0000-7000-8000-000000000041/release"
	reply = w7Request(t, f, f.devTK, http.MethodPost, releasePath,
		`{"runner_id":"s2a-claim-runner","connection_generation":"w9-gen","reason":"w9 shape fence check"}`, "")
	require.Equal(t, http.StatusBadRequest, reply.Code, reply.Body.String())
	assert.Contains(t, reply.Body.String(), "INVALID_PARAMETER")
}

// TestW9ClaimConcurrentRaceZero500 amplifies the W8 drill: six rounds of
// two bound runners targeting the same fresh item at the same moment.
// Every round lands exactly one 200 and one 409 CLAIM_TARGET_HELD whose
// held_by_runner is the lease holder the database actually kept — and no
// reply in any round is a 500.
func TestW9ClaimConcurrentRaceZero500(t *testing.T) {
	f := newW8TargetedFixture(t)
	claimPath := "/api/v3/projects/" + w7ProjectID + "/work-items/claim"

	for round := range 6 {
		item := fmt.Sprintf("018f7900-0000-7000-8000-%012d", 50+round)
		_, err := f.db.Exec(`INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, $3, 'queued')`,
			item, w7ProjectID, fmt.Sprintf("w9 race round %d", round))
		require.NoError(t, err)

		start := make(chan struct{})
		replies := make(chan *httptest.ResponseRecorder, 2)
		var wg sync.WaitGroup
		for _, runner := range []string{w7RunnerID, w8SecondRunnerID} {
			wg.Add(1)
			go func(runnerID string) {
				defer wg.Done()
				<-start
				replies <- w7Request(t, f, f.devTK, http.MethodPost, claimPath,
					fmt.Sprintf(`{"runner_id":%q,"connection_generation":%q,"work_item_id":%q}`,
						runnerID, fmt.Sprintf("w9-race-%d", round), item), "")
			}(runner)
		}
		close(start)
		wg.Wait()
		first, second := <-replies, <-replies

		assert.NotEqual(t, http.StatusInternalServerError, first.Code, first.Body.String())
		assert.NotEqual(t, http.StatusInternalServerError, second.Code, second.Body.String())
		assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, []int{first.Code, second.Code},
			"round %d: exactly one lane wins the target", round)

		var winner string
		require.NoError(t, f.db.QueryRow(
			`SELECT runner_id FROM leases WHERE work_item_id = $1 AND status = 'active'`, item).Scan(&winner))

		loser := second
		if second.Code == http.StatusOK {
			loser = first
		}
		require.Equal(t, http.StatusConflict, loser.Code, loser.Body.String())
		assert.Contains(t, loser.Body.String(), "CLAIM_TARGET_HELD")
		assert.Contains(t, loser.Body.String(), `"held_by_runner":"`+winner+`"`,
			"round %d: the loser's conflict names the lease that actually won", round)
	}

	// F64 rides along: every round's winning claim left exactly one
	// audit row and one state-change outbox event.
	var claimed int
	require.NoError(t, f.db.QueryRow(
		`SELECT count(*) FROM audit_events WHERE action = 'work_item.claimed'`).Scan(&claimed))
	assert.Equal(t, 6, claimed)
	var outbox int
	require.NoError(t, f.db.QueryRow(
		`SELECT count(*) FROM outbox_events WHERE event_type = 'work_item.state.changed'
		  AND payload ->> 'reason_code' = 'WORK_ITEM_CLAIMED'`).Scan(&outbox))
	assert.Equal(t, 6, outbox)
}

// TestW9ClaimAuditTrailShape pins the F64 wire shape end to end: the
// audit row's actor is the claiming runner, its reason carries the
// execution and lease expiry, and the outbox payload stays inside the
// frozen StateChangePayload contract.
func TestW9ClaimAuditTrailShape(t *testing.T) {
	f := newW8TargetedFixture(t)
	claimPath := "/api/v3/projects/" + w7ProjectID + "/work-items/claim"

	reply := w7Request(t, f, f.devTK, http.MethodPost, claimPath,
		fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w9-audit","work_item_id":%q}`,
			w7RunnerID, w7TailItem), "")
	require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
	executionID := replyJSONStringField(t, reply.Body.String(), "execution_id")
	leaseID := replyJSONStringField(t, reply.Body.String(), "lease_id")
	require.NotEmpty(t, executionID)
	require.NotEmpty(t, leaseID)

	var actor, reason string
	err := f.db.QueryRow(`
		SELECT actor_principal, reason FROM audit_events
		WHERE action = 'work_item.claimed' AND resource_id = $1`, w7TailItem).Scan(&actor, &reason)
	require.NoError(t, err, "the targeted claim leaves its audit row")
	assert.Equal(t, "runner:"+w7RunnerID, actor)
	assert.Contains(t, reason, "execution="+executionID)
	assert.Contains(t, reason, "lease="+leaseID)
	assert.Contains(t, reason, "lease_expires=")

	var payload []byte
	err = f.db.QueryRow(`
		SELECT payload FROM outbox_events
		WHERE event_type = 'work_item.state.changed' AND subject = $1`, w7TailItem).Scan(&payload)
	require.NoError(t, err)
	var decoded struct {
		WorkItemID string `json:"work_item_id"`
		From       string `json:"from"`
		To         string `json:"to"`
		Version    int64  `json:"version"`
		ReasonCode string `json:"reason_code"`
	}
	require.NoError(t, json.Unmarshal(payload, &decoded))
	assert.Equal(t, w7TailItem, decoded.WorkItemID)
	assert.Equal(t, "queued", decoded.From)
	assert.Equal(t, "executing", decoded.To)
	assert.Equal(t, int64(1), decoded.Version, "the seeded item's first transition (0 -> 1)")
	assert.Equal(t, "WORK_ITEM_CLAIMED", decoded.ReasonCode)
}
