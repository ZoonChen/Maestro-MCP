package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W9 (F64/F66) claim-face completion: the queued -> executing transition
// commits with its work_item.claimed audit row and work_item.state.changed
// outbox event in the same transaction (the release face's shape, F64),
// and the lease unique-index conflict answers CLAIM_TARGET_HELD instead
// of leaking a driver error (F66).

func w9ProjectUUID(index int) string {
	return fmt.Sprintf("018f4100-0000-7000-8000-%012d", index)
}

// seedW9SecondBoundRunner adds a second approved, project-bound runner so
// two lanes can race for one item.
func seedW9SecondBoundRunner(t *testing.T, db *sql.DB, projectID string) string {
	t.Helper()
	second := "018f4300-0000-7000-8000-000000000091"
	_, err := db.Exec(`INSERT INTO runners (id, display_name, device_key_hash, status)
		VALUES ($1, 'w9 second runner', 'sha256:w9', 'approved')`, second)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO runner_bindings (project_id, runner_id) VALUES ($1, $2)`,
		projectID, second)
	require.NoError(t, err)
	return second
}

// TestW9ClaimCommitsAuditAndOutboxAtomically is the F64 acceptance: every
// successful claim leaves a queryable work_item.claimed audit row (actor,
// execution, lease expiry in the reason) plus the state-change outbox
// event, and the exported audit chain still verifies.
func TestW9ClaimCommitsAuditAndOutboxAtomically(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	seedWorkItemFixture(t, db, 1)
	approveRunner(t, db, 1)
	project := w9ProjectUUID(1)

	claim, err := registry.ClaimNextWorkItem(ctx, runnerUUID(1), "w9-gen", 0, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, workItemUUID(1), claim.WorkItemID)

	var actor, reason string
	err = db.QueryRow(`
		SELECT actor_principal, reason FROM audit_events
		WHERE project_id = $1 AND action = 'work_item.claimed' AND resource_id = $2`,
		project, claim.WorkItemID).Scan(&actor, &reason)
	require.NoError(t, err, "the successful claim leaves its audit row")
	assert.Equal(t, "runner:"+runnerUUID(1), actor)
	assert.Contains(t, reason, "execution="+claim.ExecutionID)
	assert.Contains(t, reason, "lease="+claim.LeaseID)
	assert.Contains(t, reason, "lease_expires=")

	var payload []byte
	err = db.QueryRow(`
		SELECT payload FROM outbox_events
		WHERE event_type = 'work_item.state.changed' AND subject = $1
		ORDER BY occurred_at DESC LIMIT 1`, claim.WorkItemID).Scan(&payload)
	require.NoError(t, err, "the claim's state change lands in the outbox")
	var decoded struct {
		WorkItemID string `json:"work_item_id"`
		From       string `json:"from"`
		To         string `json:"to"`
		Version    int64  `json:"version"`
		ReasonCode string `json:"reason_code"`
	}
	require.NoError(t, json.Unmarshal(payload, &decoded))
	assert.Equal(t, claim.WorkItemID, decoded.WorkItemID)
	assert.Equal(t, "queued", decoded.From)
	assert.Equal(t, "executing", decoded.To)
	assert.Equal(t, int64(1), decoded.Version, "the post-transition work item version (seeded items start at 0)")
	assert.Equal(t, "WORK_ITEM_CLAIMED", decoded.ReasonCode)

	// The targeted face writes the same pair (W8 semantics unchanged).
	second := seedW9SecondBoundRunner(t, db, project)
	target := "018f4200-0000-7000-8000-000000000092"
	_, err = db.Exec(`INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, 'w9 target', 'queued')`,
		target, project)
	require.NoError(t, err)
	targeted, err := registry.ClaimTargetedWorkItem(ctx, second, "w9-gen-t", project, target, claim.QueueVersion, 90*time.Second)
	require.NoError(t, err)
	var targetedAudits int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'work_item.claimed' AND resource_id = $1`,
		targeted.WorkItemID).Scan(&targetedAudits))
	assert.Equal(t, 1, targetedAudits)

	// The chain over the project's audit slice (both claims included)
	// still verifies: new rows extend it, nothing rewrites it.
	var maxSeq int64
	require.NoError(t, db.QueryRow(`SELECT COALESCE(max(id), 0) FROM audit_events WHERE project_id = $1`, project).Scan(&maxSeq))
	rows, digests, _, err := registry.Observability().AuditExport(ctx, project, 1, maxSeq)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	require.NoError(t, registry.Observability().AuditChainVerify(ctx, project, 1, maxSeq, digests))
}

