package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PostgreSQL work-item claim and lease lifecycle (M1-RUN-001): the v3
// Runner API's dispatch core. One serializable transaction per claim
// selects the next eligible work item FOR UPDATE, enforces the
// one-active-lease invariant, creates the lease + execution with the
// caller's connection generation, and advances the queue CAS token —
// the token the daemon must present on its next claim.

// WorkItemClaim is the dispatch outcome for one runner claim.
type WorkItemClaim struct {
	LeaseID            string
	LeaseVersion       int64
	LeaseEpoch         int64
	ExecutionID        string
	WorkItemID         string
	ProjectID          string
	QueueVersion       int64
	WorkItemVersion    int64
	WorkItemRole       string
	CommandProfileJSON string
}

// ClaimNextWorkItem dispatches at most one queued work item to the runner.
//
// Guards (all inside one transaction):
//   - the runner must be approved/online and bound to the work item's
//     project (the device token already proved identity; this is scope)
//   - the presented queue token must equal the current CAS token
//   - the work item must be queued, with no active lease (partial unique
//     index backs the final insert)
//   - lease epoch = work item lease_epoch + 1; the new connection
//     generation is recorded for fencing
func (s *PostgresStore) ClaimNextWorkItem(
	ctx context.Context, runnerID, connectionGeneration string, expectedQueueVersion int64, leaseTTL time.Duration,
) (*WorkItemClaim, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Runner scope and liveness.
	var runnerStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT r.status FROM runners r WHERE r.id = $1`, runnerID).Scan(&runnerStatus)
	if err == sql.ErrNoRows {
		return nil, ErrRunnerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("claim: runner lookup: %w", err)
	}
	if runnerStatus != "approved" && runnerStatus != "online" {
		return nil, fmt.Errorf("claim: runner status %q: %w", runnerStatus, ErrRunnerStatusInvalid)
	}

	// Queue CAS token.
	var queueVersion int64
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(version, 0) FROM projects p
		JOIN runner_bindings b ON b.project_id = p.id
		WHERE b.runner_id = $1
		LIMIT 1`, runnerID).Scan(&queueVersion)
	if err == sql.ErrNoRows {
		return nil, ErrRunnerNotBound
	}
	if err != nil {
		return nil, fmt.Errorf("claim: queue token: %w", err)
	}
	if queueVersion != expectedQueueVersion {
		return nil, fmt.Errorf("claim: queue token %d, presented %d: %w",
			queueVersion, expectedQueueVersion, ErrConcurrentConflict)
	}

	// Next eligible work item in the runner's bound project.
	row := tx.QueryRowContext(ctx, `
		SELECT w.id, w.project_id, w.version, w.role, COALESCE(w.role, ''), w.lease_epoch
		FROM work_items w
		JOIN runner_bindings b ON b.project_id = w.project_id
		WHERE b.runner_id = $1 AND w.status = 'queued'
		ORDER BY w.created_at, w.id
		LIMIT 1
		FOR UPDATE OF w SKIP LOCKED`, runnerID)

	var claim WorkItemClaim
	err = row.Scan(&claim.WorkItemID, &claim.ProjectID, &claim.WorkItemVersion, &claim.WorkItemRole, &claim.WorkItemRole, &claim.LeaseEpoch)
	if err == sql.ErrNoRows {
		return nil, ErrNoAvailableTask
	}
	if err != nil {
		return nil, fmt.Errorf("claim: select work item: %w", err)
	}

	now := time.Now().UTC()
	claim.LeaseID = uuid.Must(uuid.NewV7()).String()
	claim.ExecutionID = uuid.Must(uuid.NewV7()).String()
	claim.LeaseVersion = 1
	claim.LeaseEpoch = claim.LeaseEpoch + 1
	expires := now.Add(leaseTTL)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO leases (id, project_id, work_item_id, runner_id, epoch, connection_generation, status, version, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', 1, $7)`,
		claim.LeaseID, claim.ProjectID, claim.WorkItemID, runnerID,
		claim.LeaseEpoch, connectionGeneration, expires); err != nil {
		return nil, fmt.Errorf("claim: create lease: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO executions (id, project_id, work_item_id, lease_id, runner_id, status)
		VALUES ($1, $2, $3, $4, $5, 'running')`,
		claim.ExecutionID, claim.ProjectID, claim.WorkItemID, claim.LeaseID, runnerID); err != nil {
		return nil, fmt.Errorf("claim: create execution: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE work_items SET status = 'executing', lease_epoch = $2, version = version + 1,
		     updated_at = now()
		 WHERE id = $1 AND project_id = $3 AND status = 'queued'`,
		claim.WorkItemID, claim.LeaseEpoch, claim.ProjectID); err != nil {
		return nil, fmt.Errorf("claim: transition work item: %w", err)
	}
	// Advance the CAS token so a replayed claim with the old token conflicts.
	if _, err := tx.ExecContext(ctx, `
		UPDATE projects SET version = version + 1, updated_at = now() WHERE id = $1`,
		claim.ProjectID); err != nil {
		return nil, fmt.Errorf("claim: advance queue token: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim: commit: %w", err)
	}
	claim.QueueVersion = queueVersion + 1
	return &claim, nil
}

