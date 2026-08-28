package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SQLite -> PostgreSQL importer (M1-DATA-001, TECH-DATA-001 section 13).
//
// Stages (see migrations/postgresql/README.md for the mapping table):
//   - dry-run: build and validate the full plan against PostgreSQL, write
//     nothing. Exit non-zero only on infrastructure errors, never on
//     quarantined rows (they are the human checklist).
//   - import: apply the plan in ONE transaction; any error rolls everything
//     back. Idempotent by source row identity through legacy_id_map.
//   - reconcile: coverage + status-projection comparison plus invariants.
//
// Rows that cannot be mapped without guessing (invalid state, unparseable
// timestamp, invalid JSON, dangling cross-reference) are quarantined with
// their source identity and reason; they are never silently repaired.

// ImportReport is the machine-readable artifact of every stage.
type ImportReport struct {
	Stage           string                `json:"stage"`
	Tables          []ImportTableReport   `json:"tables"`
	Quarantine      []ImportQuarantineRow `json:"quarantine,omitempty"`
	ManualChecklist []string              `json:"manual_checklist,omitempty"`
	Warnings        []string              `json:"warnings,omitempty"`
	GeneratedAt     string                `json:"generated_at"`
}

// ImportTableReport summarizes one source table disposition.
type ImportTableReport struct {
	SourceTable string `json:"source_table"`
	TargetTable string `json:"target_table"`
	SourceCount int    `json:"source_count"`
	Planned     int    `json:"planned"`          // new rows this run would insert
	Already     int    `json:"already_imported"` // mapped by a previous run
	Skipped     int    `json:"skipped"`          // not migrated by design
	Quarantined int    `json:"quarantined"`
	StatusDrift int    `json:"status_drift,omitempty"` // reconcile only
}

// ImportQuarantineRow records an unmappable source row.
type ImportQuarantineRow struct {
	SourceTable string `json:"source_table"`
	SourceID    string `json:"source_id"`
	Reason      string `json:"reason"`
}

// skipped SQLite tables and why (import fidelity contract).
var importSkippedTables = []struct {
	Table  string
	Reason string
}{
	{"agent_sessions", "runtime session state is not a durable fact; references preserved as legacy text columns"},
	{"agent_workers", "runtime worker state is not a durable fact"},
	{"task_results", "superseded by diagnostic validation evidence"},
	{"activity_log", "M0 UI feed, rebuildable from the event stream"},
	{"audit_log", "M0 shared-token audit stays in the SQLite archive; v3 audit_events start at cutover"},
	{"api_contracts", "M2 GitLab connector scope"},
	{"idempotency_records", "short-TTL runtime deduplication state"},
	{"project_queue_versions", "runtime scheduler state"},
	{"state_history", "runtime history table; facts live in audit_events"},
	{"runtime_state", "runtime recovery state"},
}

// SQLiteImporter reads one SQLite database read-only and plans/applies the
// migration into PostgreSQL.
type SQLiteImporter struct {
	sqlite *sql.DB
	pg     *sql.DB
}

// OpenSQLiteReadOnly opens the migration source with an explicit read-only
// connection so the importer can never mutate the frozen input. The path
// must be an existing plain file: SQLite URI/query forms are rejected the
// same way the runtime rejects them for db_path.
func OpenSQLiteReadOnly(path string) (*sql.DB, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errors.New("sqlite path must not be empty")
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "file:") || strings.ContainsAny(trimmed, "?#") {
		return nil, errors.New("sqlite path must be a filesystem path, not a URI or query")
	}
	if _, statErr := os.Stat(trimmed); statErr != nil {
		return nil, fmt.Errorf("sqlite source not readable: %w", statErr)
	}
	db, err := sql.Open("sqlite", "file:"+trimmed+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	return db, nil
}

// NewSQLiteImporter pairs a read-only SQLite source with the PostgreSQL
// target. Both handles stay owned by the caller.
func NewSQLiteImporter(sqlite, pg *sql.DB) (*SQLiteImporter, error) {
	if sqlite == nil || pg == nil {
		return nil, errors.New("importer requires non-nil sqlite and postgres handles")
	}
	return &SQLiteImporter{sqlite: sqlite, pg: pg}, nil
}

// ---------------------------------------------------------------------------
// Plan model
// ---------------------------------------------------------------------------

