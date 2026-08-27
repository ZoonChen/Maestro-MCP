package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// SQLiteTaskStore implements TaskStore backed by SQLite.
// All query methods require projectID as the first parameter for L4 isolation.
type SQLiteTaskStore struct {
	db *sql.DB
}

// NewSQLiteTaskStore creates a new TaskStore instance.
func NewSQLiteTaskStore(db *sql.DB) *SQLiteTaskStore {
	return &SQLiteTaskStore{db: db}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// taskColumns is the ordered column list for SELECT queries.
// Must match scanTask order exactly.
const taskColumns = `t.id, t.project_id, t.feature_id, t.title, t.description, t.role, t.status,
    t.allowed_directories, t.forbidden_patterns, t.required_apis, t.dependencies,
    t.parent_task_id, t.relation_type, t.test_requirements,
    (SELECT COALESCE(s.external_id, s.id) FROM agent_sessions AS s
        WHERE s.id = t.assigned_session_id AND s.project_id = t.project_id),
    t.assigned_worker_id, t.assigned_at,
    t.blocker_reason, t.cancel_reason, t.merge_commit,
    (SELECT COALESCE(s.external_id, s.id) FROM agent_sessions AS s
        WHERE s.id = t.verified_by AND s.project_id = t.project_id),
    t.verified_at,
    t.priority, t.summary, t.version, t.lease_epoch, t.active_lease_id, t.lease_expires_at,
    t.merged_fact_id,
    t.created_at, t.updated_at`

// ptr returns a pointer to the given string value.
func ptr(s string) *string { return &s }

// rawMessageOrDefault converts a NullableString to json.RawMessage.
// Returns an empty json.RawMessage (not nil) for NULL columns to avoid nil map/slice issues.
func rawMessageOrDefault(v sql.NullString) json.RawMessage {
	if !v.Valid || v.String == "" {
		return json.RawMessage(nil)
	}
	return json.RawMessage(v.String)
}

// scanTask scans a single row from *sql.Rows or *sql.Row into a Task struct.
// Column order must match the taskColumns constant and DDL column order.
func scanTask(sc scan) (*model.Task, error) {
	var (
		t model.Task

		// JSON TEXT columns (may be NULL or have default values)
		forbiddenPatterns sql.NullString
		requiredAPIs      sql.NullString
		dependencies      sql.NullString
		testRequirements  sql.NullString

		// Nullable TEXT columns
		parentTaskID      sql.NullString
		relationType      sql.NullString
		assignedSessionID sql.NullString
		assignedWorkerID  sql.NullString
		assignedAt        sql.NullString
		blockerReason     sql.NullString
		cancelReason      sql.NullString
		mergeCommit       sql.NullString
		verifiedBy        sql.NullString
		verifiedAt        sql.NullString
		summary           sql.NullString
		activeLeaseID     sql.NullString
		leaseExpiresAt    sql.NullString
		mergedFactID      sql.NullString
	)

	err := sc.Scan(
		&t.ID,
		&t.ProjectID,
		&t.FeatureID,
		&t.Title,
		&t.Description,
		&t.Role,
		&t.Status,
		&t.AllowedDirectories,
		&forbiddenPatterns,
		&requiredAPIs,
		&dependencies,
		&parentTaskID,
		&relationType,
		&testRequirements,
		&assignedSessionID,
		&assignedWorkerID,
		&assignedAt,
		&blockerReason,
		&cancelReason,
		&mergeCommit,
		&verifiedBy,
		&verifiedAt,
		&t.Priority,
		&summary,
		&t.Version,
		&t.LeaseEpoch,
		&activeLeaseID,
		&leaseExpiresAt,
		&mergedFactID,
		&t.CreatedAt,
		&t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	// Nullable strings -> pointer fields
	if parentTaskID.Valid {
		t.ParentTaskID = ptr(parentTaskID.String)
	}
	if relationType.Valid {
		t.RelationType = ptr(relationType.String)
	}
	if assignedSessionID.Valid {
		t.AssignedSessionID = ptr(assignedSessionID.String)
	}
	if assignedWorkerID.Valid {
		t.AssignedWorkerID = ptr(assignedWorkerID.String)
	}
	if assignedAt.Valid {
		t.AssignedAt = ptr(assignedAt.String)
	}
	if blockerReason.Valid {
		t.BlockerReason = ptr(blockerReason.String)
	}
	if cancelReason.Valid {
		t.CancelReason = ptr(cancelReason.String)
	}
	if mergeCommit.Valid {
		t.MergeCommit = ptr(mergeCommit.String)
	}
	if verifiedBy.Valid {
		t.VerifiedBy = ptr(verifiedBy.String)
	}
	if verifiedAt.Valid {
		t.VerifiedAt = ptr(verifiedAt.String)
	}
	if summary.Valid {
		t.Summary = ptr(summary.String)
	}
	if activeLeaseID.Valid {
		t.ActiveLeaseID = ptr(activeLeaseID.String)
	}
	if leaseExpiresAt.Valid {
		t.LeaseExpiresAt = ptr(leaseExpiresAt.String)
	}
	if mergedFactID.Valid {
		t.MergedFactID = ptr(mergedFactID.String)
	}

	// JSON TEXT columns -> json.RawMessage
	t.ForbiddenPatterns = rawMessageOrDefault(forbiddenPatterns)
	t.RequiredAPIs = rawMessageOrDefault(requiredAPIs)
	t.Dependencies = rawMessageOrDefault(dependencies)
	t.TestRequirements = rawMessageOrDefault(testRequirements)

	return &t, nil
}

// ---------------------------------------------------------------------------
// TaskStore implementation
// ---------------------------------------------------------------------------

// Create inserts a new task. t.ID is pre-generated by the caller (Service layer).
func (s *SQLiteTaskStore) Create(ctx context.Context, projectID string, t *model.Task) error {
	assignedSessionKey, err := resolveNullableSessionKey(ctx, s.db, projectID, t.AssignedSessionID)
	if err != nil {
		return fmt.Errorf("task create assigned session: %w", err)
	}
	verifiedByKey, err := resolveNullableSessionKey(ctx, s.db, projectID, t.VerifiedBy)
	if err != nil {
		return fmt.Errorf("task create verifier: %w", err)
	}
	const query = `INSERT INTO tasks (
		id, project_id, feature_id, title, description, role, status,
		allowed_directories, forbidden_patterns, required_apis, dependencies,
		parent_task_id, relation_type, test_requirements,
		assigned_session_id, assigned_worker_id, assigned_at,
		blocker_reason, cancel_reason, merge_commit,
		verified_by, verified_at,
		priority, summary, version, lease_epoch, active_lease_id, lease_expires_at, merged_fact_id,
		created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?,
		?, ?, ?,
		?, ?, ?,
		?, ?,
		?, ?, ?, ?, ?, ?, ?,
		?, ?
	)`
	_, err = s.db.ExecContext(ctx, query,
		t.ID, projectID, t.FeatureID, t.Title, t.Description, t.Role, t.Status,
		t.AllowedDirectories, t.ForbiddenPatterns, t.RequiredAPIs, t.Dependencies,
		t.ParentTaskID, t.RelationType, t.TestRequirements,
		assignedSessionKey, t.AssignedWorkerID, t.AssignedAt,
		t.BlockerReason, t.CancelReason, t.MergeCommit,
		verifiedByKey, t.VerifiedAt,
		t.Priority, t.Summary, t.Version, t.LeaseEpoch, t.ActiveLeaseID, t.LeaseExpiresAt, t.MergedFactID,
		t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("task create: %w", err)
	}
	return nil
}

// GetByID retrieves a task by ID within a project. Returns ErrTaskNotFound if not found.
func (s *SQLiteTaskStore) GetByID(ctx context.Context, projectID, id string) (*model.Task, error) {
	query := fmt.Sprintf(`SELECT %s FROM tasks AS t WHERE t.project_id = ? AND t.id = ?`, taskColumns)
	row := s.db.QueryRowContext(ctx, query, projectID, id)
	t, err := scanTask(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("task get by id: %w", err)
	}
	return t, nil
}

// List returns tasks matching the filter. Empty filter fields mean "no filter".
func (s *SQLiteTaskStore) List(ctx context.Context, projectID string, filter TaskFilter) ([]*model.Task, error) {
	var conditions []string
	var args []any

	conditions = append(conditions, "t.project_id = ?")
	args = append(args, projectID)

	if filter.Status != "" {
		conditions = append(conditions, "t.status = ?")
		args = append(args, filter.Status)
	}
	if filter.Role != "" {
		conditions = append(conditions, "t.role = ?")
		args = append(args, filter.Role)
	}
	if filter.FeatureID != "" {
		conditions = append(conditions, "t.feature_id = ?")
		args = append(args, filter.FeatureID)
	}

	where := strings.Join(conditions, " AND ")
	query := fmt.Sprintf(`SELECT %s FROM tasks AS t WHERE %s ORDER BY t.created_at ASC`, taskColumns, where) //nolint:gosec // taskColumns is a const, where uses parameterized values

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("task list: %w", err)
	}
	defer rows.Close()

	var tasks []*model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("task list scan: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task list rows: %w", err)
	}
	return tasks, nil
}

