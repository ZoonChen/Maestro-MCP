package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
)

// PostgreSQL implementation of the GitLab synchronization contract
// (M2-GL/MR): projection upserts keyed by external identity and the
// fact-bound ready_for_human_merge → done transition. The generic
// status path stays fact-closed (task_store guard); this is the only
// writer that may reach done, and only from the machine's own edge.
// MarkWorkItemReadyFromGates (W6-1) is the gate-driven
// validating → ready_for_human_merge writer: the machine's own edge,
// guarded in the WHERE clause, fired by the evaluation verdict.

type pgGitlabStore struct{ db *sql.DB }

// GitLab returns the projection sync store.
func (s *PostgresStore) GitLab() gitlab.SyncStore { return pgGitlabStore{db: s.DB()} }

func (s pgGitlabStore) MappingProject(ctx context.Context, instanceID string, gitlabProjectID int64) (string, error) {
	var projectID string
	err := s.db.QueryRowContext(ctx, `
		SELECT project_id::text FROM gitlab_project_mappings
		WHERE gitlab_instance_id = $1 AND gitlab_project_id = $2`,
		instanceID, gitlabProjectID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("gitlab sync: mapping: %w", err)
	}
	return projectID, nil
}

// ResolveBranchBinding resolves the frozen branch naming contract's
// project segment (W6-2, S2C-A2): maestro/<project-key>/<item> binds
// against the project whose key matches — scoped to the mapping
// project's team, falling back to a globally unique key when the repo
// is unmapped. A key that resolves but does not contain the item, an
// ambiguous key (several teams, no mapping scope) and an unresolvable
// key all fall back to the LEGACY binding (the item directly under the
// mapping project) so pre-contract branches keep working; when that
// too finds nothing the projection stays unbound instead of tripping
// the (project_id, work_item_id) composite foreign key with a stale
// work-item guess.
func (s pgGitlabStore) ResolveBranchBinding(ctx context.Context, mappingProject, projectKey, workItemID string) (string, bool, error) {
	if projectKey == "" || workItemID == "" {
		return "", false, nil
	}
	// Key resolution, team-scoped when the repo carries a mapping.
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id::text FROM projects p
		WHERE p.key = $1
		  AND ($2::text = '' OR p.team_id = (SELECT team_id FROM projects WHERE id = $2::uuid))`,
		projectKey, mappingProject)
	if err != nil {
		return "", false, fmt.Errorf("gitlab sync: branch binding: key: %w", err)
	}
	defer rows.Close()
	var candidates []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			return "", false, fmt.Errorf("gitlab sync: branch binding: scan: %w", scanErr)
		}
		candidates = append(candidates, id)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return "", false, fmt.Errorf("gitlab sync: branch binding: rows: %w", rowsErr)
	}
	// The naming contract is authoritative only when unambiguous.
	if len(candidates) == 1 {
		if bound, err := s.workItemExists(ctx, candidates[0], workItemID); err != nil {
			return "", false, err
		} else if bound {
			return candidates[0], true, nil
		}
	}
	// Legacy fallback: branches raised before the contract carried a
	// free-form marker; their items live in the mapping project.
	if mappingProject != "" {
		if bound, err := s.workItemExists(ctx, mappingProject, workItemID); err != nil {
			return "", false, err
		} else if bound {
			return mappingProject, true, nil
		}
	}
	return "", false, nil
}

func (s pgGitlabStore) workItemExists(ctx context.Context, projectID, workItemID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM work_items WHERE project_id = $1 AND id = $2`, projectID, workItemID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("gitlab sync: branch binding: work item: %w", err)
	}
	return true, nil
}

