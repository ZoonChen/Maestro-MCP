package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated coverage for the core event/migration/runner/quality paths:
// outbox retry and dead-letter settles, verdict re-evaluation upserts,
// migration validation drift, and the optionality helpers.

func newCoreGapFixture(t *testing.T) (*PostgresStore, struct{}) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_coregap_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_coregap_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_coregap_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_coregap_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7c00-0000-7000-8000-000000000001', 'coregap')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ('018f7c00-0000-7000-8000-000000000002', '018f7c00-0000-7000-8000-000000000001', 'coregap', 'Coregap', 'active')`)
	require.NoError(t, err)
	return pg, struct{}{}
}

func TestOutboxRetryAndDeadLetterSettles(t *testing.T) {
	pg, _ := newCoreGapFixture(t)
	ctx := context.Background()

	// A pending event claims, then settles through retry and dead letter.
	pending := testOutboxEvent("coregap-retry-1")
	require.NoError(t, pg.Outbox().Enqueue(ctx, pending))
	claimed, err := pg.Outbox().ClaimPending(ctx, 8, "core-owner", time.Now().UTC().Format(time.RFC3339))
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	// Wrong owner cannot settle; the owner retries; the retry re-claims
	// once available; exhaustion dead-letters.
	require.Error(t, pg.Outbox().MarkRetry(ctx, claimed[0].EventID, "not-owner", 3, time.Now().UTC().Format(time.RFC3339)))
	require.NoError(t, pg.Outbox().MarkRetry(ctx, claimed[0].EventID, "core-owner", 3,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)))

	reclaimed, err := pg.Outbox().ClaimPending(ctx, 8, "core-owner", time.Now().UTC().Format(time.RFC3339))
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	require.Equal(t, 4, reclaimed[0].Attempts, "claim(2) -> retry floor(3) -> reclaim(+1)")

	require.Error(t, pg.Outbox().MarkDeadLetter(ctx, reclaimed[0].EventID, "not-owner"))
	require.NoError(t, pg.Outbox().MarkDeadLetter(ctx, reclaimed[0].EventID, "core-owner"))

	// Delivered settle on an unknown event reports the duplicate
	// sentinel (the claim-verify path).
	err = pg.Outbox().MarkDelivered(ctx, "018f7c00-0000-7000-8000-00000000dead", "core-owner")
	assert.ErrorIs(t, err, ErrDuplicateEvent)

	// The inbox record/claim/mark trio through the generic store.
	inbox := testInboxEvent("coregap-inbox-1")
	created, err := pg.Inbox().Record(ctx, inbox)
	require.NoError(t, err)
	assert.True(t, created)
	duplicate, err := pg.Inbox().Record(ctx, inbox)
	require.NoError(t, err)
	assert.False(t, duplicate)
	require.NoError(t, pg.Inbox().ClaimProcessing(ctx, inbox.EventID))
	require.NoError(t, pg.Inbox().MarkProcessed(ctx, inbox.EventID))
	require.Error(t, pg.Inbox().ClaimProcessing(ctx, inbox.EventID), "processed rows do not re-claim")
}

