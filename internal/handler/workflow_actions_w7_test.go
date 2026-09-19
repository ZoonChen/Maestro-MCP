package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
)

// W7 slice-friction acceptance replays (brief W7 §2): the B10-1
// two-incident targeted-claim/refusal + release-to-queue-head scenario
// (F29), the truncated/wrong-SHA complete refusals (F32) and the
// validation-run reporting face with its idempotency collapse (F15) —
// all through the real /api/v3 tree against PostgreSQL.

const (
	w7ProjectID  = "018f7900-0000-7000-8000-000000000031"
	w7HeadItem   = "018f7900-0000-7000-8000-000000000032" // B10-1 stand-in: older created_at, queue head
	w7TailItem   = "018f7900-0000-7000-8000-000000000033" // the item the session was actually assigned
	w7RunnerID   = "018f7900-0000-7000-8000-000000000034"
	w7InstanceID = "018f7900-0000-7000-8000-000000000035"
)

func newW7FrictionFixture(t *testing.T) *workflowActionsFixture {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_handler_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_handler_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_handler_test WITH (FORCE)`)
		_ = admin.Close()
	})

	db, err := store.OpenPostgres(context.Background(), testDatabaseDSN(t))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'w7 friction team')`, qTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'w7-flow', 'W7 Flow', 'active')`,
		w7ProjectID, qTeamID)
	require.NoError(t, err)
	// The B10-1 shape: an older high-noise item owns the queue head; the
	// session's assigned item sits behind it in the same priority band.
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, created_at)
		VALUES ($1, $2, 'w7 head (mis-dispatch hazard)', 'queued', now() - interval '1 hour')`,
		w7HeadItem, w7ProjectID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, 'w7 tail (assigned)', 'queued')`,
		w7TailItem, w7ProjectID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO runners (id, display_name, device_key_hash, status)
		VALUES ($1, 'w7 claim runner', 'x', 'approved')`, w7RunnerID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO runner_bindings (project_id, runner_id) VALUES ($1, $2)`, w7ProjectID, w7RunnerID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ($1, 'https://w7-gitlab.example', 'W7 GitLab', 'ref', 'ref')`, w7InstanceID)
	require.NoError(t, err)

	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	idp := newHandlerTestIdP(t)
	resolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"dev-1":    {w7ProjectID: "developer"},
		"viewer-1": {w7ProjectID: "viewer"},
	}}
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, resolver)

	router := gin.New()
	router.Use(mw.Authenticate)
	RegisterControlPlane(router, ControlPlaneOptions{
		Identity: mw,
		Workflow: NewWorkflowActionsHandler(pg, pg.Assets(), pg.Quality()),
		Scope:    pg.Instances(),
	})
	return &workflowActionsFixture{
		router: router, pg: pg, db: db,
		devTK:  idp.signedToken(t, "dev-1"),
		viewTK: idp.signedToken(t, "viewer-1"),
	}
}

// w7Request drives the fixture router with an optional Idempotency-Key.
func w7Request(t *testing.T, f *workflowActionsFixture, token, method, path, body, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	t.Logf("request %s %s -> %d %s", method, path, response.Code, response.Body.String())
	return response
}

func w7QueueVersion(t *testing.T, f *workflowActionsFixture) int64 {
	t.Helper()
	var version int64
	require.NoError(t, f.db.QueryRow(`SELECT version FROM projects WHERE id = $1`, w7ProjectID).Scan(&version))
	return version
}

