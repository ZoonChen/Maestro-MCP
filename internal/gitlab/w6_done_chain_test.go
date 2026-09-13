package gitlab_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// W6 PG-gated coverage for the two shadow-phase structural gaps:
//
//   - S2C-A1 (F1): the validating → ready_for_human_merge writer. A
//     Ready verdict (every required gate passed on the exact tuple)
//     must move a VALIDATING item to ready, and the merged fact then
//     completes done — the chain that left W1's two items stuck.
//   - S2C-A2 (F2): the branch naming contract's project segment owns
//     the (project, work item) binding, so a cross-governance-domain
//     MR never trips the composite foreign key.

const (
	w6TeamID      = "018f7800-0000-7000-8000-000000000010"
	w6RepoProject = "018f7800-0000-7000-8000-000000000011" // mapped repo project, key w6repo
	w6GovProject  = "018f7800-0000-7000-8000-000000000012" // governance domain, key w6gov
	w6GovItem     = "018f7800-0000-7000-8000-000000000013"
	w6Instance    = "018f7800-0000-7000-8000-000000000014"
)

type w6Fixture struct {
	db     *sql.DB
	pg     *store.PostgresStore
	syncer *gitlab.Syncer
}

func newW6Fixture(t *testing.T) *w6Fixture {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_gitlab_w6_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_gitlab_w6_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_gitlab_w6_test WITH (FORCE)`)
		_ = admin.Close()
	})

	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := store.OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_gitlab_w6_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'w6 team')`, w6TeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO projects (id, team_id, key, name, status) VALUES
		($1, $3, 'w6repo', 'Repo Project', 'active'),
		($2, $3, 'w6gov', 'Gov Domain', 'active')`, w6RepoProject, w6GovProject, w6TeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, version)
		VALUES ($1, $2, 'w6 gov item', 'validating', 2)`, w6GovItem, w6GovProject)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ($1, 'https://gitlab.example.com', 'w6 instance', 'env:X', 'env:Y')`, w6Instance)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, default_branch)
		VALUES ($1, 9100, $2, 'main')`, w6Instance, w6RepoProject)
	require.NoError(t, err)

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
	return &w6Fixture{db: db, pg: pg, syncer: syncer}
}

func (f *w6Fixture) apply(t *testing.T, body string) gitlab.ApplyOutcome {
	t.Helper()
	outcome, err := f.syncer.ApplyBody(context.Background(), w6Instance, "merge_request", []byte(body))
	require.NoError(t, err)
	return outcome
}

// deliverPipeline lands the pipeline projection the job events need
// before their own projection can resolve.
func (f *w6Fixture) deliverPipeline(t *testing.T, pipelineID int64, branch, sha string) {
	t.Helper()
	_, err := f.syncer.ApplyBody(context.Background(), w6Instance, "pipeline", []byte(fmt.Sprintf(`{
		"object_kind": "pipeline", "project": {"id": 9100},
		"object_attributes": {"id": %d, "sha": %q, "ref": %q, "status": "success", "source": "push"}}`,
		pipelineID, sha, branch)))
	require.NoError(t, err)
}

func (f *w6Fixture) deliverGateJob(t *testing.T, pipelineID int64, jobID int, gate, branch, sha, status string) {
	t.Helper()
	_, err := f.syncer.ApplyBody(context.Background(), w6Instance, "job", []byte(fmt.Sprintf(`{
		"object_kind": "job", "project_id": 9100, "pipeline_id": %d,
		"build_id": %d, "build_name": %q, "build_status": %q, "stage": "test",
		"ref": %q, "sha": %q}`, pipelineID, jobID, gate, status, branch, sha)))
	require.NoError(t, err)
}

func mrBody(iid int64, state, branch, mergeCommit string) string {
	sourceSHA := strings.Repeat("7", 40)
	targetSHA := strings.Repeat("8", 40)
	mergedAt := ""
	if state == "merged" {
		mergedAt = `, "merge_commit_sha": "` + mergeCommit + `", "merged_at": "2026-09-13T10:00:00Z"`
	}
	return fmt.Sprintf(`{
		"object_kind": "merge_request",
		"project": {"id": 9100},
		"object_attributes": {
			"iid": %d, "state": %q, "source_branch": %q, "target_branch": "main",
			"last_commit": {"id": "%s"},
			"diff_refs": {"base_sha": "%s", "head_sha": "%s"}%s
		}}`, iid, state, branch, sourceSHA, targetSHA, sourceSHA, mergedAt)
}

