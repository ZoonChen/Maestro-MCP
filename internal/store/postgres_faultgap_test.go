package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Sixth sweep: the error-wrapping branches. A closed handle exercises
// every `if err != nil { return ... fmt.Errorf }` path honestly — the
// wrappers are the contract (stable prefixes for classification), and
// the pure helpers get direct assertions.

func closedStore(t *testing.T) *PostgresStore {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	db, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)
	return pg
}

func TestIdentityErrorBranches(t *testing.T) {
	pg := closedStore(t)
	ctx := context.Background()

	_, err := pg.Identities().GetOrCreateUser(ctx, "i", "s", "n")
	require.Error(t, err)
	_, err = pg.Identities().GetUser(ctx, "u")
	require.Error(t, err)
	require.Error(t, pg.Identities().UpdateUserStatus(ctx, "u", "a", "b"))
	require.Error(t, pg.Identities().CreateMembership(ctx, &model.TeamMembership{}))
	_, err = pg.Identities().ListMembershipsByUser(ctx, "u", "")
	require.Error(t, err)
	_, err = pg.Identities().ListProjectMemberships(ctx, "u")
	require.Error(t, err)
}

func TestRunnerErrorBranches(t *testing.T) {
	pg := closedStore(t)
	ctx := context.Background()

	_, _, err := pg.RunnerRegistry().EnrollmentByCodeHash(ctx, "h")
	require.Error(t, err)
	require.Error(t, pg.RunnerRegistry().ConsumeEnrollment(ctx, "e", "h"))
	require.Error(t, pg.RunnerRegistry().CreateEnrollment(ctx, &model.RunnerEnrollment{ID: "x"}))
	require.Error(t, pg.RunnerRegistry().CreateRunner(ctx, &model.RunnerDevice{}, nil))
	_, err = pg.RunnerRegistry().GetRunner(ctx, "r")
	require.Error(t, err)
	require.Error(t, pg.RunnerRegistry().UpdateRunnerStatus(ctx, "r", "a", "b"))
	_, err = pg.RunnerRegistry().BumpRunnerGeneration(ctx, "r")
	require.Error(t, err)
	require.Error(t, pg.RunnerRegistry().UpdateRunnerHeartbeat(ctx, "r"))
	_, err = pg.RunnerRegistry().ListRunnersByProject(ctx, "p")
	require.Error(t, err)
	require.Error(t, pg.RunnerRegistry().RevokeRunner(ctx, "r"))
}

func TestEventAndIdempotencyErrorBranches(t *testing.T) {
	pg := closedStore(t)
	ctx := context.Background()

	require.Error(t, pg.Outbox().Enqueue(ctx, testOutboxEvent("fault-1")))
	_, err := pg.Outbox().ClaimPending(ctx, 4, "o", "")
	require.Error(t, err)
	require.Error(t, pg.Outbox().MarkDelivered(ctx, "e", "o"))
	require.Error(t, pg.Outbox().MarkRetry(ctx, "e", "o", 1, ""))
	require.Error(t, pg.Outbox().MarkDeadLetter(ctx, "e", "o"))
	ok, err := pg.Inbox().Record(ctx, testInboxEvent("fault-2"))
	require.Error(t, err)
	assert.False(t, ok)
	require.Error(t, pg.Inbox().ClaimProcessing(ctx, "e"))
	require.Error(t, pg.Inbox().MarkProcessed(ctx, "e"))

	_, _, err = pg.APIIdempotency().LookupOrCreate(ctx, &IdempotencyRecord{
		PrincipalID: "p", Operation: "op", Key: "k"})
	require.Error(t, err)
}

