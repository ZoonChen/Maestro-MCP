package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL work-item claim and lease lifecycle (M1-RUN-001): the v3
// Runner API's dispatch core. One serializable transaction per claim
// selects the next eligible work item FOR UPDATE (untargeted: priority
// then FIFO head; targeted — W8-1/F58: the named item directly, no
// queue-position requirement), enforces the one-active-lease invariant,
// creates the lease + execution with the caller's connection
// generation, and advances the queue CAS token — the token the daemon
// must present on its next claim.

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

// ProjectQueueVersion reads the project's queue CAS token (the claim
// contract's expected version; the governance REST face reads it
// server-side when the caller does not present one).
func (s *PostgresStore) ProjectQueueVersion(ctx context.Context, projectID string) (int64, error) {
	var version int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT version FROM projects WHERE id = $1`, projectID).Scan(&version); err != nil {
		return 0, fmt.Errorf("claim: queue version read: %w", err)
	}
	return version, nil
}

// RunnerBoundToProject reports whether the runner carries a binding to
// the project (the governance claim face refuses cross-scope claims
// BEFORE any lease side effect).
func (s *PostgresStore) RunnerBoundToProject(ctx context.Context, runnerID, projectID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM runner_bindings WHERE runner_id = $1 AND project_id = $2`,
		runnerID, projectID).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim: runner binding check: %w", err)
	}
	return true, nil
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
	return s.claimWorkItem(ctx, runnerID, connectionGeneration, "", "", expectedQueueVersion, leaseTTL)
}

// ClaimTargetedWorkItem is the W8-1 (F58) direct dispatch: the caller
// names the work item it was assigned and the claim takes THAT item —
// explicit targeting is intent, not a queue-position guard. W7's guard
// (refuse whenever the head differs) serialized parallel lanes behind
// whatever sat at the head; the 10-10 three-lane incident (an unowned
// head froze every targeted claim for hours) retired it. The target
// must be queued, carry no active lease, live in a project the runner
// is bound to (the route scopes the lookup), and its locked gates must
// be consumable. Refusals are specific and never fall through to
// another item: absent or not queued is ErrTaskNotFound; an active
// lease is a *ClaimTargetHeld carrying the holder; a non-consumable
// gate is ErrAssetGateNotSatisfied. The untargeted path (head
// selection, FIFO, release-to-head re-entry) is unchanged.
func (s *PostgresStore) ClaimTargetedWorkItem(
	ctx context.Context, runnerID, connectionGeneration, projectID, targetWorkItemID string,
	expectedQueueVersion int64, leaseTTL time.Duration,
) (*WorkItemClaim, error) {
	return s.claimWorkItem(ctx, runnerID, connectionGeneration, projectID, targetWorkItemID, expectedQueueVersion, leaseTTL)
}

// ClaimTargetHeld answers "held by whom, until when" for a targeted
// claim that met another runner's active lease (W8-2). It wraps the
// ErrClaimTargetHeld sentinel so the transport face keeps one stable
// error code while surfacing the holder.
type ClaimTargetHeld struct {
	WorkItemID string
	RunnerID   string
	LeaseID    string
	ExpiresAt  time.Time
}

func (e *ClaimTargetHeld) Error() string {
	return fmt.Errorf("claim: target %s is held by runner %s until %s: %w",
		e.WorkItemID, e.RunnerID, e.ExpiresAt.UTC().Format(time.RFC3339), ErrClaimTargetHeld).Error()
}

func (e *ClaimTargetHeld) Unwrap() error { return ErrClaimTargetHeld }