// TestReadyVerdictDrivesValidatingToReadyThenDone reproduces the F1
// scenario end to end: an item completes execution (validating), the
// CI evidence passes every required gate, and the human merge lands —
// before W6 the item stuck in validating forever (W1 report: done=0,
// validating×2); after W6 the verdict drives ready and the merged
// fact completes done.
func TestReadyVerdictDrivesValidatingToReadyThenDone(t *testing.T) {
	f := newW6Fixture(t)
	branch := "maestro/w6gov/" + w6GovItem

	// The MR opens with a complete tuple: projections and evidence can
	// bind from here on.
	outcome := f.apply(t, mrBody(21, "opened", branch, ""))
	assert.False(t, outcome.Transitioned)

	// Every required gate passes on the exact tuple. The last one
	// flips the verdict to Ready and drives validating → ready.
	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	sourceSHA := strings.Repeat("7", 40)
	f.deliverPipeline(t, 3210, branch, sourceSHA)
	for index, gate := range company.RequiredGates {
		f.deliverGateJob(t, 3210, 10000+index, gate, branch, sourceSHA, "success")
	}

	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, w6GovItem).Scan(&status))
	assert.Equal(t, "ready_for_human_merge", status,
		"the Ready verdict must drive the F1-stuck validating item to ready")

	// The transition carries its atomic audit + outbox pair.
	var auditCount, outboxCount int
	require.NoError(t, f.db.QueryRow(
		`SELECT count(*) FROM audit_events WHERE action = 'work_item.ready' AND resource_id = $1`, w6GovItem).Scan(&auditCount))
	assert.Equal(t, 1, auditCount, "exactly one ready audit row (replays are no-ops)")
	require.NoError(t, f.db.QueryRow(
		`SELECT count(*) FROM outbox_events WHERE event_type = 'work_item.state.changed' AND subject = $1`, w6GovItem).Scan(&outboxCount))
	assert.Equal(t, 1, outboxCount, "the state change rides the registered domain event")

	// The human merge lands: the merged fact completes the done chain.
	outcome = f.apply(t, mrBody(21, "merged", branch, strings.Repeat("c", 40)))
	assert.True(t, outcome.Transitioned)
	require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, w6GovItem).Scan(&status))
	assert.Equal(t, "done", status, "F1 healed: validating → ready → done in one webhook chain")

	// A replayed verdict (re-delivered gate job) must not regress or
	// duplicate: done is terminal, the ready writer no-ops.
	f.deliverGateJob(t, 3210, 10000, company.RequiredGates[0], branch, sourceSHA, "success")
	require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, w6GovItem).Scan(&status))
	assert.Equal(t, "done", status)
	require.NoError(t, f.db.QueryRow(
		`SELECT count(*) FROM audit_events WHERE action = 'work_item.ready' AND resource_id = $1`, w6GovItem).Scan(&auditCount))
	assert.Equal(t, 1, auditCount)
}

// TestFailedGateLeavesValidatingUnmoved pins the fail-closed side: a
// non-Ready verdict never touches the state machine.
func TestFailedGateLeavesValidatingUnmoved(t *testing.T) {
	f := newW6Fixture(t)
	branch := "maestro/w6gov/" + w6GovItem
	f.apply(t, mrBody(22, "opened", branch, ""))

	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	sourceSHA := strings.Repeat("7", 40)
	f.deliverPipeline(t, 3211, branch, sourceSHA)
	f.deliverGateJob(t, 3211, 11000, company.RequiredGates[0], branch, sourceSHA, "failed")

	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, w6GovItem).Scan(&status))
	assert.Equal(t, "validating", status, "a failing gate must not release the item")

	// The merged fact on a still-validating item stays withheld — the
	// machine, not the syncer, owns the edges.
	outcome := f.apply(t, mrBody(22, "merged", branch, strings.Repeat("d", 40)))
	assert.False(t, outcome.Transitioned)
	assert.True(t, outcome.Withheld)
}

