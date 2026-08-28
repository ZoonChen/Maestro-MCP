package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/google/uuid"
)

// Plan-phase row mapping. Each planner scans one frozen SQLite table and
// produces either a fully validated target row or a quarantine entry for
// that row itself (never for the referenced parent); rows already mapped by
// a previous import run are counted as already_imported and never
// re-inserted (idempotency by source row identity).

// importWorkItemRef tracks every non-quarantined source task with its
// canonical status and target UUID so reconcile covers already-imported
// rows too, not just this run's plan.
type importWorkItemRef struct {
	legacyID string
	status   string
	targetID uuid.UUID
}

func (i *SQLiteImporter) planProjects(ctx context.Context, plan *importPlan) error {
	rows, err := i.sqlite.QueryContext(ctx, `
		SELECT id, name, workspace_path, COALESCE(description,''), status,
		       COALESCE(config,'{}'), created_at, updated_at
		FROM projects ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("read sqlite projects: %w", err)
	}
	defer rows.Close()

	usedKeys := map[string]struct{}{}
	usedTeamNames := map[string]struct{}{}
	for rows.Next() {
		var legacyID, name, workspacePath, description, status, config, createdAt, updatedAt string
		if err := rows.Scan(&legacyID, &name, &workspacePath, &description, &status, &config, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("scan sqlite projects: %w", err)
		}
		plan.sourceCount["projects"]++

		if _, ok, mapErr := i.mappedTarget(ctx, "projects", legacyID, "projects"); mapErr != nil {
			return mapErr
		} else if ok {
			plan.already["projects"]++
			continue
		}

		switch status {
		case "active", "archived":
		default:
			plan.addQuarantine("projects", legacyID, fmt.Sprintf("project status %q has no v3 mapping", status))
			continue
		}
		configJSON, jsonErr := validJSONOr(config, "{}")
		if jsonErr != nil {
			plan.addQuarantine("projects", legacyID, "config is not valid JSON")
			continue
		}
		created, timeErr := parseSQLiteTimestamp(createdAt)
		if timeErr != nil {
			plan.addQuarantine("projects", legacyID, timeErr.Error())
			continue
		}
		updated, timeErr := parseSQLiteTimestamp(updatedAt)
		if timeErr != nil {
			plan.addQuarantine("projects", legacyID, timeErr.Error())
			continue
		}

		key := slugifyProjectKey(name, legacyID, usedKeys)
		teamName := "legacy-" + key
		for suffix := 2; ; suffix++ {
			if _, taken := usedTeamNames[teamName]; !taken {
				break
			}
			teamName = fmt.Sprintf("legacy-%s-%d", key, suffix)
		}
		usedTeamNames[teamName] = struct{}{}

		plan.projects = append(plan.projects, importProject{
			legacyID:      legacyID,
			teamID:        newImportUUID(),
			teamName:      teamName,
			projectID:     newImportUUID(),
			key:           key,
			name:          name,
			description:   description,
			status:        status,
			config:        configJSON,
			createdAt:     created,
			updatedAt:     updated,
			workspacePath: workspacePath,
		})
		// No membership is synthesized: the owner of legacy data is unknown
		// and the project stays inaccessible until a human assigns one
		// (SEC-IDENTITY-RBAC section 13 manual checklist).
		plan.checklist = append(plan.checklist,
			fmt.Sprintf("project %s (key=%s) imported without owner membership; assign an owner before activation", legacyID, key))
	}
	return rows.Err()
}

func (i *SQLiteImporter) planFeatures(ctx context.Context, plan *importPlan) error {
	rows, err := i.sqlite.QueryContext(ctx, `
		SELECT id, project_id, title, COALESCE(description,''), COALESCE(reference_urls,'[]'),
		       status, created_at, updated_at
		FROM features ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("read sqlite features: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var legacyID, projectID, title, description, referenceURLs, status, createdAt, updatedAt string
		if err := rows.Scan(&legacyID, &projectID, &title, &description, &referenceURLs, &status, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("scan sqlite features: %w", err)
		}
		plan.sourceCount["features"]++

		if _, ok, mapErr := i.mappedTarget(ctx, "features", legacyID, "features"); mapErr != nil {
			return mapErr
		} else if ok {
			plan.already["features"]++
			continue
		}

		targetProject, ok := plan.resolveProject(ctx, i, projectID)
		if !ok {
			plan.addQuarantine("features", legacyID, "parent project not importable")
			continue
		}
		switch status {
		case "planning", "active", "completed", "closed":
		default:
			plan.addQuarantine("features", legacyID, fmt.Sprintf("feature status %q has no v3 mapping", status))
			continue
		}
		referenceJSON, jsonErr := validJSONOr(referenceURLs, "[]")
		if jsonErr != nil {
			plan.addQuarantine("features", legacyID, "reference_urls is not valid JSON")
			continue
		}
		created, timeErr := parseSQLiteTimestamp(createdAt)
		if timeErr != nil {
			plan.addQuarantine("features", legacyID, timeErr.Error())
			continue
		}
		updated, timeErr := parseSQLiteTimestamp(updatedAt)
		if timeErr != nil {
			plan.addQuarantine("features", legacyID, timeErr.Error())
			continue
		}

		plan.features = append(plan.features, importFeature{
			legacyID:      legacyID,
			projectID:     targetProject,
			featureID:     newImportUUID(),
			title:         title,
			description:   description,
			referenceURLs: referenceJSON,
			status:        status,
			createdAt:     created,
			updatedAt:     updated,
		})
	}
	return rows.Err()
}

func (i *SQLiteImporter) planWorkItems(ctx context.Context, plan *importPlan) error {
	rows, err := i.sqlite.QueryContext(ctx, `
		SELECT id, project_id, feature_id, title, COALESCE(description,''), status, priority,
		       COALESCE(role,''), COALESCE(dependencies,'[]'), COALESCE(test_requirements,'{}'),
		       COALESCE(version,0), COALESCE(lease_epoch,0),
		       assigned_session_id, assigned_worker_id, merge_commit,
		       created_at, updated_at
		FROM tasks ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("read sqlite tasks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			legacyID, projectID, featureID, title, description, status, priority string
			role, dependencies, testRequirements                                 string
			version, leaseEpoch                                                  int64
			assignedSession, assignedWorker, mergeCommit                         sql.NullString
			createdAt, updatedAt                                                 string
		)
		if err := rows.Scan(&legacyID, &projectID, &featureID, &title, &description, &status, &priority,
			&role, &dependencies, &testRequirements, &version, &leaseEpoch,
			&assignedSession, &assignedWorker, &mergeCommit, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("scan sqlite tasks: %w", err)
		}
		plan.sourceCount["tasks"]++

		if mapped, ok, mapErr := i.mappedTarget(ctx, "tasks", legacyID, "work_items"); mapErr != nil {
			return mapErr
		} else if ok {
			plan.already["tasks"]++
			if canonical, known := model.LegacyTaskStatusToCanonical(status); known {
				plan.workItemRefs = append(plan.workItemRefs, importWorkItemRef{legacyID: legacyID, status: canonical, targetID: mapped})
			}
			continue
		}

		canonical, known := model.LegacyTaskStatusToCanonical(status)
		if !known {
			plan.addQuarantine("tasks", legacyID, fmt.Sprintf("task status %q is not a known legacy or canonical state", status))
			continue
		}
		if canonical == model.TaskStatusDone && !mergeCommit.Valid {
			plan.addQuarantine("tasks", legacyID, "done without merge commit cannot satisfy DATA-INV-003")
			continue
		}
		switch priority {
		case "low", "normal", "high", "urgent":
		default:
			plan.addQuarantine("tasks", legacyID, fmt.Sprintf("priority %q has no v3 mapping", priority))
			continue
		}
		targetProject, ok := plan.resolveProject(ctx, i, projectID)
		if !ok {
			plan.addQuarantine("tasks", legacyID, "parent project not importable")
			continue
		}
		var targetFeature *uuid.UUID
		if featureID != "" {
			resolved, featureOK := plan.resolveFeature(ctx, i, featureID)
			if !featureOK {
				plan.addQuarantine("tasks", legacyID, "referenced feature not importable")
				continue
			}
			targetFeature = &resolved
		}
		dependenciesJSON, jsonErr := validJSONOr(dependencies, "[]")
		if jsonErr != nil {
			plan.addQuarantine("tasks", legacyID, "dependencies is not valid JSON")
			continue
		}
		requirementsJSON, jsonErr := validJSONOr(testRequirements, "{}")
		if jsonErr != nil {
			plan.addQuarantine("tasks", legacyID, "test_requirements is not valid JSON")
			continue
		}
		created, timeErr := parseSQLiteTimestamp(createdAt)
		if timeErr != nil {
			plan.addQuarantine("tasks", legacyID, timeErr.Error())
			continue
		}
		updated, timeErr := parseSQLiteTimestamp(updatedAt)
		if timeErr != nil {
			plan.addQuarantine("tasks", legacyID, timeErr.Error())
			continue
		}

		itemID := newImportUUID()
		plan.workItems = append(plan.workItems, importWorkItem{
			legacyID:         legacyID,
			projectID:        targetProject,
			workItemID:       itemID,
			featureID:        targetFeature,
			title:            title,
			description:      description,
			status:           canonical,
			priority:         priority,
			role:             nullableString(role),
			dependencies:     dependenciesJSON,
			testRequirements: requirementsJSON,
			version:          version,
			leaseEpoch:       leaseEpoch,
			legacySessionID:  nullStringPtr(assignedSession),
			legacyWorkerID:   nullStringPtr(assignedWorker),
			mergeCommit:      nullStringPtr(mergeCommit),
			createdAt:        created,
			updatedAt:        updated,
		})
		plan.workItemRefs = append(plan.workItemRefs, importWorkItemRef{legacyID: legacyID, status: canonical, targetID: itemID})
	}
	return rows.Err()
}

func (i *SQLiteImporter) planLeases(ctx context.Context, plan *importPlan) error {
	rows, err := i.sqlite.QueryContext(ctx, `
		SELECT id, project_id, task_id, session_id, worker_id, epoch, status,
		       COALESCE(version,1), expires_at, created_at, updated_at
		FROM task_leases ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("read sqlite task_leases: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var legacyID, projectID, taskID, sessionID, workerID, status, expiresAt, createdAt, updatedAt string
		var epoch, version int64
		if err := rows.Scan(&legacyID, &projectID, &taskID, &sessionID, &workerID, &epoch, &status,
			&version, &expiresAt, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("scan sqlite task_leases: %w", err)
		}
		plan.sourceCount["task_leases"]++

		if _, ok, mapErr := i.mappedTarget(ctx, "task_leases", legacyID, "leases"); mapErr != nil {
			return mapErr
		} else if ok {
			plan.already["task_leases"]++
			continue
		}

		switch status {
		case "active", "completed", "released", "expired", "cancelled":
		default:
			plan.addQuarantine("task_leases", legacyID, fmt.Sprintf("lease status %q has no v3 mapping", status))
			continue
		}
		targetProject, targetItem, ok, resolveErr := i.resolveWorkItem(ctx, plan, projectID, taskID)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			plan.addQuarantine("task_leases", legacyID, "parent task not importable")
			continue
		}
		expires, timeErr := parseSQLiteTimestamp(expiresAt)
		if timeErr != nil {
			plan.addQuarantine("task_leases", legacyID, timeErr.Error())
			continue
		}
		created, timeErr := parseSQLiteTimestamp(createdAt)
		if timeErr != nil {
			plan.addQuarantine("task_leases", legacyID, timeErr.Error())
			continue
		}
		updated, timeErr := parseSQLiteTimestamp(updatedAt)
		if timeErr != nil {
			plan.addQuarantine("task_leases", legacyID, timeErr.Error())
			continue
		}
		if version < 1 {
			version = 1
		}

		plan.leases = append(plan.leases, importLease{
			legacyID:        legacyID,
			projectID:       targetProject,
			workItemID:      targetItem,
			leaseID:         newImportUUID(),
			legacySessionID: sessionID,
			legacyWorkerID:  workerID,
			epoch:           epoch,
			status:          status,
			version:         version,
			expiresAt:       expires,
			createdAt:       created,
			updatedAt:       updated,
		})
	}
	return rows.Err()
}

func (i *SQLiteImporter) planWorktrees(ctx context.Context, plan *importPlan) error {
	rows, err := i.sqlite.QueryContext(ctx, `
		SELECT id, project_id, task_id, session_id, worktree_path, branch_name,
		       base_commit, status, COALESCE(generation,1), COALESCE(version,0),
		       created_at, COALESCE(updated_at, created_at)
		FROM worktrees ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("read sqlite worktrees: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var legacyID, projectID, taskID, workspacePath, branchName, baseCommit, status, createdAt, updatedAt string
		var sessionID sql.NullString
		var generation, version int64
		if err := rows.Scan(&legacyID, &projectID, &taskID, &sessionID, &workspacePath, &branchName,
			&baseCommit, &status, &generation, &version, &createdAt, &updatedAt); err != nil {
			return fmt.Errorf("scan sqlite worktrees: %w", err)
		}
		plan.sourceCount["worktrees"]++

		if _, ok, mapErr := i.mappedTarget(ctx, "worktrees", legacyID, "worktrees"); mapErr != nil {
			return mapErr
		} else if ok {
			plan.already["worktrees"]++
			continue
		}

		switch status {
		case "allocated", "active", "sealed", "submitted", "stale",
			"merged", "abandoned", "quarantined", "cleanup_pending":
		default:
			plan.addQuarantine("worktrees", legacyID, fmt.Sprintf("workspace status %q has no v3 mapping", status))
			continue
		}
		targetProject, targetItem, ok, resolveErr := i.resolveWorkItem(ctx, plan, projectID, taskID)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			plan.addQuarantine("worktrees", legacyID, "parent task not importable")
			continue
		}
		created, timeErr := parseSQLiteTimestamp(createdAt)
		if timeErr != nil {
			plan.addQuarantine("worktrees", legacyID, timeErr.Error())
			continue
		}
		updated, timeErr := parseSQLiteTimestamp(updatedAt)
		if timeErr != nil {
			plan.addQuarantine("worktrees", legacyID, timeErr.Error())
			continue
		}
		if generation < 1 {
			generation = 1
		}

		plan.worktrees = append(plan.worktrees, importWorktree{
			legacyID:      legacyID,
			projectID:     targetProject,
			workItemID:    targetItem,
			worktreeID:    newImportUUID(),
			sessionID:     nullStringPtr(sessionID),
			workspacePath: workspacePath,
			branchName:    branchName,
			baseCommit:    baseCommit,
			status:        status,
			generation:    generation,
			version:       version,
			createdAt:     created,
			updatedAt:     updated,
		})
	}
	return rows.Err()
}

func (i *SQLiteImporter) planValidationRuns(ctx context.Context, plan *importPlan) error {
	rows, err := i.sqlite.QueryContext(ctx, `
		SELECT id, project_id, task_id, attempt, base_commit, COALESCE(changed_files,'[]'),
		       COALESCE(profile_ref,''), COALESCE(policy_version,''), COALESCE(policy_digest,''),
		       coverage, COALESCE(boundary_ok,0), COALESCE(test_ok,0),
		       COALESCE(coverage_ok,0), result, error_code,
		       COALESCE(duration_ms,0), created_at
		FROM validation_runs ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("read sqlite validation_runs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var legacyID, projectID, taskID string
		var attempt int
		var baseCommit, changedFiles, profileRef, policyVersion, policyDigest string
		var coverage sql.NullFloat64
		var boundaryOK, testOK, coverageOK int
		var result string
		var errorCode sql.NullString
		var durationMs int64
		var createdAt string
		if err := rows.Scan(&legacyID, &projectID, &taskID, &attempt, &baseCommit, &changedFiles,
			&profileRef, &policyVersion, &policyDigest, &coverage,
			&boundaryOK, &testOK, &coverageOK, &result, &errorCode, &durationMs, &createdAt); err != nil {
			return fmt.Errorf("scan sqlite validation_runs: %w", err)
		}
		plan.sourceCount["validation_runs"]++

		if _, ok, mapErr := i.mappedTarget(ctx, "validation_runs", legacyID, "validation_runs"); mapErr != nil {
			return mapErr
		} else if ok {
			plan.already["validation_runs"]++
			continue
		}

		if attempt < 1 {
			plan.addQuarantine("validation_runs", legacyID, "attempt must be positive")
			continue
		}
		changedJSON, jsonErr := validJSONOr(changedFiles, "[]")
		if jsonErr != nil {
			plan.addQuarantine("validation_runs", legacyID, "changed_files is not valid JSON")
			continue
		}
		targetProject, targetItem, ok, resolveErr := i.resolveWorkItem(ctx, plan, projectID, taskID)
		if resolveErr != nil {
			return resolveErr
		}
		if !ok {
			plan.addQuarantine("validation_runs", legacyID, "parent task not importable")
			continue
		}
		created, timeErr := parseSQLiteTimestamp(createdAt)
		if timeErr != nil {
			plan.addQuarantine("validation_runs", legacyID, timeErr.Error())
			continue
		}

		var coveragePtr *float64
		if coverage.Valid {
			coveragePtr = &coverage.Float64
		}
		plan.validation = append(plan.validation, importValidationRun{
			legacyID:      legacyID,
			projectID:     targetProject,
			workItemID:    targetItem,
			attempt:       attempt,
			baseCommit:    baseCommit,
			changedFiles:  changedJSON,
			profileRef:    profileRef,
			policyVersion: policyVersion,
			policyDigest:  policyDigest,
			result:        result,
			errorCode:     nullStringPtr(errorCode),
			durationMs:    durationMs,
			coverage:      coveragePtr,
			boundaryOK:    boundaryOK != 0,
			testOK:        testOK != 0,
			coverageOK:    coverageOK != 0,
			createdAt:     created,
		})
	}
	return rows.Err()
}

// ---------------------------------------------------------------------------
// Parent resolution helpers: planned rows first, then previously imported
// rows via legacy_id_map. They never quarantine; callers own that decision.
// ---------------------------------------------------------------------------

func (plan *importPlan) resolveProject(ctx context.Context, i *SQLiteImporter, legacyID string) (uuid.UUID, bool) {
	for _, project := range plan.projects {
		if project.legacyID == legacyID {
			return project.projectID, true
		}
	}
	if mapped, ok, err := i.mappedTarget(ctx, "projects", legacyID, "projects"); err == nil && ok {
		return mapped, true
	}
	return uuid.Nil, false
}

func (plan *importPlan) resolveFeature(ctx context.Context, i *SQLiteImporter, legacyID string) (uuid.UUID, bool) {
	for _, feature := range plan.features {
		if feature.legacyID == legacyID {
			return feature.featureID, true
		}
	}
	if mapped, ok, err := i.mappedTarget(ctx, "features", legacyID, "features"); err == nil && ok {
		return mapped, true
	}
	return uuid.Nil, false
}

// resolveWorkItem returns the (project, work item) UUID pair for a legacy
// task reference. When the task was imported by a previous run, the project
// is read from the persisted row so a missing project map entry can never
// produce a NULL project_id insert.
func (i *SQLiteImporter) resolveWorkItem(ctx context.Context, plan *importPlan, _, taskLegacy string) (uuid.UUID, uuid.UUID, bool, error) {
	for _, item := range plan.workItems {
		if item.legacyID == taskLegacy {
			return item.projectID, item.workItemID, true, nil
		}
	}
	item, ok, err := i.mappedTarget(ctx, "tasks", taskLegacy, "work_items")
	if err != nil {
		return uuid.Nil, uuid.Nil, false, err
	}
	if !ok {
		return uuid.Nil, uuid.Nil, false, nil
	}
	var project uuid.UUID
	lookupErr := i.pg.QueryRowContext(ctx,
		`SELECT project_id FROM work_items WHERE id = $1`, item).Scan(&project)
	if lookupErr != nil {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("resolve project for imported task %s: %w", taskLegacy, lookupErr)
	}
	return project, item, true, nil
}

// ---------------------------------------------------------------------------
// Small scan helpers
// ---------------------------------------------------------------------------

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullStringPtr(value sql.NullString) *string {
	if !value.Valid || value.String == "" {
		return nil
	}
	return &value.String
}

// newImportUUID prefers UUIDv7 (time-ordered, per TECH-DATA-001) and only
// falls back to a random UUID when the v7 constructor cannot draw entropy;
// both failing means the process crypto source is dead and panic is correct.
func newImportUUID() uuid.UUID {
	if id, err := uuid.NewV7(); err == nil {
		return id
	}
	return uuid.Must(uuid.NewRandom())
}
