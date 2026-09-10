package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// PG-gated W4.5 J3 read endpoints: the anchor and reconcile wires over
// real rows, RBAC on project.read (viewer reads; anonymous 401), and
// the read-only boundary of this generation's surface.

const (
	jiraHTeamID    = "018f7e00-0000-7000-8000-00000000c101"
	jiraHProjectID = "018f7e00-0000-7000-8000-00000000c102"
	jiraHWorkItem  = "018f7e00-0000-7000-8000-00000000c103"
)

func TestJiraConnectorEndpoints(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_jira_handler_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_jira_handler_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_jira_handler_test WITH (FORCE)`)
		_ = admin.Close()
	})
	baseDSN := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := store.OpenPostgres(context.Background(),
		baseDSN[:strings.LastIndex(baseDSN, "/")+1]+"maestro_jira_handler_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'jira handler')`, jiraHTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'jira-h', 'JIRA-H', 'active')`, jiraHProjectID, jiraHTeamID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, '支付回调重试修复', 'executing')`,
		jiraHWorkItem, jiraHProjectID)
	require.NoError(t, err)

	jira := pg.Jira()
	anchor, err := jira.CreateJiraAnchor(ctx, jiraHProjectID, store.JiraAnchor{
		WorkItemID: jiraHWorkItem, IssueKey: "PEIX-701", JiraProjectKey: "PEIX",
		AnchorSource: store.JiraAnchorSourceAPI, Assignee: "zhang.san", IterationLabel: "Sprint-12",
	})
	require.NoError(t, err)
	require.NoError(t, jira.UpdateJiraSnapshot(ctx, jiraHProjectID, anchor.ID, store.JiraIssueSnapshot{
		IssueKey: "PEIX-701", Title: "Jira 侧标题", Assignee: "li.si",
		Labels: []string{"backend", "maestro:executing"}, Status: "In Progress",
		SnapshotAt: time.Now().UTC(),
	}))
	require.NoError(t, jira.RecordJiraMirrorOutcome(ctx, jiraHProjectID, anchor.ID, true, ""))
	item, _, err := jira.UpsertJiraDivergence(ctx, jiraHProjectID, anchor.ID, store.JiraFieldTitle,
		"支付回调重试修复", "Jira 侧标题")
	require.NoError(t, err)
	require.Equal(t, "open", item.State)

	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	idp := newHandlerTestIdP(t)
	resolver := &identity.StaticResolver{Memberships: map[string]map[string]string{
		"view-1": {jiraHProjectID: "viewer"},
	}}
	verifier, err := identity.NewTokenVerifier(idp.server.URL, "maestro", idp.server.Client())
	require.NoError(t, err)
	mw := NewOIDCMiddleware(policy, verifier, resolver)

	router := gin.New()
	router.Use(mw.Authenticate)
	RegisterControlPlane(router, ControlPlaneOptions{
		Identity: mw, Jira: NewJiraHandler(pg.Jira()), Scope: pg.Instances(),
	})
	viewerTK := idp.signedToken(t, "view-1")

	request := func(token, method, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Anchors: both sides of the mirror on one wire.
	rec := request(viewerTK, http.MethodGet, "/api/v3/projects/"+jiraHProjectID+"/jira-anchors")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var anchorsWire struct {
		ProjectID string `json:"project_id"`
		Anchors   []struct {
			IssueKey       string   `json:"issue_key"`
			SoRTitle       string   `json:"sor_title"`
			SoRStatus      string   `json:"sor_status"`
			SoRStatusLabel string   `json:"sor_status_label"`
			SoRAssignee    string   `json:"sor_assignee"`
			IssueTitle     string   `json:"issue_title"`
			IssueAssignee  string   `json:"issue_assignee"`
			IssueLabels    []string `json:"issue_labels"`
			IssueStatus    string   `json:"issue_status"`
			LastMirrorOK   bool     `json:"last_mirror_ok"`
		} `json:"anchors"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anchorsWire))
	require.Len(t, anchorsWire.Anchors, 1)
	wire := anchorsWire.Anchors[0]
	assert.Equal(t, "PEIX-701", wire.IssueKey)
	assert.Equal(t, "支付回调重试修复", wire.SoRTitle)
	assert.Equal(t, "executing", wire.SoRStatus)
	assert.Equal(t, "maestro:executing", wire.SoRStatusLabel)
	assert.Equal(t, "zhang.san", wire.SoRAssignee)
	assert.Equal(t, "Jira 侧标题", wire.IssueTitle)
	assert.Equal(t, "li.si", wire.IssueAssignee)
	assert.Equal(t, []string{"backend", "maestro:executing"}, wire.IssueLabels)
	assert.Equal(t, "In Progress", wire.IssueStatus)
	assert.True(t, wire.LastMirrorOK)

	// Reconcile list: the live divergence queue, then the resolved
	// history after adjudication.
	rec = request(viewerTK, http.MethodGet, "/api/v3/projects/"+jiraHProjectID+"/jira-reconcile-items")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var liveWire struct {
		Items []struct {
			ID          string `json:"id"`
			IssueKey    string `json:"issue_key"`
			Field       string `json:"field"`
			SoRValue    string `json:"sor_value"`
			MirrorValue string `json:"mirror_value"`
			State       string `json:"state"`
			OpenCycles  int    `json:"open_cycles"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &liveWire))
	require.Len(t, liveWire.Items, 1)
	assert.Equal(t, "title", liveWire.Items[0].Field)
	assert.Equal(t, "支付回调重试修复", liveWire.Items[0].SoRValue)
	assert.Equal(t, "Jira 侧标题", liveWire.Items[0].MirrorValue)
	assert.Equal(t, "open", liveWire.Items[0].State)
	assert.Equal(t, 1, liveWire.Items[0].OpenCycles)

	_, err = jira.ResolveJiraReconcileItem(ctx, jiraHProjectID, item.ID, "accept_sor", "u-admin", "以 Maestro 为准")
	require.NoError(t, err)
	rec = request(viewerTK, http.MethodGet, "/api/v3/projects/"+jiraHProjectID+"/jira-reconcile-items?state=resolved")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resolvedWire struct {
		Items []struct {
			State      string `json:"state"`
			Resolution string `json:"resolution"`
			ResolvedBy string `json:"resolved_by"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolvedWire))
	require.Len(t, resolvedWire.Items, 1)
	assert.Equal(t, "resolved", resolvedWire.Items[0].State)
	assert.Equal(t, "accept_sor", resolvedWire.Items[0].Resolution)
	assert.Equal(t, "u-admin", resolvedWire.Items[0].ResolvedBy)

	// Anonymous access fails closed; an unknown project scope hides
	// behind 404 (resource hiding stays indistinguishable from
	// absence).
	rec = request("", http.MethodGet, "/api/v3/projects/"+jiraHProjectID+"/jira-anchors")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = request(viewerTK, http.MethodGet, "/api/v3/projects/"+jiraHTeamID+"/jira-anchors")
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// The honest empty state.
	rec = request(viewerTK, http.MethodGet, "/api/v3/projects/"+jiraHProjectID+"/jira-reconcile-items")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &liveWire))
	assert.Empty(t, liveWire.Items, "the adjudicated project carries no live divergence")
}
