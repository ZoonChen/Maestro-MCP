package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated claim/lease lifecycle: dispatch, queue-token CAS, generation
// fencing, heartbeat renewal and honest completion mapping — the v3
// Runner API core (M0.5 blocker #3 closeout).

func TestClaimHeartbeatCompleteLifecycle(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	seedWorkItemFixture(t, db, 1)
	approveRunner(t, db, 1)

	// Stale queue token conflicts.
	_, err = registry.ClaimNextWorkItem(ctx, runnerUUID(1), "gen-1", 99, 90*time.Second)
	require.ErrorIs(t, err, ErrConcurrentConflict)

	// Fresh token dispatches the queued item.
	claim, err := registry.ClaimNextWorkItem(ctx, runnerUUID(1), "gen-1", 0, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, workItemUUID(1), claim.WorkItemID)
	assert.Equal(t, int64(1), claim.LeaseVersion)
	assert.Equal(t, int64(1), claim.LeaseEpoch)
	assert.NotEmpty(t, claim.ExecutionID)
	assert.Equal(t, int64(1), claim.QueueVersion, "the queue token advances for the next claim")

	// No second active lease for the same item.
	_, err = registry.ClaimNextWorkItem(ctx, runnerUUID(1), "gen-1", claim.QueueVersion, 90*time.Second)
	assert.ErrorIs(t, err, ErrNoAvailableTask)

	// Heartbeat: wrong generation fences out, right one renews.
	_, err = registry.RunnerLeaseHeartbeat(ctx, claim.LeaseID, runnerUUID(1), "gen-other", claim.LeaseVersion, 90*time.Second)
	assert.ErrorIs(t, err, ErrLeaseVersionMismatch)
	newVersion, err := registry.RunnerLeaseHeartbeat(ctx, claim.LeaseID, runnerUUID(1), "gen-1", claim.LeaseVersion, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, claim.LeaseVersion+1, newVersion)

	// A stale generation cannot complete even with the right version.
	sha := "0123456789abcdef0123456789abcdef01234567"
	err = registry.CompleteExecution(ctx, claim.ExecutionID, runnerUUID(1), "gen-other", "completed", &sha, "done")
	assert.ErrorIs(t, err, ErrRunnerGenerationStale)

	// Honest completion maps to validating and releases the lease.
	err = registry.CompleteExecution(ctx, claim.ExecutionID, runnerUUID(1), "gen-1", "completed", &sha, "done")
	require.NoError(t, err)

	var status string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT status FROM work_items WHERE id = $1`, workItemUUID(1)).Scan(&status))
	assert.Equal(t, "validating", status)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT status FROM leases WHERE id = $1`, claim.LeaseID).Scan(&status))
	assert.Equal(t, "completed", status)

	// Heartbeats on a completed lease are gone (410 semantics upstream).
	_, err = registry.RunnerLeaseHeartbeat(ctx, claim.LeaseID, runnerUUID(1), "gen-1", newVersion, 90*time.Second)
	assert.ErrorIs(t, err, ErrLeaseVersionMismatch)
}

func TestClaimRequiresEligibleRunner(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	seedWorkItemFixture(t, db, 2)

	// The fixture runner is pending_approval: claiming is forbidden.
	_, err = registry.ClaimNextWorkItem(ctx, runnerUUID(2), "gen-1", 0, 90*time.Second)
	assert.ErrorIs(t, err, ErrRunnerStatusInvalid)

	approveRunner(t, db, 2)
	claim, err := registry.ClaimNextWorkItem(ctx, runnerUUID(2), "gen-1", 0, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, workItemUUID(2), claim.WorkItemID)
}