type importProject struct {
	legacyID      string
	teamID        uuid.UUID
	teamName      string
	projectID     uuid.UUID
	key           string
	name          string
	description   string
	status        string
	config        string // validated JSON
	version       int64
	createdAt     time.Time
	updatedAt     time.Time
	workspacePath string
}

type importFeature struct {
	legacyID      string
	projectID     uuid.UUID
	featureID     uuid.UUID
	title         string
	description   string
	referenceURLs string
	status        string
	createdAt     time.Time
	updatedAt     time.Time
}

type importWorkItem struct {
	legacyID         string
	projectID        uuid.UUID
	workItemID       uuid.UUID
	featureID        *uuid.UUID
	title            string
	description      string
	status           string
	priority         string
	role             *string
	dependencies     string
	testRequirements string
	version          int64
	leaseEpoch       int64
	legacySessionID  *string
	legacyWorkerID   *string
	mergeCommit      *string
	createdAt        time.Time
	updatedAt        time.Time
}

type importLease struct {
	legacyID        string
	projectID       uuid.UUID
	workItemID      uuid.UUID
	leaseID         uuid.UUID
	legacySessionID string
	legacyWorkerID  string
	epoch           int64
	status          string
	version         int64
	expiresAt       time.Time
	createdAt       time.Time
	updatedAt       time.Time
}

type importWorktree struct {
	legacyID      string
	projectID     uuid.UUID
	workItemID    uuid.UUID
	worktreeID    uuid.UUID
	sessionID     *string
	workspacePath string
	branchName    string
	baseCommit    string
	status        string
	generation    int64
	version       int64
	createdAt     time.Time
	updatedAt     time.Time
}

type importValidationRun struct {
	legacyID      string
	projectID     uuid.UUID
	workItemID    uuid.UUID
	attempt       int
	baseCommit    string
	changedFiles  string
	profileRef    string
	policyVersion string
	policyDigest  string
	result        string
	errorCode     *string
	durationMs    int64
	coverage      *float64
	boundaryOK    bool
	testOK        bool
	coverageOK    bool
	createdAt     time.Time
}

type importPlan struct {
	projects     []importProject
	features     []importFeature
	workItems    []importWorkItem
	leases       []importLease
	worktrees    []importWorktree
	validation   []importValidationRun
	workItemRefs []importWorkItemRef
	quarantine   []ImportQuarantineRow
	checklist    []string
	warnings     []string
	sourceCount  map[string]int
	skipped      map[string]int
	already      map[string]int
}

// ---------------------------------------------------------------------------
// Stage entry points
// ---------------------------------------------------------------------------

// DryRun builds and validates the full plan without writing anything.
func (i *SQLiteImporter) DryRun(ctx context.Context) (*ImportReport, error) {
	plan, err := i.buildPlan(ctx)
	if err != nil {
		return nil, err
	}
	return plan.report("dry-run"), nil
}

