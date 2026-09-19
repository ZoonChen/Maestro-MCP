package gitlab_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W7-4 (F20) acceptance: a projected terminal job state never regresses
// to a stale non-terminal status delivered late (the deferred-replay
// shape that froze S2B4's pipelines at created/pending while GitLab
// showed success).
func TestW7JobProjectionTerminalIsMonotonic(t *testing.T) {
	f := newSyncFixture(t)
	f.deliver(t, "w7-pipe-555", "pipeline", fmt.Sprintf(`{
		"object_kind": "pipeline", "project": {"id": 9001},
		"object_attributes": {"id": 555, "sha": %q, "ref": "main", "status": "success", "source": "push"}}`,
		strings.Repeat("d", 40)))

	jobStatus := func(t *testing.T) string {
		t.Helper()
		var status string
		err := f.db.QueryRow(
			`SELECT status FROM pipeline_jobs WHERE gitlab_job_id = 9002`).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return "" // not projected yet (job deferred behind its pipeline)
		}
		require.NoError(t, err)
		return status
	}
	// claimUntil drives the consumer until the projection reaches the
	// expected state: within one batch a job event may apply before its
	// pipeline event (deferral replays on the next batch — the
	// documented convergence path).
	claimUntil := func(t *testing.T, expected string) {
		t.Helper()
		for range 6 {
			_, err := f.consumer.ProcessBatch(context.Background(), "w7-consumer")
			require.NoError(t, err)
			if jobStatus(t) == expected {
				return
			}
		}
		t.Fatalf("job projection never reached %q (now %q)", expected, jobStatus(t))
	}

	f.deliver(t, "w7-job-created", "job", `{
		"object_kind": "job", "project_id": 9001, "pipeline_id": 555,
		"build_id": 9002, "build_name": "unit", "build_status": "created", "stage": "test"}`)
	claimUntil(t, "created")

	f.deliver(t, "w7-job-running", "job", `{
		"object_kind": "job", "project_id": 9001, "pipeline_id": 555,
		"build_id": 9002, "build_name": "unit", "build_status": "running", "stage": "test"}`)
	claimUntil(t, "running")

	f.deliver(t, "w7-job-success", "job", `{
		"object_kind": "job", "project_id": 9001, "pipeline_id": 555,
		"build_id": 9002, "build_name": "unit", "build_status": "success", "stage": "test"}`)
	claimUntil(t, "success")

	// The F20 incident shape: a deferred/stale event for the same job
	// lands AFTER the terminal state (last-write-wins used to drag the
	// projection back, killing the terminal-only evidence ingestor).
	f.deliver(t, "w7-job-late-pending", "job", `{
		"object_kind": "job", "project_id": 9001, "pipeline_id": 555,
		"build_id": 9002, "build_name": "unit", "build_status": "pending", "stage": "test"}`)
	f.deliver(t, "w7-job-late-running", "job", `{
		"object_kind": "job", "project_id": 9001, "pipeline_id": 555,
		"build_id": 9002, "build_name": "unit", "build_status": "running", "stage": "test"}`)
	// Two extra batches prove the late events were claimed and applied
	// (as no-ops) — the projection stays terminal.
	claimUntil(t, "success")
	claimUntil(t, "success")
	assert.Equal(t, "success", jobStatus(t), "terminal states refuse regression to late non-terminal events")
}

// W7-5 (F26) acceptance: a terminal gate-named job on a marker branch
// whose evidence tuple is not yet complete is DEFERRED through the
// outbox replay path instead of dropped — when the reconciliation
// completes the tuple, the replay mints the evidence without a manual
// job retry (the S2B10 one-retry-round loss per incident).
func TestW7TerminalJobEvidenceDeferredUntilTuple(t *testing.T) {
	f := newSyncFixture(t)
	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	f.syncer.Ingest = &gitlab.EvidenceIngestor{
		Eval:          &evidence.Service{Company: company, Store: f.pg.Quality()},
		PolicyVersion: company.Version,
		Append:        f.pg.Quality(),
		Tuples:        f.pg.GitLab(),
	}

	sourceSHA := strings.Repeat("7", 40)
	targetSHA := strings.Repeat("8", 40)
	branch := "maestro/sync/" + syncWorkItem

	f.deliver(t, "w7-pipe-888", "pipeline", fmt.Sprintf(`{
		"object_kind": "pipeline", "project": {"id": 9001},
		"object_attributes": {"id": 888, "sha": %q, "ref": %q, "status": "success", "source": "push"}}`, sourceSHA, branch))

	// The terminal job event arrives while the tuple is NOT yet built
	// (the F26 race: terminal event vs the reconciliation that binds
	// the SHAs). F26's old behavior: silently dropped evidence.
	f.deliver(t, "w7-job-race", "job", fmt.Sprintf(`{
		"object_kind": "job", "project_id": 9001, "pipeline_id": 888,
		"build_id": 9000, "build_name": "unit", "build_status": "success", "stage": "test",
		"ref": %q, "sha": %q}`, branch, sourceSHA))
	for range 3 {
		_, err := f.consumer.ProcessBatch(context.Background(), "w7-consumer")
		require.NoError(t, err)
	}

	records, err := f.pg.Quality().ListEvidenceForWorkItem(context.Background(), syncProject, syncWorkItem)
	require.NoError(t, err)
	assert.Empty(t, records, "no evidence before the tuple exists")

	var deferred int
	require.NoError(t, f.db.QueryRow(`
		SELECT count(*) FROM outbox_events
		WHERE status = 'retry_wait' AND payload->>'event_kind' = 'job'`).Scan(&deferred))
	assert.GreaterOrEqual(t, deferred, 1,
		"the terminal job event is parked for replay, not dropped and not delivered")

	// The reconciliation completes the tuple (MR projection with both
	// SHAs); the deferred event replays and mints its evidence without
	// a manual job retry.
	f.deliver(t, "w7-mr-tuple", "merge_request", fmt.Sprintf(`{
		"object_kind": "merge_request", "project": {"id": 9001},
		"object_attributes": {
			"iid": 40, "state": "opened",
			"source_branch": %q, "target_branch": "main",
			"last_commit": {"id": %q},
			"diff_refs": {"base_sha": %q, "head_sha": %q}
		}}`, branch, sourceSHA, targetSHA, sourceSHA))
	for range 4 {
		_, err := f.consumer.ProcessBatch(context.Background(), "w7-consumer")
		require.NoError(t, err)
	}

	records, err = f.pg.Quality().ListEvidenceForWorkItem(context.Background(), syncProject, syncWorkItem)
	require.NoError(t, err)
	// The replay mints the unit CI evidence; the same evaluation also
	// mints the D1 control-plane self-attestations (policy_integrity /
	// baseline_freshness) — assert the CI record specifically.
	var unitRecord *evidence.Record
	for index := range records {
		if records[index].Kind == "unit" && records[index].Authority == evidence.AuthorityMergeGate {
			unitRecord = &records[index]
		}
	}
	if assert.NotNil(t, unitRecord, "the replayed deferred event mints its CI evidence") {
		assert.Equal(t, evidence.EvidencePassed, unitRecord.Status)
		assert.Equal(t, "gitlab_job", unitRecord.Producer.Type)
	}

	snapshots, err := f.pg.Quality().ListGateSnapshots(context.Background(), syncProject, syncWorkItem)
	require.NoError(t, err)
	states := map[string]string{}
	for _, snapshot := range snapshots {
		states[snapshot.Check] = snapshot.Status
	}
	assert.Equal(t, "passed", states["unit"],
		"the replayed deferred event drives the gate without a manual job retry")
}
