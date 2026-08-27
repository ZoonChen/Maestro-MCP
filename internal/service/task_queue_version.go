package service

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// insertNewTaskTx persists only creation-time fields. Execution authority,
// lease, verification and merge facts are deliberately initialized by their
// dedicated state transitions rather than accepted from a create request.
func insertNewTaskTx(ctx context.Context, tx *sql.Tx, projectID string, task *model.Task) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO tasks (
		id, project_id, feature_id, title, description, role, status,
		allowed_directories, forbidden_patterns, required_apis, dependencies,
		parent_task_id, relation_type, test_requirements,
		assigned_session_id, assigned_worker_id, assigned_at,
		blocker_reason, cancel_reason, merge_commit, verified_by, verified_at,
		priority, summary, version, lease_epoch, active_lease_id, lease_expires_at,
		merged_fact_id, created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
		?, ?, 0, 0, NULL, NULL, NULL, ?, ?
	)`,
		task.ID, projectID, task.FeatureID, task.Title, task.Description, task.Role, task.Status,
		task.AllowedDirectories, task.ForbiddenPatterns, task.RequiredAPIs, task.Dependencies,
		task.ParentTaskID, task.RelationType, task.TestRequirements,
		task.Priority, task.Summary, task.CreatedAt, task.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert task %s: %w", task.ID, err)
	}
	return nil
}

func validTaskRole(role string) bool {
	switch role {
	case model.RoleBackend, model.RoleFrontend, model.RoleDevops, model.RoleVerifier, model.RoleCoordinator:
		return true
	default:
		return false
	}
}

func validTaskPriority(priority string) bool {
	switch priority {
	case model.PriorityLow, model.PriorityNormal, model.PriorityHigh, model.PriorityUrgent:
		return true
	default:
		return false
	}
}

// queuedTaskOrderingChanged reports whether an edit changes either a queue
// partition (role), its ordering (priority), or eligibility (dependencies).
func queuedTaskOrderingChanged(before, after *model.Task) bool {
	return before.Status == model.TaskStatusQueued &&
		(before.Role != after.Role ||
			before.Priority != after.Priority ||
			string(before.Dependencies) != string(after.Dependencies))
}

// bumpProjectQueueVersionTx advances the CAS token for a project's claimable
// queue. The queue row is application-owned state: projects created after the
// schema migration do not have one yet, so initialization and increment must
// happen in the caller's business transaction.
func bumpProjectQueueVersionTx(ctx context.Context, tx *sql.Tx, projectID string) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO project_queue_versions(project_id, version) VALUES (?, 0)`,
		projectID,
	); err != nil {
		return fmt.Errorf("initialize project queue version: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE project_queue_versions SET version = version + 1 WHERE project_id = ?`,
		projectID,
	)
	if err != nil {
		return fmt.Errorf("increment project queue version: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("increment project queue version rows affected: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("project queue version row missing for %s: %w", projectID, store.ErrRecoveryIntegrity)
	}
	return nil
}