// UpdateStatus atomically updates a task's status and updated_at timestamp.
func (s *SQLiteTaskStore) UpdateStatus(ctx context.Context, projectID, taskID, newStatus string) error {
	current, err := s.GetByID(ctx, projectID, taskID)
	if err != nil {
		return err
	}
	return s.UpdateStatusFromVersion(ctx, projectID, taskID, current.Status, current.Version, newStatus)
}

// UpdateStatusFrom atomically updates task status only if the current status
// matches expectedOldStatus. Returns ErrTaskNotFound if the task doesn't exist
// or its status has changed concurrently.
func (s *SQLiteTaskStore) UpdateStatusFrom(ctx context.Context, projectID, taskID, expectedOldStatus, newStatus string) error {
	var version int64
	err := s.db.QueryRowContext(ctx,
		`SELECT version FROM tasks WHERE project_id = ? AND id = ? AND status = ?`,
		projectID, taskID, expectedOldStatus,
	).Scan(&version)
	if err == sql.ErrNoRows {
		return ErrConcurrentConflict
	}
	if err != nil {
		return fmt.Errorf("task read version for status update: %w", err)
	}
	return s.UpdateStatusFromVersion(ctx, projectID, taskID, expectedOldStatus, version, newStatus)
}

func (s *SQLiteTaskStore) UpdateStatusFromVersion(ctx context.Context, projectID, taskID, expectedOldStatus string, expectedVersion int64, newStatus string) error {
	if !model.IsTaskStatus(newStatus) || !model.CanTaskTransition(expectedOldStatus, newStatus) {
		return fmt.Errorf("task transition %s -> %s: %w", expectedOldStatus, newStatus, ErrTaskStateInvalid)
	}
	const query = `UPDATE tasks
		SET status = ?, version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND id = ? AND status = ? AND version = ?`
	result, err := s.db.ExecContext(ctx, query, newStatus, projectID, taskID, expectedOldStatus, expectedVersion)
	if err != nil {
		return fmt.Errorf("task update status from %s version %d: %w", expectedOldStatus, expectedVersion, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("task update status rows affected: %w", err)
	}
	if n == 0 {
		return ErrConcurrentConflict
	}
	return nil
}

// Update modifies editable task fields.
func (s *SQLiteTaskStore) Update(ctx context.Context, projectID string, t *model.Task) error {
	var currentStatus string
	if err := s.db.QueryRowContext(ctx,
		`SELECT status FROM tasks WHERE project_id = ? AND id = ? AND version = ?`,
		projectID, t.ID, t.Version,
	).Scan(&currentStatus); err != nil {
		if err == sql.ErrNoRows {
			return ErrConcurrentConflict
		}
		return fmt.Errorf("task update read current state: %w", err)
	}
	if !model.IsTaskStatus(t.Status) || !model.CanTaskTransition(currentStatus, t.Status) {
		return fmt.Errorf("task update transition %s -> %s: %w", currentStatus, t.Status, ErrTaskStateInvalid)
	}
	assignedSessionKey, err := resolveNullableSessionKey(ctx, s.db, projectID, t.AssignedSessionID)
	if err != nil {
		return fmt.Errorf("task update assigned session: %w", err)
	}
	verifiedByKey, err := resolveNullableSessionKey(ctx, s.db, projectID, t.VerifiedBy)
	if err != nil {
		return fmt.Errorf("task update verifier: %w", err)
	}
	const query = `UPDATE tasks SET
		status = ?, title = ?, description = ?, role = ?, priority = ?,
		feature_id = ?, allowed_directories = ?,
		forbidden_patterns = ?, required_apis = ?,
		dependencies = ?, test_requirements = ?,
		parent_task_id = ?, relation_type = ?,
		summary = ?, merge_commit = ?, merged_fact_id = ?,
			assigned_session_id = ?, assigned_worker_id = ?, assigned_at = ?,
			blocker_reason = ?, cancel_reason = ?,
			verified_by = ?, verified_at = ?,
		version = version + 1, updated_at = datetime('now')
	WHERE project_id = ? AND id = ? AND version = ?`
	result, err := s.db.ExecContext(ctx, query,
		t.Status, t.Title, t.Description, t.Role, t.Priority,
		t.FeatureID, t.AllowedDirectories,
		t.ForbiddenPatterns, t.RequiredAPIs,
		t.Dependencies, t.TestRequirements,
		t.ParentTaskID, t.RelationType,
		t.Summary, t.MergeCommit, t.MergedFactID,
		assignedSessionKey, t.AssignedWorkerID, t.AssignedAt,
		t.BlockerReason, t.CancelReason,
		verifiedByKey, t.VerifiedAt,
		projectID, t.ID, t.Version,
	)
	if err != nil {
		return fmt.Errorf("task update: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("task update rows affected: %w", err)
	}
	if n == 0 {
		return ErrConcurrentConflict
	}
	t.Version++
	return nil
}

// FindNextAvailable finds the next claimable queued task for the given role,
// considering dynamic dependency resolution.
// Dependencies are satisfied when:
//   - If require_state='validating': dep task is validating/ready_for_human_merge/done/cancelled
//   - Otherwise (default 'done'): dep task is in done/cancelled
//   - Cancelled dependencies are always treated as satisfied (never block downstream).
func (s *SQLiteTaskStore) FindNextAvailable(ctx context.Context, projectID, role string) (*model.Task, error) {
	const depCheckSQL = `
		SELECT 1 FROM json_each(t.dependencies) AS dep
		LEFT JOIN tasks AS dep_task
			ON dep_task.project_id = ?
			AND dep_task.id = json_extract(dep.value, '$.task_id')
		WHERE dep_task.id IS NULL
		   OR (
			   COALESCE(json_extract(dep.value, '$.require_state'), 'done') = 'validating'
			   AND dep_task.status NOT IN ('validating','ready_for_human_merge','done','cancelled')
		   )
		   OR (
			   COALESCE(json_extract(dep.value, '$.require_state'), 'done') <> 'validating'
			   AND dep_task.status NOT IN ('done','cancelled')
		   )`

	query := fmt.Sprintf(`
			SELECT %s FROM tasks AS t
			WHERE t.project_id = ? AND t.role = ? AND t.status = 'queued'
		  AND NOT EXISTS (%s)
		ORDER BY
		  CASE t.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
		  t.created_at ASC
		LIMIT 1`, taskColumns, depCheckSQL)

	// depCheckSQL uses projectID as its first parameter, so we pass it twice:
	// once for the outer WHERE, once for the dep_task sub-query.
	row := s.db.QueryRowContext(ctx, query, projectID, role, projectID)
	t, err := scanTask(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNoAvailableTask
		}
		return nil, fmt.Errorf("task find next available: %w", err)
	}
	return t, nil
}

// FindNextSubmitted finds the next unclaimed validating task for verification.
// Does not filter by role; orders by created_at.
func (s *SQLiteTaskStore) FindNextSubmitted(ctx context.Context, projectID string) (*model.Task, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM tasks AS t
		WHERE t.project_id = ? AND t.status = 'validating' AND t.verified_by IS NULL
		ORDER BY t.created_at ASC
		LIMIT 1`, taskColumns)

	row := s.db.QueryRowContext(ctx, query, projectID)
	t, err := scanTask(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNoAvailableTask
		}
		return nil, fmt.Errorf("task find next submitted: %w", err)
	}
	return t, nil
}

// Claim is retained only for source compatibility. A claim spans Task, Lease,
// Worker, history and activity rows and therefore must be owned by TaskService.
func (s *SQLiteTaskStore) Claim(context.Context, string, string, string, string) error {
	return fmt.Errorf("task store claim: %w", ErrOperationDisabled)
}

// ClaimVerification atomically transitions a task from submitted to verifying.
// Does not modify assigned_session_id/assigned_worker_id (keeps pointing to the original executor).
func (s *SQLiteTaskStore) ClaimVerification(ctx context.Context, projectID, taskID, verifierSessionID, _ string) error {
	verifierKey, err := resolveSessionKey(ctx, s.db, projectID, verifierSessionID)
	if err != nil {
		return fmt.Errorf("task claim verification session: %w", err)
	}
	const query = `UPDATE tasks
		SET verified_by = ?, verified_at = datetime('now'),
		    version = version + 1,
		    updated_at = datetime('now')
		WHERE id = ? AND project_id = ? AND status = 'validating' AND verified_by IS NULL`

	result, err := s.db.ExecContext(ctx, query, verifierKey, taskID, projectID)
	if err != nil {
		return fmt.Errorf("task claim verification: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("task claim verification rows affected: %w", err)
	}
	if n == 0 {
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM tasks WHERE id = ? AND project_id = ?`, taskID, projectID,
		).Scan(&exists)
		if err == sql.ErrNoRows {
			return ErrTaskNotFound
		}
		if err != nil {
			return fmt.Errorf("task claim verification existence check: %w", err)
		}
		return ErrTaskStateInvalid
	}
	return nil
}

