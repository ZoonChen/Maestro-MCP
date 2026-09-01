package gitlab_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated reconciliation tests: the reconciler pulls the provider (a
// stub API v4 server), applies the fact through the shared truth path,
// and drives the done edge with reconcile-lineage facts.

type reconcileFixture struct {
	server *httptest.Server
	db     *sql.DB
	pg     *store.PostgresStore
}

const (
	rcProject  = "018f7800-0000-7000-8000-000000000002"
	rcWorkItem = "018f7800-0000-7000-8000-000000000003"
)

func newReconcileFixture(t *testing.T) *reconcileFixture {
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
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7800-0000-7000-8000-000000000004', 'rc team')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7800-0000-7000-8000-000000000004', 'rc-proj', 'RC', 'active')`, rcProject)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, version)
		VALUES ($1, $2, 'rc task', 'ready_for_human_merge', 3)`, rcWorkItem, rcProject)
	require.NoError(t, err)

	// The stub provider serves API v4 shapes.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/projects/9001/merge_requests/12" {
			fmt.Fprintf(w, `{"iid": 12, "state": "merged",
				"source_branch": "maestro/rc/%s", "target_branch": "main",
				"sha": "%s", "merge_commit_sha": "%s", "merged_at": "2026-08-31T15:00:00Z",
				"diff_refs": {"base_sha": "%s", "head_sha": "%s"}}`,
				rcWorkItem, strings.Repeat("a", 40), strings.Repeat("c", 40),
				strings.Repeat("b", 40), strings.Repeat("a", 40))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	// Register the stub host as an approved instance (the egress rules
	// demand HTTPS names; the stub is http, so the reconciler's client
	// constructor is stubbed with the same contract semantics).
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref)
		VALUES ('018f7800-0000-7000-8000-000000000001', 'https://gitlab.rc.example', 'rc', 'env:MAESTRO_RC_BOT', 'env:MAESTRO_RC_HOOK')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO gitlab_project_mappings (gitlab_instance_id, gitlab_project_id, project_id, default_branch)
		VALUES ('018f7800-0000-7000-8000-000000000001', 9001, $1, 'main')`, rcProject)
	require.NoError(t, err)
	t.Setenv("MAESTRO_RC_BOT", "rc-bot-token")

	return &reconcileFixture{server: server, db: db, pg: pg}
}

func (f *reconcileFixture) reconciler(t *testing.T) *gitlab.Reconciler {
	t.Helper()
	// The stub speaks http on loopback; the production constructor pins
	// https hosts, so the test injects an http-pinned equivalent with
	// identical redirect/TLS/timeout semantics.
	transport := stubTransport{server: f.server}
	return &gitlab.Reconciler{
		Mapping: f.pg.Instances(),
		Secrets: stubSecretResolver{},
		Syncer:  &gitlab.Syncer{Store: f.pg.GitLab()},
		NewClient: func(baseURL, token string) (*gitlab.Client, error) {
			client, err := gitlab.NewClient("https://gitlab.rc.example", token)
			if err != nil {
				return nil, err
			}
			return client.WithTestTransport(transport), nil
		},
	}
}

type stubTransport struct{ server *httptest.Server }

func (s stubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	stubbed := request.Clone(request.Context())
	stubbed.URL.Scheme = "http"
	stubbed.URL.Host = strings.TrimPrefix(s.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(stubbed)
}

type stubSecretResolver struct{}

func (stubSecretResolver) Resolve(_ context.Context, ref string) (string, error) {
	if ref != "env:MAESTRO_RC_BOT" {
		return "", fmt.Errorf("unknown ref %s", ref)
	}
	return "rc-bot-token", nil
}

func TestReconcileDrivesDone(t *testing.T) {
	f := newReconcileFixture(t)
	reconciler := f.reconciler(t)

	outcome, err := reconciler.ReconcileMergeRequest(context.Background(), rcProject, 12)
	require.NoError(t, err)
	assert.Equal(t, "merged", outcome.RemoteState)
	assert.True(t, outcome.Transitioned, "the provider-pulled merged fact drives done")

	var status, factID string
	require.NoError(t, f.db.QueryRow(`
		SELECT status, merged_fact_id FROM work_items WHERE id = $1`, rcWorkItem).Scan(&status, &factID))
	assert.Equal(t, "done", status)
	assert.Equal(t, "gitlab:reconcile:mr:12", factID, "the lineage names its source kind")

	// Idempotent re-reconcile.
	outcome, err = reconciler.ReconcileMergeRequest(context.Background(), rcProject, 12)
	require.NoError(t, err)
	assert.False(t, outcome.Transitioned)
}

func TestReconcileUnknownMRAndOutage(t *testing.T) {
	f := newReconcileFixture(t)
	reconciler := f.reconciler(t)

	_, err := reconciler.ReconcileMergeRequest(context.Background(), rcProject, 999)
	require.Error(t, err, "unknown MRs are surfaced, not fabricated")

	outage := &gitlab.Reconciler{
		Mapping: f.pg.Instances(),
		Secrets: stubSecretResolver{},
		Syncer:  &gitlab.Syncer{Store: f.pg.GitLab()},
		NewClient: func(baseURL, token string) (*gitlab.Client, error) {
			client, clientErr := gitlab.NewClient(baseURL, token)
			if clientErr != nil {
				return nil, clientErr
			}
			return client.WithTestTransport(deadTransport{}), nil
		},
	}
	_, err = outage.ReconcileMergeRequest(context.Background(), rcProject, 12)
	require.Error(t, err, "provider unavailability propagates; the cached model stays")

	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM work_items WHERE id = $1`, rcWorkItem).Scan(&status))
	assert.Equal(t, "ready_for_human_merge", status, "an outage never regresses or invents state")
}

type deadTransport struct{}

func (deadTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("connection refused")
}

func TestReconcileUnmappedProject(t *testing.T) {
	f := newReconcileFixture(t)
	_, err := f.reconciler(t).ReconcileMergeRequest(context.Background(),
		"018f7800-0000-7000-8000-00000000dead", 12)
	require.ErrorIs(t, err, gitlab.ErrUnmappedProject)
}
