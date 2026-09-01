// Package m2drill is the M2-P5 convergence playbook: one continuous,
// PG-gated scenario that walks the frozen exit-gate anchors end to end
// against the REAL surfaces — the HTTP receiver (token verification,
// replay), the inbox consumer (out-of-order convergence), the evidence
// ingestion and gate evaluation (exact-SHA), the merged-fact done
// edge, and provider-outage reconciliation. Each subtest asserts the
// playbook line it names; the fixture is cumulative on purpose: drift
// in one stage must surface in the next, never silently pass.
package m2drill

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/ZoonChen/Maestro-MCP/internal/handler"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	drillInstance = "018f7900-0000-7000-8000-000000000001"
	drillTeam     = "018f7900-0000-7000-8000-000000000004"
	drillProject  = "018f7900-0000-7000-8000-000000000002"
	drillWorkItem = "018f7900-0000-7000-8000-000000000003"
	drillBranch   = "maestro/drill/" + drillWorkItem
	drillSecret   = "drill-shared-token"
	drillKey      = "drill-payload-key"
)

type drillFixture struct {
	db         *sql.DB
	pg         *store.PostgresStore
	router     *gin.Engine
	consumer   *gitlab.Consumer
	dispatch   *webhook.Dispatcher
	cipher     *webhook.PayloadCipher
	reconcile  *gitlab.Reconciler
	provider   *httptest.Server
	providerUp bool
}

