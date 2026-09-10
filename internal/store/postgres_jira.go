package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL persistence for the W4.5 J3 Jira connector over the
// migration 0018 tables. The sync semantics are frozen by
// SOLUTION-BLUEPRINT section 1.2 and enforced HERE at the storage
// boundary, not just in the workers:
//
//   - the Jira-side snapshot columns are written only by
//     UpdateJiraSnapshot; nothing in this file ever touches
//     work_items.status from Jira data (rule 1);
//   - a live reconcile item is the divergence freeze: while one is
//     open or escalated, the mirror worker must not push that field
//     (rule 2 — the mirror update happens only after human
//     adjudication);
//   - adjudication is a guarded transaction that resolves the item
//     and applies the chosen value in the same commit (accept_sor
//     lifts the freeze so the next mirror push carries the SoR value;
//     accept_mirror rewrites the SoR-side field — a Jira-direction
//     write that only a human may trigger).

// Jira anchor/reconcile sentinels.
var (
	ErrJiraAnchorNotFound          = errors.New("jira anchor not found")
	ErrJiraAnchorExists            = errors.New("work item already anchored")
	ErrJiraAnchorIssueTaken        = errors.New("issue key already anchored in project")
	ErrJiraAnchorInvalid           = errors.New("jira anchor fields are invalid")
	ErrJiraReconcileNotFound       = errors.New("jira reconcile item not found")
	ErrJiraReconcileNotAdjudicable = errors.New("jira reconcile item is not adjudicable")
)

// Jira anchor sources (the migration 0018 enum).
const (
	JiraAnchorSourceManual = "manual"
	JiraAnchorSourceAPI    = "api"
)

// Reconcile item fields (the migration 0018 enum).
const (
	JiraFieldTitle     = "title"
	JiraFieldAssignee  = "assignee"
	JiraFieldIteration = "iteration_label"
	JiraFieldStatus    = "status_label"
)

// JiraReconcileEscalateAfterCycles is the frozen escalation line: a
// divergence still live after two reconcile cycles escalates to the
// owner (SOLUTION-BLUEPRINT section 1.2 rule 2, "连续两个周期").
const JiraReconcileEscalateAfterCycles = 2

// jiraSystemActor is the audit actor for worker-side reconcile events
// (escalations and auto-healed resolutions); adjudication rows carry
// the human principal instead.
const jiraSystemActor = "system:jira-reconciler"

type pgJiraStore struct{ db *sql.DB }

// Jira returns the Jira connector store.
func (s *PostgresStore) Jira() pgJiraStore { return pgJiraStore{db: s.DB()} }

// JiraAnchor is one stored WorkItem <-> issue anchor (the migration
// 0018 row). SoR-side desired values (assignee/iteration) live here;
// title and the maestro:<status> label derive from work_items at
// mirror time and are not duplicated.
type JiraAnchor struct {
	ID             string
	ProjectID      string
	WorkItemID     string
	IssueKey       string
	JiraProjectKey string
	AnchorSource   string
	Assignee       string
	IterationLabel string
	IssueTitle     string
	IssueAssignee  string
	IssueLabels    []string
	IssueStatus    string
	SnapshotAt     string // RFC3339, empty before the first snapshot
	LastMirrorAt   string
	LastMirrorOK   bool
	HasMirrorRun   bool
	LastError      string
	// What Maestro last established on the Jira side, per mirror
	// field; nil = not established (or re-armed by an accept_sor
	// adjudication) — the mirror pushes.
	PushedTitle       *string
	PushedAssignee    *string
	PushedIteration   *string
	PushedStatusLabel *string
	CreatedAt         string
	UpdatedAt         string
}

const jiraAnchorColumns = `a.id::text, a.project_id::text, a.work_item_id::text, a.issue_key,
	a.jira_project_key, a.anchor_source, a.assignee, a.iteration_label,
	a.issue_title, a.issue_assignee, a.issue_labels, a.issue_status, a.snapshot_at,
	a.pushed_title, a.pushed_assignee, a.pushed_iteration, a.pushed_status_label,
	a.last_mirror_at, a.last_mirror_ok, a.last_error, a.created_at, a.updated_at`

