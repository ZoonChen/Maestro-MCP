package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W8 (F58) targeted-claim semantics: explicit targeting is intent — the
// claim takes the named item directly, and an unowned queue head can no
// longer freeze parallel lanes. The failure faces are specific and never
// fall through to another item: 404 TASK_NOT_FOUND (absent or not
// queued), 409 CLAIM_TARGET_HELD (active lease, holder surfaced), 409
// ASSET_GATE_NOT_SATISFIED (locked gate not consumable). The
// untargeted face keeps its skip semantics.

const w8SecondRunnerID = "018f7900-0000-7000-8000-000000000036"

// newW8TargetedFixture reuses the W7 friction shape (older unowned head,
// assigned tail behind it) and adds a second bound runner for the
// concurrency drill.
func newW8TargetedFixture(t *testing.T) *workflowActionsFixture {
	t.Helper()
	f := newW7FrictionFixture(t)
	_, err := f.db.Exec(`INSERT INTO runners (id, display_name, device_key_hash, status)
		VALUES ($1, 'w8 second claim runner', 'y', 'approved')`, w8SecondRunnerID)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO runner_bindings (project_id, runner_id) VALUES ($1, $2)`,
		w7ProjectID, w8SecondRunnerID)
	require.NoError(t, err)
	return f
}

// TestW8TargetedClaimFailureSemantics drills the three refusal faces the
// direct dispatch can meet, each refusing BEFORE any lease side effect
// and without moving the queue token.
func TestW8TargetedClaimFailureSemantics(t *testing.T) {
	f := newW8TargetedFixture(t)
	claimPath := "/api/v3/projects/" + w7ProjectID + "/work-items/claim"

	t.Run("targeted claim of an unknown item is 404 TASK_NOT_FOUND", func(t *testing.T) {
		before := w7QueueVersion(t, f)
		reply := w7Request(t, f, f.devTK, http.MethodPost, claimPath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w8-gen-404","work_item_id":"018f7900-0000-7000-8000-000000000099"}`, w7RunnerID), "")
		require.Equal(t, http.StatusNotFound, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "TASK_NOT_FOUND")

		var leases int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM leases`).Scan(&leases))
		assert.Zero(t, leases, "the refusal happens before any lease side effect")
		assert.Equal(t, before, w7QueueVersion(t, f), "the refusal does not move the queue token")
	})

	t.Run("targeted claim of an executing item is 409 with the holder", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost, claimPath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w8-gen-exec"}`, w7RunnerID), "")
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
		holderLease := replyJSONStringField(t, reply.Body.String(), "lease_id")
		require.NotEmpty(t, holderLease)

		before := w7QueueVersion(t, f)
		reply = w7Request(t, f, f.devTK, http.MethodPost, claimPath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w8-gen-exec","work_item_id":%q}`, w8SecondRunnerID, w7HeadItem), "")
		require.Equal(t, http.StatusConflict, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "CLAIM_TARGET_HELD")
		assert.Contains(t, reply.Body.String(), `"held_by_runner":"`+w7RunnerID+`"`,
			"the refusal names the current holder")
		assert.Contains(t, reply.Body.String(), `"lease_id":"`+holderLease+`"`)
		assert.Contains(t, reply.Body.String(), `"lease_expires_at":"`)

		var leases int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM leases`).Scan(&leases))
		assert.Equal(t, 1, leases, "the holder keeps its lease; no second lease appears")
		assert.Equal(t, before, w7QueueVersion(t, f), "the refusal does not move the queue token")
	})

	t.Run("targeted claim with a stale queue token is 409 CONCURRENCY_CONFLICT", func(t *testing.T) {
		// The targeted face keeps the queue CAS contract: a presented
		// token that is not the current one conflicts before any lease.
		stale := w7QueueVersion(t, f) + 1
		before := w7QueueVersion(t, f)
		reply := w7Request(t, f, f.devTK, http.MethodPost, claimPath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w8-gen-cas","work_item_id":%q,"queue_version":%d}`,
				w8SecondRunnerID, w7TailItem, stale), "")
		require.Equal(t, http.StatusConflict, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "CONCURRENCY_CONFLICT")
		assert.Equal(t, before, w7QueueVersion(t, f), "the refusal does not move the queue token")
	})

	t.Run("targeted claim behind a non-consumable gate is 409 without fall-through", func(t *testing.T) {
		// A stale binding (the digest-drift family) puts the tail behind a
		// locked gate the claim may not consume — and, being targeted, may
		// not skip either.
		digest := "sha256:" + strings.Repeat("a", 64)
		_, err := f.db.Exec(`INSERT INTO assets (asset_id, version, project_id, asset_type, title, status, owner_principal, sensitivity, source_digest, reviewed_at, approved_at)
			VALUES ('ART-w-gate-001', 1, $1, 'detailed-design', 'w8 gate asset', 'approved', 'user:dev-1', 'internal', $2, now(), now())`,
			w7ProjectID, digest)
		require.NoError(t, err)
		_, err = f.db.Exec(`INSERT INTO asset_gate_bindings (id, project_id, work_item_id, asset_id, bound_version, bound_digest, gate_id, status, staled_at)
			VALUES (gen_random_uuid(), $1, $2, 'ART-w-gate-001', 1, $3, 'boundary', 'stale', now())`,
			w7ProjectID, w7TailItem, digest)
		require.NoError(t, err)

		before := w7QueueVersion(t, f)
		reply := w7Request(t, f, f.devTK, http.MethodPost, claimPath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w8-gen-gate","work_item_id":%q}`, w8SecondRunnerID, w7TailItem), "")
		require.Equal(t, http.StatusConflict, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "ASSET_GATE_NOT_SATISFIED")
		assert.Equal(t, before, w7QueueVersion(t, f), "the refusal does not move the queue token")

		// The untargeted face keeps its skip semantics: the head is
		// executing and the tail is gate-blocked, so nothing is available
		// — the blocked item is never handed out.
		reply = w7Request(t, f, f.devTK, http.MethodPost, claimPath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w8-gen-gate"}`, w8SecondRunnerID), "")
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), `"available":false`)
	})
}

