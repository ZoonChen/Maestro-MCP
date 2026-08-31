package gitlab_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated end-to-end: verified webhook delivery → inbox → envelope →
// consumer → merge-request projection → merged fact drives the work
// item's fact-bound ready_for_human_merge → done transition.

const (
	syncInstance = "018f7700-0000-7000-8000-000000000001"
	syncProject  = "018f7700-0000-7000-8000-000000000002"
	syncWorkItem = "018f7700-0000-7000-8000-000000000003"
)

type syncFixture struct {
	db       *sql.DB
	pg       *store.PostgresStore
	syncer   *gitlab.Syncer
	consumer *gitlab.Consumer
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_gitlab_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_gitlab_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_gitlab_test WITH (FORCE)`)
		_ = admin.Close()
	})

	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := store.OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_gitlab_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7700-0000-7000-8000-000000000004', 'sync team')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7700-0000-7000-8000-000000000004', 'sync', 'Sync', 'active')`, syncProject)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, version)
		VALUES ($1, $2, 'sync task', 'ready_for_human_merge', 4)`, syncWorkItem, syncProject)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ($1, 'https://gitlab.example.com', 'sync instance', 'env:X', 'env:Y')`, syncInstance)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, default_branch)
		VALUES ($1, 9001, $2, 'main')`, syncInstance, syncProject)
	require.NoError(t, err)

	cipher, err := webhook.NewPayloadCipher("sync-test-key")
	require.NoError(t, err)
	syncer := &gitlab.Syncer{Store: pg.GitLab()}
	return &syncFixture{
		db: db, pg: pg, syncer: syncer,
		consumer: &gitlab.Consumer{
			Outbox:     pg.Outbox(),
			Deliveries: pg.WebhookDeliveries(),
			Syncer:     syncer,
			Cipher:     cipher,
			BatchSize:  8,
			RetryDelay: time.Nanosecond, // convergence checks run immediately
		},
	}
}

func mergedMRBody() string {
	return `{
		"object_kind": "merge_request",
		"project": {"id": 9001},
		"object_attributes": {
			"iid": 12,
			"state": "merged",
			"source_branch": "maestro/sync/` + syncWorkItem + `",
			"target_branch": "main",
			"last_commit": {"id": "` + strings.Repeat("a", 40) + `"},
			"merge_commit_sha": "` + strings.Repeat("c", 40) + `",
			"merged_at": "2026-08-31T12:00:00Z",
			"diff_refs": {"base_sha": "` + strings.Repeat("b", 40) + `", "head_sha": "` + strings.Repeat("a", 40) + `"}
		}
	}`
}

func (f *syncFixture) deliver(t *testing.T, key, kind, body string) {
	t.Helper()
	cipher, err := webhook.NewPayloadCipher("sync-test-key")
	require.NoError(t, err)
	sealed, err := cipher.Seal([]byte(body))
	require.NoError(t, err)
	_, err = f.pg.Webhooks().IngestDelivery(context.Background(), webhook.IngestRecord{
		InstanceID: syncInstance, ExternalEventID: key, EventKind: kind,
		PayloadDigest: "sha256:" + strings.Repeat("0", 64), RawBodyEncrypted: sealed,
	})
	require.NoError(t, err)

	// Drive the inbox dispatcher to emit the envelope.
	dispatcher := &webhook.Dispatcher{
		Store: f.pg.Webhooks(), Cipher: cipher,
		MaxAttempts: 3, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	}
	outcome, err := dispatcher.DispatchOne(context.Background(), "sync-test-worker")
	require.NoError(t, err)
	require.Equal(t, "applied", outcome)
}

func TestMergedFactDrivesDone(t *testing.T) {
	f := newSyncFixture(t)
	f.deliver(t, "evt-mr-merged-1", "merge_request", mergedMRBody())

	applied, err := f.consumer.ProcessBatch(context.Background(), "sync-test-consumer")
	require.NoError(t, err)
	assert.Equal(t, 1, applied)

	// The projection recorded the merged MR with its SHA tuple.
	var state, sourceSHA, mergeCommit string
	var workItem *string
	require.NoError(t, f.db.QueryRow(`
		SELECT state, source_sha, merge_commit_sha, work_item_id::text FROM merge_requests WHERE mr_iid = 12`).
		Scan(&state, &sourceSHA, &mergeCommit, &workItem))
	assert.Equal(t, "merged", state)
	assert.Equal(t, strings.Repeat("a", 40), sourceSHA)
	assert.Equal(t, strings.Repeat("c", 40), mergeCommit)
	require.NotNil(t, workItem)
	assert.Equal(t, syncWorkItem, *workItem)

	// The fact-bound transition landed with lineage.
	var status string
	var factID string
	require.NoError(t, f.db.QueryRow(`
		SELECT status, merged_fact_id FROM work_items WHERE id = $1`, syncWorkItem).Scan(&status, &factID))
	assert.Equal(t, "done", status)
	assert.Equal(t, "gitlab:"+syncInstance+":mr:12", factID)

	// Re-delivery is idempotent: no second transition, no error.
	f.deliver(t, "evt-mr-merged-2", "merge_request", mergedMRBody())
	_, err = f.consumer.ProcessBatch(context.Background(), "sync-test-consumer")
	require.NoError(t, err)
	require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, syncWorkItem).Scan(&status))
	assert.Equal(t, "done", status)
}

func TestMergedFactWithheldOutsideReady(t *testing.T) {
	f := newSyncFixture(t)
	_, err := f.db.Exec(`UPDATE work_items SET status = 'executing' WHERE id = $1`, syncWorkItem)
	require.NoError(t, err)

	f.deliver(t, "evt-mr-early-1", "merge_request", mergedMRBody())
	applied, err := f.consumer.ProcessBatch(context.Background(), "sync-test-consumer")
	require.NoError(t, err, "a withheld fact is a recorded outcome, not a failure")
	assert.Equal(t, 1, applied)

	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, syncWorkItem).Scan(&status))
	assert.Equal(t, "executing", status, "no state regresses on an early merged fact")
}

func TestPipelineAndJobProjections(t *testing.T) {
	f := newSyncFixture(t)
	f.deliver(t, "evt-pipe-1", "pipeline", `{
		"object_kind": "pipeline", "project": {"id": 9001},
		"object_attributes": {"id": 555, "sha": "`+strings.Repeat("d", 40)+`", "ref": "main", "status": "success", "source": "merge_request_event"}}`)
	f.deliver(t, "evt-job-1", "job", `{
		"object_kind": "job", "project_id": 9001, "pipeline_id": 555,
		"build_id": 9002, "build_name": "unit", "build_status": "success", "stage": "test"}`)

	// Out-of-order claim is legitimate (uuid-ordered); deferred jobs
	// converge on the next batch once the pipeline row exists.
	for range 5 {
		if _, err := f.consumer.ProcessBatch(context.Background(), "sync-test-consumer"); err != nil {
			t.Fatalf("batch: %v", err)
		}
		var done int
		require.NoError(t, f.db.QueryRow(`
			SELECT (SELECT count(*) FROM pipelines WHERE gitlab_pipeline_id = 555)
			     + (SELECT count(*) FROM pipeline_jobs WHERE gitlab_job_id = 9002)`).Scan(&done))
		if done == 2 {
			break
		}
	}

	var pipelineStatus, ref string
	require.NoError(t, f.db.QueryRow(`SELECT status, ref FROM pipelines WHERE gitlab_pipeline_id = 555`).Scan(&pipelineStatus, &ref))
	assert.Equal(t, "success", pipelineStatus)
	assert.Equal(t, "main", ref)

	var jobName string
	require.NoError(t, f.db.QueryRow(`SELECT name FROM pipeline_jobs WHERE gitlab_job_id = 9002`).Scan(&jobName))
	assert.Equal(t, "unit", jobName)
}

func TestWorkItemIDFromBranch(t *testing.T) {
	assert.Equal(t, syncWorkItem, gitlab.WorkItemIDFromBranch("maestro/sync/"+syncWorkItem))
	assert.Equal(t, "abc", gitlab.WorkItemIDFromBranch("maestro/p/abc"))
	assert.Empty(t, gitlab.WorkItemIDFromBranch("main"), "target branches carry no marker")
	assert.Empty(t, gitlab.WorkItemIDFromBranch("feature/one"), "non-maestro prefixes are ignored")
	assert.Empty(t, gitlab.WorkItemIDFromBranch("maestro/onlymarker"))
	assert.Empty(t, gitlab.WorkItemIDFromBranch("maestro"))
}