func (s *PostgresStore) claimWorkItem(
	ctx context.Context, runnerID, connectionGeneration, projectID, targetWorkItemID string,
	expectedQueueVersion int64, leaseTTL time.Duration,
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

	var queueVersion int64
	var claim WorkItemClaim
	if targetWorkItemID != "" {
		// W8-1 (F58): explicit targeting takes the named item directly.
		// The route's project scopes the lookup (an item outside it is
		// indistinguishable from absence) and the runner must be bound to
		// that project. FOR UPDATE serializes concurrent targeted claims
		// on the same item; the partial unique index
		// (idx_leases_one_active) stays the structural last line.
		var status string
		err = tx.QueryRowContext(ctx, `
			SELECT w.id, w.project_id, w.version, COALESCE(w.role, ''), w.lease_epoch, w.status
			FROM work_items w
			JOIN runner_bindings b ON b.project_id = w.project_id
			WHERE b.runner_id = $1 AND w.id = $2 AND w.project_id = $3
			FOR UPDATE OF w`, runnerID, targetWorkItemID, projectID).
			Scan(&claim.WorkItemID, &claim.ProjectID, &claim.WorkItemVersion,
				&claim.WorkItemRole, &claim.LeaseEpoch, &status)
		if err == sql.ErrNoRows {
			return nil, ErrTaskNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("claim: target lookup: %w", err)
		}
		// An active lease answers "held by whom" instead of a bare
		// conflict (W8-2) — even for an item whose status flip lagged.
		var holderRunner, holderLease string
		var heldUntil time.Time
		err = tx.QueryRowContext(ctx, `
			SELECT runner_id, id, expires_at FROM leases
			WHERE project_id = $1 AND work_item_id = $2 AND status = 'active'`,
			claim.ProjectID, claim.WorkItemID).Scan(&holderRunner, &holderLease, &heldUntil)
		if err == nil {
			return nil, &ClaimTargetHeld{
				WorkItemID: claim.WorkItemID, RunnerID: holderRunner,
				LeaseID: holderLease, ExpiresAt: heldUntil,
			}
		}
		if err != sql.ErrNoRows {
			return nil, fmt.Errorf("claim: holder lookup: %w", err)
		}
		if status != "queued" {
			// executing/validating/done/... — nothing to claim; the held
			// case above already answered the informative conflict.
			return nil, fmt.Errorf("claim: target %s is %s: %w", targetWorkItemID, status, ErrTaskNotFound)
		}
		// The same consumability predicate the head selection applies as
		// a skip — targeted, it is a refusal: the caller named this item,
		// so the claim must not silently fall through to another one.
		var blocked int
		err = tx.QueryRowContext(ctx, `
			SELECT 1 FROM asset_gate_bindings g
			WHERE g.work_item_id = $1
			  AND (g.status <> 'bound'
			       OR NOT EXISTS (
				SELECT 1 FROM assets a
				WHERE a.asset_id = g.asset_id AND a.version = g.bound_version
				  AND a.status = 'approved' AND a.source_digest = g.bound_digest))`,
			claim.WorkItemID).Scan(&blocked)
		if err == nil {
			return nil, fmt.Errorf("claim: target %s gate not consumable: %w",
				targetWorkItemID, ErrAssetGateNotSatisfied)
		}
		if err != sql.ErrNoRows {
			return nil, fmt.Errorf("claim: gate consumability: %w", err)
		}
		// Queue CAS token of the target's own project.
		err = tx.QueryRowContext(ctx,
			`SELECT COALESCE(version, 0) FROM projects WHERE id = $1`, claim.ProjectID).Scan(&queueVersion)
		if err != nil {
			return nil, fmt.Errorf("claim: queue token: %w", err)
		}
		if queueVersion != expectedQueueVersion {
			return nil, fmt.Errorf("claim: queue token %d, presented %d: %w",
				queueVersion, expectedQueueVersion, ErrConcurrentConflict)
		}
	} else {
		// Queue CAS token.
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

		// Next eligible work item in the runner's bound project, priority
		// first (urgent > high > normal > low — the product's BOM ordering
		// rides into dispatch, S2B2-F8), then FIFO within one priority
		// band. Released items re-enter at the HEAD of their band (W7-1:
		// queue_requeued_at sorts ahead of natural FIFO order, newest
		// release first). Items whose locked-gate assets are not consumable
		// (not approved, digest drift or a stale binding) are skipped — the
		// asset gate fails closed at dispatch time (WGM-INV-015, J2a-4).
		row := tx.QueryRowContext(ctx, `
			SELECT w.id, w.project_id, w.version, COALESCE(w.role, ''), w.lease_epoch
			FROM work_items w
			JOIN runner_bindings b ON b.project_id = w.project_id
			WHERE b.runner_id = $1 AND w.status = 'queued'
			  AND NOT EXISTS (
				SELECT 1 FROM asset_gate_bindings g
				WHERE g.work_item_id = w.id
				  AND (g.status <> 'bound'
				       OR NOT EXISTS (
						SELECT 1 FROM assets a
						WHERE a.asset_id = g.asset_id AND a.version = g.bound_version
						  AND a.status = 'approved' AND a.source_digest = g.bound_digest))
			  )
			ORDER BY CASE w.priority
					WHEN 'urgent' THEN 0
					WHEN 'high' THEN 1
					WHEN 'normal' THEN 2
					ELSE 3 END,
				(w.queue_requeued_at IS NULL),
				w.queue_requeued_at DESC NULLS LAST,
				w.created_at, w.id
			LIMIT 1
			FOR UPDATE OF w SKIP LOCKED`, runnerID)

		err = row.Scan(&claim.WorkItemID, &claim.ProjectID, &claim.WorkItemVersion, &claim.WorkItemRole, &claim.LeaseEpoch)
		if err == sql.ErrNoRows {
			return nil, ErrNoAvailableTask
		}
		if err != nil {
			return nil, fmt.Errorf("claim: select work item: %w", err)
		}
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
		// F66 (W9-1): the partial unique index (idx_leases_one_active)
		// is the structural last line — a lease writer that does not
		// pass the row lock above (the SQLite import, direct surgery)
		// can commit an active lease inside the claim's own read
		// window. The loser learns who holds the target instead of a
		// bare internal error.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback()
			return nil, s.heldByActiveLease(ctx, claim.ProjectID, claim.WorkItemID)
		}
		return nil, fmt.Errorf("claim: create lease: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO executions (id, project_id, work_item_id, lease_id, runner_id, status)
		VALUES ($1, $2, $3, $4, $5, 'running')`,
		claim.ExecutionID, claim.ProjectID, claim.WorkItemID, claim.LeaseID, runnerID); err != nil {
		return nil, fmt.Errorf("claim: create execution: %w", err)
	}
	var itemVersion int64
	err = tx.QueryRowContext(ctx, `
		UPDATE work_items SET status = 'executing', lease_epoch = $2, version = version + 1,
		     updated_at = now()
		 WHERE id = $1 AND project_id = $3 AND status = 'queued'
		 RETURNING version`,
		claim.WorkItemID, claim.LeaseEpoch, claim.ProjectID).Scan(&itemVersion)
	if err == sql.ErrNoRows {
		// The row lock plus the queued read above make this
		// unreachable; refusing is the fail-closed answer if it ever
		// is not.
		return nil, fmt.Errorf("claim: target %s left queued: %w", claim.WorkItemID, ErrConcurrentConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("claim: transition work item: %w", err)
	}
	// Advance the CAS token so a replayed claim with the old token conflicts.
	if _, err := tx.ExecContext(ctx, `
		UPDATE projects SET version = version + 1, updated_at = now() WHERE id = $1`,
		claim.ProjectID); err != nil {
		return nil, fmt.Errorf("claim: advance queue token: %w", err)
	}
	// F64 (W9-2): the queued -> executing state change commits with its
	// audit row (work_item.claimed — the action the claim route's
	// contract already declares) and its work_item.state.changed outbox
	// event in the same transaction (WGM-INV-012), the release face's
	// shape. The audit reason carries the claim's own identifiers
	// (execution, lease expiry); the payload stays inside the frozen
	// StateChangePayload contract.
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: claim.ProjectID, Action: "work_item.claimed",
		ResourceType: "work_item", ResourceID: claim.WorkItemID,
		Actor: "runner:" + runnerID,
		Reason: fmt.Sprintf("claim: execution=%s lease=%s lease_expires=%s",
			claim.ExecutionID, claim.LeaseID, expires.UTC().Format(time.RFC3339)),
		OutboxType: "work_item.state.changed",
		Payload: mustMarshal(map[string]any{
			"work_item_id": claim.WorkItemID, "from": "queued", "to": "executing",
			"version": itemVersion, "reason_code": "WORK_ITEM_CLAIMED",
		}),
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim: commit: %w", err)
	}
	claim.QueueVersion = queueVersion + 1
	return &claim, nil
}

// heldByActiveLease answers the F66 conflict shape from committed state:
// the active lease that made a claim lose the unique-index race, with
// its holder, lease id and expiry — or the bare sentinel when the
// conflicting lease is not (yet) visible. The claim transaction is
// already rolled back; this reads on the pool.
func (s *PostgresStore) heldByActiveLease(ctx context.Context, projectID, workItemID string) error {
	var holderRunner, holderLease string
	var heldUntil time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT runner_id, id, expires_at FROM leases
		WHERE project_id = $1 AND work_item_id = $2 AND status = 'active'`,
		projectID, workItemID).Scan(&holderRunner, &holderLease, &heldUntil)
	if err == sql.ErrNoRows {
		return fmt.Errorf("claim: unique-index conflict on %s: %w", workItemID, ErrClaimTargetHeld)
	}
	if err != nil {
		return fmt.Errorf("claim: holder lookup after conflict: %w", err)
	}
	return &ClaimTargetHeld{
		WorkItemID: workItemID, RunnerID: holderRunner,
		LeaseID: holderLease, ExpiresAt: heldUntil,
	}
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
	// W7-2 (F32): the platform's known branch head is the MR projection
	// bound to this work item (the branch tuple's source SHA). A
	// presented SHA that disagrees with a projected head is refused —
	// the two mis-acceptances (one truncated, one wrong-branch 40-hex)
	// proved the client-side discipline alone does not hold. No
	// projection yet means nothing to contradict; the format check at
	// the handler already fenced the shape.
	if commitSHA != nil && *commitSHA != "" {
		var knownHead sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT mr.source_sha FROM merge_requests mr
			WHERE mr.project_id = $1 AND mr.work_item_id = $2
			  AND mr.source_sha IS NOT NULL AND mr.source_sha <> ''
			ORDER BY mr.observed_at DESC LIMIT 1`,
			projectID, workItemID).Scan(&knownHead)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("complete: branch head lookup: %w", err)
		}
		if knownHead.Valid && !strings.EqualFold(knownHead.String, *commitSHA) {
			return fmt.Errorf("complete: sha %s vs known head %s: %w",
				*commitSHA, knownHead.String, ErrCommitSHAMismatch)
		}
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

// ReleaseExecution is the W7-1 (F29) give-back face: the runner that
// holds the lease returns it BEFORE any terminal outcome — the work
// item goes back to queued at the HEAD of its priority band
// (queue_requeued_at, newest first), the queue CAS token advances, and
// the state change commits with its audit row (work_item.released) and
// outbox event (work_item.state.changed executing -> queued) in the
// same transaction (WGM-INV-012). The B10-1 alternative — parking a
// mis-claimed lease for its full TTL — cost the pilot two days.
func (s *PostgresStore) ReleaseExecution(
	ctx context.Context, executionID, runnerID, connectionGeneration, reason, actor string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("release: begin: %w", err)
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
		return fmt.Errorf("release: lookup: %w", err)
	}
	// Fencing: the releasing connection must own the lease generation.
	var owned int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM leases WHERE id = $1 AND connection_generation = $2 AND status = 'active'`,
		leaseID, connectionGeneration).Scan(&owned); err == sql.ErrNoRows {
		return ErrRunnerGenerationStale
	} else if err != nil {
		return fmt.Errorf("release: fence: %w", err)
	}

	// The machine's own edge: only an executing item returns to queued.
	var itemVersion int64
	err = tx.QueryRowContext(ctx, `
		UPDATE work_items
		SET status = 'queued', version = version + 1,
		    queue_requeued_at = now(), updated_at = now()
		WHERE id = $1 AND project_id = $2 AND status = 'executing'
		RETURNING version`,
		workItemID, projectID).Scan(&itemVersion)
	if err == sql.ErrNoRows {
		return fmt.Errorf("release: work item outside executing: %w", ErrConcurrentConflict)
	}
	if err != nil {
		return fmt.Errorf("release: work item: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE executions SET status = 'released', ended_at = now() WHERE id = $1`,
		executionID); err != nil {
		return fmt.Errorf("release: execution: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE leases SET status = 'released', updated_at = now() WHERE id = $1 AND version = $2`,
		leaseID, leaseVersion); err != nil {
		return fmt.Errorf("release: lease: %w", err)
	}
	// Queue changes again: waiters must re-observe the token.
	if _, err := tx.ExecContext(ctx, `
		UPDATE projects SET version = version + 1, updated_at = now() WHERE id = $1`, projectID); err != nil {
		return fmt.Errorf("release: queue token: %w", err)
	}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "work_item.released",
		ResourceType: "work_item", ResourceID: workItemID,
		Actor: actor, Reason: reason,
		OutboxType: "work_item.state.changed",
		Payload: mustMarshal(map[string]any{
			"work_item_id": workItemID, "from": "executing", "to": "queued",
			"version": itemVersion, "reason_code": "WORK_ITEM_RELEASED",
		}),
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("release: commit: %w", err)
	}
	return nil
}
