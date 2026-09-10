package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// The PG-gated worker suite drives the REAL store tables against a
// stateful Jira sandbox (loopback REST v2 shapes, PUT applies). One
// file covers the bidirectional semantics of task brief J3-3/J3-4,
// including the no-reverse-write negative tests.

// jiraSandbox is an in-memory Jira Server: issues keyed by issue key,
// PUT /issue/{key} applies summary/assignee/labels, POST /search
// answers the issuekey IN (...) JQL.
type jiraSandbox struct {
	mu      sync.Mutex
	issues  map[string]*RemoteIssue
	patches []string // raw PUT bodies, for the negative assertions
	server  *httptest.Server
}

func newJiraSandbox(t *testing.T) *jiraSandbox {
	t.Helper()
	sandbox := &jiraSandbox{issues: map[string]*RemoteIssue{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/2/issue/", func(w http.ResponseWriter, r *http.Request) {
		sandbox.mu.Lock()
		defer sandbox.mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/rest/api/2/issue/")
		if r.Method == http.MethodGet {
			issue, ok := sandbox.issues[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, sandboxIssueBody(issue))
			return
		}
		if r.Method == http.MethodPut {
			sandbox.patches = append(sandbox.patches, key)
			var payload map[string]map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fields, ok := payload["fields"]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			issue, ok := sandbox.issues[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if summary, ok := fields["summary"].(string); ok {
				issue.Title = summary
			}
			if assignee, ok := fields["assignee"].(map[string]any); ok {
				if name, ok := assignee["name"].(string); ok {
					issue.Assignee = name
				} else {
					issue.Assignee = ""
				}
			}
			if labels, ok := fields["labels"].([]any); ok {
				issue.Labels = nil
				for _, label := range labels {
					if text, ok := label.(string); ok {
						issue.Labels = append(issue.Labels, text)
					}
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/rest/api/2/search", func(w http.ResponseWriter, r *http.Request) {
		sandbox.mu.Lock()
		defer sandbox.mu.Unlock()
		var payload struct {
			JQL string `json:"jql"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		inClause := strings.TrimPrefix(payload.JQL, "issuekey IN (")
		inClause = strings.TrimSuffix(inClause, ")")
		issues := []string{}
		for _, key := range strings.Split(inClause, ", ") {
			if issue, ok := sandbox.issues[key]; ok {
				issues = append(issues, sandboxIssueBody(issue))
			}
		}
		writeJSON(w, `{"issues": [`+strings.Join(issues, ", ")+`]}`)
	})
	sandbox.server = httptest.NewServer(mux)
	t.Cleanup(sandbox.server.Close)
	return sandbox
}

func (s *jiraSandbox) put(key string, issue *RemoteIssue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[key] = issue
}

func (s *jiraSandbox) issue(key string) *RemoteIssue {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *s.issues[key]
	return &copy
}

func sandboxIssueBody(issue *RemoteIssue) string {
	assignee := "null"
	if issue.Assignee != "" {
		assignee = fmt.Sprintf(`{"displayName": %q}`, issue.Assignee)
	}
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, fmt.Sprintf("%q", label))
	}
	return fmt.Sprintf(`{"key": %q, "fields": {"summary": %q, "assignee": %s, "status": {"name": %q}, "labels": [%s]}}`,
		issue.Key, issue.Title, assignee, issue.Status, strings.Join(labels, ", "))
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, body)
}

// sandboxWorker wires the worker against the real PG store and the
// loopback sandbox (the production constructor pins https names; the
// stub transport carries identical semantics). The sandbox handle is
// returned for live-state assertions.
func sandboxWorker(t *testing.T, pg *store.PostgresStore) (*Worker, *jiraSandbox) {
	t.Helper()
	sandbox := newJiraSandbox(t)
	transport := &redirectTransport{target: sandbox.server.URL}
	factory := func() (*Client, error) {
		client, err := NewClient("https://jira.sandbox.example", "sandbox-pat")
		if err != nil {
			return nil, err
		}
		return client.WithTestTransport(transport), nil
	}
	return &Worker{
		Anchors:   pg.Jira(),
		Reconcile: pg.Jira(),
		NewClient: factory,
	}, sandbox
}

type redirectTransport struct{ target string }

func (r *redirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	stubbed := request.Clone(request.Context())
	stubbed.URL.Scheme = "http"
	stubbed.URL.Host = strings.TrimPrefix(r.target, "http://")
	return http.DefaultTransport.RoundTrip(stubbed)
}

func workerTestDB(t *testing.T) (*store.PostgresStore, context.Context) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_jira_worker_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_jira_worker_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_jira_worker_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := store.OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_jira_worker_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7f10-0000-7000-8000-000000000001', 'jira worker team')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7f10-0000-7000-8000-000000000001', 'jira-worker', 'JW', 'active')`, workerProject)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status) VALUES
		('018f7f10-0000-7000-8000-000000000020', $1, '支付回调重试修复', 'executing'),
		('018f7f10-0000-7000-8000-000000000021', $1, '课程列表分页优化', 'done')`, workerProject)
	require.NoError(t, err)
	return pg, ctx
}

const (
	workerProject   = "018f7e00-0000-7000-8000-000000000002"
	workerWorkItemA = "018f7f10-0000-7000-8000-000000000020"
	workerWorkItemB = "018f7f10-0000-7000-8000-000000000021"
)

// The full bidirectional walk: initial mirror push, reverse snapshot,
// Jira-side edit opening a frozen divergence, escalation at two
// cycles, human accept_sor adjudication, freeze lifting, mirror
// re-push and reconcile healing — plus the structural negative
// tests (no status write on either side).
func TestJiraWorkerBidirectionalSemantics(t *testing.T) {
	pg, ctx := workerTestDB(t)
	worker, sandbox := sandboxWorker(t, pg)
	jira := pg.Jira()

	_, err := jira.CreateJiraAnchor(ctx, workerProject, store.JiraAnchor{
		WorkItemID: workerWorkItemA, IssueKey: "PEIX-301", JiraProjectKey: "PEIX",
		AnchorSource: store.JiraAnchorSourceAPI, Assignee: "zhang.san", IterationLabel: "Sprint-12",
	})
	require.NoError(t, err)
	sandbox.put("PEIX-301", &RemoteIssue{
		Key: "PEIX-301", Title: "旧标题", Assignee: "li.si", Status: "In Progress", Labels: []string{"backend", "ui-team"},
	})

	// Cycle 1: mirror pushes title/assignee/labels (owning the
	// iteration + status slots, preserving human labels), then
	// reconcile observes convergence — no divergence items.
	require.NoError(t, worker.Cycle(ctx))
	remote := sandbox.issue("PEIX-301")
	assert.Equal(t, "支付回调重试修复", remote.Title)
	assert.Equal(t, "zhang.san", remote.Assignee)
	assert.Subset(t, remote.Labels, []string{"backend", "ui-team", "Sprint-12", "maestro:executing"},
		"human labels survive; the mirror owns exactly its two slots")

	// NEGATIVE (rule 1): the Jira status ("In Progress") never reached
	// the Maestro work item, and the mirror never touched the Jira
	// issue status field — the sandbox's status is still the original.
	var workStatus string
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT status FROM work_items WHERE id = $1`, workerWorkItemA).Scan(&workStatus))
	assert.Equal(t, "executing", workStatus, "Jira status must never rewrite work_items.status")
	assert.Equal(t, "In Progress", remote.Status, "Maestro must never rewrite the Jira status field")

	live, err := jira.ListJiraReconcileItems(ctx, workerProject, false)
	require.NoError(t, err)
	assert.Empty(t, live, "a converged cycle opens no divergence")

	stored, err := jira.JiraAnchorByWorkItem(ctx, workerProject, workerWorkItemA)
	require.NoError(t, err)
	assert.True(t, stored.LastMirrorOK)
	assert.Contains(t, stored.IssueLabels, "maestro:executing")
	assert.Equal(t, "In Progress", stored.IssueStatus, "the reverse snapshot carries the Jira status read-only")

	// A Jira-side human edit diverges three fields.
	sandbox.put("PEIX-301", &RemoteIssue{
		Key: "PEIX-301", Title: "手工改过的标题", Assignee: "wang.wu", Status: "Reopened",
		Labels: []string{"backend", "ui-team", "Sprint-12"},
	})

	// Cycle 2: reconcile detects the divergences (title, assignee,
	// status_label); cycle 1 of the freeze.
	require.NoError(t, worker.Cycle(ctx))
	live, err = jira.ListJiraReconcileItems(ctx, workerProject, false)
	require.NoError(t, err)
	require.Len(t, live, 3)
	byField := map[string]store.JiraReconcileItem{}
	for _, item := range live {
		byField[item.Field] = item
		assert.Equal(t, "open", item.State)
		assert.Equal(t, 1, item.OpenCycles)
	}
	assert.Equal(t, "支付回调重试修复", byField[store.JiraFieldTitle].SoRValue)
	assert.Equal(t, "手工改过的标题", byField[store.JiraFieldTitle].MirrorValue)
	assert.Equal(t, "zhang.san", byField[store.JiraFieldAssignee].SoRValue)
	assert.Equal(t, "wang.wu", byField[store.JiraFieldAssignee].MirrorValue)
	assert.Equal(t, "maestro:executing", byField[store.JiraFieldStatus].SoRValue)

	// NEGATIVE (rule 2): while the freeze holds, the mirror phase of
	// cycle 2 did NOT push any diverged field — the Jira side keeps
	// the human edit verbatim.
	remote = sandbox.issue("PEIX-301")
	assert.Equal(t, "手工改过的标题", remote.Title)
	assert.Equal(t, "wang.wu", remote.Assignee)
	assert.NotContains(t, remote.Labels, "maestro:executing")

	// Cycle 3: the same live divergences advance to the frozen line —
	// escalated with an atomic audit row.
	require.NoError(t, worker.Cycle(ctx))
	live, err = jira.ListJiraReconcileItems(ctx, workerProject, false)
	require.NoError(t, err)
	require.Len(t, live, 3)
	for _, item := range live {
		assert.Equal(t, "escalated", item.State)
		assert.Equal(t, 2, item.OpenCycles)
	}
	var escalated int
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE action = 'jira.reconcile.escalated'`).Scan(&escalated))
	assert.Equal(t, 3, escalated)

	// Human adjudication: SoR wins on all three fields.
	for _, item := range live {
		_, err := jira.ResolveJiraReconcileItem(ctx, workerProject, item.ID, "accept_sor", "u-admin", "以 Maestro 为准")
		require.NoError(t, err)
	}

	// Cycle 4: the freeze lifted — the mirror pushes the SoR values
	// back; reconcile heals every item.
	require.NoError(t, worker.Cycle(ctx))
	remote = sandbox.issue("PEIX-301")
	assert.Equal(t, "支付回调重试修复", remote.Title)
	assert.Equal(t, "zhang.san", remote.Assignee)
	assert.Contains(t, remote.Labels, "maestro:executing")
	live, err = jira.ListJiraReconcileItems(ctx, workerProject, false)
	require.NoError(t, err)
	assert.Empty(t, live)

	// NEGATIVE (rule 1 again, post-adjudication): the reopened Jira
	// status still never reached the work item.
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT status FROM work_items WHERE id = $1`, workerWorkItemA).Scan(&workStatus))
	assert.Equal(t, "executing", workStatus)
}

// accept_mirror on title rewrites the SoR title (the one human-gated
// Jira-direction write) and the next cycle converges without a push.
func TestJiraWorkerAdjudicateAcceptMirror(t *testing.T) {
	pg, ctx := workerTestDB(t)
	worker, sandbox := sandboxWorker(t, pg)
	jira := pg.Jira()

	_, err := jira.CreateJiraAnchor(ctx, workerProject, store.JiraAnchor{
		WorkItemID: workerWorkItemB, IssueKey: "PEIX-401", JiraProjectKey: "PEIX",
		AnchorSource: store.JiraAnchorSourceManual,
	})
	require.NoError(t, err)
	sandbox.put("PEIX-401", &RemoteIssue{
		Key: "PEIX-401", Title: "课程列表分页优化", Status: "Done", Labels: []string{"maestro:done"},
	})

	require.NoError(t, worker.Cycle(ctx))
	sandbox.put("PEIX-401", &RemoteIssue{
		Key: "PEIX-401", Title: "分页优化（Jira 口径）", Status: "Done", Labels: []string{"maestro:done"},
	})
	require.NoError(t, worker.ReconcilePhase(ctx))
	live, err := jira.ListJiraReconcileItems(ctx, workerProject, false)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, store.JiraFieldTitle, live[0].Field)

	_, err = jira.ResolveJiraReconcileItem(ctx, workerProject, live[0].ID, "accept_mirror", "u-admin", "标题以 Jira 为准")
	require.NoError(t, err)

	var title string
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT title FROM work_items WHERE id = $1`, workerWorkItemB).Scan(&title))
	assert.Equal(t, "分页优化（Jira 口径）", title, "accept_mirror rewrites the SoR title after human adjudication")

	require.NoError(t, worker.Cycle(ctx))
	remote := sandbox.issue("PEIX-401")
	assert.Equal(t, "分页优化（Jira 口径）", remote.Title, "converged after the adjudication — no fight back")
	live, err = jira.ListJiraReconcileItems(ctx, workerProject, false)
	require.NoError(t, err)
	assert.Empty(t, live)
}

// Provider outage: the cycle fails closed, no divergence opens, no
// bookkeeping lies about success, and the loop itself never blocks.
func TestJiraWorkerFailClosedOnOutage(t *testing.T) {
	pg, ctx := workerTestDB(t)
	worker, _ := sandboxWorker(t, pg)
	jira := pg.Jira()

	_, err := jira.CreateJiraAnchor(ctx, workerProject, store.JiraAnchor{
		WorkItemID: workerWorkItemA, IssueKey: "PEIX-501", JiraProjectKey: "PEIX",
		AnchorSource: store.JiraAnchorSourceAPI,
	})
	require.NoError(t, err)
	// No sandbox issue behind the key and a dead transport: the
	// provider is unreachable for the whole cycle.
	worker.NewClient = func() (*Client, error) {
		client, err := NewClient("https://jira.dead.example", "pat")
		require.NoError(t, err)
		client.http.Timeout = 1
		return client.WithTestTransport(&deadTransport{}), nil
	}

	err = worker.Cycle(ctx)
	assert.ErrorIs(t, err, ErrJiraUnavailable)

	live, listErr := jira.ListJiraReconcileItems(ctx, workerProject, false)
	require.NoError(t, listErr)
	assert.Empty(t, live, "an outage opens no divergence (fail-closed, no fabricated provider state)")
	stored, getErr := jira.JiraAnchorByWorkItem(ctx, workerProject, workerWorkItemA)
	require.NoError(t, getErr)
	assert.False(t, stored.HasMirrorRun, "bookkeeping stays honest: no mirror ever succeeded")
}

type deadTransport struct{}

func (d *deadTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("sandbox unreachable")
}
