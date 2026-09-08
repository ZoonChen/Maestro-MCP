package m4drill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/ZoonChen/Maestro-MCP/internal/handler"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
)

// The webhook-pipeline-failure drill (M4-RBK-001, TC-WHK-001/TC-WHK-002):
// the runbook's four detection points (RUNBOOK-WEBHOOK-PIPELINE-FAILURE
// §4.1/§11) as executable fault injections against the REAL receive,
// dispatch, replay and reconcile surfaces — 拒签风暴, 重复投递, 乱序版本
// and DLQ 积压超阈值 — then the §4.2 recovery branches: the authorized
// DLQ replay under the original event identity, and the post-outage
// reconcile with its audited manual residue. Constants and helpers are
// wh-prefixed to stay disjoint from the database-restore anchor that
// owns this package's package comment.
const (
	whDrillDB = "maestro_wh_drill"

	whTeam     = "018f7f00-0000-7000-8000-000000000001"
	whProject  = "018f7f00-0000-7000-8000-000000000002"
	whWorkItem = "018f7f00-0000-7000-8000-000000000003"
	whInstance = "018f7f00-0000-7000-8000-000000000004"
	whBranch   = "maestro/wh/" + whWorkItem
	whSecret   = "wh-drill-shared-token" //nolint:gosec // drill fixture token, never a real credential
	whKey      = "wh-drill-payload-key"

	// A second project for the operator-fix branch: project mappings are
	// 1:1 (migration 0008), so the unmapped GitLab project maps here.
	whSecondProject = "018f7f00-0000-7000-8000-000000000005"

	whGitlabProject   int64 = 9600
	whUnmappedProject int64 = 9601 // mapped only after the operator fix
	whStormSize             = 25
	whDLQP2Threshold        = 1 // runbook §3: a single DLQ row already classifies P2

	whSource    = "1111111111111111111111111111111111111111"
	whTarget    = "2222222222222222222222222222222222222222"
	whMerge     = "3333333333333333333333333333333333333333"
	whOldSource = "4444444444444444444444444444444444444444" // the stale, older version
)

type whFixture struct {
	db         *sql.DB
	pg         *store.PostgresStore
	router     *gin.Engine
	dispatch   *webhook.Dispatcher
	consumer   *gitlab.Consumer
	reconcile  *gitlab.Reconciler
	cipher     *webhook.PayloadCipher
	provider   *httptest.Server
	providerUp bool
}

func newWebhookDrillFixture(t *testing.T) *whFixture {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+whDrillDB+` WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `CREATE DATABASE `+whDrillDB)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+whDrillDB+` WITH (FORCE)`)
		_ = admin.Close()
	})

	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := store.OpenPostgres(ctx, dsn[:strings.LastIndex(dsn, "/")+1]+whDrillDB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = store.ApplyPostgresMigrations(ctx, db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'wh drill team')`, whTeam)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'whdrill', 'WH Drill', 'active')`,
		whProject, whTeam)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, version)
		VALUES ($1, $2, 'wh drill task', 'ready_for_human_merge', 1)`, whWorkItem, whProject)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ($1, 'https://gitlab.whdrill.example', 'whdrill', 'env:MAESTRO_WH_DRILL_BOT', 'env:MAESTRO_WH_DRILL_HOOK')`,
		whInstance)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, default_branch)
		VALUES ($1, $2, $3, 'main')`, whInstance, whGitlabProject, whProject)
	require.NoError(t, err)
	t.Setenv("MAESTRO_WH_DRILL_HOOK", whSecret)
	t.Setenv("MAESTRO_WH_DRILL_BOT", "wh-drill-bot-token")

	cipher, err := webhook.NewPayloadCipher(whKey)
	require.NoError(t, err)

	fixture := &whFixture{db: db, pg: pg, cipher: cipher, providerUp: true}
	fixture.provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fixture.providerUp {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Path == fmt.Sprintf("/api/v4/projects/%d/merge_requests/42", whGitlabProject) {
			fmt.Fprintf(w, `{"iid": 42, "state": "merged",
				"source_branch": %q, "target_branch": "main",
				"sha": "%s", "merge_commit_sha": "%s", "merged_at": "2026-09-08T00:00:00Z",
				"diff_refs": {"base_sha": "%s", "head_sha": "%s"}}`,
				whBranch, whSource, whMerge, whTarget, whSource)
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
			return client.WithTestTransport(whLoopbackTransport{server: fixture.provider}), nil
		},
	}

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