// TestW8TargetedClaimConcurrentRace is the parallel-lane proof: two
// bound runners target the same queued item at the same moment and
// exactly one lease survives — the row lock orders them inside the
// claim transaction and the partial unique index
// (idx_leases_one_active) is the structural last line. The loser learns
// who won (CLAIM_TARGET_HELD names the holder), not a bare conflict.
func TestW8TargetedClaimConcurrentRace(t *testing.T) {
	f := newW8TargetedFixture(t)
	claimPath := "/api/v3/projects/" + w7ProjectID + "/work-items/claim"

	start := make(chan struct{})
	replies := make(chan *httptest.ResponseRecorder, 2)
	for _, runner := range []string{w7RunnerID, w8SecondRunnerID} {
		go func(runnerID string) {
			<-start
			replies <- w7Request(t, f, f.devTK, http.MethodPost, claimPath,
				fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w8-race","work_item_id":%q}`, runnerID, w7TailItem), "")
		}(runner)
	}
	close(start)

	first, second := <-replies, <-replies
	codes := []int{first.Code, second.Code}
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, codes,
		"exactly one lane wins the target")

	var winner string
	require.NoError(t, f.db.QueryRow(
		`SELECT runner_id FROM leases WHERE work_item_id = $1 AND status = 'active'`, w7TailItem).Scan(&winner))
	assert.Contains(t, []string{w7RunnerID, w8SecondRunnerID}, winner)

	loser := second
	if second.Code == http.StatusOK {
		loser = first
	}
	require.Equal(t, http.StatusConflict, loser.Code, loser.Body.String())
	assert.Contains(t, loser.Body.String(), "CLAIM_TARGET_HELD")
	assert.Contains(t, loser.Body.String(), `"held_by_runner":"`+winner+`"`,
		"the loser's conflict names the lease that actually won")

	var activeLeases int
	require.NoError(t, f.db.QueryRow(
		`SELECT count(*) FROM leases WHERE work_item_id = $1 AND status = 'active'`, w7TailItem).Scan(&activeLeases))
	assert.Equal(t, 1, activeLeases, "the partial unique index keeps one active lease on the target")
}