func (s pgGitlabStore) UpsertMergeRequest(ctx context.Context, projectID string, rec gitlab.MergeRequestRecord, workItemID string) error {
	if projectID == "" {
		// Unmapped projects still record the projection under a NULL
		// project reference? No: the FK demands a real project, and an
		// unmapped delivery is reconciliation's problem, not a row.
		return nil
	}
	bind := any(nil)
	if workItemID != "" {
		bind = workItemID
	}
	mergedAt := any(nil)
	if rec.MergedAt != "" {
		mergedAt = pgTimeArg(rec.MergedAt)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO merge_requests (id, project_id, gitlab_instance_id, gitlab_project_id, mr_iid,
			work_item_id, state, source_branch, target_branch, source_sha, target_sha, merge_commit_sha, merged_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10,''), NULLIF($11,''), NULLIF($12,''), $13)
		ON CONFLICT (gitlab_instance_id, gitlab_project_id, mr_iid) DO UPDATE SET
			project_id = EXCLUDED.project_id, work_item_id = COALESCE(EXCLUDED.work_item_id, merge_requests.work_item_id),
			state = EXCLUDED.state, source_branch = EXCLUDED.source_branch, target_branch = EXCLUDED.target_branch,
			source_sha = EXCLUDED.source_sha, target_sha = EXCLUDED.target_sha,
			merge_commit_sha = EXCLUDED.merge_commit_sha, merged_at = EXCLUDED.merged_at,
			observed_at = now(), updated_at = now()`,
		pgNewUUID(), projectID, rec.InstanceID, rec.GitlabProject, rec.IID, bind, rec.State,
		rec.SourceBranch, rec.TargetBranch, rec.SourceSHA, rec.TargetSHA, rec.MergeCommit, mergedAt); err != nil {
		return fmt.Errorf("gitlab sync: merge-request upsert: %w", err)
	}
	return nil
}

// MarkWorkItemReadyFromGates applies the gate-driven transition
// (W6-1, S2C-A1): a Ready verdict — every required gate passed or
// waived on the exact SHA tuple — moves a VALIDATING work item to
// ready_for_human_merge, which is the state the merged fact's done
// edge requires. The machine's own edge, enforced in the WHERE clause:
// items outside validating are a no-op (transitioned=false), so
// replays and out-of-order verdicts never regress or duplicate. The
// state change, its audit row and its outbox event commit atomically
// (WGM-INV-012).
func (s pgGitlabStore) MarkWorkItemReadyFromGates(ctx context.Context, projectID, workItemID, actor, reason string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("gitlab sync: ready begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE work_items
		SET status = 'ready_for_human_merge', version = version + 1, updated_at = now()
		WHERE project_id = $1 AND id = $2 AND status = 'validating'`,
		projectID, workItemID)
	if err != nil {
		return false, fmt.Errorf("gitlab sync: ready update: %w", err)
	}
	transitioned, _ := result.RowsAffected()
	if transitioned == 0 {
		// Not validating (already ready, done, or elsewhere in the
		// machine): the verdict stands on the gate snapshots; nothing
		// to drive.
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("gitlab sync: ready commit: %w", err)
		}
		return false, nil
	}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "work_item.ready",
		ResourceType: "work_item", ResourceID: workItemID,
		Actor: actor, Reason: reason,
		OutboxType: "work_item.state.changed",
		Payload: mustMarshal(map[string]any{
			"work_item_id": workItemID, "from": "validating", "to": "ready_for_human_merge",
			"reason": reason,
		}),
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("gitlab sync: ready commit: %w", err)
	}
	return true, nil
}