// TestW7FrictionTargetedClaimAndRelease replays the B10-1 incident
// shape end to end: the assigned item is NOT the queue head, the
// targeted claim refuses the mis-dispatch, the untargeted head claim
// can be RELEASED back to the queue head (audit + outbox atomic), and
// the released item is the next dispatch.
func TestW7FrictionTargetedClaimAndRelease(t *testing.T) {
	f := newW7FrictionFixture(t)

	t.Run("targeted claim of the non-head item is refused", func(t *testing.T) {
		before := w7QueueVersion(t, f)
		reply := w7Request(t, f, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/work-items/claim",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w7-gen-1","work_item_id":%q}`, w7RunnerID, w7TailItem), "")
		require.Equal(t, http.StatusConflict, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "CLAIM_TARGET_MISMATCH")

		var leases, executions int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM leases`).Scan(&leases))
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM executions`).Scan(&executions))
		assert.Zero(t, leases, "the refusal happens before any lease side effect")
		assert.Zero(t, executions)
		assert.Equal(t, before, w7QueueVersion(t, f), "the queue token does not move on refusal")
	})

	var headExecution string
	t.Run("targeted claim of the head lands", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/work-items/claim",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w7-gen-1","work_item_id":%q}`, w7RunnerID, w7HeadItem), "")
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), `"work_item_id":"`+w7HeadItem+`"`)
		headExecution = replyJSONStringField(t, reply.Body.String(), "execution_id")
		require.NotEmpty(t, headExecution)
	})

	t.Run("release is developer-plane", func(t *testing.T) {
		reply := w7Request(t, f, f.viewTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/executions/"+headExecution+"/release",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w7-gen-1","reason":"wrong assignment"}`, w7RunnerID), "")
		assert.Equal(t, http.StatusForbidden, reply.Code)
	})

	t.Run("release returns the item to the queue head with audit and outbox", func(t *testing.T) {
		before := w7QueueVersion(t, f)
		reply := w7Request(t, f, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/executions/"+headExecution+"/release",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w7-gen-1","reason":"mis-claimed: session was assigned the tail item"}`, w7RunnerID), "")
		require.Equal(t, http.StatusAccepted, reply.Code, reply.Body.String())

		var status string
		require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, w7HeadItem).Scan(&status))
		assert.Equal(t, "queued", status)

		var executionStatus, leaseStatus string
		require.NoError(t, f.db.QueryRow(`SELECT status FROM executions WHERE id = $1`, headExecution).Scan(&executionStatus))
		require.NoError(t, f.db.QueryRow(`
			SELECT l.status FROM leases l JOIN executions e ON e.lease_id = l.id WHERE e.id = $1`, headExecution).Scan(&leaseStatus))
		assert.Equal(t, "released", executionStatus)
		assert.Equal(t, "released", leaseStatus)

		var audits int
		require.NoError(t, f.db.QueryRow(
			`SELECT count(*) FROM audit_events WHERE action = 'work_item.released' AND resource_id = $1`, w7HeadItem).Scan(&audits))
		assert.Equal(t, 1, audits, "the release audit row commits with the state change")

		var outbox int
		require.NoError(t, f.db.QueryRow(`
			SELECT count(*) FROM outbox_events
			WHERE event_type = 'work_item.state.changed' AND subject = $1
			  AND payload->>'from' = 'executing' AND payload->>'to' = 'queued'`, w7HeadItem).Scan(&outbox))
		assert.Equal(t, 1, outbox, "the release outbox event commits with the state change")

		assert.Greater(t, w7QueueVersion(t, f), before, "waiters must re-observe the queue token")
	})

	t.Run("the released item is the next dispatch", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/work-items/claim",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w7-gen-2"}`, w7RunnerID), "")
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), `"work_item_id":"`+w7HeadItem+`"`,
			"release re-enters at the queue head, ahead of its original FIFO position")
	})
}

// TestW7FrictionCommitSHAFences replays the two F32 mis-acceptances:
// the truncated 8-hex SHA (S2B9 B5-2) and the wrong-branch 40-hex SHA
// (S2B8 B2-3) are both refused; the head-matching full SHA lands.
func TestW7FrictionCommitSHAFences(t *testing.T) {
	f := newW7FrictionFixture(t)

	reply := w7Request(t, f, f.devTK, http.MethodPost,
		"/api/v3/projects/"+w7ProjectID+"/work-items/claim",
		fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w7-gen-1"}`, w7RunnerID), "")
	require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
	executionID := replyJSONStringField(t, reply.Body.String(), "execution_id")
	require.Equal(t, w7HeadItem, replyJSONStringField(t, reply.Body.String(), "work_item_id"))

	t.Run("truncated SHA refused at the shape fence", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/executions/"+executionID+"/complete",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w7-gen-1","outcome":"completed","commit_sha":"650f5ff9"}`, w7RunnerID), "")
		require.Equal(t, http.StatusBadRequest, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "INVALID_PARAMETER")
	})

	t.Run("wrong-branch full SHA refused at the branch-head fence", func(t *testing.T) {
		knownHead := strings.Repeat("aa", 20)
		_, err := f.db.ExecContext(context.Background(), `
			INSERT INTO merge_requests (id, project_id, gitlab_instance_id, gitlab_project_id, mr_iid,
				work_item_id, state, source_branch, target_branch, source_sha, target_sha)
			VALUES (gen_random_uuid(), $1, $2, 9001, 1, $3, 'opened',
				$4, 'main', $5, $6)`,
			w7ProjectID, w7InstanceID, w7HeadItem,
			"maestro/w7-flow/"+w7HeadItem, knownHead, strings.Repeat("bb", 20))
		require.NoError(t, err)

		reply := w7Request(t, f, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/executions/"+executionID+"/complete",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w7-gen-1","outcome":"completed","commit_sha":"%s"}`,
				w7RunnerID, strings.Repeat("cc", 20)), "")
		require.Equal(t, http.StatusBadRequest, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "COMMIT_SHA_MISMATCH")

		var status string
		require.NoError(t, f.db.QueryRow(`SELECT status FROM executions WHERE id = $1`, executionID).Scan(&status))
		assert.Equal(t, "running", status, "the refused completion leaves the execution untouched")
	})

	t.Run("head-matching full SHA accepted", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/executions/"+executionID+"/complete",
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"w7-gen-1","outcome":"completed","commit_sha":"%s"}`,
				w7RunnerID, strings.Repeat("aa", 20)), "")
		require.Equal(t, http.StatusAccepted, reply.Code, reply.Body.String())

		var status string
		require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, w7HeadItem).Scan(&status))
		assert.Equal(t, "validating", status)
	})
}

// TestW7FrictionValidationRunReporting replays the F15 per-slice psql
// backfill as the reporting face: permission-gated, key-collapsed and
// attempt-sequenced, with the reported profile_ref landing where
// judgeBoundary reads it.
func TestW7FrictionValidationRunReporting(t *testing.T) {
	f := newW7FrictionFixture(t)
	const path = "/api/v3/projects/" + w7ProjectID + "/work-items/" + w7TailItem + "/validation-runs"
	body := `{"profile_ref":"profile-maven-build@3","base_commit":"%s","source_commit":"%s","changed_files":["a.java"],"duration_ms":154321,"boundary_ok":true,"test_ok":true,"coverage_ok":true,"result":"passed","producer":"ci-smoke"}`

	t.Run("the Idempotency-Key is required", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost, path, fmt.Sprintf(body, strings.Repeat("11", 20), strings.Repeat("22", 20)), "")
		require.Equal(t, http.StatusBadRequest, reply.Code, reply.Body.String())
	})

	t.Run("reporting is submit-plane", func(t *testing.T) {
		reply := w7Request(t, f, f.viewTK, http.MethodPost, path, fmt.Sprintf(body, strings.Repeat("11", 20), strings.Repeat("22", 20)), "w7-key-denied")
		assert.Equal(t, http.StatusForbidden, reply.Code)
	})

	var firstID string
	t.Run("first report creates attempt 1", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost, path, fmt.Sprintf(body, strings.Repeat("11", 20), strings.Repeat("22", 20)), "w7-key-1")
		require.Equal(t, http.StatusCreated, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), `"attempt":1`)
		assert.Contains(t, reply.Body.String(), `"created":true`)
		firstID = replyJSONIntField(t, reply.Body.String(), "id")
	})

	t.Run("the same key replays the SAME row", func(t *testing.T) {
		// A different body under the same key still collapses onto the
		// original row — the key, not the content, owns the identity.
		reply := w7Request(t, f, f.devTK, http.MethodPost, path, fmt.Sprintf(body, strings.Repeat("33", 20), strings.Repeat("44", 20)), "w7-key-1")
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), `"attempt":1`)
		assert.Contains(t, reply.Body.String(), `"replayed":true`)
		assert.Equal(t, firstID, replyJSONIntField(t, reply.Body.String(), "id"))
	})

	t.Run("a new key mints the next attempt", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost, path, fmt.Sprintf(body, strings.Repeat("55", 20), strings.Repeat("66", 20)), "w7-key-2")
		require.Equal(t, http.StatusCreated, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), `"attempt":2`)
	})

	t.Run("the reported profile_ref is what judgeBoundary reads", func(t *testing.T) {
		var profileRef string
		require.NoError(t, f.db.QueryRow(`
			SELECT profile_ref FROM validation_runs
			WHERE project_id = $1 AND work_item_id = $2 AND profile_ref <> ''
			ORDER BY id DESC LIMIT 1`, w7ProjectID, w7TailItem).Scan(&profileRef))
		assert.Equal(t, "profile-maven-build@3", profileRef)
	})
}

// replyJSONIntField extracts a flat integer field from a handler reply.
func replyJSONIntField(t *testing.T, body, field string) string {
	t.Helper()
	prefix := `"` + field + `":`
	start := strings.Index(body, prefix)
	if start < 0 {
		return ""
	}
	rest := body[start+len(prefix):]
	end := strings.IndexAny(rest, ",}")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// TestW7FrictionReleaseAndCompleteFences drills the store error fences
// the happy-path tests leave cold: the release lease lookup, release
// generation fence and release concurrency guard; the complete lease
// lookup and outcome validation; the validation-run not-found and
// default-value paths. These branches are the CI pg-store 80% gate's
// remaining gap (881/1114 = 79.1% before this function).
func TestW7FrictionReleaseAndCompleteFences(t *testing.T) {
	f := newW7FrictionFixture(t)
	claimPath := "/api/v3/projects/" + w7ProjectID + "/work-items/claim"
	fullSHA := strings.Repeat("ab", 20)

	var execID string
	t.Run("claim head for the fence drills", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost, claimPath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"fence-1"}`, w7RunnerID), "")
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
		execID = replyJSONStringField(t, reply.Body.String(), "execution_id")
		require.NotEmpty(t, execID)
	})

	releasePath := "/api/v3/projects/" + w7ProjectID + "/executions/" + execID + "/release"
	t.Run("release with the wrong runner is LEASE_EXPIRED", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost, releasePath,
			`{"runner_id":"018f7900-0000-7000-8000-000000000099","connection_generation":"fence-1","reason":"wrong runner probe"}`, "")
		require.Equal(t, http.StatusGone, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "LEASE_EXPIRED")
	})

	t.Run("release with a stale generation is fenced", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost, releasePath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"fence-0","reason":"stale generation probe"}`, w7RunnerID), "")
		require.Equal(t, http.StatusConflict, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "LEASE_VERSION_MISMATCH")
	})

	t.Run("release of an item that left executing is a conflict", func(t *testing.T) {
		_, err := f.db.Exec(`UPDATE work_items SET status='queued' WHERE id=$1`, w7HeadItem)
		require.NoError(t, err)
		reply := w7Request(t, f, f.devTK, http.MethodPost, releasePath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"fence-1","reason":"concurrency probe"}`, w7RunnerID), "")
		require.Equal(t, http.StatusConflict, reply.Code, reply.Body.String())
		assert.Contains(t, reply.Body.String(), "CONCURRENCY_CONFLICT")
		// The refused release leaves the stale pair behind (the store
		// rolled back); retire it the way the offline monitor would so
		// the queue's partial unique index frees the item for re-claim.
		_, err = f.db.Exec(`UPDATE executions SET status='released', ended_at=now() WHERE id=$1`, execID)
		require.NoError(t, err)
		_, err = f.db.Exec(`
			UPDATE leases l SET status='released', updated_at=now()
			FROM executions e WHERE e.lease_id = l.id AND e.id = $1`, execID)
		require.NoError(t, err)
	})

	t.Run("complete fences: lease lookup and outcome validation", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost, claimPath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"fence-2"}`, w7RunnerID), "")
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
		execID = replyJSONStringField(t, reply.Body.String(), "execution_id")
		completePath := "/api/v3/projects/" + w7ProjectID + "/executions/" + execID + "/complete"

		wrongRunner := w7Request(t, f, f.devTK, http.MethodPost, completePath,
			fmt.Sprintf(`{"runner_id":"018f7900-0000-7000-8000-000000000099","connection_generation":"fence-2","outcome":"completed","commit_sha":%q}`, fullSHA), "")
		require.Equal(t, http.StatusGone, wrongRunner.Code, wrongRunner.Body.String())
		assert.Contains(t, wrongRunner.Body.String(), "LEASE_EXPIRED")

		badOutcome := w7Request(t, f, f.devTK, http.MethodPost, completePath,
			fmt.Sprintf(`{"runner_id":%q,"connection_generation":"fence-2","outcome":"exploded","commit_sha":%q}`, w7RunnerID, fullSHA), "")
		require.Equal(t, http.StatusBadRequest, badOutcome.Code, badOutcome.Body.String())
		assert.Contains(t, badOutcome.Body.String(), "INVALID_PARAMETER")
	})

	t.Run("validation run on an unknown item is 404", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/work-items/018f7900-0000-7000-8000-000000000099/validation-runs",
			`{"profile_ref":"p@1"}`, "fence-key-404")
		require.Equal(t, http.StatusNotFound, reply.Code, reply.Body.String())
	})

	t.Run("validation run defaults fill result, producer and changed_files", func(t *testing.T) {
		reply := w7Request(t, f, f.devTK, http.MethodPost,
			"/api/v3/projects/"+w7ProjectID+"/work-items/"+w7HeadItem+"/validation-runs",
			`{"profile_ref":"p@2"}`, "fence-key-defaults")
		require.Equal(t, http.StatusCreated, reply.Code, reply.Body.String())
		var result, producer, changed string
		require.NoError(t, f.db.QueryRow(`
			SELECT result, producer, changed_files::text FROM validation_runs
			WHERE project_id=$1 AND work_item_id=$2 AND idempotency_key='fence-key-defaults'`,
			w7ProjectID, w7HeadItem).Scan(&result, &producer, &changed))
		assert.Equal(t, "reported", result)
		assert.Equal(t, "maestro-local", producer)
		assert.Equal(t, "[]", changed)
	})
}