// jiraAnchorAux holds the scan targets that need decoding after the
// Scan call (jsonb, nullable timestamps); one instance serves one row.
type jiraAnchorAux struct {
	labels                                                     []byte
	snapshotAt                                                 sql.NullTime
	pushedTitle, pushedAssignee, pushedIteration, pushedStatus sql.NullString
	lastMirrorAt                                               sql.NullTime
	lastMirrorOK                                               sql.NullBool
	lastError                                                  sql.NullString
	createdAt                                                  time.Time
	updatedAt                                                  time.Time
}

// dest returns the Scan destination slice for the jiraAnchorColumns
// prefix; callers may append trailing columns (the work-item join).
func (aux *jiraAnchorAux) dest(anchor *JiraAnchor) []any {
	return []any{
		&anchor.ID, &anchor.ProjectID, &anchor.WorkItemID, &anchor.IssueKey,
		&anchor.JiraProjectKey, &anchor.AnchorSource, &anchor.Assignee, &anchor.IterationLabel,
		&anchor.IssueTitle, &anchor.IssueAssignee, &aux.labels, &anchor.IssueStatus, &aux.snapshotAt,
		&aux.pushedTitle, &aux.pushedAssignee, &aux.pushedIteration, &aux.pushedStatus,
		&aux.lastMirrorAt, &aux.lastMirrorOK, &aux.lastError, &aux.createdAt, &aux.updatedAt,
	}
}

// apply decodes the auxiliary targets into the anchor value.
func (aux *jiraAnchorAux) apply(anchor *JiraAnchor) error {
	if err := json.Unmarshal(aux.labels, &anchor.IssueLabels); err != nil {
		return fmt.Errorf("jira store: issue labels are not valid json: %w", err)
	}
	if aux.snapshotAt.Valid {
		anchor.SnapshotAt = aux.snapshotAt.Time.UTC().Format(time.RFC3339Nano)
	}
	anchor.PushedTitle = pushedStringPtr(aux.pushedTitle)
	anchor.PushedAssignee = pushedStringPtr(aux.pushedAssignee)
	anchor.PushedIteration = pushedStringPtr(aux.pushedIteration)
	anchor.PushedStatusLabel = pushedStringPtr(aux.pushedStatus)
	if aux.lastMirrorAt.Valid {
		anchor.LastMirrorAt = aux.lastMirrorAt.Time.UTC().Format(time.RFC3339Nano)
		anchor.HasMirrorRun = true
		anchor.LastMirrorOK = aux.lastMirrorOK.Bool
	}
	if aux.lastError.Valid {
		anchor.LastError = aux.lastError.String
	}
	anchor.CreatedAt = aux.createdAt.UTC().Format(time.RFC3339Nano)
	anchor.UpdatedAt = aux.updatedAt.UTC().Format(time.RFC3339Nano)
	return nil
}

// pushedStringPtr decodes one pushed_* column; unlike the importer's
// nullStringPtr it preserves empty strings — "Maestro established
// unassigned" is a real pushed value, not "not established".
func pushedStringPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

// scanJiraAnchor scans one bare anchor row.
func scanJiraAnchor(scanner interface{ Scan(dest ...any) error }) (JiraAnchor, error) {
	anchor := JiraAnchor{}
	aux := jiraAnchorAux{}
	if err := scanner.Scan(aux.dest(&anchor)...); err != nil {
		return anchor, err
	}
	return anchor, aux.apply(&anchor)
}

// JiraIssueSnapshot is the reverse (read-only) channel payload: what
// the issue currently says on the Jira side.
type JiraIssueSnapshot struct {
	IssueKey   string
	Title      string
	Assignee   string
	Labels     []string
	Status     string
	SnapshotAt time.Time
}