func (s pgGitlabStore) MarkWorkItemDoneFromMerge(ctx context.Context, projectID, workItemID, mergeCommitSHA, factID string) (bool, bool, error) {
	if mergeCommitSHA == "" || factID == "" {
		return false, false, errors.New("gitlab sync: done requires the merge SHA and the fact identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, fmt.Errorf("gitlab sync: done begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The machine's own edge, enforced in the WHERE clause: only
	// ready_for_human_merge reaches done, an already-done item replays
	// as a no-op, and every other state is left untouched.
	result, err := tx.ExecContext(ctx, `
		UPDATE work_items
		SET status = 'done', merge_commit_sha = $3, merged_at = now(),
		    merged_fact_id = $4, version = version + 1, updated_at = now()
		WHERE project_id = $1 AND id = $2 AND status = 'ready_for_human_merge'`,
		projectID, workItemID, mergeCommitSHA, factID)
	if err != nil {
		return false, false, fmt.Errorf("gitlab sync: done update: %w", err)
	}
	transitioned, _ := result.RowsAffected()
	if transitioned == 0 {
		var status string
		lookupErr := tx.QueryRowContext(ctx,
			`SELECT status FROM work_items WHERE project_id = $1 AND id = $2`, projectID, workItemID).Scan(&status)
		switch {
		case errors.Is(lookupErr, sql.ErrNoRows):
			return false, true, nil // unresolvable binding: reconciliation territory
		case lookupErr != nil:
			return false, false, fmt.Errorf("gitlab sync: done verify: %w", lookupErr)
		case status == "done":
			return false, false, nil // idempotent replay of the same fact
		default:
			// Nothing regresses: the machine, not the syncer, owns the
			// edges; the withheld fact stays visible on the projection.
			return false, true, nil
		}
	}
	if err := tx.Commit(); err != nil {
		return false, false, fmt.Errorf("gitlab sync: done commit: %w", err)
	}
	return true, false, nil
}

// BranchTuple resolves the MR projection's work-item binding and SHA
// tuple for a source branch (the evidence ingestor's resolver). The
// projection may live under the BRANCH-resolved project (W6-2), so the
// branch marker is resolved first; branches without a resolvable marker
// read under the mapping project as before. The resolved project is
// returned so evidence and evaluation bind under the project the
// naming contract names.
func (s pgGitlabStore) BranchTuple(ctx context.Context, projectID, sourceBranch string) (string, string, string, string, bool, error) {
	lookupProject := projectID
	if key, item := gitlab.BranchMarker(sourceBranch); key != "" {
		if resolved, bound, err := s.ResolveBranchBinding(ctx, projectID, key, item); err != nil {
			return "", "", "", "", false, err
		} else if bound {
			lookupProject = resolved
		}
	}
	var workItem sql.NullString
	var sourceSHA, targetSHA sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT work_item_id, source_sha, target_sha FROM merge_requests
		WHERE project_id = $1 AND source_branch = $2
		ORDER BY observed_at DESC LIMIT 1`, lookupProject, sourceBranch).
		Scan(&workItem, &sourceSHA, &targetSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return lookupProject, "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", "", false, fmt.Errorf("gitlab sync: branch tuple: %w", err)
	}
	if !workItem.Valid || !sourceSHA.Valid || !targetSHA.Valid {
		return lookupProject, "", "", "", false, nil
	}
	return lookupProject, workItem.String, sourceSHA.String, targetSHA.String, true, nil
}

func (s pgGitlabStore) UpsertPipeline(ctx context.Context, rec gitlab.PipelineRecord) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO pipelines (id, project_id, gitlab_instance_id, gitlab_project_id, gitlab_pipeline_id,
			sha, ref, status, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''))
		ON CONFLICT (gitlab_instance_id, gitlab_pipeline_id) DO UPDATE SET
			status = EXCLUDED.status, source = EXCLUDED.source, observed_at = now(), updated_at = now()`,
		pgNewUUID(), rec.ProjectID, rec.InstanceID, rec.GitlabProject, rec.PipelineID,
		rec.SHA, rec.Ref, rec.Status, rec.Source); err != nil {
		return fmt.Errorf("gitlab sync: pipeline upsert: %w", err)
	}
	return nil
}

func (s pgGitlabStore) UpsertJob(ctx context.Context, rec gitlab.JobRecord) error {
	// A job event may precede its pipeline event; deferring is the
	// convergence path, not an error state.
	var pipelineRow string
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text FROM pipelines WHERE gitlab_instance_id = $1 AND gitlab_pipeline_id = $2`,
		rec.InstanceID, rec.PipelineID).Scan(&pipelineRow)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("gitlab sync: pipeline %d not projected yet, job deferred", rec.PipelineID)
	}
	if err != nil {
		return fmt.Errorf("gitlab sync: pipeline lookup: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO pipeline_jobs (id, pipeline_id, gitlab_job_id, name, status, stage)
		VALUES ($1, (SELECT id FROM pipelines WHERE gitlab_instance_id = $2 AND gitlab_pipeline_id = $3),
			$4, $5, $6, NULLIF($7, ''))
		ON CONFLICT (pipeline_id, gitlab_job_id) DO UPDATE SET
			status = EXCLUDED.status, stage = EXCLUDED.stage, observed_at = now()`,
		pgNewUUID(), rec.InstanceID, rec.PipelineID, rec.JobID, rec.Name, rec.Status, rec.Stage); err != nil {
		return fmt.Errorf("gitlab sync: job upsert: %w", err)
	}
	return nil
}