// Import applies the plan in a single transaction. Failure of any statement
// rolls the whole import back (anchor M1-DATA-001).
func (i *SQLiteImporter) Import(ctx context.Context) (*ImportReport, error) {
	plan, err := i.buildPlan(ctx)
	if err != nil {
		return nil, err
	}

	tx, err := i.pg.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin import transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	for _, project := range plan.projects {
		// The synthetic team is keyed under "<id>/team" so it shares no
		// legacy_id_map primary key with the project mapping itself.
		if err := insertLegacyMap(ctx, tx, "projects", project.legacyID+"/team", "teams", project.teamID,
			fmt.Sprintf(`{"workspace_path":%q}`, project.workspacePath)); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO teams (id, name, status) VALUES ($1, $2, 'active')
			ON CONFLICT (id) DO NOTHING`, project.teamID, project.teamName); err != nil {
			return nil, fmt.Errorf("insert team for project %s: %w", project.legacyID, err)
		}
		if err := insertLegacyMap(ctx, tx, "projects", project.legacyID, "projects", project.projectID, "{}"); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, team_id, key, name, description, status, config, version, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10)
			ON CONFLICT (id) DO NOTHING`,
			project.projectID, project.teamID, project.key, project.name, project.description,
			project.status, project.config, project.version, project.createdAt, project.updatedAt); err != nil {
			return nil, fmt.Errorf("insert project %s: %w", project.legacyID, err)
		}
	}

	for _, feature := range plan.features {
		if err := insertLegacyMap(ctx, tx, "features", feature.legacyID, "features", feature.featureID, "{}"); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO features (id, project_id, title, description, reference_urls, status, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8)
			ON CONFLICT (id) DO NOTHING`,
			feature.featureID, feature.projectID, feature.title, feature.description,
			feature.referenceURLs, feature.status, feature.createdAt, feature.updatedAt); err != nil {
			return nil, fmt.Errorf("insert feature %s: %w", feature.legacyID, err)
		}
	}

	for _, item := range plan.workItems {
		if err := insertLegacyMap(ctx, tx, "tasks", item.legacyID, "work_items", item.workItemID, "{}"); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_items (
				id, project_id, feature_id, type, title, description, status, priority,
				role, dependencies, test_requirements, lease_epoch, version,
				legacy_session_id, legacy_worker_id, merge_commit_sha, created_at, updated_at)
			VALUES ($1,$2,$3,'task',$4,$5,$6,$7,$8,$9::jsonb,$10::jsonb,$11,$12,$13,$14,$15,$16,$17)
			ON CONFLICT (id) DO NOTHING`,
			item.workItemID, item.projectID, item.featureID, item.title, item.description,
			item.status, item.priority, item.role, item.dependencies, item.testRequirements,
			item.leaseEpoch, item.version, item.legacySessionID, item.legacyWorkerID,
			item.mergeCommit, item.createdAt, item.updatedAt); err != nil {
			return nil, fmt.Errorf("insert work item %s: %w", item.legacyID, err)
		}
	}

	for _, lease := range plan.leases {
		if err := insertLegacyMap(ctx, tx, "task_leases", lease.legacyID, "leases", lease.leaseID, "{}"); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO leases (
				id, project_id, work_item_id, legacy_session_id, legacy_worker_id,
				epoch, status, version, expires_at, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (id) DO NOTHING`,
			lease.leaseID, lease.projectID, lease.workItemID, lease.legacySessionID,
			lease.legacyWorkerID, lease.epoch, lease.status, lease.version,
			lease.expiresAt, lease.createdAt, lease.updatedAt); err != nil {
			return nil, fmt.Errorf("insert lease %s: %w", lease.legacyID, err)
		}
	}

	for _, worktree := range plan.worktrees {
		if err := insertLegacyMap(ctx, tx, "worktrees", worktree.legacyID, "worktrees", worktree.worktreeID, "{}"); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO worktrees (
				id, project_id, work_item_id, legacy_session_id, workspace_path,
				branch_name, base_commit, status, generation, version, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (id) DO NOTHING`,
			worktree.worktreeID, worktree.projectID, worktree.workItemID, worktree.sessionID,
			worktree.workspacePath, worktree.branchName, worktree.baseCommit, worktree.status,
			worktree.generation, worktree.version, worktree.createdAt, worktree.updatedAt); err != nil {
			return nil, fmt.Errorf("insert worktree %s: %w", worktree.legacyID, err)
		}
	}

	for _, run := range plan.validation {
		if err := insertLegacyMap(ctx, tx, "validation_runs", run.legacyID, "validation_runs", uuid.Nil, "{}"); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO validation_runs (
				project_id, work_item_id, attempt, base_commit, source_commit,
				changed_files, profile_ref, policy_version, policy_digest,
				evidence_digest, workspace_digest, authority, producer,
				coverage, boundary_ok, test_ok, coverage_ok,
				result, error_code, duration_ms, created_at)
			VALUES ($1,$2,$3,$4,'',$5::jsonb,$6,$7,$8,'','','diagnostic','maestro-local',
				$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT DO NOTHING`,
			run.projectID, run.workItemID, run.attempt, run.baseCommit,
			run.changedFiles, run.profileRef, run.policyVersion, run.policyDigest,
			run.coverage, run.boundaryOK, run.testOK, run.coverageOK,
			run.result, run.errorCode, run.durationMs, run.createdAt); err != nil {
			return nil, fmt.Errorf("insert validation run %s: %w", run.legacyID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit import transaction: %w", err)
	}
	return plan.report("import"), nil
}

func insertLegacyMap(ctx context.Context, tx *sql.Tx, sourceTable, sourceID, targetTable string, targetID uuid.UUID, metadata string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO legacy_id_map (source_table, source_id, target_table, target_id, metadata)
		VALUES ($1,$2,$3,$4::text,$5::jsonb)
		ON CONFLICT (source_table, source_id) DO NOTHING`,
		sourceTable, sourceID, targetTable, targetID, metadata)
	if err != nil {
		return fmt.Errorf("record legacy map %s/%s: %w", sourceTable, sourceID, err)
	}
	return nil
}