// TestCrossDomainMRBindsByBranchProjectKey reproduces the F2 scenario:
// the repo maps to w6repo, the branch names w6gov — the projection
// must land under the BRANCH-resolved project with the work item
// bound, and the merged fact must drive the item to done. Before W6
// the composite FK rejected the row (23503) and the projection never
// landed (the W1 "evidence=0" artifact).
func TestCrossDomainMRBindsByBranchProjectKey(t *testing.T) {
	f := newW6Fixture(t)
	// The done chain needs ready first; put the item one edge from done
	// to isolate the binding behavior.
	_, err := f.db.Exec(`UPDATE work_items SET status = 'ready_for_human_merge' WHERE id = $1`, w6GovItem)
	require.NoError(t, err)

	branch := "maestro/w6gov/" + w6GovItem
	outcome := f.apply(t, mrBody(23, "merged", branch, strings.Repeat("e", 40)))
	assert.True(t, outcome.Transitioned)

	var rowProject, workItem string
	require.NoError(t, f.db.QueryRow(`
		SELECT project_id::text, work_item_id::text FROM merge_requests WHERE mr_iid = 23`).
		Scan(&rowProject, &workItem))
	assert.Equal(t, w6GovProject, rowProject, "the projection lands under the branch-named project")
	assert.Equal(t, w6GovItem, workItem)

	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, w6GovItem).Scan(&status))
	assert.Equal(t, "done", status)
}

// TestLegacyMarkerFallsBackToMappingProject keeps pre-contract
// branches working: a marker that names no project still binds against
// the mapping project when the item lives there.
func TestLegacyMarkerFallsBackToMappingProject(t *testing.T) {
	f := newW6Fixture(t)
	// An item in the MAPPED project, ready for the done edge.
	_, err := f.db.Exec(`
		INSERT INTO work_items (id, project_id, title, status, version)
		VALUES ('018f7800-0000-7000-8000-000000000019', $1, 'repo item', 'ready_for_human_merge', 1)`, w6RepoProject)
	require.NoError(t, err)

	branch := "maestro/legacy-marker/018f7800-0000-7000-8000-000000000019"
	outcome := f.apply(t, mrBody(24, "merged", branch, strings.Repeat("f", 40)))
	assert.True(t, outcome.Transitioned)

	var status string
	require.NoError(t, f.db.QueryRow(
		`SELECT status FROM work_items WHERE id = '018f7800-0000-7000-8000-000000000019'`).Scan(&status))
	assert.Equal(t, "done", status, "legacy free-form markers keep the mapping-project binding")
}

// TestUnresolvableMarkerLeavesProjectionUnbound: a branch naming an
// unknown project whose item exists nowhere lands unbound instead of
// tripping the composite FK — recorded, reconciliation territory.
func TestUnresolvableMarkerLeavesProjectionUnbound(t *testing.T) {
	f := newW6Fixture(t)
	branch := "maestro/no-such-project/018f7800-0000-7000-8000-000000000099"
	outcome := f.apply(t, mrBody(25, "opened", branch, ""))
	assert.False(t, outcome.Transitioned)
	assert.False(t, outcome.Withheld)

	var rowProject string
	var workItem sql.NullString
	require.NoError(t, f.db.QueryRow(`
		SELECT project_id::text, work_item_id FROM merge_requests WHERE mr_iid = 25`).
		Scan(&rowProject, &workItem))
	assert.Equal(t, w6RepoProject, rowProject, "unresolvable markers record under the mapping project")
	assert.False(t, workItem.Valid, "no binding is invented")
}

// TestBranchMarkerContract pins the naming-contract parser itself.
func TestBranchMarkerContract(t *testing.T) {
	key, item := gitlab.BranchMarker("maestro/w6gov/" + w6GovItem)
	assert.Equal(t, "w6gov", key)
	assert.Equal(t, w6GovItem, item)
	key, item = gitlab.BranchMarker("feature/one")
	assert.Empty(t, key)
	assert.Empty(t, item)
	key, item = gitlab.BranchMarker("maestro/onlymarker")
	assert.Empty(t, key)
	assert.Empty(t, item)
}

// The fixture's ingest wiring covers the tuple resolver too: BranchTuple
// must find the cross-domain projection through the branch key.
func TestBranchTupleResolvesCrossDomainProjection(t *testing.T) {
	f := newW6Fixture(t)
	branch := "maestro/w6gov/" + w6GovItem
	f.apply(t, mrBody(26, "opened", branch, ""))

	_, workItemID, sourceSHA, targetSHA, complete, err := f.pg.GitLab().BranchTuple(
		context.Background(), w6RepoProject, branch)
	require.NoError(t, err)
	assert.True(t, complete)
	assert.Equal(t, w6GovItem, workItemID)
	assert.Equal(t, strings.Repeat("7", 40), sourceSHA)
	assert.Equal(t, strings.Repeat("8", 40), targetSHA)
}