// CreateJiraAnchor establishes one WorkItem <-> issue anchor. The
// anchor_source records how the pair was keyed; an unanchored issue
// never reaches this table and therefore never produces Maestro side
// effects (blueprint section 1.2 rule 3).
func (s pgJiraStore) CreateJiraAnchor(ctx context.Context, projectID string, anchor JiraAnchor) (JiraAnchor, error) {
	if anchor.WorkItemID == "" {
		return JiraAnchor{}, fmt.Errorf("%w: work item is required", ErrJiraAnchorInvalid)
	}
	if anchor.AnchorSource != JiraAnchorSourceManual && anchor.AnchorSource != JiraAnchorSourceAPI {
		return JiraAnchor{}, fmt.Errorf("%w: unknown anchor source %q", ErrJiraAnchorInvalid, anchor.AnchorSource)
	}
	if anchor.IssueTitle != "" || anchor.IssueAssignee != "" || anchor.IssueStatus != "" ||
		len(anchor.IssueLabels) != 0 || anchor.SnapshotAt != "" {
		return JiraAnchor{}, fmt.Errorf("%w: a new anchor carries no snapshot", ErrJiraAnchorInvalid)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO jira_anchors
			(id, project_id, work_item_id, issue_key, jira_project_key, anchor_source, assignee, iteration_label)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		pgNewUUID(), projectID, anchor.WorkItemID, anchor.IssueKey, anchor.JiraProjectKey,
		anchor.AnchorSource, anchor.Assignee, anchor.IterationLabel); err != nil {
		return JiraAnchor{}, classifyJiraAnchorWrite(err)
	}
	return s.JiraAnchorByWorkItem(ctx, projectID, anchor.WorkItemID)
}

// classifyJiraAnchorWrite maps driver constraint failures onto the
// stable sentinels: the two uniqueness violations carry their exact
// constraint names; every other CHECK/FK shape violation is an
// invalid-anchor rejection.
func classifyJiraAnchorWrite(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "jira_anchors_project_id_work_item_id_key":
			return fmt.Errorf("%w", ErrJiraAnchorExists)
		case "jira_anchors_project_id_issue_key_key":
			return fmt.Errorf("%w", ErrJiraAnchorIssueTaken)
		default:
			return fmt.Errorf("%w: %s (%s)", ErrJiraAnchorInvalid, pgErr.Message, pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("jira store: create anchor: %w", err)
}

// JiraAnchorByWorkItem resolves the project's anchor for one work item.
func (s pgJiraStore) JiraAnchorByWorkItem(ctx context.Context, projectID, workItemID string) (JiraAnchor, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jiraAnchorColumns+` FROM jira_anchors a WHERE a.project_id = $1 AND a.work_item_id = $2`,
		projectID, workItemID)
	anchor, err := scanJiraAnchor(row)
	if errors.Is(err, sql.ErrNoRows) {
		return JiraAnchor{}, ErrJiraAnchorNotFound
	}
	if err != nil {
		return JiraAnchor{}, fmt.Errorf("jira store: get anchor: %w", err)
	}
	return anchor, nil
}

// JiraAnchorView pairs one anchor with its work item's current SoR
// values — the two sides divergence detection compares, in one row.
type JiraAnchorView struct {
	Anchor         JiraAnchor
	WorkItemTitle  string
	WorkItemStatus string
}