func TestQualityWebhookGitlabInstanceErrorBranches(t *testing.T) {
	pg := closedStore(t)
	ctx := context.Background()

	_, _, err := pg.Quality().GetProjectPolicy(ctx, "p")
	require.Error(t, err)
	company, companyErr := evidence.CompanyPolicy()
	require.NoError(t, companyErr)
	_, err = pg.Quality().PutProjectPolicy(ctx, "p", company, 0)
	require.Error(t, err)
	require.Error(t, pg.Quality().AppendEvidence(ctx, &evidence.Record{EvidenceID: "x"}))
	_, err = pg.Quality().ListEvidenceForWorkItem(ctx, "p", "w")
	require.Error(t, err)
	require.Error(t, pg.Quality().PersistVerdict(ctx, nil))
	_, err = pg.Quality().ListGateSnapshots(ctx, "p", "w")
	require.Error(t, err)
	_, err = pg.Quality().CreateWaiver(ctx, &evidence.Waiver{}, "p", "w")
	require.Error(t, err)
	require.Error(t, pg.Quality().ApproveWaiver(ctx, "w", "a"))
	require.Error(t, pg.Quality().RevokeWaiver(ctx, "w"))
	_, err = pg.Quality().ListWaiversForWorkItem(ctx, "p", "w")
	require.Error(t, err)
	_, _, err = pg.Quality().GateSnapshotByID(ctx, "p", "g")
	require.Error(t, err)
	_, _, err = pg.Quality().WaiverByID(ctx, "p", "w")
	require.Error(t, err)
	_, err = pg.Quality().WorkItemExists(ctx, "p", "w")
	require.Error(t, err)

	_, _, err = pg.Webhooks().InstanceByID(ctx, "i")
	require.Error(t, err)
	_, _, err = pg.Webhooks().MappingWebhookUUID(ctx, "i", 1)
	require.Error(t, err)
	require.Error(t, pg.Webhooks().RecordDenial(ctx, webhook.AuditRow{}))
	_, err = pg.Webhooks().IngestDelivery(ctx, webhook.IngestRecord{})
	require.Error(t, err)
	_, err = pg.Webhooks().ClaimInbox(ctx, "o")
	require.Error(t, err)
	_, err = pg.Webhooks().ReplayDeadLetter(ctx, "i")
	require.Error(t, err)
	_, _, err = pg.WebhookDeliveries().InboxEncryptedBody(ctx, "i", "k")
	require.Error(t, err)

	_, err = pg.GitLab().MappingProject(ctx, "i", 1)
	require.Error(t, err)
	require.Error(t, pg.GitLab().UpsertMergeRequest(ctx, "p", gitlab.MergeRequestRecord{}, ""))
	_, _, err = pg.GitLab().MarkWorkItemDoneFromMerge(ctx, "p", "w", "c", "f")
	require.Error(t, err)
	require.Error(t, pg.GitLab().UpsertPipeline(ctx, gitlab.PipelineRecord{}))
	require.Error(t, pg.GitLab().UpsertJob(ctx, gitlab.JobRecord{}))
	_, _, _, _, err = pg.GitLab().BranchTuple(ctx, "p", "b")
	require.Error(t, err)

	_, err = pg.Instances().ListInstances(ctx)
	require.Error(t, err)
	_, err = pg.Instances().CreateInstance(ctx, "https://x.example", "n", "env:A", "env:B")
	require.Error(t, err)
	_, err = pg.Instances().GetMapping(ctx, "p")
	require.Error(t, err)
	_, err = pg.Instances().PutMapping(ctx, "p", "i", 1, "main", 0)
	require.Error(t, err)
	_, _, err = pg.Instances().MergeRequestProjection(ctx, "p", 1)
	require.Error(t, err)
	_, err = pg.Instances().ProjectExists(ctx, "p")
	require.Error(t, err)
	_, _, _, _, _, _, _, err = pg.Instances().ProjectMappingContext(ctx, "p")
	require.Error(t, err)
}

func TestMigrationErrorBranches(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	db, err := OpenPostgres(ctx, os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = ApplyPostgresMigrations(ctx, db)
	require.Error(t, err)
	require.Error(t, ValidatePostgresSchema(ctx, db))
	_, err = RevertPostgresMigrations(ctx, db, 1)
	require.Error(t, err)
}

func TestPGHelperFamilies(t *testing.T) {
	// Malformed timestamps pass through so PostgreSQL reports the bad
	// value (the pgTimeArg contract), valid ones parse.
	assert.Equal(t, "not-a-time", pgTimeArg("not-a-time"))
	assert.IsType(t, time.Time{}, pgTimeArg("2026-09-01T00:00:00Z"))

	assert.Nil(t, pgOptionalTime(""))
	assert.NotNil(t, pgOptionalTime("2026-09-01T00:00:00Z"))

	var nilPtr *string
	assert.Nil(t, pgOptionalTimePtr(nilPtr))
	empty := ""
	assert.Nil(t, pgOptionalTimePtr(&empty))
	value := "2026-09-01T00:00:00Z"
	assert.NotNil(t, pgOptionalTimePtr(&value))

	assert.Nil(t, pgOptionalJSON(nil))
	assert.Equal(t, `{"a":1}`, pgOptionalJSON([]byte(`{"a":1}`)))

	// The digest is the sha256 of the exact bytes.
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, contentDigest([]byte("x")))

	record := &evidence.Record{Producer: evidence.Producer{Type: "gitlab_job", ID: "ci", Version: "1"}}
	assert.Contains(t, producerJSON(record), `"type":"gitlab_job"`)
}