func newDrillFixture(t *testing.T) *drillFixture {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_p5_drill WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_p5_drill`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_p5_drill WITH (FORCE)`)
		_ = admin.Close()
	})

	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := store.OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_p5_drill")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'drill team')`, drillTeam)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'drill', 'Drill', 'active')`, drillProject, drillTeam)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, version)
		VALUES ($1, $2, 'drill task', 'ready_for_human_merge', 9)`, drillWorkItem, drillProject)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ($1, 'https://gitlab.drill.example', 'drill', 'env:MAESTRO_DRILL_BOT', 'env:MAESTRO_DRILL_HOOK')`, drillInstance)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, default_branch)
		VALUES ($1, 9500, $2, 'main')`, drillInstance, drillProject)
	require.NoError(t, err)
	t.Setenv("MAESTRO_DRILL_HOOK", drillSecret)
	t.Setenv("MAESTRO_DRILL_BOT", "drill-bot-token")

	cipher, err := webhook.NewPayloadCipher(drillKey)
	require.NoError(t, err)

	// The stub provider for outage/recovery drills.
	fixture := &drillFixture{db: db, pg: pg, cipher: cipher, providerUp: true}
	fixture.provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fixture.providerUp {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Path == "/api/v4/projects/9500/merge_requests/42" {
			fmt.Fprintf(w, `{"iid": 42, "state": "merged",
				"source_branch": %q, "target_branch": "main",
				"sha": "%s", "merge_commit_sha": "%s", "merged_at": "2026-09-01T00:00:00Z",
				"diff_refs": {"base_sha": "%s", "head_sha": "%s"}}`,
				drillBranch, sourceA, mergeB, targetA, sourceA)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(fixture.provider.Close)

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
	fixture.consumer = &gitlab.Consumer{
		Outbox:     pg.Outbox(),
		Deliveries: pg.WebhookDeliveries(),
		Syncer:     syncer,
		Cipher:     cipher,
		BatchSize:  32,
		RetryDelay: time.Nanosecond,
	}
	fixture.dispatch = &webhook.Dispatcher{
		Store: pg.Webhooks(), Cipher: cipher,
		MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
	}
	fixture.reconcile = &gitlab.Reconciler{
		Mapping: pg.Instances(),
		Secrets: webhook.EnvSecretResolver{},
		Syncer:  syncer,
		NewClient: func(baseURL, token string) (*gitlab.Client, error) {
			client, clientErr := gitlab.NewClient(baseURL, token)
			if clientErr != nil {
				return nil, clientErr
			}
			return client.WithTestTransport(loopbackTransport{server: fixture.provider}), nil
		},
	}

	// The real receiver route, mounted like production.
	router := gin.New()
	router.Use(handler.MaxBodySize(1 << 20))
	handler.RegisterGitLabWebhookIngest(router, handler.GitLabWebhookOptions{
		Ingestor: &webhook.Ingestor{
			Store:   pg.Webhooks(),
			Secrets: webhook.EnvSecretResolver{},
			Cipher:  cipher,
		},
	})
	fixture.router = router
	return fixture
}

type loopbackTransport struct{ server *httptest.Server }

func (l loopbackTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	stubbed := request.Clone(request.Context())
	stubbed.URL.Scheme = "http"
	stubbed.URL.Host = strings.TrimPrefix(l.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(stubbed)
}

const (
	sourceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	targetA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mergeB  = "cccccccccccccccccccccccccccccccccccccccc"
)

func (f *drillFixture) deliver(t *testing.T, eventUUID, kind, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost,
		"/api/v3/webhooks/gitlab/"+drillInstance, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Gitlab-Token", drillSecret)
	request.Header.Set("X-Gitlab-Event-UUID", eventUUID)
	request.Header.Set("X-Gitlab-Event", eventHeaderFor(kind))
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

func eventHeaderFor(kind string) string {
	switch kind {
	case "merge_request":
		return "Merge Request Hook"
	case "pipeline":
		return "Pipeline Hook"
	case "job":
		return "Job Hook"
	default:
		return "Push Hook"
	}
}

// drain runs the inbox dispatcher and consumer until quiet.
func (f *drillFixture) drain(t *testing.T) {
	t.Helper()
	for range 10 {
		for {
			outcome, err := f.dispatch.DispatchOne(context.Background(), "drill-worker")
			require.NoError(t, err)
			if outcome == "empty" {
				break
			}
		}
		_, err := f.consumer.ProcessBatch(context.Background(), "drill-consumer")
		require.NoError(t, err)
	}
}

func (f *drillFixture) gateStates(t *testing.T) map[string]string {
	t.Helper()
	snapshots, err := f.pg.Quality().ListGateSnapshots(context.Background(), drillProject, drillWorkItem)
	require.NoError(t, err)
	states := map[string]string{}
	for _, snapshot := range snapshots {
		states[snapshot.Check+"/"+shortSHA(snapshot.SourceSHA)] = snapshot.Status
	}
	return states
}

func shortSHA(sha string) string { return sha[:8] }

func mrEvent(uuid, state, source, target, mergeCommit string) string {
	return fmt.Sprintf(`{"object_kind": "merge_request", "project": {"id": 9500},
		"object_attributes": {"iid": 42, "state": %q,
			"source_branch": %q, "target_branch": "main",
			"last_commit": {"id": %q},
			"merge_commit_sha": %q, "merged_at": "2026-09-01T00:00:00Z",
			"diff_refs": {"base_sha": %q, "head_sha": %q}}}`,
		state, drillBranch, source, mergeCommit, target, source)
}

func jobEvent(pipelineID int64, seq int, check, status string) string {
	return fmt.Sprintf(`{"object_kind": "job", "project_id": 9500, "pipeline_id": %d,
		"build_id": %d, "build_name": %q, "build_status": %q, "stage": "test",
		"ref": %q, "sha": %q}`, pipelineID, 6000+seq, check, status, drillBranch, sourceA)
}

func TestP5ConvergencePlaybook(t *testing.T) {
	f := newDrillFixture(t)

	t.Run("invalid signature has no business effect", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost,
			"/api/v3/webhooks/gitlab/"+drillInstance,
			strings.NewReader(mrEvent("evt-forged", "opened", sourceA, targetA, "")))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Gitlab-Token", "forged-token")
		request.Header.Set("X-Gitlab-Event-UUID", "evt-forged")
		request.Header.Set("X-Gitlab-Event", "Merge Request Hook")
		response := httptest.NewRecorder()
		f.router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusUnauthorized, response.Code)

		var inboxRows int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM webhook_inbox`).Scan(&inboxRows))
		assert.Zero(t, inboxRows, "an invalid signature leaves no business rows")
	})

	t.Run("out-of-order jobs wait for the tuple without inventing facts", func(t *testing.T) {
		// The pipeline projection lands first (jobs key on it).
		require.Equal(t, http.StatusAccepted, f.deliver(t, "evt-pipe-500", "pipeline",
			fmt.Sprintf(`{"object_kind": "pipeline", "project": {"id": 9500},
				"object_attributes": {"id": 500, "sha": %q, "ref": %q, "status": "running", "source": "merge_request_event"}}`, sourceA, drillBranch)).Code)

		// Jobs BEFORE any MR tuple exists: projections only, no evidence.
		for seq, check := range []string{"unit", "lint_typecheck"} {
			require.Equal(t, http.StatusAccepted, f.deliver(t, "evt-job-"+check, "job", jobEvent(500, seq, check, "success")).Code)
		}
		f.drain(t)
		records, err := f.pg.Quality().ListEvidenceForWorkItem(context.Background(), drillProject, drillWorkItem)
		require.NoError(t, err)
		assert.Empty(t, records, "no evidence without a completed SHA tuple")
	})

	t.Run("duplicate MR deliveries are exactly once and complete the tuple", func(t *testing.T) {
		body := mrEvent("evt-open-1", "opened", sourceA, targetA, "")
		require.Equal(t, http.StatusAccepted, f.deliver(t, "evt-open-1", "merge_request", body).Code)
		require.Equal(t, http.StatusAccepted, f.deliver(t, "evt-open-1", "merge_request", body).Code)

		var inboxRows int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM webhook_inbox`).Scan(&inboxRows))
		assert.Equal(t, 4, inboxRows, "the replay collapsed on the dedup key (pipeline+2 jobs+1 MR)")

		f.drain(t)
		var deliveries int
		require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE outcome = 'duplicate'`).Scan(&deliveries))
		assert.Equal(t, 1, deliveries, "the replay is audited as a duplicate")
	})

	t.Run("gate evidence converges and a missing gate blocks ready", func(t *testing.T) {
		// A second pipeline run re-reports every gate EXCEPT sast: the
		// out-of-order gates land, sast stays missing.
		require.Equal(t, http.StatusAccepted, f.deliver(t, "evt-pipe-501", "pipeline",
			fmt.Sprintf(`{"object_kind": "pipeline", "project": {"id": 9500},
				"object_attributes": {"id": 501, "sha": %q, "ref": %q, "status": "success", "source": "merge_request_event"}}`, sourceA, drillBranch)).Code)
		company := testCompany(t)
		for seq, check := range company.RequiredGates {
			if check == "sast" {
				continue // the webhook loss the playbook asks about
			}
			require.Equal(t, http.StatusAccepted, f.deliver(t, "evt-job2-"+check, "job", jobEvent(501, seq, check, "success")).Code)
		}
		f.drain(t)

		states := f.gateStates(t)
		for _, check := range company.RequiredGates {
			expected := "passed"
			if check == "sast" {
				expected = "pending"
			}
			assert.Equal(t, expected, states[check+"/"+shortSHA(sourceA)], check)
		}
	})

	t.Run("sha drift stales old snapshots immediately", func(t *testing.T) {
		newSource := strings.Repeat("d", 40)
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, "evt-drift", "merge_request", mrEvent("evt-drift", "opened", newSource, targetA, "")).Code)
		f.drain(t)

		var stale int
		require.NoError(t, f.db.QueryRow(`
			SELECT count(*) FROM gate_snapshots WHERE status = 'stale'`).Scan(&stale))
		assert.GreaterOrEqual(t, stale, 12, "every old-tuple snapshot went stale")

		states := f.gateStates(t)
		assert.Equal(t, "pending", states["unit/"+shortSHA(newSource)],
			"the new tuple starts over: old evidence never answers for a new SHA")
	})

	t.Run("outage keeps the cached model, reconciliation recovers and drives done", func(t *testing.T) {
		// Provider down: reconcile answers unavailable, nothing changes.
		f.providerUp = false
		_, err := f.reconcile.ReconcileMergeRequest(context.Background(), drillProject, 42)
		require.Error(t, err, "outage propagates")
		var status string
		require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, drillWorkItem).Scan(&status))
		assert.Equal(t, "ready_for_human_merge", status, "outage never invents or regresses state")

		// Provider back: reconciliation pulls the merged fact and drives
		// the done edge on the ORIGINAL tuple.
		f.providerUp = true
		outcome, err := f.reconcile.ReconcileMergeRequest(context.Background(), drillProject, 42)
		require.NoError(t, err)
		assert.True(t, outcome.Transitioned)

		require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, drillWorkItem).Scan(&status))
		assert.Equal(t, "done", status)
		var fact string
		require.NoError(t, f.db.QueryRow(`SELECT merged_fact_id FROM work_items WHERE id = $1`, drillWorkItem).Scan(&fact))
		assert.Equal(t, "gitlab:reconcile:mr:42", fact)
	})

	t.Run("merged webhook replay after done is inert", func(t *testing.T) {
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, "evt-merged-1", "merge_request", mrEvent("evt-merged-1", "merged", sourceA, targetA, mergeB)).Code)
		f.drain(t)
		var status, version string
		require.NoError(t, f.db.QueryRow(`SELECT status, merged_fact_id FROM work_items WHERE id = $1`, drillWorkItem).Scan(&status, &version))
		assert.Equal(t, "done", status, "the webhook fact lands on an already-done item without effect")
		assert.Equal(t, "gitlab:reconcile:mr:42", version, "the original lineage is never rewritten")
	})
}

func testCompany(t *testing.T) *evidence.Policy {
	t.Helper()
	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	return company
}