// ListJiraAnchors returns the project's anchors in issue-key order,
// each joined with the work item's SoR title/status.
func (s pgJiraStore) ListJiraAnchors(ctx context.Context, projectID string) ([]JiraAnchorView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+jiraAnchorColumns+`, w.title, w.status
		FROM jira_anchors a
		JOIN work_items w ON w.project_id = a.project_id AND w.id = a.work_item_id
		WHERE a.project_id = $1
		ORDER BY a.issue_key`, projectID)
	if err != nil {
		return nil, fmt.Errorf("jira store: list: %w", err)
	}
	defer rows.Close()

	views := []JiraAnchorView{}
	for rows.Next() {
		view := JiraAnchorView{}
		aux := jiraAnchorAux{}
		if err := rows.Scan(append(aux.dest(&view.Anchor), &view.WorkItemTitle, &view.WorkItemStatus)...); err != nil {
			return nil, fmt.Errorf("jira store: scan: %w", err)
		}
		if err := aux.apply(&view.Anchor); err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

// ListAllJiraAnchors lists every project's anchors — the worker
// surface, platform-wide like the webhook dispatcher.
func (s pgJiraStore) ListAllJiraAnchors(ctx context.Context) ([]JiraAnchorView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+jiraAnchorColumns+`, w.title, w.status
		FROM jira_anchors a
		JOIN work_items w ON w.project_id = a.project_id AND w.id = a.work_item_id
		ORDER BY a.issue_key`)
	if err != nil {
		return nil, fmt.Errorf("jira store: list all: %w", err)
	}
	defer rows.Close()

	views := []JiraAnchorView{}
	for rows.Next() {
		view := JiraAnchorView{}
		aux := jiraAnchorAux{}
		if err := rows.Scan(append(aux.dest(&view.Anchor), &view.WorkItemTitle, &view.WorkItemStatus)...); err != nil {
			return nil, fmt.Errorf("jira store: scan all: %w", err)
		}
		if err := aux.apply(&view.Anchor); err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

// UpdateJiraSnapshot is the ONLY writer of the Jira-side snapshot
// columns (the reverse channel). It never touches work_items: Jira
// status can never reach Maestro state through this store.
func (s pgJiraStore) UpdateJiraSnapshot(ctx context.Context, projectID, anchorID string, snapshot JiraIssueSnapshot) error {
	labels, err := json.Marshal(snapshot.Labels)
	if err != nil {
		return fmt.Errorf("jira store: encode labels: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE jira_anchors
		SET issue_title = $3, issue_assignee = $4, issue_labels = $5::jsonb,
		    issue_status = $6, snapshot_at = $7, updated_at = now()
		WHERE project_id = $1 AND id = $2`,
		projectID, anchorID, snapshot.Title, snapshot.Assignee, string(labels),
		snapshot.Status, snapshot.SnapshotAt)
	if err != nil {
		return fmt.Errorf("jira store: update snapshot: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrJiraAnchorNotFound
	}
	return nil
}

// RecordJiraMirrorOutcome stamps the mirror bookkeeping columns. An
// error outcome records the message; a success clears it — degraded
// states are honest per-anchor, never process-fatal.
func (s pgJiraStore) RecordJiraMirrorOutcome(ctx context.Context, projectID, anchorID string, ok bool, errorMessage string) error {
	var failure any
	if errorMessage != "" {
		failure = errorMessage
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE jira_anchors
		SET last_mirror_at = now(), last_mirror_ok = $3, last_error = $4, updated_at = now()
		WHERE project_id = $1 AND id = $2`,
		projectID, anchorID, ok, failure)
	if err != nil {
		return fmt.Errorf("jira store: record mirror: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrJiraAnchorNotFound
	}
	return nil
}

// JiraPushedMarks records what the mirror established on the Jira
// side; nil members stay untouched.
type JiraPushedMarks struct {
	Title       *string
	Assignee    *string
	Iteration   *string
	StatusLabel *string
}

// JiraMarkPushed stamps the pushed-* memory. The mirror writes it
// after a push or a confirmation (the Jira side already holds the SoR
// value) — this memory is what keeps a Jira-side drift OUT of the
// push path and inside the reconcile list.
func (s pgJiraStore) JiraMarkPushed(ctx context.Context, projectID, anchorID string, marks JiraPushedMarks) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE jira_anchors SET
			pushed_title        = COALESCE($3, pushed_title),
			pushed_assignee     = COALESCE($4, pushed_assignee),
			pushed_iteration    = COALESCE($5, pushed_iteration),
			pushed_status_label = COALESCE($6, pushed_status_label),
			updated_at = now()
		WHERE project_id = $1 AND id = $2`,
		projectID, anchorID, marks.Title, marks.Assignee, marks.Iteration, marks.StatusLabel)
	if err != nil {
		return fmt.Errorf("jira store: mark pushed: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrJiraAnchorNotFound
	}
	return nil
}

type jiraExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// jiraReArmPushed clears one field's pushed-* memory (static SQL per
// field — no dynamic column assembly). The next mirror run pushes the
// SoR value again; the accept_sor adjudication path calls this inside
// its transaction so "SoR wins" lands as an actual push.
func jiraReArmPushed(ctx context.Context, q jiraExecer, projectID, anchorID, field string) error {
	var err error
	switch field {
	case JiraFieldTitle:
		_, err = q.ExecContext(ctx, `
			UPDATE jira_anchors SET pushed_title = NULL, updated_at = now()
			WHERE project_id = $1 AND id = $2`, projectID, anchorID)
	case JiraFieldAssignee:
		_, err = q.ExecContext(ctx, `
			UPDATE jira_anchors SET pushed_assignee = NULL, updated_at = now()
			WHERE project_id = $1 AND id = $2`, projectID, anchorID)
	case JiraFieldIteration:
		_, err = q.ExecContext(ctx, `
			UPDATE jira_anchors SET pushed_iteration = NULL, updated_at = now()
			WHERE project_id = $1 AND id = $2`, projectID, anchorID)
	case JiraFieldStatus:
		_, err = q.ExecContext(ctx, `
			UPDATE jira_anchors SET pushed_status_label = NULL, updated_at = now()
			WHERE project_id = $1 AND id = $2`, projectID, anchorID)
	default:
		return fmt.Errorf("jira store: unknown mirror field %q", field)
	}
	if err != nil {
		return fmt.Errorf("jira store: re-arm pushed: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reconcile items (the divergence list, blueprint section 1.2 rule 2)
// ---------------------------------------------------------------------------

// JiraReconcileItem is one stored divergence between a SoR value and
// the Jira-side snapshot of the same field.
type JiraReconcileItem struct {
	ID             string
	ProjectID      string
	AnchorID       string
	IssueKey       string // joined for display
	WorkItemID     string // joined for display
	Field          string
	SoRValue       string
	MirrorValue    string
	State          string // open | escalated | resolved
	OpenCycles     int
	DetectedAt     string
	UpdatedAt      string
	ResolvedBy     string
	ResolvedAt     string
	Resolution     string // accept_sor | accept_mirror (empty while live)
	ResolutionNote string
}

const jiraReconcileColumns = `i.id::text, i.project_id::text, i.anchor_id::text, a.issue_key, a.work_item_id,
	i.field, i.sor_value, i.mirror_value, i.state, i.open_cycles, i.detected_at, i.updated_at,
	i.resolved_by, i.resolved_at, i.resolution, i.resolution_note`

func scanJiraReconcileItem(scanner interface{ Scan(dest ...any) error }) (JiraReconcileItem, error) {
	item := JiraReconcileItem{}
	var detectedAt, updatedAt time.Time
	var resolvedBy, resolution, note sql.NullString
	var resolvedAt sql.NullTime
	if err := scanner.Scan(&item.ID, &item.ProjectID, &item.AnchorID, &item.IssueKey, &item.WorkItemID,
		&item.Field, &item.SoRValue, &item.MirrorValue, &item.State, &item.OpenCycles,
		&detectedAt, &updatedAt, &resolvedBy, &resolvedAt, &resolution, &note); err != nil {
		return item, err
	}
	item.DetectedAt = detectedAt.UTC().Format(time.RFC3339Nano)
	item.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
	if resolvedBy.Valid {
		item.ResolvedBy = resolvedBy.String
	}
	if resolvedAt.Valid {
		item.ResolvedAt = resolvedAt.Time.UTC().Format(time.RFC3339Nano)
	}
	if resolution.Valid {
		item.Resolution = resolution.String
	}
	if note.Valid {
		item.ResolutionNote = note.String
	}
	return item, nil
}

// UpsertJiraDivergence records one field divergence seen by a
// reconcile run. The first detection opens an item (cycle 1); every
// re-detection of the same live divergence advances the cycle
// counter, and the cycle reaching the frozen escalation line flips
// the item to escalated with an atomic audit row. A changed value on
// a live item updates the row in place (it is the same divergence,
// re-observed).
func (s pgJiraStore) UpsertJiraDivergence(ctx context.Context, projectID, anchorID, field, sorValue, mirrorValue string) (JiraReconcileItem, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return JiraReconcileItem{}, false, fmt.Errorf("jira store: begin divergence: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	liveRow := tx.QueryRowContext(ctx, `
		SELECT `+jiraReconcileColumns+` FROM jira_reconcile_items i
		JOIN jira_anchors a ON a.id = i.anchor_id
		WHERE i.anchor_id = $1 AND i.field = $2 AND i.state IN ('open', 'escalated')`,
		anchorID, field)
	live, liveErr := scanJiraReconcileItem(liveRow)

	escalated := false
	switch {
	case liveErr == nil:
		cycles := live.OpenCycles + 1
		state := live.State
		if cycles >= JiraReconcileEscalateAfterCycles {
			state = "escalated"
			escalated = state != live.State
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jira_reconcile_items
			SET sor_value = $3, mirror_value = $4, state = $5, open_cycles = $6, updated_at = now()
			WHERE id = $1 AND project_id = $2`,
			live.ID, projectID, sorValue, mirrorValue, state, cycles); err != nil {
			return JiraReconcileItem{}, false, fmt.Errorf("jira store: advance divergence: %w", err)
		}
		if escalated {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO audit_events
					(actor_principal, project_id, action, resource_type, resource_id, decision, reason, correlation_id)
				VALUES ($1, $2, 'jira.reconcile.escalated', 'jira_reconcile_item', $3, 'allow', $4, $3)`,
				jiraSystemActor, projectID, live.ID,
				fmt.Sprintf("field %s diverged for two reconcile cycles without adjudication", field)); err != nil {
				return JiraReconcileItem{}, false, fmt.Errorf("jira store: escalate audit: %w", err)
			}
		}
	case errors.Is(liveErr, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jira_reconcile_items (id, project_id, anchor_id, field, sor_value, mirror_value, state, open_cycles)
			VALUES ($1, $2, $3, $4, $5, $6, 'open', 1)`,
			pgNewUUID(), projectID, anchorID, field, sorValue, mirrorValue); err != nil {
			return JiraReconcileItem{}, false, fmt.Errorf("jira store: open divergence: %w", err)
		}
	default:
		return JiraReconcileItem{}, false, fmt.Errorf("jira store: lock divergence: %w", liveErr)
	}

	readBack := tx.QueryRowContext(ctx, `
		SELECT `+jiraReconcileColumns+` FROM jira_reconcile_items i
		JOIN jira_anchors a ON a.id = i.anchor_id
		WHERE i.anchor_id = $1 AND i.field = $2 AND i.state IN ('open', 'escalated')`,
		anchorID, field)
	item, err := scanJiraReconcileItem(readBack)
	if err != nil {
		return JiraReconcileItem{}, false, fmt.Errorf("jira store: read back divergence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return JiraReconcileItem{}, false, fmt.Errorf("jira store: commit divergence: %w", err)
	}
	return item, escalated, nil
}

// HealJiraDivergence resolves a live item whose field converged (the
// divergence no longer exists, so the freeze lifts itself). The
// healing resolution and its audit row commit atomically; only a
// human adjudication may rewrite a SoR value, and healing never does.
func (s pgJiraStore) HealJiraDivergence(ctx context.Context, projectID, anchorID, field string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("jira store: begin heal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE jira_reconcile_items
		SET state = 'resolved', resolution = 'accept_sor', resolved_by = $4,
		    resolved_at = now(), resolution_note = 'divergence healed before adjudication', updated_at = now()
		WHERE project_id = $1 AND anchor_id = $2 AND field = $3 AND state IN ('open', 'escalated')`,
		projectID, anchorID, field, jiraSystemActor)
	if err != nil {
		return fmt.Errorf("jira store: heal divergence: %w", err)
	}
	healed, _ := result.RowsAffected()
	if healed > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO audit_events
				(actor_principal, project_id, action, resource_type, resource_id, decision, reason, correlation_id)
			VALUES ($1, $2, 'jira.reconcile.healed', 'jira_reconcile_item', $3, 'allow',
				'divergence converged before adjudication', $3)`,
			jiraSystemActor, projectID, anchorID); err != nil {
			return fmt.Errorf("jira store: heal audit: %w", err)
		}
	}
	return tx.Commit()
}

// ResolveJiraReconcileItem applies one human adjudication atomically:
// the item flips to resolved, the chosen value lands on the SoR side,
// and the jira.reconcile.resolved audit row commits with it.
//
//	accept_sor    — SoR wins; the freeze lifts and the next mirror run
//	                pushes the SoR value to Jira.
//	accept_mirror — the Jira-side value wins; assignee rewrites the
//	                anchor's SoR field and title rewrites
//	                work_items.title (a Jira-direction write only a
//	                human may trigger). iteration_label and status_label
//	                are pure Maestro→Jira projections with no writable
//	                SoR field (work_items.status is inviolable from the
//	                Jira side; the desired iteration label is Maestro
//	                state), so they accept accept_sor only.
func (s pgJiraStore) ResolveJiraReconcileItem(ctx context.Context, projectID, itemID, resolution, actor, note string) (JiraReconcileItem, error) {
	if resolution != "accept_sor" && resolution != "accept_mirror" {
		return JiraReconcileItem{}, fmt.Errorf("%w: unknown resolution %q", ErrJiraReconcileNotAdjudicable, resolution)
	}
	if actor == "" {
		return JiraReconcileItem{}, fmt.Errorf("%w: actor is required", ErrJiraReconcileNotAdjudicable)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return JiraReconcileItem{}, fmt.Errorf("jira store: begin adjudication: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT `+jiraReconcileColumns+` FROM jira_reconcile_items i
		JOIN jira_anchors a ON a.id = i.anchor_id
		WHERE i.id = $1 AND i.project_id = $2 FOR UPDATE OF i`,
		itemID, projectID)
	live, liveErr := scanJiraReconcileItem(row)
	if errors.Is(liveErr, sql.ErrNoRows) {
		return JiraReconcileItem{}, ErrJiraReconcileNotFound
	}
	if liveErr != nil {
		return JiraReconcileItem{}, fmt.Errorf("jira store: lock item: %w", liveErr)
	}
	if live.State == "resolved" {
		return JiraReconcileItem{}, fmt.Errorf("%w: item is already resolved", ErrJiraReconcileNotAdjudicable)
	}
	if resolution == "accept_mirror" && (live.Field == JiraFieldIteration || live.Field == JiraFieldStatus) {
		return JiraReconcileItem{}, fmt.Errorf("%w: %s is a projection field and accepts accept_sor only",
			ErrJiraReconcileNotAdjudicable, live.Field)
	}

	if resolution == "accept_sor" {
		// SoR wins — re-arm the field so the next mirror run pushes
		// the SoR value back to Jira (same transaction as the
		// resolution; the push itself stays on the mirror cadence).
		if err := jiraReArmPushed(ctx, tx, projectID, live.AnchorID, live.Field); err != nil {
			return JiraReconcileItem{}, err
		}
	}

	if resolution == "accept_mirror" {
		switch live.Field {
		case JiraFieldAssignee:
			if _, err := tx.ExecContext(ctx, `
				UPDATE jira_anchors SET assignee = $3, updated_at = now() WHERE id = $1 AND project_id = $2`,
				live.AnchorID, projectID, live.MirrorValue); err != nil {
				return JiraReconcileItem{}, fmt.Errorf("jira store: accept mirror assignee: %w", err)
			}
		case JiraFieldTitle:
			if _, err := tx.ExecContext(ctx, `
				UPDATE work_items SET title = $3, updated_at = now()
				WHERE id = $2 AND project_id = $1`,
				projectID, live.WorkItemID, live.MirrorValue); err != nil {
				return JiraReconcileItem{}, fmt.Errorf("jira store: accept mirror title: %w", err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE jira_reconcile_items
		SET state = 'resolved', resolution = $3, resolved_by = $4, resolved_at = now(),
		    resolution_note = $5, updated_at = now()
		WHERE id = $1 AND project_id = $2`,
		itemID, projectID, resolution, actor, note); err != nil {
		return JiraReconcileItem{}, fmt.Errorf("jira store: resolve item: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events
			(actor_principal, project_id, action, resource_type, resource_id, decision, reason, correlation_id)
		VALUES ($1, $2, 'jira.reconcile.resolved', 'jira_reconcile_item', $3, 'allow', $4, $3)`,
		actor, projectID, itemID, adjudicationReason(resolution, note)); err != nil {
		return JiraReconcileItem{}, fmt.Errorf("jira store: adjudication audit: %w", err)
	}

	readBack := tx.QueryRowContext(ctx, `
		SELECT `+jiraReconcileColumns+` FROM jira_reconcile_items i
		JOIN jira_anchors a ON a.id = i.anchor_id
		WHERE i.id = $1 AND i.project_id = $2`, itemID, projectID)
	resolved, err := scanJiraReconcileItem(readBack)
	if err != nil {
		return JiraReconcileItem{}, fmt.Errorf("jira store: read back item: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return JiraReconcileItem{}, fmt.Errorf("jira store: commit adjudication: %w", err)
	}
	return resolved, nil
}

func adjudicationReason(resolution, note string) string {
	if note != "" {
		return resolution + ": " + note
	}
	return resolution
}

// ListJiraReconcileItems lists the project's divergence items — live
// (open/escalated) first; resolved=true flips to the resolved history.
// Newest detection first inside each group.
func (s pgJiraStore) ListJiraReconcileItems(ctx context.Context, projectID string, resolved bool) ([]JiraReconcileItem, error) {
	stateFilter := `('open', 'escalated')`
	if resolved {
		stateFilter = `('resolved')`
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+jiraReconcileColumns+` FROM jira_reconcile_items i
		JOIN jira_anchors a ON a.id = i.anchor_id
		WHERE i.project_id = $1 AND i.state IN `+stateFilter+`
		ORDER BY i.detected_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("jira store: list items: %w", err)
	}
	defer rows.Close()

	items := []JiraReconcileItem{}
	for rows.Next() {
		item, scanErr := scanJiraReconcileItem(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("jira store: scan item: %w", scanErr)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// JiraFrozenFields reports which mirror fields of one anchor carry a
// live divergence — the freeze set the mirror worker must skip.
func (s pgJiraStore) JiraFrozenFields(ctx context.Context, projectID, anchorID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT field FROM jira_reconcile_items
		WHERE project_id = $1 AND anchor_id = $2 AND state IN ('open', 'escalated')`,
		projectID, anchorID)
	if err != nil {
		return nil, fmt.Errorf("jira store: frozen fields: %w", err)
	}
	defer rows.Close()

	fields := []string{}
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			return nil, fmt.Errorf("jira store: scan frozen field: %w", err)
		}
		fields = append(fields, field)
	}
	return fields, rows.Err()
}