// CountByStatus returns a map of status -> count for all tasks in a project.
func (s *SQLiteTaskStore) CountByStatus(ctx context.Context, projectID string) (map[string]int, error) {
	const query = `SELECT status, COUNT(*) FROM tasks WHERE project_id = ? GROUP BY status`
	rows, err := s.db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("task count by status: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("task count by status scan: %w", err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task count by status rows: %w", err)
	}
	return counts, nil
}

// CheckDependencies checks whether all given dependencies are satisfied.
// A dependency is satisfied when the target task has reached the required state.
// Cancelled tasks are treated as satisfied (never block downstream).
func (s *SQLiteTaskStore) CheckDependencies(ctx context.Context, projectID string, deps []model.Dependency) (bool, error) {
	if len(deps) == 0 {
		return true, nil
	}

	for _, dep := range deps {
		requireState := dep.RequireState
		if requireState == "" {
			requireState = model.TaskStatusDone
		}

		var statuses []string
		canonicalRequireState, ok := model.LegacyTaskStatusToCanonical(requireState)
		if !ok {
			return false, fmt.Errorf("check dependency %s: %w: unknown require_state %q", dep.TaskID, ErrInvalidParameter, requireState)
		}
		if canonicalRequireState == model.TaskStatusValidating {
			// validating means the dependency reached server-side validation.
			statuses = []string{
				model.TaskStatusValidating,
				model.TaskStatusReadyForHumanMerge,
				model.TaskStatusDone,
				model.TaskStatusCancelled,
			}
		} else {
			// default "done": must be done or cancelled
			statuses = []string{
				model.TaskStatusDone,
				model.TaskStatusCancelled,
			}
		}

		placeholders := make([]string, len(statuses))
		args := make([]any, 0, 2+len(statuses))
		args = append(args, projectID, dep.TaskID)
		for i, st := range statuses {
			placeholders[i] = "?"
			args = append(args, st)
		}

		query := fmt.Sprintf( //nolint:gosec // placeholders are "?" constants
			`SELECT COUNT(*) FROM tasks WHERE project_id = ? AND id = ? AND status IN (%s)`,
			strings.Join(placeholders, ","),
		)

		var count int
		if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			return false, fmt.Errorf("check dependency %s: %w", dep.TaskID, err)
		}
		if count == 0 {
			return false, nil
		}
	}
	return true, nil
}

