package gitlab_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/app"
	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openW6VerifyStore opens the restored pilot copy WITHOUT migrations
// or resets — the replay must run on the backup as restored.
func openW6VerifyStore(t *testing.T, dsn string) (*sql.DB, *store.PostgresStore, error) {
	t.Helper()
	db, err := store.OpenPostgres(context.Background(), dsn)
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(func() { db.Close() })
	pg, err := store.NewPostgresStore(db)
	if err != nil {
		return nil, nil, err
	}
	return db, pg, nil
}

func pilotMRBody(gitlabProject int64, iid int64, state, branch, source, target, mergeCommit string) string {
	merged := ""
	if state == "merged" {
		merged = `, "merge_commit_sha": "` + mergeCommit + `", "merged_at": "2026-09-13T12:00:00Z"`
	}
	return fmt.Sprintf(`{
		"object_kind": "merge_request",
		"project": {"id": %d},
		"object_attributes": {
			"iid": %d, "state": %q, "source_branch": %q, "target_branch": "main",
			"last_commit": {"id": "%s"},
			"diff_refs": {"base_sha": "%s", "head_sha": "%s"}%s
		}}`, gitlabProject, iid, state, branch, source, target, source, merged)
}

func pilotPipelineBody(gitlabProject int64, pipelineID int64, sha, branch string) string {
	return fmt.Sprintf(`{
		"object_kind": "pipeline", "project": {"id": %d},
		"object_attributes": {"id": %d, "sha": "%s", "ref": %q, "status": "success", "source": "push"}}`,
		gitlabProject, pipelineID, sha, branch)
}

func pilotJobBody(gitlabProject int64, pipelineID int64, jobID int64, gate, branch, sha string) string {
	return fmt.Sprintf(`{
		"object_kind": "job", "project_id": %d, "pipeline_id": %d,
		"build_id": %d, "build_name": %q, "build_status": "success", "stage": "verify",
		"ref": %q, "sha": "%s"}`, gitlabProject, pipelineID, jobID, gate, branch, sha)
}