func TestVerdictReEvaluationUpsertsAndStales(t *testing.T) {
	pg, _ := newCoreGapFixture(t)
	ctx := context.Background()
	projectID, workItemID := "018f7c00-0000-7000-8000-000000000002", "018f7c00-0000-7000-8000-000000000003"
	_, err := pg.DB().ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, 'cg', 'validating')`,
		workItemID, projectID)
	require.NoError(t, err)

	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	resolved, err := evidence.ResolveEffective(company, nil)
	require.NoError(t, err)

	tuple := evidence.Tuple{ProjectID: projectID, WorkItemID: workItemID,
		SourceSHA: strings.Repeat("3", 40), TargetSHA: strings.Repeat("4", 40), PolicyVersion: "3.0.0"}

	// All-gates-passing evidence set, then a re-evaluation over the same
	// tuple (the upsert path), then a drifted tuple (the stale path).
	records := []evidence.Record{}
	for index, check := range resolved.Policy.RequiredGates {
		pipeline, job := int64(70), int64(700+index)
		record := evidence.Record{
			EvidenceID: fmt.Sprintf("018f7c00-0000-7000-8000-%012d", 100+index),
			ProjectID:  projectID, WorkItemID: workItemID, Kind: check,
			Authority: evidence.AuthorityMergeGate, Status: evidence.EvidencePassed,
			SourceSHA: tuple.SourceSHA, TargetSHA: tuple.TargetSHA,
			PipelineID: &pipeline, JobID: &job, PolicyVersion: "3.0.0", Attempt: 1,
			Producer: evidence.Producer{Type: "gitlab_job", ID: "ci", Version: "1"},
			Digest:   "sha256:" + strings.Repeat(string(rune('0'+index%10)), 64),
		}
		require.NoError(t, pg.Quality().AppendEvidence(ctx, &record), check)
		records = append(records, record)
	}
	verdict, err := evidence.Evaluate(tuple, resolved, records, nil, time.Now())
	require.NoError(t, err)
	require.True(t, verdict.Ready)
	require.NoError(t, pg.Quality().PersistVerdict(ctx, verdict))
	require.NoError(t, pg.Quality().PersistVerdict(ctx, verdict), "re-evaluation is an idempotent upsert")

	drifted := tuple
	drifted.SourceSHA = strings.Repeat("5", 40)
	driftedVerdict, err := evidence.Evaluate(drifted, resolved, nil, nil, time.Now())
	require.NoError(t, err)
	require.NoError(t, pg.Quality().PersistVerdict(ctx, driftedVerdict))

	snapshots, err := pg.Quality().ListGateSnapshots(ctx, projectID, workItemID)
	require.NoError(t, err)
	stale, passed := 0, 0
	for _, snapshot := range snapshots {
		switch snapshot.Status {
		case "stale":
			stale++
		case "pending":
			passed++
		}
	}
	assert.Equal(t, 12, stale, "the drifted evaluation stales the old tuple")
	assert.Equal(t, 12, passed, "the new tuple starts pending")
}

func TestMigrationValidationDrift(t *testing.T) {
	pg, _ := newCoreGapFixture(t)
	ctx := context.Background()

	// A catalog row whose digest no longer matches the embedded script
	// fails validation with the integrity sentinel.
	_, err := pg.DB().ExecContext(ctx,
		`UPDATE maestro_meta.schema_migrations SET digest = 'sha256:' || repeat('f', 64) WHERE version = 2`)
	require.NoError(t, err)
	err = ValidatePostgresSchema(ctx, pg.DB())
	require.Error(t, err, "digest drift fails validation")
	assert.ErrorIs(t, err, ErrPostgresMigrationIntegrity)

	// Zero-step revert is rejected; a digest-drifted catalog also
	// blocks the revert path with the same sentinel (drift never
	// repairs itself silently).
	_, err = RevertPostgresMigrations(ctx, pg.DB(), 0)
	require.Error(t, err, "revert steps must be positive")
	_, err = RevertPostgresMigrations(ctx, pg.DB(), 1)
	assert.ErrorIs(t, err, ErrPostgresMigrationIntegrity)
}

func TestRunnerInitialStatusMapping(t *testing.T) {
	pg, _ := newCoreGapFixture(t)
	ctx := context.Background()
	projectID := "018f7c00-0000-7000-8000-000000000002"

	// Pending devices map through the initial-status path.
	pending := &model.RunnerDevice{DisplayName: "cg-pending", DeviceKeyHash: "sha256:x",
		Status: model.RunnerStatusPendingApproval, Capabilities: []byte(`["a","b","c"]`)}
	require.NoError(t, pg.RunnerRegistry().CreateRunner(ctx, pending, &model.RunnerBinding{ProjectID: projectID}))

	listed, err := pg.RunnerRegistry().ListRunnersByProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	// Revocation lands and is observable.
	require.NoError(t, pg.RunnerRegistry().RevokeRunner(ctx, listed[0].ID))
	revoked, err := pg.RunnerRegistry().GetRunner(ctx, listed[0].ID)
	require.NoError(t, err)
	assert.Equal(t, model.RunnerStatusRevoked, revoked.Status)
	// Re-revocation is tolerated (idempotent terminal state).
	require.NoError(t, pg.RunnerRegistry().RevokeRunner(ctx, listed[0].ID))
}

func TestSensitivityAndOptionalityHelpers(t *testing.T) {
	pg, _ := newCoreGapFixture(t)
	ctx := context.Background()
	projectID, workItemID := "018f7c00-0000-7000-8000-000000000002", "018f7c00-0000-7000-8000-000000000003"

	// A record with no explicit sensitivity defaults to confidential.
	record := &evidence.Record{
		EvidenceID: "018f7c00-0000-7000-8000-0000000000e1", ProjectID: projectID,
		WorkItemID: workItemID, Kind: evidence.GateUnit, Authority: evidence.AuthorityDiagnostic,
		Status: evidence.EvidencePassed, SourceSHA: strings.Repeat("3", 40), TargetSHA: strings.Repeat("4", 40),
		PolicyVersion: "3.0.0", Attempt: 1,
		Producer: evidence.Producer{Type: "runner_profile", ID: "p", Version: "1"},
	}
	require.NoError(t, pg.Quality().AppendEvidence(ctx, record))
	var sensitivity string
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT sensitivity FROM evidence WHERE id = $1`, record.EvidenceID).Scan(&sensitivity))
	assert.Equal(t, "confidential", sensitivity)

	// Outbox envelope with optional timestamps and payload rides the
	// optionality helpers both empty and set.
	bare := testOutboxEvent("coregap-opt-1")
	require.NoError(t, pg.Outbox().Enqueue(ctx, bare))
	full := testOutboxEvent("coregap-opt-2")
	full.AvailableAt = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	require.NoError(t, pg.Outbox().Enqueue(ctx, full))
}