// RunnerLeaseHeartbeat renews a lease owned by the runner's connection
// generation; stale generations and version mismatches fence out.
func (s *PostgresStore) RunnerLeaseHeartbeat(
	ctx context.Context, leaseID, runnerID, connectionGeneration string, expectedVersion int64, ttl time.Duration,
) (int64, error) {
	var newVersion int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE leases SET version = version + 1, expires_at = now() + $5::interval, updated_at = now()
		WHERE id = $1 AND runner_id = $2 AND connection_generation = $3
		  AND status = 'active' AND version = $4
		RETURNING version`,
		leaseID, runnerID, connectionGeneration, expectedVersion, fmt.Sprintf("%d seconds", int(ttl.Seconds()))).
		Scan(&newVersion)
	if err == sql.ErrNoRows {
		// Distinguish fencing from absence for stable error codes.
		var exists int
		if lookupErr := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM leases WHERE id = $1`, leaseID).Scan(&exists); lookupErr == sql.ErrNoRows {
			return 0, ErrLeaseNotFound
		}
		return 0, ErrLeaseVersionMismatch
	}
	if err != nil {
		return 0, fmt.Errorf("heartbeat: %w", err)
	}
	return newVersion, nil
}

// CompleteExecution records the terminal outcome and releases the lease.
func (s *PostgresStore) CompleteExecution(
	ctx context.Context, executionID, runnerID, connectionGeneration, outcome string, commitSHA *string, _ string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("complete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var leaseID, workItemID, projectID string
	var leaseVersion int64
	err = tx.QueryRowContext(ctx, `
		SELECT e.lease_id, e.work_item_id, e.project_id, l.version
		FROM executions e JOIN leases l ON l.id = e.lease_id
		WHERE e.id = $1 AND e.runner_id = $2 AND e.status = 'running'`,
		executionID, runnerID).Scan(&leaseID, &workItemID, &projectID, &leaseVersion)
	if err == sql.ErrNoRows {
		return ErrLeaseNotFound
	}
	if err != nil {
		return fmt.Errorf("complete: lookup: %w", err)
	}
	// Fencing: the completing connection must own the lease generation.
	var owned int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM leases WHERE id = $1 AND connection_generation = $2 AND status = 'active'`,
		leaseID, connectionGeneration).Scan(&owned); err == sql.ErrNoRows {
		return ErrRunnerGenerationStale
	} else if err != nil {
		return fmt.Errorf("complete: fence: %w", err)
	}

	executionStatus := map[string]string{
		"completed": "completed", "blocked": "failed", "failed": "failed", "cancelled": "cancelled",
	}[outcome]
	if executionStatus == "" {
		return fmt.Errorf("complete: outcome %q: %w", outcome, ErrInvalidParameter)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE executions SET status = $2, ended_at = now() WHERE id = $1`,
		executionID, executionStatus); err != nil {
		return fmt.Errorf("complete: execution: %w", err)
	}

	// Terminal work-item mapping: completed -> validating (server-side
	// validation decides ready_for_human_merge); blocked/failed -> failed;
	// cancelled -> cancelled.
	nextStatus := map[string]string{
		"completed": "validating", "blocked": "blocked", "failed": "failed", "cancelled": "cancelled",
	}[outcome]
	if _, err := tx.ExecContext(ctx, `
		UPDATE work_items SET status = $2, version = version + 1,
		     merge_commit_sha = COALESCE($3, merge_commit_sha), updated_at = now()
		 WHERE id = $1 AND project_id = $4`,
		workItemID, nextStatus, commitSHA, projectID); err != nil {
		return fmt.Errorf("complete: work item: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE leases SET status = 'completed', updated_at = now() WHERE id = $1 AND version = $2`,
		leaseID, leaseVersion); err != nil {
		return fmt.Errorf("complete: lease: %w", err)
	}
	// Queue changes again: waiters must re-observe the token.
	if _, err := tx.ExecContext(ctx, `
		UPDATE projects SET version = version + 1, updated_at = now() WHERE id = $1`, projectID); err != nil {
		return fmt.Errorf("complete: queue token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("complete: commit: %w", err)
	}
	return nil
}