// W6 DoD: the W1 shadow-report comparison replayed against a restored
// copy of the pilot database (pre-s2b backup). Gated on
// MAESTRO_W6_VERIFY_DSN — absent in CI and normal test runs; set it to
// a scratch restore to execute. The replay reproduces the exact W1
// artifacts on pilot data (S2B slice → validating×2, 163 pending
// domain outbox events, evidence=0, no MR projection) and then walks
// the W6 fixes through them (branch-contract binding, gate-driven
// ready writer, merged fact → done, domain sink drain).
func TestW6PilotReplayComparison(t *testing.T) {
	dsn := os.Getenv("MAESTRO_W6_VERIFY_DSN")
	if dsn == "" {
		t.Skip("MAESTRO_W6_VERIFY_DSN not set; this is the local W1-comparison replay against a restored pilot backup")
	}
	db, pg, err := openW6VerifyStore(t, dsn)
	require.NoError(t, err)
	ctx := context.Background()

	const (
		govProject  = "018fb5b0-0000-7000-8000-000000000006"
		itemA11     = "018fb5b4-0000-7000-8000-000000000001"
		itemA12     = "018fb5b4-0000-7000-8000-000000000002"
		claimRunner = "018fb5b2-0000-7000-8000-000000000002"
	)

	var instanceID string
	require.NoError(t, db.QueryRow(`SELECT gitlab_instance_id::text FROM gitlab_project_mappings LIMIT 1`).Scan(&instanceID))
	var gitlabProject int64
	require.NoError(t, db.QueryRow(`SELECT gitlab_project_id FROM gitlab_project_mappings LIMIT 1`).Scan(&gitlabProject))

	validating := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM work_items WHERE status = 'validating'`).Scan(&n))
		return n
	}
	doneCount := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM work_items WHERE status = 'done'`).Scan(&n))
		return n
	}
	evidenceCount := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM evidence`).Scan(&n))
		return n
	}
	outboxPending := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM outbox_events WHERE status = 'pending' OR status = 'retry_wait' OR status = 'sending'`).Scan(&n))
		return n
	}
	mrUnderGov := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM merge_requests WHERE project_id = $1`, govProject).Scan(&n))
		return n
	}

	pendingBefore := outboxPending()
	evidenceBefore := evidenceCount()
	t.Logf("BEFORE      validating=%d done=%d evidence=%d outbox_pending=%d mr_under_gov=%d",
		validating(), doneCount(), evidenceBefore, pendingBefore, mrUnderGov())
	assert.Zero(t, evidenceBefore, "the W1 artifact: no evidence ever landed")
	assert.Zero(t, mrUnderGov(), "the W1 artifact: no MR projection under the governance domain")
	assert.NotZero(t, pendingBefore, "the W1 artifact: domain events spinning in the outbox")

	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	syncer := &gitlab.Syncer{
		Store: pg.GitLab(),
		Ingest: &gitlab.EvidenceIngestor{
			Eval:          &evidence.Service{Company: company, Store: pg.Quality()},
			PolicyVersion: company.Version,
			Append:        pg.Quality(),
			Tuples:        pg.GitLab(),
		},
	}

	// Phase B — the S2B slice on pilot data: claim → complete → the W1
	// stuck state (validating×2).
	for _, item := range []string{itemA11, itemA12} {
		var queueVersion int64
		require.NoError(t, db.QueryRow(`SELECT version FROM projects WHERE id = $1`, govProject).Scan(&queueVersion))
		claim, err := pg.ClaimNextWorkItem(ctx, claimRunner, "w6-verify-gen", queueVersion, time.Hour)
		require.NoError(t, err, "claim %s", item)
		assert.Equal(t, item, claim.WorkItemID)
		commit := "b7d14c7520243f352422cc24067122aaffb41bcd"
		if item == itemA12 {
			commit = "00a5802a0e98e9c09aaeba60eb44c5e9fb79c5d5"
		}
		require.NoError(t, pg.CompleteExecution(ctx, claim.ExecutionID, claimRunner, "w6-verify-gen", "completed", &commit, "w6 replay"))
		// D1: the S2B slices executed under approved command profiles
		// (maven/npm sandbox validation); the boundary self-attestation
		// derives its profile fact from validation_runs, so the replay
		// reconstructs that record for each claimed item.
		_, execErr := db.ExecContext(ctx, `
			INSERT INTO validation_runs (project_id, work_item_id, attempt, profile_ref, result)
			VALUES ($1, $2, 1, $3, 'passed')`, govProject, item,
			"maven-build@1.0.0@sha256:"+strings.Repeat("ab", 32))
		require.NoError(t, execErr, "validation fact %s", item)
	}
	t.Logf("S2B-STATE   validating=%d (the W1 stuck state)", validating())
	require.Equal(t, 2, validating())

	// Phases C+D+E — the W6 chain per item: MR tuple binds by the
	// branch contract, gate-named CI facts land evidence and drive the
	// Ready verdict, the merged fact completes done.
	for index, item := range []string{itemA11, itemA12} {
		branch := "maestro/peixun/" + item
		source := strings.Repeat("b7", 20)
		mergeCommit := strings.Repeat("a7", 20)
		if item == itemA12 {
			source = strings.Repeat("00", 20)
			mergeCommit = strings.Repeat("ad", 20)
		}
		target := strings.Repeat("e5", 20)

		open := pilotMRBody(gitlabProject, int64(5+index), "opened", branch, source, target, "")
		_, err := syncer.ApplyBody(ctx, instanceID, "merge_request", []byte(open))
		require.NoError(t, err)

		pipeline := pilotPipelineBody(gitlabProject, int64(410+index), source, branch)
		_, err = syncer.ApplyBody(ctx, instanceID, "pipeline", []byte(pipeline))
		require.NoError(t, err)
		for gateIndex, gate := range company.RequiredGates {
			job := pilotJobBody(gitlabProject, int64(410+index), int64(20000+gateIndex), gate, branch, source)
			_, err = syncer.ApplyBody(ctx, instanceID, "job", []byte(job))
			require.NoError(t, err, "gate %s", gate)
		}

		merged := pilotMRBody(gitlabProject, int64(5+index), "merged", branch, source, target, mergeCommit)
		outcome, err := syncer.ApplyBody(ctx, instanceID, "merge_request", []byte(merged))
		require.NoError(t, err)
		assert.True(t, outcome.Transitioned, "%s must reach done", item)
	}
	t.Logf("DONE-CHAIN  validating=%d done=%d evidence=%d mr_under_gov=%d",
		validating(), doneCount(), evidenceCount(), mrUnderGov())
	assert.Zero(t, validating(), "W1 comparison: validating 2→0")
	assert.Equal(t, 2, doneCount(), "the two W1 items completed the done chain")
	assert.NotZero(t, evidenceCount(), "W1 comparison: evidence coverage is non-zero")
	assert.Equal(t, 2, mrUnderGov(), "the cross-domain MR projections landed under the governance project")

	// Phase F — the domain sink drains every unowned channel.
	sink := &app.DomainEventSink{
		Source:             pg.Outbox(),
		ExcludedEventTypes: []string{"gitlab.webhook.received"},
		BatchSize:          64,
		RetryDelay:         time.Second,
	}
	for round := range 12 {
		delivered, err := sink.ProcessBatch(ctx, "w6-verify-sink")
		require.NoError(t, err)
		if delivered == 0 {
			break
		}
		t.Logf("sink        round %d delivered %d", round+1, delivered)
	}
	assert.Zero(t, outboxPending(), "W1 comparison: outbox lag drained to zero")
	t.Logf("AFTER       validating=%d done=%d evidence=%d outbox_pending=0",
		validating(), doneCount(), evidenceCount())
}
