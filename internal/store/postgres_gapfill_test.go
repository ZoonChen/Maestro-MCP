package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated branch coverage for the M2 store surfaces: the miss/error
// and update paths the happy-path drills do not reach.

func newGapfillFixture(t *testing.T) (*PostgresStore, string, string) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_gapfill_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_gapfill_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_gapfill_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_gapfill_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7b00-0000-7000-8000-000000000001', 'gapfill')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ('018f7b00-0000-7000-8000-000000000002', '018f7b00-0000-7000-8000-000000000001', 'gapfill', 'Gapfill', 'active')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO work_items (id, project_id, title, status) VALUES ('018f7b00-0000-7000-8000-000000000003', '018f7b00-0000-7000-8000-000000000002', 'gap task', 'draft')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ('018f7b00-0000-7000-8000-000000000004', 'https://gitlab.gap.example', 'gap', 'env:A', 'env:B')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, default_branch)
		VALUES ('018f7b00-0000-7000-8000-000000000004', 9700, '018f7b00-0000-7000-8000-000000000002', 'main')`)
	require.NoError(t, err)
	return pg, "018f7b00-0000-7000-8000-000000000002", "018f7b00-0000-7000-8000-000000000003"
}

func TestValidateInstanceURLRejects(t *testing.T) {
	_, _, _ = newGapfillFixture(t) // migrations for a live context
	for _, raw := range []string{
		"http://plain.example/gitlab",
		"https://user:pass@gitlab.example",
		"https://10.1.2.3/gitlab",
		"https:///no-host",
		"://not-a-url",
	} {
		_, err := pgInstanceStore{db: nil}.byPass(t, raw)
		require.Error(t, err, raw)
	}
}

// byPass routes through CreateInstance so validation runs on the real
// store surface (nil db is fine: validation rejects before any query).
func (s pgInstanceStore) byPass(t *testing.T, raw string) (InstanceView, error) {
	t.Helper()
	view, err := s.CreateInstance(context.Background(), raw, "n", "env:A", "env:B")
	return view, err
}

func TestGitLabStoreBranches(t *testing.T) {
	pg, projectID, workItemID := newGapfillFixture(t)
	ctx := context.Background()
	gitlabStore := pg.GitLab()
	instanceID := "018f7b00-0000-7000-8000-000000000004"

	// Mapping miss and hit.
	mapped, err := gitlabStore.MappingProject(ctx, instanceID, 9700)
	require.NoError(t, err)
	assert.Equal(t, projectID, mapped)
	missed, err := gitlabStore.MappingProject(ctx, instanceID, 99999)
	require.NoError(t, err)
	assert.Empty(t, missed)

	// Unmapped projects record nothing (early return).
	require.NoError(t, gitlabStore.UpsertMergeRequest(ctx, "", gitlab.MergeRequestRecord{
		InstanceID: instanceID, GitlabProject: 99999, IID: 1,
		SourceBranch: "x", TargetBranch: "main",
	}, ""))

	// MR upsert then conflict-update on the same external identity.
	rec := gitlab.MergeRequestRecord{
		InstanceID: instanceID, GitlabProject: 9700, IID: 7,
		State: "opened", SourceBranch: "maestro/gap/mr", TargetBranch: "main",
		SourceSHA: strings.Repeat("a", 40), TargetSHA: strings.Repeat("b", 40),
	}
	require.NoError(t, gitlabStore.UpsertMergeRequest(ctx, projectID, rec, workItemID))
	rec.State = "merged"
	rec.MergeCommit = strings.Repeat("c", 40)
	rec.MergedAt = time.Now().UTC().Format(time.RFC3339)
	require.NoError(t, gitlabStore.UpsertMergeRequest(ctx, projectID, rec, workItemID))

	// Pipeline/job upserts with conflict-update on second write.
	pipe := gitlab.PipelineRecord{
		InstanceID: instanceID, ProjectID: projectID, GitlabProject: 9700,
		PipelineID: 8800, SHA: strings.Repeat("a", 40), Ref: "maestro/gap/mr",
		Status: "running", Source: "merge_request_event",
	}
	require.NoError(t, gitlabStore.UpsertPipeline(ctx, pipe))
	pipe.Status = "success"
	require.NoError(t, gitlabStore.UpsertPipeline(ctx, pipe))
	job := gitlab.JobRecord{InstanceID: instanceID, PipelineID: 8800, JobID: 99001,
		Name: "unit", Status: "success", Stage: "test"}
	require.NoError(t, gitlabStore.UpsertJob(ctx, job))
	require.NoError(t, gitlabStore.UpsertJob(ctx, job))

	// Done edge: missing fact arguments, unresolvable binding, replay.
	_, _, err = gitlabStore.MarkWorkItemDoneFromMerge(ctx, projectID, workItemID, "", "")
	require.Error(t, err)
	_, withheld, err := gitlabStore.MarkWorkItemDoneFromMerge(ctx, projectID,
		"018f7b00-0000-7000-8000-00000000dead", strings.Repeat("c", 40), "fact-1")
	require.NoError(t, err)
	assert.True(t, withheld, "an unresolvable binding withholds")

	// Gate snapshot by id: miss and hit.
	missing, found, err := pg.Quality().GateSnapshotByID(ctx, projectID, "018f7b00-0000-7000-8000-00000000dead")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, missing)

	absentWaiver, found, err := pg.Quality().WaiverByID(ctx, projectID, "018f7b00-0000-7000-8000-00000000dead")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, absentWaiver)

	// Projection: found (with pipelines) and not-found.
	projection, found, err := pg.Instances().MergeRequestProjection(ctx, projectID, 7)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "merged", projection["state"])
	_, found, err = pg.Instances().MergeRequestProjection(ctx, projectID, 999)
	require.NoError(t, err)
	assert.False(t, found)

	// Mapping context resolves the full reconcile scope.
	_, _, _, numeric, branch, version, found, err := pg.Instances().ProjectMappingContext(ctx, projectID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(9700), numeric)
	assert.Equal(t, "main", branch)
	assert.GreaterOrEqual(t, version, int64(1))
	_, _, _, _, _, _, found, err = pg.Instances().ProjectMappingContext(ctx, "018f7b00-0000-7000-8000-00000000dead")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestWebhookStoreBranches(t *testing.T) {
	pg, _, _ := newGapfillFixture(t)
	ctx := context.Background()
	instanceID := "018f7b00-0000-7000-8000-000000000004"

	// Claim requires an owner.
	_, err := pg.Webhooks().ClaimInbox(ctx, "")
	require.Error(t, err)

	// Sealed ingest then claim, retry settle, dead-letter settle.
	sealed := []byte("gapfill-sealed")
	_, err = pg.Webhooks().IngestDelivery(ctx, webhook.IngestRecord{
		InstanceID: instanceID, ExternalEventID: "gap-1", EventKind: "push",
		PayloadDigest: "sha256:" + strings.Repeat("0", 64), RawBodyEncrypted: sealed,
	})
	require.NoError(t, err)
	row, err := pg.Webhooks().ClaimInbox(ctx, "gap-owner")
	require.NoError(t, err)
	require.NotNil(t, row)

	unit, err := pg.Webhooks().BeginApply(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, unit.MarkProcessed(ctx, row.ID, "wrong-owner"), ErrWebhookClaimMismatch)
	require.NoError(t, unit.Rollback())

	unit, err = pg.Webhooks().BeginApply(ctx)
	require.NoError(t, err)
	require.NoError(t, unit.MarkRetry(ctx, row.ID, "gap-owner", 30*time.Second))
	require.NoError(t, unit.Commit())

	// Not claimable again until the retry window passes.
	again, err := pg.Webhooks().ClaimInbox(ctx, "gap-owner")
	require.NoError(t, err)
	assert.Nil(t, again, "retry_wait rows wait for their window")

	// A fresh delivery exercises the dead-letter settle under its own
	// claim (the retried row above released its lease and waits out
	// its window).
	_, err = pg.Webhooks().IngestDelivery(ctx, webhook.IngestRecord{
		InstanceID: instanceID, ExternalEventID: "gap-2", EventKind: "job",
		PayloadDigest: "sha256:" + strings.Repeat("1", 64), RawBodyEncrypted: sealed,
	})
	require.NoError(t, err)
	second, err := pg.Webhooks().ClaimInbox(ctx, "gap-owner")
	require.NoError(t, err)
	require.NotNil(t, second)

	unit, err = pg.Webhooks().BeginApply(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, unit.MarkDeadLetter(ctx, second.ID, "wrong-owner", "X"), ErrWebhookClaimMismatch)
	require.NoError(t, unit.MarkDeadLetter(ctx, second.ID, "gap-owner", "GAP"))
	require.NoError(t, unit.Commit())

	// Replay then re-claim.
	replayed, err := pg.Webhooks().ReplayDeadLetter(ctx, second.ID)
	require.NoError(t, err)
	require.True(t, replayed)
	claimed, err := pg.Webhooks().ClaimInbox(ctx, "gap-owner")
	require.NoError(t, err)
	require.NotNil(t, claimed)

	// Encrypted body reads: hit and miss.
	body, kind, err := pg.WebhookDeliveries().InboxEncryptedBody(ctx, instanceID, "gap-1")
	require.NoError(t, err)
	assert.Equal(t, sealed, body)
	assert.Equal(t, "push", kind)
	_, _, err = pg.WebhookDeliveries().InboxEncryptedBody(ctx, instanceID, "absent")
	require.Error(t, err)

	// Instance projections: suspended reads, removed hiding.
	view, found, err := pg.Webhooks().InstanceByID(ctx, instanceID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "active", view.Status)
	_, err = pg.DB().ExecContext(ctx, `UPDATE gitlab_instances SET status = 'removed' WHERE id = $1`, instanceID)
	require.NoError(t, err)
	_, found, err = pg.Webhooks().InstanceByID(ctx, instanceID)
	require.NoError(t, err)
	assert.False(t, found, "removed instances hide like unknown ones")

	// Mapping webhook uuid miss.
	_, mapped, err := pg.Webhooks().MappingWebhookUUID(ctx, instanceID, 99999)
	require.NoError(t, err)
	assert.False(t, mapped)
}

func TestQualityPolicyBranches(t *testing.T) {
	pg, projectID, _ := newGapfillFixture(t)
	ctx := context.Background()

	overlay := qualityOverlayFor("gap-overlay")
	_, err := pg.Quality().PutProjectPolicy(ctx, projectID, overlay, -1)
	require.Error(t, err, "negative expected versions are rejected")

	version, err := pg.Quality().PutProjectPolicy(ctx, projectID, overlay, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), version)
	stored, gotVersion, err := pg.Quality().GetProjectPolicy(ctx, projectID)
	require.NoError(t, err)
	assert.Equal(t, "gap-overlay", stored.ID)
	assert.Equal(t, int64(1), gotVersion)
}

// -- fixture helpers -------------------------------------------------------

func qualityOverlayFor(id string) *evidence.Policy {
	base, err := evidence.CompanyPolicy()
	if err != nil {
		panic(err)
	}
	overlay := *base
	overlay.ID = id
	overlay.Scope = "project"
	extends := "company-baseline"
	overlay.Extends = &extends
	return &overlay
}
