package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated integration tests for the quality-engine persistence: policy
// CAS, immutable evidence, verdict upserts with drift-driven staling,
// and the SQL-guarded waiver lifecycle.

func newQualityFixture(t *testing.T) (*PostgresStore, string, string) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_quality_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_quality_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_quality_test WITH (FORCE)`)
		_ = admin.Close()
	})

	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	isolated, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_quality_test")
	require.NoError(t, err)
	t.Cleanup(func() { isolated.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), isolated)
	require.NoError(t, err)

	pg, err := NewPostgresStore(isolated)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = isolated.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7100-0000-7000-8000-000000000001', 'quality team')`)
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ('018f7100-0000-7000-8000-000000000002', '018f7100-0000-7000-8000-000000000001', 'q-proj', 'Q Proj', 'active')`)
	require.NoError(t, err)
	return pg, "018f7100-0000-7000-8000-000000000002", "018f7100-0000-7000-8000-000000000003"
}

func qualityOverlay(id string, mutate func(*evidence.Policy)) *evidence.Policy {
	base, err := evidence.CompanyPolicy()
	if err != nil {
		panic(err)
	}
	overlay := *base
	overlay.ID = id
	overlay.Scope = "project"
	extends := "company-baseline"
	overlay.Extends = &extends
	if mutate != nil {
		mutate(&overlay)
	}
	return &overlay
}

func qualityTuple(projectID, workItemID string) evidence.Tuple {
	return evidence.Tuple{
		ProjectID:     projectID,
		WorkItemID:    workItemID,
		SourceSHA:     strings.Repeat("1", 40),
		TargetSHA:     strings.Repeat("2", 40),
		PolicyVersion: "3.0.0",
	}
}

func TestQualityPolicyCAS(t *testing.T) {
	pg, projectID, _ := newQualityFixture(t)
	ctx := context.Background()
	store := pg.Quality()

	// If-None-Match insert.
	version, err := store.PutProjectPolicy(ctx, projectID, qualityOverlay("acme-strict", nil), 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), version)

	// A second create conflicts.
	_, err = store.PutProjectPolicy(ctx, projectID, qualityOverlay("acme-other", nil), 0)
	assert.ErrorIs(t, err, ErrQualityPolicyConflict)

	// Wrong expected version conflicts; right one replaces.
	_, err = store.PutProjectPolicy(ctx, projectID, qualityOverlay("acme-v2", nil), 99)
	assert.ErrorIs(t, err, ErrQualityPolicyConflict)
	version, err = store.PutProjectPolicy(ctx, projectID, qualityOverlay("acme-v2", func(p *evidence.Policy) {
		p.Version = "3.1.0"
	}), version)
	require.NoError(t, err)
	assert.Equal(t, int64(2), version)

	loaded, gotVersion, err := store.GetProjectPolicy(ctx, projectID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), gotVersion)
	assert.Equal(t, "acme-v2", loaded.ID)
	assert.Equal(t, "3.1.0", loaded.Version)

	// Absent project reads as no overlay.
	none, noVersion, err := store.GetProjectPolicy(ctx, "018f7100-0000-7000-8000-00000000dead")
	require.NoError(t, err)
	assert.Nil(t, none)
	assert.Zero(t, noVersion)
}

func TestQualityEvidenceImmutableAndQueryable(t *testing.T) {
	pg, projectID, workItemID := newQualityFixture(t)
	ctx := context.Background()
	store := pg.Quality()

	tup := qualityTuple(projectID, workItemID)
	pipeline := int64(15)
	job := int64(150)
	record := &evidence.Record{
		EvidenceID: "018f7200-0000-7000-8000-000000000001", ProjectID: projectID, WorkItemID: workItemID,
		Kind: evidence.GateUnit, Authority: evidence.AuthorityMergeGate, Status: evidence.EvidencePassed,
		SourceSHA: tup.SourceSHA, TargetSHA: tup.TargetSHA, PipelineID: &pipeline, JobID: &job,
		PolicyVersion: "3.0.0", Attempt: 1,
		Producer: evidence.Producer{Type: "gitlab_job", ID: "ci", Version: "1.0"},
	}
	require.NoError(t, store.AppendEvidence(ctx, record))

	// Identity re-append collapses (at-least-once ingestion).
	require.NoError(t, store.AppendEvidence(ctx, record))

	records, err := store.ListEvidenceForWorkItem(ctx, projectID, workItemID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, evidence.GateUnit, records[0].Kind)
	assert.Equal(t, "ci", records[0].Producer.ID)

	// The append-only trigger rejects any UPDATE.
	_, err = pg.DB().ExecContext(ctx, `UPDATE evidence SET status = 'failed'`)
	require.Error(t, err, "evidence rows are append-only")
}

func TestQualityVerdictPersistsAndStales(t *testing.T) {
	pg, projectID, workItemID := newQualityFixture(t)
	ctx := context.Background()
	store := pg.Quality()

	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	resolved, err := evidence.ResolveEffective(company, nil)
	require.NoError(t, err)

	// Build a full passing set for the tuple.
	tup := qualityTuple(projectID, workItemID)
	records := []evidence.Record{}
	for index, check := range resolved.Policy.RequiredGates {
		pipeline, job := int64(20), int64(200+index)
		records = append(records, evidence.Record{
			EvidenceID: fmt.Sprintf("018f7300-0000-7000-8000-%012d", 100+index),
			ProjectID:  projectID, WorkItemID: workItemID, Kind: check,
			Authority: evidence.AuthorityMergeGate, Status: evidence.EvidencePassed,
			SourceSHA: tup.SourceSHA, TargetSHA: tup.TargetSHA, PipelineID: &pipeline, JobID: &job,
			PolicyVersion: "3.0.0", Attempt: 1,
			Producer: evidence.Producer{Type: "gitlab_job", ID: "ci", Version: "1.0"},
		})
	}
	for index := range records {
		require.NoError(t, store.AppendEvidence(ctx, &records[index]), records[index].Kind)
	}

	verdict, err := evidence.Evaluate(tup, resolved, records, nil, time.Now())
	require.NoError(t, err)
	require.True(t, verdict.Ready)
	require.NoError(t, store.PersistVerdict(ctx, verdict))

	snapshots, err := store.ListGateSnapshots(ctx, projectID, workItemID)
	require.NoError(t, err)
	require.Len(t, snapshots, 12)
	for _, snapshot := range snapshots {
		assert.Equal(t, evidence.GatePassed, snapshot.Status)
	}

	// Re-evaluating the same tuple is an idempotent upsert.
	require.NoError(t, store.PersistVerdict(ctx, verdict))
	snapshots, err = store.ListGateSnapshots(ctx, projectID, workItemID)
	require.NoError(t, err)
	assert.Len(t, snapshots, 12)

	// A SHA drift stales every old snapshot in the same transaction.
	drifted := tup
	drifted.SourceSHA = strings.Repeat("3", 40)
	records[0].EvidenceID = "018f7300-0000-7000-8000-0000000000ff"
	driftedVerdict, err := evidence.Evaluate(drifted, resolved, nil, nil, time.Now())
	require.NoError(t, err)
	require.False(t, driftedVerdict.Ready)
	require.NoError(t, store.PersistVerdict(ctx, driftedVerdict))

	snapshots, err = store.ListGateSnapshots(ctx, projectID, workItemID)
	require.NoError(t, err)
	assert.Len(t, snapshots, 24, "old and new tuple snapshots coexist")
	stale := 0
	for _, snapshot := range snapshots {
		if snapshot.Status == evidence.GateStale {
			stale++
		}
	}
	assert.Equal(t, 12, stale, "every old-tuple snapshot went stale")
}

func TestQualityWaiverLifecycle(t *testing.T) {
	pg, projectID, workItemID := newQualityFixture(t)
	ctx := context.Background()
	store := pg.Quality()
	now := time.Now()

	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	resolved, err := evidence.ResolveEffective(company, nil)
	require.NoError(t, err)
	tup := qualityTuple(projectID, workItemID)

	waiver, err := evidence.NewWaiver(resolved, evidence.WaiverRequestInput{
		GateID:          evidence.StableGateID(tup, evidence.GateUnit),
		Check:           evidence.GateUnit,
		SourceSHA:       tup.SourceSHA,
		MergeRequestIID: 7,
		Requester:       "user-requester",
		Reason:          "documented infra flake ticket-999",
		ExpiresAt:       now.Add(24 * time.Hour),
	}, now)
	require.NoError(t, err)

	waiverID, err := store.CreateWaiver(ctx, waiver, projectID, workItemID)
	require.NoError(t, err)
	assert.NotEmpty(t, waiverID)

	// The same tuple+check cannot stack a second waiver.
	_, err = store.CreateWaiver(ctx, waiver, projectID, workItemID)
	assert.ErrorIs(t, err, ErrWaiverConflict)

	// Self-approval is rejected in SQL with its own condition.
	err = store.ApproveWaiver(ctx, waiverID, "user-requester")
	assert.ErrorIs(t, err, ErrWaiverSelfApprove)

	// A distinct approver succeeds; double approval then conflicts.
	require.NoError(t, store.ApproveWaiver(ctx, waiverID, "user-approver"))
	assert.ErrorIs(t, store.ApproveWaiver(ctx, waiverID, "user-approver"), ErrWaiverConflict)
	require.NoError(t, store.RevokeWaiver(ctx, waiverID))
	assert.ErrorIs(t, store.RevokeWaiver(ctx, waiverID), ErrWaiverConflict)

	waivers, err := store.ListWaiversForWorkItem(ctx, projectID, workItemID)
	require.NoError(t, err)
	require.Len(t, waivers, 1)
	assert.Equal(t, evidence.WaiverRevoked, waivers[0].State)
	assert.Equal(t, "user-approver", waivers[0].Approver)

	assert.ErrorIs(t, store.ApproveWaiver(ctx, "018f7400-0000-7000-8000-00000000dead", "x"), ErrWaiverAbsent)
}