type whLoopbackTransport struct{ server *httptest.Server }

func (l whLoopbackTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	stubbed := request.Clone(request.Context())
	stubbed.URL.Scheme = "http"
	stubbed.URL.Host = strings.TrimPrefix(l.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(stubbed)
}

func (f *whFixture) deliver(t *testing.T, token, eventUUID, kind, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost,
		"/api/v3/webhooks/gitlab/"+whInstance, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Gitlab-Token", token)
	request.Header.Set("X-Gitlab-Event-UUID", eventUUID)
	request.Header.Set("X-Gitlab-Event", whEventHeaderFor(kind))
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

func whEventHeaderFor(kind string) string {
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

// drain settles the inbox (through the given dispatcher) and the outbox
// until quiet.
func (f *whFixture) drain(t *testing.T, dispatch *webhook.Dispatcher) {
	t.Helper()
	for range 10 {
		for {
			outcome, err := dispatch.DispatchOne(context.Background(), "wh-drill-worker")
			require.NoError(t, err)
			if outcome == webhook.DispatchEmpty {
				break
			}
		}
		_, err := f.consumer.ProcessBatch(context.Background(), "wh-drill-consumer")
		require.NoError(t, err)
	}
}

func whMREvent(state, source, target, mergeCommit string) string {
	return fmt.Sprintf(`{"object_kind": "merge_request", "project": {"id": %d},
		"object_attributes": {"iid": 42, "state": %q,
			"source_branch": %q, "target_branch": "main",
			"last_commit": {"id": "%s"},
			"merge_commit_sha": %q, "merged_at": "2026-09-08T00:00:00Z",
			"diff_refs": {"base_sha": "%s", "head_sha": "%s"}}}`,
		whGitlabProject, state, whBranch, source, mergeCommit, target, source)
}

func whPipelineEvent(pipelineID, gitlabProject int64, status string) string {
	return fmt.Sprintf(`{"object_kind": "pipeline", "project": {"id": %d},
		"object_attributes": {"id": %d, "sha": "%s", "ref": %q, "status": %q, "source": "merge_request_event"}}`,
		gitlabProject, pipelineID, whSource, whBranch, status)
}

func (f *whFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

func (f *whFixture) oneString(t *testing.T, query string, args ...any) string {
	t.Helper()
	var value string
	require.NoError(t, f.db.QueryRowContext(context.Background(), query, args...).Scan(&value))
	return value
}

func (f *whFixture) inboxID(t *testing.T, externalEventID string) string {
	t.Helper()
	return f.oneString(t, `SELECT id::text FROM webhook_inbox WHERE external_event_id = $1`, externalEventID)
}

// whTransientFaults injects the infrastructure transient the retry/DLQ
// path exists for: while armed, every outbox enqueue fails. The fault
// stays test-side; the engine under drill is untouched.
type whTransientFaults struct{ armed bool }

type whFaultInjectionStore struct {
	webhook.Store
	faults *whTransientFaults
}

func (s whFaultInjectionStore) BeginApply(ctx context.Context) (webhook.ApplyUnit, error) {
	unit, err := s.Store.BeginApply(ctx)
	if err != nil {
		return nil, err
	}
	return whFaultInjectionUnit{ApplyUnit: unit, faults: s.faults}, nil
}

type whFaultInjectionUnit struct {
	webhook.ApplyUnit
	faults *whTransientFaults
}

func (u whFaultInjectionUnit) EnqueueOutbox(ctx context.Context, event *model.OutboxEvent) error {
	if u.faults.armed {
		return errors.New("wh drill: injected transient outbox enqueue failure")
	}
	return u.ApplyUnit.EnqueueOutbox(ctx, event)
}

// TestRunbookWebhookFailureInjectionMatrix is the D1 anchor: the four
// injected failure classes each land in their contracted classification
// with a durable audit row and no silent side effect — TC-WHK-001's
// invalid-signature posture and TC-WHK-002's single-state-change rule at
// the runbook §4.1/§11 detection points.
func TestRunbookWebhookFailureInjectionMatrix(t *testing.T) {
	f := newWebhookDrillFixture(t)
	ctx := context.Background()

	t.Run("拒签风暴: every forged delivery is denied, audited and side-effect free", func(t *testing.T) {
		stormStart := time.Now()
		for index := range whStormSize {
			response := f.deliver(t, "forged-token-"+fmt.Sprint(index), fmt.Sprintf("evt-forged-%03d", index),
				"merge_request", whMREvent("opened", whSource, whTarget, ""))
			require.Equal(t, http.StatusUnauthorized, response.Code)
		}
		stormDuration := time.Since(stormStart)

		// §9/§11: the audit covers every signature rejection and the
		// grouped query is the security_owner alert input — a signature
		// failure spike alerts even without business impact; with zero
		// business rows it stays short of the §3 P0 (side-effect) bar.
		classifyStart := time.Now()
		rejections := f.count(t, `
			SELECT count(*) FROM webhook_deliveries
			WHERE outcome = 'rejected' AND reject_reason = 'TOKEN_MISMATCH' AND token_verified = false`)
		classifyDuration := time.Since(classifyStart)
		require.Equal(t, whStormSize, rejections, "every forged delivery left its audit row")
		assert.Zero(t, f.count(t, `SELECT count(*) FROM webhook_inbox`),
			"an invalid token leaves no business rows")
		assert.Zero(t, f.count(t, `SELECT count(*) FROM outbox_events`),
			"the storm produced no business effect")

		meanPersist := stormDuration / whStormSize
		assert.Less(t, meanPersist, 2*time.Second, "runbook §11: delivery persistence P95 < 2s")
		t.Logf("drill evidence: rejection storm size=%d wall=%s mean_persist=%s detection_latency=%s",
			whStormSize, stormDuration.Truncate(time.Millisecond), meanPersist.Truncate(time.Microsecond),
			(stormDuration + classifyDuration).Truncate(time.Millisecond))
	})

	t.Run("重复投递: redeliveries collapse onto one inbox row and one effect", func(t *testing.T) {
		body := whMREvent("opened", whSource, whTarget, "")
		for range 4 {
			require.Equal(t, http.StatusAccepted, f.deliver(t, whSecret, "evt-dup-1", "merge_request", body).Code)
		}
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM webhook_inbox`),
			"the replay collapsed on the dedup key (WEBHOOK-RULE-002)")
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM webhook_deliveries WHERE outcome = 'accepted'`))
		assert.Equal(t, 3, f.count(t, `SELECT count(*) FROM webhook_deliveries WHERE outcome = 'duplicate'`),
			"each redelivery is audited as a duplicate")

		f.drain(t, f.dispatch)
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM outbox_events`),
			"exactly one durable event per delivery identity")
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM merge_requests WHERE mr_iid = 42`),
			"重复事件对同一业务对象最多产生一次状态变化 (§6)")
		assert.Equal(t, "processed", f.oneString(t, `SELECT status FROM webhook_inbox WHERE external_event_id = 'evt-dup-1'`))
	})

	t.Run("乱序版本: a stale older event after the merged fact never regresses state", func(t *testing.T) {
		// The newer version (merged fact) lands first...
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-merged-1", "merge_request",
				whMREvent("merged", whSource, whTarget, whMerge)).Code)
		f.drain(t, f.dispatch)
		require.Equal(t, "done", f.oneString(t, `SELECT status FROM work_items WHERE id = $1`, whWorkItem))

		// ...then GitLab redelivers the OLDER version (opened, older head
		// SHA): 事件顺序不能覆盖更新的版本 (§6/§8).
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-stale-1", "merge_request",
				whMREvent("opened", whOldSource, whTarget, "")).Code)
		f.drain(t, f.dispatch)

		var status, fact, mergeCommit string
		var version int
		require.NoError(t, f.db.QueryRowContext(ctx, `
			SELECT status, merged_fact_id, merge_commit_sha, version FROM work_items WHERE id = $1`,
			whWorkItem).Scan(&status, &fact, &mergeCommit, &version))
		assert.Equal(t, "done", status, "旧事件不得覆盖新状态 (§8)")
		assert.Equal(t, "gitlab:"+whInstance+":mr:42", fact, "the merged lineage is never rewritten")
		assert.Equal(t, whMerge, mergeCommit)
		assert.Equal(t, 2, version, "exactly one done transition — no second state change (§6)")
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM merge_requests WHERE mr_iid = 42`))
	})

	t.Run("DLQ 积压超阈值: the backlog is detected, classified and never silently dropped", func(t *testing.T) {
		// Class 1 — ingest quarantine: verified sender, unresolvable payload.
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-quarantine-1", "merge_request", `{}`).Code)
		// Class 2 — unmapped project: accepted at ingest, dead-letters at dispatch.
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-unmapped-1", "pipeline",
				whPipelineEvent(9101, whUnmappedProject, "success")).Code)
		f.drain(t, f.dispatch)
		// Class 3 — retry exhaustion: a transient outbox outage spends the
		// attempt budget (§5 handler 指数退避 → DLQ).
		faults := &whTransientFaults{armed: true}
		exhausting := &webhook.Dispatcher{
			Store:       whFaultInjectionStore{Store: f.pg.Webhooks(), faults: faults},
			Cipher:      f.cipher,
			MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
		}
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-exhausted-1", "pipeline",
				whPipelineEvent(9102, whGitlabProject, "success")).Code)
		outcome, err := exhausting.DispatchOne(ctx, "wh-drill-worker")
		require.NoError(t, err)
		require.Equal(t, webhook.DispatchRetry, outcome)
		time.Sleep(5 * time.Millisecond) // the retry backoff must elapse before reclaim
		outcome, err = exhausting.DispatchOne(ctx, "wh-drill-worker")
		require.NoError(t, err)
		require.Equal(t, webhook.DispatchDead, outcome)
		faults.armed = false

		// §3: a single DLQ row already classifies P2 — the depth query is
		// the detection point.
		backlog := f.count(t, `SELECT count(*) FROM webhook_inbox WHERE status = 'dead_letter'`)
		require.GreaterOrEqual(t, backlog, whDLQP2Threshold)

		// Each injected class carries its contracted classification.
		rows, err := f.db.QueryContext(ctx, `
			SELECT reject_reason, count(*) FROM webhook_deliveries
			WHERE outcome = 'dead_letter' GROUP BY reject_reason`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		classification := map[string]int{}
		for rows.Next() {
			var reason string
			var n int
			require.NoError(t, rows.Scan(&reason, &n))
			classification[reason] = n
		}
		require.NoError(t, rows.Err())
		require.Len(t, classification, 3)
		assert.Equal(t, 1, classification["PAYLOAD_PROJECT_MISSING"])
		assert.Equal(t, 1, classification["UNMAPPED_PROJECT"])
		assert.Equal(t, 1, classification["RETRY_EXHAUSTED"])

		// Nothing silently discarded: every DLQ row keeps its sealed body
		// and digest for the authorized replay (§8).
		assert.Equal(t, backlog, f.count(t, `
			SELECT count(*) FROM webhook_inbox
			WHERE status = 'dead_letter'
			  AND payload_digest ~ '^sha256:' AND raw_body_encrypted IS NOT NULL`))
		t.Logf("drill evidence: dlq backlog=%d classes=%v p2_threshold=%d",
			backlog, classification, whDLQP2Threshold)
	})
}

// TestRunbookWebhookDLQReplay is the D2 anchor: runbook §4.2's
// "已持久化但 handler 失败" recovery branch — after the fault clears, the
// authorized replay under the ORIGINAL event identity converges the row
// exactly once (§8: 原 event ID、不生成第二业务事件), fresh deliveries
// during the replay window do not interleave, and unreplayable rows
// return to the DLQ instead of vanishing.
func TestRunbookWebhookDLQReplay(t *testing.T) {
	f := newWebhookDrillFixture(t)
	ctx := context.Background()

	faults := &whTransientFaults{armed: true}
	exhausting := &webhook.Dispatcher{
		Store:       whFaultInjectionStore{Store: f.pg.Webhooks(), faults: faults},
		Cipher:      f.cipher,
		MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
	}

	t.Run("a transient outage exhausts the retry budget into the DLQ", func(t *testing.T) {
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-replay-1", "merge_request",
				whMREvent("opened", whSource, whTarget, "")).Code)
		outcome, err := exhausting.DispatchOne(ctx, "wh-drill-worker")
		require.NoError(t, err)
		require.Equal(t, webhook.DispatchRetry, outcome)
		time.Sleep(5 * time.Millisecond) // the retry backoff must elapse before reclaim
		outcome, err = exhausting.DispatchOne(ctx, "wh-drill-worker")
		require.NoError(t, err)
		require.Equal(t, webhook.DispatchDead, outcome)

		assert.Equal(t, "dead_letter", f.oneString(t,
			`SELECT status FROM webhook_inbox WHERE external_event_id = 'evt-replay-1'`))
		assert.Equal(t, 1, f.count(t, `
			SELECT count(*) FROM webhook_deliveries
			WHERE external_event_id = 'evt-replay-1' AND outcome = 'dead_letter'
			  AND reject_reason = 'RETRY_EXHAUSTED'`))
	})

	t.Run("fresh deliveries during the outage process independently", func(t *testing.T) {
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-fresh-1", "pipeline",
				whPipelineEvent(9200, whGitlabProject, "success")).Code)
		f.drain(t, f.dispatch)

		assert.Equal(t, "processed", f.oneString(t,
			`SELECT status FROM webhook_inbox WHERE external_event_id = 'evt-fresh-1'`))
		assert.Equal(t, "dead_letter", f.oneString(t,
			`SELECT status FROM webhook_inbox WHERE external_event_id = 'evt-replay-1'`),
			"the faulted row is untouched by the healthy delivery's settle")
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM outbox_events`),
			"only the fresh delivery emitted its event")
		assert.Zero(t, f.count(t, `SELECT count(*) FROM merge_requests`),
			"the faulted delivery's business effect stayed withheld")
	})

	t.Run("replay under the original event identity converges exactly once", func(t *testing.T) {
		faults.armed = false // the transient is cleared — the §4.2 fix
		inboxID := f.inboxID(t, "evt-replay-1")

		replayStarted := time.Now()
		requeued, err := f.pg.Webhooks().ReplayDeadLetter(ctx, inboxID, drillReplayApproval())
		require.NoError(t, err)
		require.True(t, requeued, "the dead-letter row re-queues under its original identity")
		f.drain(t, f.dispatch)
		convergence := time.Since(replayStarted)

		assert.Equal(t, "processed", f.oneString(t,
			`SELECT status FROM webhook_inbox WHERE id = $1::uuid`, inboxID))
		assert.Equal(t, "evt-replay-1", f.oneString(t,
			`SELECT external_event_id FROM webhook_inbox WHERE id = $1::uuid`, inboxID),
			"the idempotency key is never rewritten by the replay")
		assert.Equal(t, 2, f.count(t, `SELECT count(*) FROM outbox_events`),
			"the replay emitted exactly one new event")
		assert.Equal(t, "evt-replay-1", f.oneString(t,
			`SELECT payload->>'delivery_key' FROM outbox_events WHERE event_id = $1::uuid`, inboxID),
			"the replayed event binds the original delivery key")
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM merge_requests WHERE mr_iid = 42`),
			"the replayed delivery produced its business projection")
		t.Logf("drill evidence: dlq replay convergence=%s (requeue→processed→consumed)",
			convergence.Truncate(time.Millisecond))
	})

	t.Run("re-replay and redelivery stay inert", func(t *testing.T) {
		inboxID := f.inboxID(t, "evt-replay-1")
		requeued, err := f.pg.Webhooks().ReplayDeadLetter(ctx, inboxID, drillReplayApproval())
		require.NoError(t, err)
		assert.False(t, requeued, "only dead-letter rows are replayable")

		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-replay-1", "merge_request",
				whMREvent("opened", whSource, whTarget, "")).Code)
		assert.Equal(t, "processed", f.oneString(t,
			`SELECT status FROM webhook_inbox WHERE id = $1::uuid`, inboxID),
			"a redelivery never resets or regresses inbox progress (GL-INV-004)")
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM outbox_events WHERE event_id = $1::uuid`, inboxID),
			"no second business event")
		assert.Equal(t, 1, f.count(t, `
			SELECT count(*) FROM webhook_deliveries
			WHERE external_event_id = 'evt-replay-1' AND outcome = 'duplicate'`),
			"the redelivery is audited as a duplicate")
	})

	t.Run("an unreplayable row returns to the DLQ instead of vanishing", func(t *testing.T) {
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-bad-1", "merge_request", `{}`).Code)
		inboxID := f.inboxID(t, "evt-bad-1")
		require.Equal(t, "dead_letter", f.oneString(t,
			`SELECT status FROM webhook_inbox WHERE id = $1::uuid`, inboxID))

		requeued, err := f.pg.Webhooks().ReplayDeadLetter(ctx, inboxID, drillReplayApproval())
		require.NoError(t, err)
		require.True(t, requeued)
		f.drain(t, f.dispatch)

		assert.Equal(t, "dead_letter", f.oneString(t,
			`SELECT status FROM webhook_inbox WHERE id = $1::uuid`, inboxID),
			"a still-unresolvable payload re-quarantines — data is never lost (§5)")
		assert.Equal(t, 2, f.count(t, `
			SELECT count(*) FROM webhook_deliveries
			WHERE inbox_id = $1::uuid AND outcome = 'dead_letter' AND reject_reason = 'PAYLOAD_PROJECT_MISSING'`,
			inboxID), "both the ingest quarantine and the replay verdict are audited")
	})

	t.Run("the operator fix unlocks the unmapped class", func(t *testing.T) {
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-mapfix-1", "pipeline",
				whPipelineEvent(9201, whUnmappedProject, "success")).Code)
		f.drain(t, f.dispatch)
		inboxID := f.inboxID(t, "evt-mapfix-1")
		require.Equal(t, "dead_letter", f.oneString(t,
			`SELECT status FROM webhook_inbox WHERE id = $1::uuid`, inboxID))

		// The operator action the runbook prescribes: map the project, then
		// replay — never auto-mapping the delivery. Mappings are 1:1, so
		// the new GitLab project maps to its own Maestro project.
		_, err := f.db.ExecContext(ctx, `
			INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'whdrill2', 'WH Drill 2', 'active')`,
			whSecondProject, whTeam)
		require.NoError(t, err)
		_, err = f.db.ExecContext(ctx, `
			INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, default_branch)
			VALUES ($1, $2, $3, 'main')`, whInstance, whUnmappedProject, whSecondProject)
		require.NoError(t, err)
		requeued, err := f.pg.Webhooks().ReplayDeadLetter(ctx, inboxID, drillReplayApproval())
		require.NoError(t, err)
		require.True(t, requeued)
		f.drain(t, f.dispatch)

		assert.Equal(t, "processed", f.oneString(t,
			`SELECT status FROM webhook_inbox WHERE id = $1::uuid`, inboxID))
		assert.Equal(t, whSecondProject, f.oneString(t, `
			SELECT project_id::text FROM pipelines WHERE gitlab_pipeline_id = 9201`),
			"the replayed delivery converged onto its projection, bound to the newly mapped project")
	})
}

