package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated W4.5 J3 storage: anchor uniqueness, the reverse snapshot
// channel (never reaching work_items), the divergence freeze with its
// two-cycle escalation, and the two adjudication directions.

// jiraTestDB provisions a scratch database with migrations applied and
// one team/project/work-item fixture.
func jiraTestDB(t *testing.T) (*PostgresStore, context.Context) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_jira_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_jira_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_jira_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_jira_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7f00-0000-7000-8000-000000000001', 'jira team')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7f00-0000-7000-8000-000000000001', 'jira', 'JIRA', 'active')`, m3Project)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status) VALUES
		('018f7f00-0000-7000-8000-000000000010', $1, '支付回调重试修复', 'executing'),
		('018f7f00-0000-7000-8000-000000000011', $1, '课程列表分页优化', 'queued')`, m3Project)
	require.NoError(t, err)
	return pg, ctx
}

const (
	jiraWorkItemA = "018f7f00-0000-7000-8000-000000000010"
	jiraWorkItemB = "018f7f00-0000-7000-8000-000000000011"
)

func TestJiraAnchorStorage(t *testing.T) {
	pg, ctx := jiraTestDB(t)
	jira := pg.Jira()

	// Anchor creation with an invalid source fails closed.
	_, err := jira.CreateJiraAnchor(ctx, m3Project, JiraAnchor{
		WorkItemID: jiraWorkItemA, IssueKey: "PEIX-1", JiraProjectKey: "PEIX",
		AnchorSource: "telepathy",
	})
	assert.ErrorIs(t, err, ErrJiraAnchorInvalid)

	created, err := jira.CreateJiraAnchor(ctx, m3Project, JiraAnchor{
		WorkItemID: jiraWorkItemA, IssueKey: "PEIX-101", JiraProjectKey: "PEIX",
		AnchorSource: JiraAnchorSourceManual, Assignee: "zhang.san", IterationLabel: "Sprint-12",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.Empty(t, created.SnapshotAt, "a new anchor carries no snapshot")

	// One anchor per work item; one issue key per project.
	_, err = jira.CreateJiraAnchor(ctx, m3Project, JiraAnchor{
		WorkItemID: jiraWorkItemA, IssueKey: "PEIX-102", JiraProjectKey: "PEIX", AnchorSource: JiraAnchorSourceAPI,
	})
	assert.ErrorIs(t, err, ErrJiraAnchorExists)
	_, err = jira.CreateJiraAnchor(ctx, m3Project, JiraAnchor{
		WorkItemID: jiraWorkItemB, IssueKey: "PEIX-101", JiraProjectKey: "PEIX", AnchorSource: JiraAnchorSourceAPI,
	})
	assert.ErrorIs(t, err, ErrJiraAnchorIssueTaken)

	// An unknown work item violates the FK shape.
	_, err = jira.CreateJiraAnchor(ctx, m3Project, JiraAnchor{
		WorkItemID: "018f7f00-0000-7000-8000-00000000dead", IssueKey: "PEIX-103",
		JiraProjectKey: "PEIX", AnchorSource: JiraAnchorSourceManual,
	})
	assert.ErrorIs(t, err, ErrJiraAnchorInvalid)

	// Malformed issue keys fail the CHECK shape.
	_, err = jira.CreateJiraAnchor(ctx, m3Project, JiraAnchor{
		WorkItemID: jiraWorkItemB, IssueKey: "peix-9", JiraProjectKey: "PEIX", AnchorSource: JiraAnchorSourceManual,
	})
	assert.ErrorIs(t, err, ErrJiraAnchorInvalid)

	// The reverse channel updates the snapshot columns only.
	snapAt := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, jira.UpdateJiraSnapshot(ctx, m3Project, created.ID, JiraIssueSnapshot{
		IssueKey: "PEIX-101", Title: "支付回调重试修复（Jira 侧标题）", Assignee: "zhang.san",
		Labels: []string{"maestro:executing", "Sprint-12"}, Status: "In Progress", SnapshotAt: snapAt,
	}))
	var workItemStatus string
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT status FROM work_items WHERE id = $1`, jiraWorkItemA).Scan(&workItemStatus))
	assert.Equal(t, "executing", workItemStatus, "the Jira snapshot must never touch work_items.status")

	views, err := jira.ListJiraAnchors(ctx, m3Project)
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, "PEIX-101", views[0].Anchor.IssueKey)
	assert.Equal(t, "支付回调重试修复", views[0].WorkItemTitle)
	assert.Equal(t, "executing", views[0].WorkItemStatus)
	assert.Equal(t, []string{"maestro:executing", "Sprint-12"}, views[0].Anchor.IssueLabels)
	assert.Equal(t, "In Progress", views[0].Anchor.IssueStatus)
	assert.Equal(t, snapAt.Format(time.RFC3339Nano), views[0].Anchor.SnapshotAt)

	// Mirror bookkeeping round-trips, error text included then cleared.
	require.NoError(t, jira.RecordJiraMirrorOutcome(ctx, m3Project, created.ID, false, "connection refused"))
	stored, err := jira.JiraAnchorByWorkItem(ctx, m3Project, jiraWorkItemA)
	require.NoError(t, err)
	assert.True(t, stored.HasMirrorRun)
	assert.False(t, stored.LastMirrorOK)
	assert.Equal(t, "connection refused", stored.LastError)
	require.NoError(t, jira.RecordJiraMirrorOutcome(ctx, m3Project, created.ID, true, ""))
	stored, err = jira.JiraAnchorByWorkItem(ctx, m3Project, jiraWorkItemA)
	require.NoError(t, err)
	assert.True(t, stored.LastMirrorOK)
	assert.Empty(t, stored.LastError)

	assert.ErrorIs(t, jira.UpdateJiraSnapshot(ctx, m3Project, pgNewUUID(), JiraIssueSnapshot{SnapshotAt: snapAt}), ErrJiraAnchorNotFound)
}

func TestJiraReconcileDivergenceLifecycle(t *testing.T) {
	pg, ctx := jiraTestDB(t)
	jira := pg.Jira()

	anchor, err := jira.CreateJiraAnchor(ctx, m3Project, JiraAnchor{
		WorkItemID: jiraWorkItemA, IssueKey: "PEIX-201", JiraProjectKey: "PEIX",
		AnchorSource: JiraAnchorSourceManual, Assignee: "zhang.san",
	})
	require.NoError(t, err)
	require.NoError(t, jira.UpdateJiraSnapshot(ctx, m3Project, anchor.ID, JiraIssueSnapshot{
		IssueKey: "PEIX-201", Title: "另一个标题", Assignee: "li.si", Status: "Open",
		SnapshotAt: time.Now().UTC(),
	}))

	escalatedAudit := func() int {
		var count int
		require.NoError(t, pg.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM audit_events WHERE action = 'jira.reconcile.escalated'`).Scan(&count))
		return count
	}

	// Cycle 1: first detection opens the item.
	item, escalated, err := jira.UpsertJiraDivergence(ctx, m3Project, anchor.ID, JiraFieldTitle,
		"支付回调重试修复", "另一个标题")
	require.NoError(t, err)
	assert.False(t, escalated)
	assert.Equal(t, "open", item.State)
	assert.Equal(t, 1, item.OpenCycles)
	assert.Equal(t, "支付回调重试修复", item.SoRValue)
	assert.Equal(t, "另一个标题", item.MirrorValue)

	frozen, err := jira.JiraFrozenFields(ctx, m3Project, anchor.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{JiraFieldTitle}, frozen, "a live divergence freezes its field")

	// Cycle 2: still divergent — the frozen line escalates atomically.
	item, escalated, err = jira.UpsertJiraDivergence(ctx, m3Project, anchor.ID, JiraFieldTitle,
		"支付回调重试修复", "另一个标题")
	require.NoError(t, err)
	assert.True(t, escalated)
	assert.Equal(t, "escalated", item.State)
	assert.Equal(t, 2, item.OpenCycles)
	assert.Equal(t, 1, escalatedAudit())

	// Cycle 3+: already escalated — no duplicate escalation audit.
	_, escalated, err = jira.UpsertJiraDivergence(ctx, m3Project, anchor.ID, JiraFieldTitle,
		"支付回调重试修复", "另一个标题")
	require.NoError(t, err)
	assert.False(t, escalated, "an escalated item does not re-escalate")
	assert.Equal(t, 1, escalatedAudit())

	// Adjudicating an escalated item with accept_mirror rewrites the
	// SoR title (the only Jira-direction write, human-triggered).
	resolved, err := jira.ResolveJiraReconcileItem(ctx, m3Project, item.ID, "accept_mirror", "u-admin", "Jira 侧标题是最新口径")
	require.NoError(t, err)
	assert.Equal(t, "resolved", resolved.State)
	assert.Equal(t, "accept_mirror", resolved.Resolution)
	assert.Equal(t, "u-admin", resolved.ResolvedBy)
	var title string
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT title FROM work_items WHERE id = $1`, jiraWorkItemA).Scan(&title))
	assert.Equal(t, "另一个标题", title)
	var resolutionAudits int
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE action = 'jira.reconcile.resolved' AND actor_principal = 'u-admin'`).Scan(&resolutionAudits))
	assert.Equal(t, 1, resolutionAudits)

	// Resolved items are history; the freeze lifts; a fresh detection
	// opens a new item.
	frozen, err = jira.JiraFrozenFields(ctx, m3Project, anchor.ID)
	require.NoError(t, err)
	assert.Empty(t, frozen)
	item2, escalated, err := jira.UpsertJiraDivergence(ctx, m3Project, anchor.ID, JiraFieldAssignee,
		"zhang.san", "li.si")
	require.NoError(t, err)
	assert.False(t, escalated)
	assert.NotEqual(t, item.ID, item2.ID)

	// Double adjudication fails closed.
	_, err = jira.ResolveJiraReconcileItem(ctx, m3Project, item.ID, "accept_sor", "u-admin", "")
	assert.ErrorIs(t, err, ErrJiraReconcileNotAdjudicable)
	_, err = jira.ResolveJiraReconcileItem(ctx, m3Project, item2.ID, "accept_compromise", "u-admin", "")
	assert.ErrorIs(t, err, ErrJiraReconcileNotAdjudicable)
	_, err = jira.ResolveJiraReconcileItem(ctx, m3Project, pgNewUUID(), "accept_sor", "u-admin", "")
	assert.ErrorIs(t, err, ErrJiraReconcileNotFound)

	// accept_mirror on assignee rewrites the anchor's SoR field;
	// projection fields (iteration/status_label) reject accept_mirror.
	_, err = jira.ResolveJiraReconcileItem(ctx, m3Project, item2.ID, "accept_mirror", "u-admin", "指派以 Jira 为准")
	require.NoError(t, err)
	after, err := jira.JiraAnchorByWorkItem(ctx, m3Project, jiraWorkItemA)
	require.NoError(t, err)
	assert.Equal(t, "li.si", after.Assignee)

	// accept_sor resolves without rewriting anything; projection
	// fields (iteration_label, status_label) take accept_sor only.
	item3, _, err := jira.UpsertJiraDivergence(ctx, m3Project, anchor.ID, JiraFieldIteration, "Sprint-12", "Sprint-11")
	require.NoError(t, err)
	_, err = jira.ResolveJiraReconcileItem(ctx, m3Project, item3.ID, "accept_mirror", "u-admin", "")
	assert.ErrorIs(t, err, ErrJiraReconcileNotAdjudicable, "iteration_label is a projection field")
	_, err = jira.ResolveJiraReconcileItem(ctx, m3Project, item3.ID, "accept_sor", "u-admin", "迭代以 Maestro 为准")
	require.NoError(t, err)
	item4, _, err := jira.UpsertJiraDivergence(ctx, m3Project, anchor.ID, JiraFieldStatus, "maestro:executing", "")
	require.NoError(t, err)
	require.Equal(t, "open", item4.State)
	_, err = jira.ResolveJiraReconcileItem(ctx, m3Project, item4.ID, "accept_mirror", "u-admin", "")
	assert.ErrorIs(t, err, ErrJiraReconcileNotAdjudicable, "status_label is a projection field")
	require.NoError(t, jira.HealJiraDivergence(ctx, m3Project, anchor.ID, JiraFieldStatus))
	var healed int
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE action = 'jira.reconcile.healed'`).Scan(&healed))
	assert.Equal(t, 1, healed)

	live, err := jira.ListJiraReconcileItems(ctx, m3Project, false)
	require.NoError(t, err)
	assert.Empty(t, live, "every item above ended resolved")
	history, err := jira.ListJiraReconcileItems(ctx, m3Project, true)
	require.NoError(t, err)
	assert.Len(t, history, 4)
}