// Reconcile compares the frozen SQLite source against the imported
// PostgreSQL state: per-table coverage through legacy_id_map, status
// projection drift, and data invariants (single active lease per work item).
func (i *SQLiteImporter) Reconcile(ctx context.Context) (*ImportReport, error) {
	plan, err := i.buildPlan(ctx)
	if err != nil {
		return nil, err
	}
	report := plan.report("reconcile")

	// Status projection: every mapped task must carry the same canonical
	// status in PostgreSQL as the plan derived from SQLite.
	drift, err := i.countStatusDrift(ctx, plan)
	if err != nil {
		return nil, err
	}
	for index := range report.Tables {
		if report.Tables[index].SourceTable == "tasks" {
			report.Tables[index].StatusDrift = drift
		}
	}

	// DATA-INV-002 invariant on the target.
	var activeConflicts int
	if err := i.pg.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT project_id, work_item_id FROM leases
			WHERE status = 'active'
			GROUP BY project_id, work_item_id HAVING count(*) > 1
		) c`).Scan(&activeConflicts); err != nil {
		return nil, fmt.Errorf("reconcile active lease invariant: %w", err)
	}
	if activeConflicts > 0 {
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("DATA-INV-002 violation: %d work items have more than one active lease", activeConflicts))
	}

	// Manual checklist is recomputed from the target so it also covers
	// projects imported by earlier runs: every project without a membership
	// stays inaccessible until a human assigns an owner.
	rows, err := i.pg.QueryContext(ctx, `
		SELECT p.key FROM projects p
		WHERE NOT EXISTS (
			SELECT 1 FROM memberships m WHERE m.team_id = p.team_id
		)
		ORDER BY p.key`)
	if err != nil {
		return nil, fmt.Errorf("reconcile ownerless projects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan ownerless project: %w", err)
		}
		report.ManualChecklist = append(report.ManualChecklist,
			fmt.Sprintf("project key=%s has no owner membership; assign an owner before activation", key))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ownerless projects: %w", err)
	}

	if drift > 0 || activeConflicts > 0 {
		report.Warnings = append(report.Warnings, "reconcile found drift; review before cutover")
	}
	return report, nil
}

func (i *SQLiteImporter) countStatusDrift(ctx context.Context, plan *importPlan) (int, error) {
	drift := 0
	for _, ref := range plan.workItemRefs {
		var status string
		err := i.pg.QueryRowContext(ctx, `SELECT status FROM work_items WHERE id = $1`, ref.targetID).
			Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			drift++
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("reconcile work item %s: %w", ref.legacyID, err)
		}
		if status != ref.status {
			drift++
		}
	}
	return drift, nil
}

// ---------------------------------------------------------------------------
// Plan construction
// ---------------------------------------------------------------------------

func (i *SQLiteImporter) buildPlan(ctx context.Context) (*importPlan, error) {
	plan := &importPlan{
		sourceCount: map[string]int{},
		skipped:     map[string]int{},
		already:     map[string]int{},
	}
	for _, skipped := range importSkippedTables {
		count, err := i.countSQLite(ctx, skipped.Table)
		if err != nil {
			return nil, err
		}
		if count == 0 {
			continue
		}
		plan.skipped[skipped.Table] = count
		plan.warnings = append(plan.warnings,
			fmt.Sprintf("table %s: %d rows not migrated by design: %s", skipped.Table, count, skipped.Reason))
	}
	if err := i.planProjects(ctx, plan); err != nil {
		return nil, err
	}
	if err := i.planFeatures(ctx, plan); err != nil {
		return nil, err
	}
	if err := i.planWorkItems(ctx, plan); err != nil {
		return nil, err
	}
	if err := i.planLeases(ctx, plan); err != nil {
		return nil, err
	}
	if err := i.planWorktrees(ctx, plan); err != nil {
		return nil, err
	}
	if err := i.planValidationRuns(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func (i *SQLiteImporter) countSQLite(ctx context.Context, table string) (int, error) {
	exists, err := i.sqliteTableExists(ctx, table)
	if err != nil || !exists {
		return 0, err
	}
	var count int
	if err := i.sqlite.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
		return 0, fmt.Errorf("count sqlite %s: %w", table, err)
	}
	return count, nil
}

func (i *SQLiteImporter) sqliteTableExists(ctx context.Context, table string) (bool, error) {
	var name string
	err := i.sqlite.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect sqlite schema: %w", err)
	}
	return true, nil
}

// mappedTarget resolves a previously imported source row to its target UUID.
func (i *SQLiteImporter) mappedTarget(ctx context.Context, sourceTable, sourceID, targetTable string) (uuid.UUID, bool, error) {
	var target string
	err := i.pg.QueryRowContext(ctx, `
		SELECT target_id::text FROM legacy_id_map
		WHERE source_table = $1 AND source_id = $2 AND target_table = $3`,
		sourceTable, sourceID, targetTable).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("lookup legacy map %s/%s: %w", sourceTable, sourceID, err)
	}
	parsed, parseErr := uuid.Parse(target)
	if parseErr != nil {
		return uuid.Nil, false, fmt.Errorf("legacy map %s/%s holds invalid uuid: %w", sourceTable, sourceID, parseErr)
	}
	return parsed, true, nil
}

func (plan *importPlan) addQuarantine(table, id, reason string) {
	plan.quarantine = append(plan.quarantine, ImportQuarantineRow{SourceTable: table, SourceID: id, Reason: reason})
}

var projectKeyRe = regexp.MustCompile(`^[a-z][a-z0-9-]{2,31}$`)
var slugDisallowedRe = regexp.MustCompile(`[^a-z0-9-]+`)

func slugifyProjectKey(name, fallback string, used map[string]struct{}) string {
	base := strings.ToLower(strings.TrimSpace(name))
	base = slugDisallowedRe.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if !projectKeyRe.MatchString(base) {
		base = "legacy-" + strings.ToLower(fallback)
		base = slugDisallowedRe.ReplaceAllString(base, "-")
		base = strings.Trim(base, "-")
		if len(base) > 31 {
			base = base[:31]
		}
	}
	if len(base) < 3 {
		base = "legacy-" + base
	}
	if !projectKeyRe.MatchString(base) {
		base = "legacy-project"
	}
	key := base
	for suffix := 2; ; suffix++ {
		if _, taken := used[key]; !taken {
			used[key] = struct{}{}
			return key
		}
		key = fmt.Sprintf("%s-%d", base, suffix)
		if len(key) > 31 {
			key = key[:28] + "-x"[:1] + fmt.Sprint(suffix%10)
			key = strings.TrimSuffix(key, "-")
		}
	}
}

var sqliteTimeLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
}

func parseSQLiteTimestamp(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, errors.New("empty timestamp")
	}
	for _, layout := range sqliteTimeLayouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp %q", value)
}

// validJSONOr returns the validated JSON text, or the default when the
// source stored an empty/absent value.
func validJSONOr(value string, def string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "''" {
		return def, nil
	}
	if !json.Valid([]byte(trimmed)) {
		return "", fmt.Errorf("invalid JSON")
	}
	return trimmed, nil
}

func (plan *importPlan) report(stage string) *ImportReport {
	tableOrder := []struct{ source, target string }{
		{"projects", "teams+projects"},
		{"features", "features"},
		{"tasks", "work_items"},
		{"task_leases", "leases"},
		{"worktrees", "worktrees"},
		{"validation_runs", "validation_runs"},
	}
	counts := map[string]int{}
	for _, entry := range plan.quarantine {
		counts[entry.SourceTable]++
	}
	report := &ImportReport{
		Stage:           stage,
		Quarantine:      plan.quarantine,
		ManualChecklist: plan.checklist,
		Warnings:        plan.warnings,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	for _, table := range tableOrder {
		report.Tables = append(report.Tables, ImportTableReport{
			SourceTable: table.source,
			TargetTable: table.target,
			SourceCount: plan.sourceCount[table.source],
			Planned:     tableRowCount(plan, table.source),
			Already:     plan.already[table.source],
			Quarantined: counts[table.source],
			Skipped:     plan.skipped[table.source],
		})
	}
	return report
}

func tableRowCount(plan *importPlan, source string) int {
	switch source {
	case "projects":
		return len(plan.projects)
	case "features":
		return len(plan.features)
	case "tasks":
		return len(plan.workItems)
	case "task_leases":
		return len(plan.leases)
	case "worktrees":
		return len(plan.worktrees)
	case "validation_runs":
		return len(plan.validation)
	}
	return 0
}