func TestCompleteFailureOutcomeMapsHonestly(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	seedWorkItemFixture(t, db, 3)
	approveRunner(t, db, 3)

	claim, err := registry.ClaimNextWorkItem(ctx, runnerUUID(3), "gen-1", 0, 90*time.Second)
	require.NoError(t, err)

	// A failed outcome needs no commit sha and maps the work item to failed.
	err = registry.CompleteExecution(ctx, claim.ExecutionID, runnerUUID(3), "gen-1", "failed", nil, "tests failed")
	require.NoError(t, err)
	var status string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT status FROM work_items WHERE id = $1`, workItemUUID(3)).Scan(&status))
	assert.Equal(t, "failed", status)

	// Unknown outcomes are parameter errors, never silent mappings.
	// Unknown outcomes are parameter errors, never silent mappings —
	// verified on a second, still-running claim so the finished one does
	// not mask the parameter check.
	seedWorkItemFixture(t, db, 4)
	approveRunner(t, db, 4)
	second, err := registry.ClaimNextWorkItem(ctx, runnerUUID(4), "gen-1", 0, 90*time.Second)
	require.NoError(t, err)
	err = registry.CompleteExecution(ctx, second.ExecutionID, runnerUUID(4), "gen-1", "mystery", nil, "")
	assert.ErrorIs(t, err, ErrInvalidParameter)
}

// --- shared fixture -------------------------------------------------------

func resetWorkItemSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;")
	require.NoError(t, err)
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
}

func approveRunner(t *testing.T, db *sql.DB, index int) {
	t.Helper()
	_, err := db.Exec(`UPDATE runners SET status = 'approved' WHERE id = $1`, runnerUUID(index))
	require.NoError(t, err)
}

func runnerUUID(index int) string { return fmt.Sprintf("018f4300-0000-7000-8000-%012d", index) }

func workItemUUID(index int) string { return fmt.Sprintf("018f4200-0000-7000-8000-%012d", index) }

func seedWorkItemFixture(t *testing.T, db *sql.DB, index int) {
	t.Helper()
	ctx := context.Background()
	teamID := fmt.Sprintf("018f4000-0000-7000-8000-%012d", index)
	projectID := fmt.Sprintf("018f4100-0000-7000-8000-%012d", index)
	_, err := db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, $2)`, teamID, fmt.Sprintf("team-%02d", index))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, $3, $4, 'active')`,
		projectID, teamID, fmt.Sprintf("proj-%02d", index), fmt.Sprintf("Project %d", index))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, priority, role)
		VALUES ($1, $2, $3, 'queued', 'normal', 'backend')`,
		fmt.Sprintf("018f4200-0000-7000-8000-%012d", index), projectID, fmt.Sprintf("Work item %d", index))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO runners (id, display_name, device_key_hash, status)
		VALUES ($1, $2, $3, 'pending_approval')`,
		fmt.Sprintf("018f4300-0000-7000-8000-%012d", index), fmt.Sprintf("runner-%02d", index), fmt.Sprintf("sha256:runner-%02d", index))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO runner_bindings (project_id, runner_id) VALUES ($1, $2)`,
		projectID, fmt.Sprintf("018f4300-0000-7000-8000-%012d", index))
	require.NoError(t, err)
}

func TestClaimGuardsAndErrorPaths(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	seedWorkItemFixture(t, db, 5)
	approveRunner(t, db, 5)

	// Unknown runner fails closed.
	_, err = registry.ClaimNextWorkItem(ctx, "018f4300-0000-7000-8000-00000000dead", "gen-1", 0, 90*time.Second)
	assert.ErrorIs(t, err, ErrRunnerNotFound)

	// Revoked runners never claim.
	seedWorkItemFixture(t, db, 6)
	_, err = db.Exec(`UPDATE runners SET status = 'revoked' WHERE id = $1`, runnerUUID(6))
	require.NoError(t, err)
	_, err = registry.ClaimNextWorkItem(ctx, runnerUUID(6), "gen-1", 0, 90*time.Second)
	assert.ErrorIs(t, err, ErrRunnerStatusInvalid)

	// Heartbeat on an unknown lease is 410-mapped; known-but-stale is a
	// version mismatch.
	_, err = registry.RunnerLeaseHeartbeat(ctx, "018f4400-0000-7000-8000-00000000dead", runnerUUID(5), "gen-1", 1, 90*time.Second)
	assert.ErrorIs(t, err, ErrLeaseNotFound)
	claim, err := registry.ClaimNextWorkItem(ctx, runnerUUID(5), "gen-1", 0, 90*time.Second)
	require.NoError(t, err)
	_, err = registry.RunnerLeaseHeartbeat(ctx, claim.LeaseID, runnerUUID(5), "gen-1", 999, 90*time.Second)
	assert.ErrorIs(t, err, ErrLeaseVersionMismatch)

	// Completion of an unknown execution fences closed.
	err = registry.CompleteExecution(ctx, "018f4500-0000-7000-8000-00000000dead", runnerUUID(5), "gen-1", "failed", nil, "")
	assert.ErrorIs(t, err, ErrLeaseNotFound)

	// Blocked and cancelled outcomes map honestly.
	err = registry.CompleteExecution(ctx, claim.ExecutionID, runnerUUID(5), "gen-1", "blocked", nil, "why")
	require.NoError(t, err)
	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, claim.WorkItemID).Scan(&status))
	assert.Equal(t, "blocked", status)
}