// TestRunbookWebhookReconcileAndEscalation is the D3 anchor: runbook
// §4.1/§4.2's GitLab-outage semantics — degrade fail-closed, then the
// recovery reconcile converges the missed fact without loss or
// duplication (TC-GL-REC-001's webhook-loss branch), and what cannot
// auto-recover lands on the durable manual list with its audit entry
// instead of being silently dropped.
func TestRunbookWebhookReconcileAndEscalation(t *testing.T) {
	f := newWebhookDrillFixture(t)
	ctx := context.Background()

	// The webhook delivered the opened tuple; the merged fact is missed.
	require.Equal(t, http.StatusAccepted,
		f.deliver(t, whSecret, "evt-open-r", "merge_request",
			whMREvent("opened", whSource, whTarget, "")).Code)
	f.drain(t, f.dispatch)

	t.Run("GitLab outage degrades fail-closed without inventing facts", func(t *testing.T) {
		f.providerUp = false
		probeStart := time.Now()
		_, err := f.reconcile.ReconcileMergeRequest(ctx, whProject, 42)
		outageProbe := time.Since(probeStart)
		require.Error(t, err, "provider unavailability propagates — 远端事实不可确认 (§5)")

		assert.Equal(t, "ready_for_human_merge", f.oneString(t,
			`SELECT status FROM work_items WHERE id = $1`, whWorkItem),
			"outage never invents or regresses state")
		assert.Equal(t, "opened", f.oneString(t,
			`SELECT state FROM merge_requests WHERE mr_iid = 42`),
			"the cached projection stays untouched while degraded")
		f.providerUp = true
		t.Logf("drill evidence: outage probe latency=%s (fail-closed, no state change)",
			outageProbe.Truncate(time.Microsecond))
	})

	t.Run("recovery reconcile converges the missed fact exactly once", func(t *testing.T) {
		recoveryStart := time.Now()
		outcome, err := f.reconcile.ReconcileMergeRequest(ctx, whProject, 42)
		require.NoError(t, err)
		convergence := time.Since(recoveryStart)
		require.True(t, outcome.Transitioned, "the missed merged fact drove the done edge")

		var status, fact, mergeCommit string
		var version int
		require.NoError(t, f.db.QueryRowContext(ctx, `
			SELECT status, merged_fact_id, merge_commit_sha, version FROM work_items WHERE id = $1`,
			whWorkItem).Scan(&status, &fact, &mergeCommit, &version))
		assert.Equal(t, "done", status)
		assert.Equal(t, "gitlab:reconcile:mr:42", fact, "the reconcile lineage is recorded")
		assert.Equal(t, whMerge, mergeCommit)

		// Idempotent re-reconcile: no second transition, no duplication.
		again, err := f.reconcile.ReconcileMergeRequest(ctx, whProject, 42)
		require.NoError(t, err)
		assert.False(t, again.Transitioned)
		require.NoError(t, f.db.QueryRowContext(ctx,
			`SELECT version FROM work_items WHERE id = $1`, whWorkItem).Scan(&version))
		assert.Equal(t, 2, version, "exactly one done transition across reconcile passes")
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM merge_requests WHERE mr_iid = 42`),
			"无重复: one projection row")
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM outbox_events`),
			"无丢失/无重复: reconcile invents no substitute event stream")
		t.Logf("drill evidence: reconcile convergence=%s (missed fact → done)",
			convergence.Truncate(time.Microsecond))
	})

	t.Run("a late webhook merged delivery after reconcile stays inert", func(t *testing.T) {
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-merged-r", "merge_request",
				whMREvent("merged", whSource, whTarget, whMerge)).Code)
		f.drain(t, f.dispatch)

		assert.Equal(t, "done", f.oneString(t, `SELECT status FROM work_items WHERE id = $1`, whWorkItem))
		assert.Equal(t, "gitlab:reconcile:mr:42", f.oneString(t,
			`SELECT merged_fact_id FROM work_items WHERE id = $1`, whWorkItem),
			"the late delivery never rewrites the reconcile lineage (§8: no last-write-wins)")
	})

	t.Run("the unrecoverable residue lands on the audited manual list", func(t *testing.T) {
		require.Equal(t, http.StatusAccepted,
			f.deliver(t, whSecret, "evt-manual-1", "pipeline",
				whPipelineEvent(9301, whUnmappedProject, "success")).Code)
		f.drain(t, f.dispatch)
		f.drain(t, f.dispatch) // a second pass proves nothing auto-retries the row away

		backlog := f.count(t, `SELECT count(*) FROM webhook_inbox WHERE status = 'dead_letter'`)
		require.GreaterOrEqual(t, backlog, whDLQP2Threshold,
			"runbook §3: one DLQ row already classifies P2 — the escalation trigger")

		// The manual list row carries everything on-call needs (identity,
		// classification, digest) and its escalation is audited (§9).
		var kind, digest string
		require.NoError(t, f.db.QueryRowContext(ctx, `
			SELECT event_kind, payload_digest FROM webhook_inbox
			WHERE status = 'dead_letter' LIMIT 1`).Scan(&kind, &digest))
		assert.Equal(t, "pipeline", kind)
		assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)
		assert.Equal(t, 1, f.count(t, `
			SELECT count(*) FROM webhook_deliveries d
			JOIN webhook_inbox i ON i.id = d.inbox_id
			WHERE i.status = 'dead_letter'
			  AND d.outcome = 'dead_letter' AND d.reject_reason = 'UNMAPPED_PROJECT'`),
			"the human-queue escalation is audited, never silently dropped")
	})
}

func drillReplayApproval() webhook.ReplayApproval {
	return webhook.ReplayApproval{
		RequestedBy: "oncall-1",
		ApprovedBy:  "ops-lead-1",
		Reason:      "drill: transient fault drained; approved replay",
	}
}