// CheckCircular checks whether adding deps to taskID would create a circular dependency.
// Returns true if a cycle would be formed, false otherwise.
func (s *SQLiteTaskStore) CheckCircular(ctx context.Context, projectID, taskID string, deps []model.Dependency) (bool, error) {
	if len(deps) == 0 {
		return false, nil
	}

	// Direct self-reference check.
	for _, dep := range deps {
		if dep.TaskID == taskID {
			return true, nil
		}
	}

	// BFS: walk the dependency graph starting from each dep, checking if taskID is reachable.
	visited := make(map[string]bool)
	queue := make([]string, 0, len(deps))
	for _, dep := range deps {
		queue = append(queue, dep.TaskID)
	}

	for len(queue) > 0 {
		currentID := queue[0]
		queue = queue[1:]

		if currentID == taskID {
			return true, nil
		}
		if visited[currentID] {
			continue
		}
		visited[currentID] = true

		// Look up current task's dependencies.
		var depsJSON sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT dependencies FROM tasks WHERE project_id = ? AND id = ?`,
			projectID, currentID,
		).Scan(&depsJSON)
		if err == sql.ErrNoRows {
			// Task not found, skip.
			continue
		}
		if err != nil {
			return false, fmt.Errorf("check circular for task %s: %w", currentID, err)
		}
		if !depsJSON.Valid || depsJSON.String == "" || depsJSON.String == "[]" {
			continue
		}

		var depList []model.Dependency
		if err := json.Unmarshal([]byte(depsJSON.String), &depList); err != nil {
			continue
		}
		for _, d := range depList {
			if !visited[d.TaskID] {
				queue = append(queue, d.TaskID)
			}
		}
	}

	return false, nil
}

// Compile-time interface assertion.
var _ TaskStore = (*SQLiteTaskStore)(nil)