// --- S2B2-F8: claim dispatch is priority-first, FIFO within a band ---

// seedPriorityFixture seeds one project/runner pair with queued items
// shaped by the caller (id suffix, priority, created_at age in
// seconds) so ordering assertions are exact.
func seedPriorityFixture(t *testing.T, db *sql.DB, index int, items [][3]any) {
	t.Helper()
	ctx := context.Background()
	teamID := fmt.Sprintf("018f4000-0000-7000-8000-%012d", index)
	projectID := fmt.Sprintf("018f4100-0000-7000-8000-%012d", index)
	_, err := db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, $2)`, teamID, fmt.Sprintf("prio-team-%02d", index))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, $3, $4, 'active')`,
		projectID, teamID, fmt.Sprintf("prio-%02d", index), fmt.Sprintf("Prio %d", index))
	require.NoError(t, err)
	for _, item := range items {
		_, err = db.ExecContext(ctx, `
			INSERT INTO work_items (id, project_id, title, status, priority, created_at)
			VALUES ($1, $2, $3, 'queued', $4, now() - make_interval(secs => $5::int))`,
			fmt.Sprintf("018f4600-0000-7000-8000-%012d", item[0]), projectID,
			fmt.Sprintf("Prio item %d", item[0]), item[1], item[2])
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO runners (id, display_name, device_key_hash, status)
		VALUES ($1, $2, $3, 'approved')`,
		fmt.Sprintf("018f4700-0000-7000-8000-%012d", index), fmt.Sprintf("prio-runner-%02d", index), fmt.Sprintf("sha256:prio-%02d", index))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO runner_bindings (project_id, runner_id) VALUES ($1, $2)`,
		projectID, fmt.Sprintf("018f4700-0000-7000-8000-%012d", index))
	require.NoError(t, err)
}

func prioItemUUID(suffix int) string {
	return fmt.Sprintf("018f4600-0000-7000-8000-%012d", suffix)
}

func prioRunnerUUID(index int) string {
	return fmt.Sprintf("018f4700-0000-7000-8000-%012d", index)
}

func TestClaimDispatchesPriorityBeforeFIFO(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)

	// The FIFO story alone (S2B first slice) dispatched the earliest
	// item; F8 showed a P1 head blocking P0 targets. Items: 1=normal
	// (oldest), 2=high (middle), 3=urgent (newest), 4=low (oldest).
	seedPriorityFixture(t, db, 90, [][3]any{
		{1, "normal", 400},
		{2, "high", 300},
		{3, "urgent", 200},
		{4, "low", 500},
	})

	first, err := registry.ClaimNextWorkItem(ctx, prioRunnerUUID(90), "gen-1", 0, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, prioItemUUID(3), first.WorkItemID, "urgent outranks FIFO age")

	second, err := registry.ClaimNextWorkItem(ctx, prioRunnerUUID(90), "gen-2", first.QueueVersion, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, prioItemUUID(2), second.WorkItemID, "high follows urgent")

	third, err := registry.ClaimNextWorkItem(ctx, prioRunnerUUID(90), "gen-3", second.QueueVersion, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, prioItemUUID(1), third.WorkItemID, "normal beats low regardless of age")

	fourth, err := registry.ClaimNextWorkItem(ctx, prioRunnerUUID(90), "gen-4", third.QueueVersion, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, prioItemUUID(4), fourth.WorkItemID, "low is last")
}

func TestClaimStaysFIFOWithinOnePriorityBand(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)

	seedPriorityFixture(t, db, 91, [][3]any{
		{1, "high", 300},
		{2, "high", 200},
		{3, "high", 100},
	})

	first, err := registry.ClaimNextWorkItem(ctx, prioRunnerUUID(91), "gen-1", 0, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, prioItemUUID(1), first.WorkItemID, "same band: oldest (300s ago) first")

	second, err := registry.ClaimNextWorkItem(ctx, prioRunnerUUID(91), "gen-2", first.QueueVersion, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, prioItemUUID(2), second.WorkItemID)
}