// TestW9ClaimLeaseUniqueViolationMapsToHeld drives the F66 mapping: the
// partial unique index firing inside the claim transaction must surface
// ErrClaimTargetHeld (with the holder when one is visible), never a raw
// driver error, and nothing may commit when it fires.
func TestW9ClaimLeaseUniqueViolationMapsToHeld(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	seedWorkItemFixture(t, db, 1)
	approveRunner(t, db, 1)
	project := w9ProjectUUID(1)

	// A BEFORE INSERT trigger raising SQLSTATE 23505 simulates the
	// unique-index race the row lock cannot order (a lease writer that
	// bypasses the claim path committing inside the claim's window).
	_, err = db.Exec(`CREATE FUNCTION w9_raise_unique() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'w9 simulated lease race' USING ERRCODE = '23505'; END $$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER w9_lease_race BEFORE INSERT ON leases
		FOR EACH ROW EXECUTE FUNCTION w9_raise_unique()`)
	require.NoError(t, err)

	_, err = registry.ClaimNextWorkItem(ctx, runnerUUID(1), "w9-gen-race", 0, 90*time.Second)
	require.ErrorIs(t, err, ErrClaimTargetHeld, "the unique-index loser learns the 409 face, not a 500")
	require.NotErrorIs(t, err, ErrConcurrentConflict)

	// Atomicity: the failed claim leaves no lease, execution, audit or
	// outbox row behind.
	for _, row := range []struct {
		query string
		args  []any
		label string
	}{
		{`SELECT count(*) FROM leases`, nil, "leases"},
		{`SELECT count(*) FROM executions`, nil, "executions"},
		{`SELECT count(*) FROM audit_events WHERE action = 'work_item.claimed'`, nil, "claim audit rows"},
		{`SELECT count(*) FROM outbox_events WHERE subject = $1`, []any{workItemUUID(1)}, "claim outbox rows"},
	} {
		var count int
		require.NoError(t, db.QueryRow(row.query, row.args...).Scan(&count))
		assert.Zero(t, count, "the refused claim commits no %s", row.label)
	}

	_, err = db.Exec(`DROP TRIGGER w9_lease_race ON leases; DROP FUNCTION w9_raise_unique()`)
	require.NoError(t, err)

	// With the simulated racer gone the same claim succeeds — the
	// mapping refused the attempt, it did not poison the item.
	claim, err := registry.ClaimNextWorkItem(ctx, runnerUUID(1), "w9-gen-after", 0, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, workItemUUID(1), claim.WorkItemID)

	// The holder-enriched variant: with a committed active lease visible,
	// the conflict answer names the holder, lease and expiry.
	second := seedW9SecondBoundRunner(t, db, project)
	target := "018f4200-0000-7000-8000-000000000093"
	_, err = db.Exec(`INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, 'w9 held target', 'queued')`,
		target, project)
	require.NoError(t, err)
	expires := time.Now().UTC().Add(30 * time.Minute)
	holderLease := "018f4400-0000-7000-8000-000000000094"
	_, err = db.Exec(`INSERT INTO leases (id, project_id, work_item_id, runner_id, epoch, connection_generation, status, version, expires_at)
		VALUES ($1, $2, $3, $4, 1, 'w9-holder', 'active', 1, $5)`,
		holderLease, project, target, second, expires)
	require.NoError(t, err)

	heldErr := registry.heldByActiveLease(ctx, project, target)
	require.ErrorIs(t, heldErr, ErrClaimTargetHeld)
	var held *ClaimTargetHeld
	require.ErrorAs(t, heldErr, &held)
	assert.Equal(t, target, held.WorkItemID)
	assert.Equal(t, second, held.RunnerID)
	assert.Equal(t, holderLease, held.LeaseID)
	assert.WithinDuration(t, expires, held.ExpiresAt, time.Second)

	// The generic lookup failure stays a lookup failure (wrapped, no
	// sentinel): a canceled context is the cheapest honest failure.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	lookupErr := registry.heldByActiveLease(canceled, project, target)
	require.Error(t, lookupErr)
	assert.NotErrorIs(t, lookupErr, ErrClaimTargetHeld)
	assert.Contains(t, lookupErr.Error(), "holder lookup after conflict")
}

// TestW9ClaimRefusesWhenTransitionSkipped covers the fail-closed guard
// on the guarded transition: if the queued -> executing UPDATE matches
// no rows (here a BEFORE UPDATE trigger returning NULL), the claim
// refuses with CONCURRENCY_CONFLICT and commits nothing — it never
// leaves a lease over an item it did not transition.
func TestW9ClaimRefusesWhenTransitionSkipped(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	seedWorkItemFixture(t, db, 1)
	approveRunner(t, db, 1)

	_, err = db.Exec(`CREATE FUNCTION w9_skip_update() RETURNS trigger AS $$
		BEGIN RETURN NULL; END $$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER w9_skip_transition BEFORE UPDATE ON work_items
		FOR EACH ROW EXECUTE FUNCTION w9_skip_update()`)
	require.NoError(t, err)

	_, err = registry.ClaimNextWorkItem(ctx, runnerUUID(1), "w9-gen-skip", 0, 90*time.Second)
	require.ErrorIs(t, err, ErrConcurrentConflict)

	for _, probe := range []struct {
		query string
		label string
	}{
		{`SELECT count(*) FROM leases`, "leases"},
		{`SELECT count(*) FROM executions`, "executions"},
		{`SELECT count(*) FROM audit_events WHERE action = 'work_item.claimed'`, "claim audit rows"},
		{`SELECT count(*) FROM outbox_events`, "outbox rows"},
	} {
		var count int
		require.NoError(t, db.QueryRow(probe.query).Scan(&count))
		assert.Zero(t, count, "the refused claim commits no %s", probe.label)
	}
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, workItemUUID(1)).Scan(&status))
	assert.Equal(t, "queued", status)
}
