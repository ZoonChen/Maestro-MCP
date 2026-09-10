package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ZoonChen/Maestro-MCP/internal/budget"
	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
)

// Graph-path execution (J2b-2/J2b-3): replan, claim with the
// ExecutionEnvelope, lease fencing heartbeat/completion, expiry
// recovery, aggregation projection and cancellation projection.

// graphBudgetLimits derives the durable ledger ceilings for one node
// from its spec budget (wall time floor respects the 0010 CHECK).
func graphBudgetLimits(spec workgraph.NodeSpec) budget.Limits {
	wall := time.Duration(spec.BudgetUnits/10+1) * time.Minute
	if wall < 3600*time.Second {
		wall = 3600 * time.Second
	}
	return budget.Limits{BudgetUnits: spec.BudgetUnits, MaxAttempts: 3, WallTimeLimit: wall}
}

// ReplanPlanRevisionInput opens revision N+1 after a seal.
type ReplanPlanRevisionInput struct {
	PlanID               string
	ExpectedGraphVersion int64
	// SpecOverrides maps node id -> replacement spec; overridden nodes
	// have their outcome invalidated (status reset) in the new
	// revision — the stale propagation of a re-plan.
	SpecOverrides map[string][]byte
	Actor                 string
	CorrelationID         string
}

// ReplanPlanRevision creates the next draft revision: every node's
// live spec is carried forward except the overridden ones, whose
// outcomes reset to queued (their prior done/failed evidence was
// derived against the old spec). Sealed revisions and in-flight
// attempts are untouched — attempts stay pinned to their original
// node_revision and spec_digest (ADR-009 §5).
func (s pgWorkGraphStore) ReplanPlanRevision(ctx context.Context, in ReplanPlanRevisionInput) (*PlanRevision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var projectID string
	var graphVersion int64
	err = tx.QueryRowContext(ctx, `
		SELECT project_id::text, graph_version FROM work_plans WHERE id = $1 FOR UPDATE`,
		in.PlanID).Scan(&projectID, &graphVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: lock plan: %w", err)
	}
	if graphVersion != in.ExpectedGraphVersion {
		return nil, ErrGraphVersionMismatch
	}

	var currentRevisionID string
	var revisionNo int
	var revisionStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT id, revision_no, status FROM plan_revisions
		WHERE plan_id = $1 ORDER BY revision_no DESC LIMIT 1 FOR UPDATE`,
		in.PlanID).Scan(&currentRevisionID, &revisionNo, &revisionStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: plan %s has no revision", ErrReplanRejected, in.PlanID)
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: lock revision: %w", err)
	}
	if revisionStatus != "sealed" {
		return nil, fmt.Errorf("%w: current revision %d is still %s", ErrReplanRejected, revisionNo, revisionStatus)
	}

	// Live specs: the newest node revision row per node.
	type liveRow struct {
		nodeID, specDigest string
		spec               []byte
	}
	live := []liveRow{}
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT ON (nr.node_id) nr.node_id::text, nr.spec_digest, nr.spec
		FROM work_node_revisions nr
		JOIN plan_revisions pr ON pr.id = nr.plan_revision_id
		WHERE pr.plan_id = $1
		ORDER BY nr.node_id, pr.revision_no DESC, nr.created_at DESC`, in.PlanID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: live specs: %w", err)
	}
	for rows.Next() {
		var r liveRow
		if err := rows.Scan(&r.nodeID, &r.specDigest, &r.spec); err != nil {
			rows.Close()
			return nil, fmt.Errorf("workgraph: scan live spec: %w", err)
		}
		live = append(live, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: live specs: %w", err)
	}

	// Validate overrides against existing nodes (no phantom edits).
	for nodeID := range in.SpecOverrides {
		found := false
		for _, r := range live {
			if r.nodeID == nodeID {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: override targets unknown node %s", ErrReplanRejected, nodeID)
		}
	}

	// The revision digest covers the carried snapshot (node ->
	// spec_digest after overrides), so replays of the same replan are
	// comparable.
	digestDoc := map[string]any{"nodes": map[string]string{}, "revision_of": revisionNo}
	nodeDigests := digestDoc["nodes"].(map[string]string)
	for _, r := range live {
		digest, err := workgraph.SpecDigest(r.spec)
		if err != nil {
			return nil, err
		}
		nodeDigests[r.nodeID] = digest
	}
	overridden := map[string]bool{}
	for nodeID, spec := range in.SpecOverrides {
		canonical, err := workgraph.CanonicalJSON(spec)
		if err != nil {
			return nil, fmt.Errorf("%w: override spec for %s is not JSON: %v", ErrReplanRejected, nodeID, err)
		}
		digest, err := workgraph.SpecDigest(canonical)
		if err != nil {
			return nil, err
		}
		nodeDigests[nodeID] = digest
		overridden[nodeID] = true
	}
	encoded, err := json.Marshal(digestDoc)
	if err != nil {
		return nil, fmt.Errorf("workgraph: replan digest: %w", err)
	}
	revisionDigest, err := workgraph.SpecDigest(encoded)
	if err != nil {
		return nil, err
	}

	newRevisionID := uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO plan_revisions (id, plan_id, revision_no, status, spec_digest)
		VALUES ($1, $2, $3, 'draft', $4)`,
		newRevisionID, in.PlanID, revisionNo+1, revisionDigest); err != nil {
		return nil, fmt.Errorf("workgraph: create revision: %w", err)
	}

	inserted := 0
	for _, r := range live {
		spec, digest := r.spec, r.specDigest
		if overridden[r.nodeID] {
			spec, err = workgraph.CanonicalJSON(in.SpecOverrides[r.nodeID])
			if err != nil {
				return nil, err
			}
			digest, err = workgraph.SpecDigest(spec)
			if err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_node_revisions (id, node_id, plan_revision_id, spec_digest, spec)
			VALUES ($1, $2, $3, $4, $5::jsonb)`,
			uuid.Must(uuid.NewV7()).String(), r.nodeID, newRevisionID, digest, string(spec)); err != nil {
			return nil, fmt.Errorf("workgraph: carry node revision: %w", err)
		}
		inserted++
	}

	// Stale propagation: overridden nodes lose their prior outcome —
	// reset to queued unless cancelled (WGS-RULE-005: cancelled nodes
	// are never resurrected).
	for nodeID := range overridden {
		if _, err := tx.ExecContext(ctx, `
			UPDATE work_nodes SET status = 'queued', node_version = node_version + 1, updated_at = now()
			WHERE id = $1 AND plan_id = $2 AND status NOT IN ('cancelled', 'queued', 'draft')`,
			nodeID, in.PlanID); err != nil {
			return nil, fmt.Errorf("workgraph: reset overridden node: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE work_plans SET status = 'replanned', updated_at = now() WHERE id = $1`, in.PlanID); err != nil {
		return nil, fmt.Errorf("workgraph: mark replanned: %w", err)
	}
	if err := bumpGraphVersion(ctx, tx, in.PlanID); err != nil {
		return nil, err
	}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.plan.replanned", ResourceType: "plan_revision",
		ResourceID: newRevisionID, Actor: in.Actor, CorrelationID: in.CorrelationID,
		OutboxType: "workgraph.plan.replanned",
		Payload: mustMarshal(map[string]any{
			"plan_id": in.PlanID, "revision_no": revisionNo + 1, "supersedes": revisionNo,
			"overridden_nodes": len(overridden), "carried_nodes": inserted}),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph: commit: %w", err)
	}
	return s.GetPlanRevision(ctx, newRevisionID)
}

// ClaimWorkNodeInput is one graph-path claim. Identity is server-side
// (the session decides principal/role; the worker presents its
// connection generation and capabilities — never its own scope).
type ClaimWorkNodeInput struct {
	ProjectID            string
	Principal            string
	Role                 string
	SessionID            string
	WorkerID             string
	ConnectionGeneration string
	IdempotencyKey       string
	ExpectedQueueVersion int64
	LeaseTTL             time.Duration
	Capabilities         []string
}

// ClaimNextWorkNode dispatches at most one ready graph node (J2b-3):
// readiness is the WGS-REQ-001 conjunction enforced in the candidate
// query (sealed current revision, work_item, queued, required
// dependencies satisfied, consumable assets approved, capability
// match, no active attempt); budget reservation runs under the node
// lock; the five-way binding plus context digest is written once and
// never re-pointed. The queue CAS mirrors the flat claim path.
func (s pgWorkGraphStore) ClaimNextWorkNode(ctx context.Context, in ClaimWorkNodeInput) (*WorkNodeClaim, error) {
	if in.IdempotencyKey == "" || in.Principal == "" || in.WorkerID == "" || in.SessionID == "" {
		return nil, fmt.Errorf("%w: principal, session, worker and idempotency key are required", ErrInvalidParameter)
	}
	if in.LeaseTTL <= 0 {
		in.LeaseTTL = 90 * time.Second
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotent replay: an attempt bound to this key returns its
	// envelope again — same conclusion, no second attempt.
	if claim, err := replayWorkNodeClaim(ctx, tx, in.IdempotencyKey); err == nil {
		return claim, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Queue CAS (same token the flat claim path advances).
	var queueVersion int64
	err = tx.QueryRowContext(ctx, `
		SELECT version FROM projects WHERE id = $1 FOR UPDATE`, in.ProjectID).Scan(&queueVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: project %s", ErrInvalidParameter, in.ProjectID)
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph claim: queue token: %w", err)
	}
	if queueVersion != in.ExpectedQueueVersion {
		return nil, fmt.Errorf("workgraph claim: queue token %d, presented %d: %w",
			queueVersion, in.ExpectedQueueVersion, ErrConcurrentConflict)
	}

	type candidate struct {
		nodeID, humanCode, nodeRevisionID, specDigest string
		nodeVersion                                    int64
		spec                                           workgraph.NodeSpec
	}
	candidates := []candidate{}
	rows, err := tx.QueryContext(ctx, `
		SELECT n.id::text, n.human_code, n.node_version, nr.id::text, nr.spec_digest, nr.spec
		FROM work_nodes n
		JOIN work_plans p ON p.id = n.plan_id
		JOIN LATERAL (
			SELECT pr.id, pr.status FROM plan_revisions pr
			WHERE pr.plan_id = n.plan_id ORDER BY pr.revision_no DESC LIMIT 1
		) cur ON cur.status = 'sealed'
		JOIN work_node_revisions nr ON nr.node_id = n.id AND nr.plan_revision_id = cur.id
		WHERE p.project_id = $1
		  AND p.status IN ('sealed', 'executing', 'aggregating')
		  AND n.node_type = 'work_item'
		  AND n.status = 'queued'
		  AND (nr.spec->>'owning_capability' IS NULL OR nr.spec->>'owning_capability' = ANY($2))
		  AND NOT EXISTS (
			SELECT 1 FROM execution_attempts ea
			WHERE ea.node_id = n.id AND ea.status = 'running')
		  AND NOT EXISTS (
			SELECT 1 FROM work_dependencies d
			JOIN work_nodes dep ON dep.id = d.to_node_id
			WHERE d.from_node_id = n.id AND d.requirement = 'required'
			  AND dep.status NOT IN ('done', 'satisfied'))
		  AND NOT EXISTS (
			SELECT 1 FROM node_artifact_flows f
			JOIN assets a ON a.asset_id = f.asset_id AND a.version = f.asset_version
			WHERE f.plan_id = n.plan_id AND f.node_id = n.id AND f.direction = 'consumes'
			  AND a.status <> 'approved')
		ORDER BY COALESCE((nr.spec->>'priority')::int, 0) DESC,
		         nr.spec->>'deadline' ASC NULLS LAST,
		         n.created_at ASC, n.id ASC
		LIMIT 5
		FOR UPDATE OF n SKIP LOCKED`, in.ProjectID, capabilitiesArray(in.Capabilities))
	if err != nil {
		return nil, fmt.Errorf("workgraph claim: candidates: %w", err)
	}
	for rows.Next() {
		var c candidate
		var rawSpec []byte
		if err := rows.Scan(&c.nodeID, &c.humanCode, &c.nodeVersion, &c.nodeRevisionID, &c.specDigest, &rawSpec); err != nil {
			rows.Close()
			return nil, fmt.Errorf("workgraph claim: scan candidate: %w", err)
		}
		if err := json.Unmarshal(rawSpec, &c.spec); err != nil {
			rows.Close()
			return nil, fmt.Errorf("workgraph claim: decode spec: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph claim: candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, ErrNoAvailableTask
	}

	for _, c := range candidates {
		claim, err := claimCandidate(ctx, tx, in, c.nodeID, c.humanCode, c.nodeVersion,
			c.nodeRevisionID, c.specDigest, c.spec)
		if errors.Is(err, budget.ErrInsufficient) || errors.Is(err, budget.ErrStopped) {
			continue // readiness conjunction: budget unreserved -> skip
		}
		if err != nil {
			return nil, err
		}
		return claim, nil
	}
	return nil, ErrNoAvailableTask
}

func capabilitiesArray(caps []string) any {
	if len(caps) == 0 {
		return []string{}
	}
	return caps
}

// claimCandidate binds one selected node: budget reserve, context
// assembly, attempt insert, node CAS, queue advance, audit.
func claimCandidate(ctx context.Context, tx *sql.Tx, in ClaimWorkNodeInput,
	nodeID, humanCode string, nodeVersion int64, nodeRevisionID, specDigest string, spec workgraph.NodeSpec,
) (*WorkNodeClaim, error) {
	var projectID, planID string
	if err := tx.QueryRowContext(ctx, `
		SELECT p.project_id::text, n.plan_id::text
		FROM work_nodes n JOIN work_plans p ON p.id = n.plan_id
		WHERE n.id = $1`, nodeID).Scan(&projectID, &planID); err != nil {
		return nil, fmt.Errorf("workgraph claim: node lookup: %w", err)
	}

	// Budget: open-or-reuse the node-scoped ledger and reserve within
	// the REMAINING allowance — the spec budget is the node's
	// cumulative ceiling across attempts, so retries reserve what is
	// left after prior attempts' spend (out of budget = not ready).
	ledgerID, err := openGraphBudgetLedger(ctx, tx, projectID, nodeID, graphBudgetLimits(spec))
	if err != nil {
		return nil, err
	}
	budgetUnits, err := graphBudgetRemaining(ctx, tx, ledgerID, spec.BudgetUnits)
	if err != nil {
		return nil, err
	}
	if err := appendBudgetEntryTx(ctx, tx, ledgerID, budget.Reserve, budgetUnits, "workgraph.claim"); err != nil {
		return nil, err
	}

	// Minimal context (ADR-009 §9): exact SHA, workspace boundary,
	// direct dependency artifacts with digests, acceptance, budget.
	inputs := []workgraph.ContextInput{}
	inputRows, err := tx.QueryContext(ctx, `
		SELECT f.port_key, f.asset_id, f.asset_version, a.source_digest
		FROM node_artifact_flows f
		JOIN assets a ON a.asset_id = f.asset_id AND a.version = f.asset_version
		WHERE f.node_id = $1 AND f.direction = 'consumes'`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("workgraph claim: inputs: %w", err)
	}
	for inputRows.Next() {
		var port, assetID, digest string
		var version int
		if err := inputRows.Scan(&port, &assetID, &version, &digest); err != nil {
			inputRows.Close()
			return nil, fmt.Errorf("workgraph claim: scan input: %w", err)
		}
		inputs = append(inputs, workgraph.ContextInput{
			Port: port, AssetRef: fmt.Sprintf("%s@%d", assetID, version), AssetDigest: digest})
	}
	inputRows.Close()
	if err := inputRows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph claim: inputs: %w", err)
	}
	contextSet := workgraph.ContextSet{
		Repo: spec.Repo, BaseSHA: spec.BaselineSHA, WorkspacePaths: spec.WorkspacePaths,
		Inputs: inputs, AcceptanceCriteria: spec.AcceptanceCriteria, BudgetUnits: budgetUnits,
	}
	contextJSON, err := json.Marshal(contextSet)
	if err != nil {
		return nil, fmt.Errorf("workgraph claim: marshal context: %w", err)
	}
	contextDigest, err := workgraph.SpecDigest(contextJSON)
	if err != nil {
		return nil, err
	}

	var attemptNo int
	var previousAttempt sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(attempt_no), 0) + 1,
		       (SELECT id::text FROM execution_attempts WHERE node_id = $1 ORDER BY attempt_no DESC LIMIT 1)
		FROM execution_attempts WHERE node_id = $1`, nodeID).Scan(&attemptNo, &previousAttempt); err != nil {
		return nil, fmt.Errorf("workgraph claim: attempt no: %w", err)
	}
	retryOf := any(nil)
	if previousAttempt.Valid {
		retryOf = previousAttempt.String
	}
	worktree := fmt.Sprintf("%s/worktrees/%s/%s/%d", spec.Repo, planID, nodeID, attemptNo)

	attemptID := uuid.Must(uuid.NewV7()).String()
	leaseToken := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO execution_attempts
			(id, project_id, plan_id, node_id, node_revision_id, spec_digest, attempt_no,
			 retry_of_attempt_id, principal, role, session_id, worker_id, worktree_path,
			 context_digest, context_set, budget_ledger_id, budget_units,
			 lease_token, lease_epoch, lease_version, connection_generation, lease_expires_at,
			 idempotency_key, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb,
		        $16, $17, $18, $19, 1, $20, $21, $22, 'running')`,
		attemptID, projectID, planID, nodeID, nodeRevisionID, specDigest, attemptNo,
		retryOf, in.Principal, in.Role, in.SessionID, in.WorkerID, worktree,
		contextDigest, string(contextJSON), ledgerID, budgetUnits,
		leaseToken, attemptNo, in.ConnectionGeneration, now.Add(in.LeaseTTL),
		in.IdempotencyKey); err != nil {
		return nil, fmt.Errorf("workgraph claim: insert attempt: %w", err)
	}

	// Node CAS into executing.
	result, err := tx.ExecContext(ctx, `
		UPDATE work_nodes SET status = 'executing', node_version = node_version + 1, updated_at = now()
		WHERE id = $1 AND node_version = $2 AND status = 'queued'`, nodeID, nodeVersion)
	if err != nil {
		return nil, fmt.Errorf("workgraph claim: node cas: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return nil, ErrNodeVersionMismatch
	}

	// Advance the queue token so a replayed claim conflicts.
	if _, err := tx.ExecContext(ctx, `
		UPDATE projects SET version = version + 1, updated_at = now() WHERE id = $1`, projectID); err != nil {
		return nil, fmt.Errorf("workgraph claim: advance queue token: %w", err)
	}

	correlation := "wgs-" + uuid.Must(uuid.NewV7()).String()
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.node.claimed", ResourceType: "work_node",
		ResourceID: nodeID, Actor: in.Principal, CorrelationID: correlation,
		OutboxType: "workgraph.node.claimed",
		Payload: mustMarshal(map[string]any{
			"node_id": nodeID, "attempt_id": attemptID, "context_digest": contextDigest,
			"worker_id": in.WorkerID, "budget_units": budgetUnits}),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph claim: commit: %w", err)
	}

	envelope := workgraph.ExecutionEnvelope{
		TaskID: humanCode, NodeID: nodeID, NodeRevisionID: nodeRevisionID,
		ExecutionAttemptID: attemptID, LeaseToken: leaseToken, LeaseEpoch: int64(attemptNo),
		WorkspacePath: worktree, Generation: in.ConnectionGeneration, BaseSHA: spec.BaselineSHA,
		ContextDigest: contextDigest, BudgetUnits: budgetUnits,
		LeaseExpiresAt: now.Add(in.LeaseTTL), CorrelationID: correlation,
	}
	return &WorkNodeClaim{Envelope: envelope}, nil
}

// replayWorkNodeClaim rebuilds the envelope of an already-bound
// idempotency key.
func replayWorkNodeClaim(ctx context.Context, tx *sql.Tx, key string) (*WorkNodeClaim, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT ea.id::text, ea.node_id::text, ea.node_revision_id::text, ea.spec_digest,
		       ea.lease_token::text, ea.lease_epoch, ea.worktree_path, ea.connection_generation,
		       ea.context_digest, ea.budget_units, ea.lease_expires_at, ea.status,
		       (SELECT human_code FROM work_nodes WHERE id = ea.node_id),
		       (SELECT spec->>'baseline_sha' FROM work_node_revisions WHERE id = ea.node_revision_id)
		FROM execution_attempts ea WHERE ea.idempotency_key = $1`, key)
	var env workgraph.ExecutionEnvelope
	var expires time.Time
	var status, humanCode, specDigest string
	var baseSHA *string
	if err := row.Scan(&env.ExecutionAttemptID, &env.NodeID, &env.NodeRevisionID, &specDigest,
		&env.LeaseToken, &env.LeaseEpoch, &env.WorkspacePath, &env.Generation,
		&env.ContextDigest, &env.BudgetUnits, &expires, &status, &humanCode, &baseSHA); err != nil {
		return nil, err
	}
	_ = specDigest // the digest travels on the envelope via ContextDigest
	env.TaskID = humanCode
	env.LeaseExpiresAt = expires
	if baseSHA != nil {
		env.BaseSHA = *baseSHA
	}
	if status != "running" {
		return nil, fmt.Errorf("%w: idempotency key belongs to a %s attempt", ErrAttemptNotResumable, status)
	}
	return &WorkNodeClaim{Envelope: env}, nil
}

// openGraphBudgetLedger provisions or reuses the work_node-scoped
// ledger inside the caller's transaction. On reuse the ceiling is
// re-aligned with the CURRENT sealed spec's budget: the spec is the
// budget authority, and a replan may have changed it (the old
// attempt's spend history stays untouched).
func openGraphBudgetLedger(ctx context.Context, tx *sql.Tx, projectID, nodeID string, limits budget.Limits) (string, error) {
	var ledgerID string
	err := tx.QueryRowContext(ctx, `
		SELECT id::text FROM budget_ledgers WHERE scope_kind = 'work_node' AND scope_id = $1 FOR UPDATE`,
		nodeID).Scan(&ledgerID)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `
			UPDATE budget_ledgers SET budget_units = $2, updated_at = now()
			WHERE id = $1 AND state = 'open'`, ledgerID, limits.BudgetUnits); err != nil {
			return "", fmt.Errorf("workgraph claim: align ledger ceiling: %w", err)
		}
		return ledgerID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("workgraph claim: ledger lookup: %w", err)
	}
	ledgerID = uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO budget_ledgers (id, project_id, scope_kind, scope_id, budget_version,
			budget_units, max_attempts, wall_time_limit_seconds)
		VALUES ($1, $2, 'work_node', $3, 'graph-v1', $4, $5, $6)`,
		ledgerID, projectID, nodeID, limits.BudgetUnits, limits.MaxAttempts,
		int(limits.WallTimeLimit.Seconds())); err != nil {
		return "", fmt.Errorf("workgraph claim: open ledger: %w", err)
	}
	return ledgerID, nil
}

// appendBudgetEntryTx mirrors pgBudgetStore.AppendEntry arithmetic on
// the caller's transaction (row lock + ceiling check + append +
// totals).
func appendBudgetEntryTx(ctx context.Context, tx *sql.Tx, ledgerID string, direction budget.Direction, units int64, toolRef string) error {
	var state string
	if err := tx.QueryRowContext(ctx, `
		SELECT state FROM budget_ledgers WHERE id = $1 FOR UPDATE`, ledgerID).Scan(&state); err != nil {
		return fmt.Errorf("workgraph: budget lock: %w", err)
	}
	if state != "open" {
		return budget.ErrStopped
	}
	var spentUnits, reservedUnits int64
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(units) FILTER (WHERE direction = 'spend'), 0),
			COALESCE(SUM(units) FILTER (WHERE direction = 'reserve'), 0) -
			COALESCE(SUM(units) FILTER (WHERE direction = 'release'), 0)
		FROM budget_entries WHERE ledger_id = $1`, ledgerID).Scan(&spentUnits, &reservedUnits); err != nil {
		return fmt.Errorf("workgraph: budget totals: %w", err)
	}
	var ceiling int64
	if err := tx.QueryRowContext(ctx, `
		SELECT budget_units FROM budget_ledgers WHERE id = $1`, ledgerID).Scan(&ceiling); err != nil {
		return fmt.Errorf("workgraph: budget ceiling: %w", err)
	}
	if direction == budget.Reserve && spentUnits+reservedUnits+units > ceiling {
		return budget.ErrInsufficient
	}
	var nextSeq int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(entry_seq), 0) + 1 FROM budget_entries WHERE ledger_id = $1`,
		ledgerID).Scan(&nextSeq); err != nil {
		return fmt.Errorf("workgraph: budget seq: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO budget_entries (id, ledger_id, entry_seq, direction, units, tool_ref)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.Must(uuid.NewV7()).String(), ledgerID, nextSeq, string(direction), units, toolRef); err != nil {
		return fmt.Errorf("workgraph: budget append: %w", err)
	}
	switch direction {
	case budget.Spend:
		spentUnits += units
	case budget.Reserve:
		reservedUnits += units
	case budget.Release:
		reservedUnits -= units
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE budget_ledgers SET spent_units = $2, reserved_units = $3, updated_at = now()
		WHERE id = $1`, ledgerID, spentUnits, reservedUnits); err != nil {
		return fmt.Errorf("workgraph: budget totals update: %w", err)
	}
	return nil
}

// graphBudgetRemaining returns the reserveable allowance for one
// attempt: the lesser of the current spec budget and what the ledger
// ceiling still has unspent and unreserved (the spec budget is the
// node's cumulative ceiling across attempts).
func graphBudgetRemaining(ctx context.Context, tx *sql.Tx, ledgerID string, specBudget int64) (int64, error) {
	var ceiling int64
	var committed int64 // spent + outstanding reservations
	err := tx.QueryRowContext(ctx, `
		SELECT l.budget_units,
		       (SELECT COALESCE(SUM(units) FILTER (WHERE direction = 'spend'), 0)
		          + COALESCE(SUM(units) FILTER (WHERE direction = 'reserve'), 0)
		          - COALESCE(SUM(units) FILTER (WHERE direction = 'release'), 0)
		        FROM budget_entries WHERE ledger_id = l.id)
		FROM budget_ledgers l WHERE l.id = $1`, ledgerID).
		Scan(&ceiling, &committed)
	if err != nil {
		return 0, fmt.Errorf("workgraph claim: budget remaining: %w", err)
	}
	remaining := specBudget
	if headroom := ceiling - committed; headroom < remaining {
		remaining = headroom
	}
	if remaining < 1 {
		return 0, budget.ErrInsufficient
	}
	return remaining, nil
}

// WorkNodeAttemptHeartbeat renews a graph attempt's lease with the
// same fencing discipline as the flat runner lease.
func (s pgWorkGraphStore) WorkNodeAttemptHeartbeat(ctx context.Context, attemptID, workerID, connectionGeneration string, expectedVersion int64, ttl time.Duration) (int64, error) {
	var newVersion int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE execution_attempts
		SET lease_version = lease_version + 1, lease_expires_at = now() + $5::interval
		WHERE id = $1 AND worker_id = $2 AND connection_generation = $3
		  AND status = 'running' AND lease_version = $4
		RETURNING lease_version`,
		attemptID, workerID, connectionGeneration, expectedVersion, fmt.Sprintf("%d seconds", int(ttl.Seconds()))).
		Scan(&newVersion)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		var worker, generation string
		lookupErr := s.db.QueryRowContext(ctx,
			`SELECT status, worker_id, connection_generation FROM execution_attempts WHERE id = $1`, attemptID).
			Scan(&status, &worker, &generation)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return 0, ErrAttemptNotFound
		}
		if lookupErr != nil {
			return 0, fmt.Errorf("workgraph heartbeat: lookup: %w", lookupErr)
		}
		if worker != workerID || generation != connectionGeneration {
			return 0, ErrRunnerGenerationStale
		}
		if status != "running" {
			return 0, ErrAttemptNotResumable
		}
		return 0, ErrLeaseVersionMismatch
	}
	if err != nil {
		return 0, fmt.Errorf("workgraph heartbeat: %w", err)
	}
	return newVersion, nil
}

// CompleteWorkNodeAttemptInput records one terminal outcome.
type CompleteWorkNodeAttemptInput struct {
	AttemptID             string
	WorkerID              string
	ConnectionGeneration  string
	Outcome               string // succeeded | failed | cancelled | blocked
	Summary               string
	UsageUnits            int64
	Actor                 string
}

// CompleteWorkNodeAttempt fences on (worker, generation, running),
// records the outcome capsule, settles the budget (spend the lesser
// of usage and reservation, release the remainder) and projects the
// node status. A succeeded attempt moves the node to validating —
// never to done: agents do not mark their own work complete
// (WGM-INV-010 discipline).
func (s pgWorkGraphStore) CompleteWorkNodeAttempt(ctx context.Context, in CompleteWorkNodeAttemptInput) error {
	switch in.Outcome {
	case AttemptOutcomeSucceeded, AttemptOutcomeFailed, AttemptOutcomeCancelled, AttemptOutcomeBlocked:
	default:
		return fmt.Errorf("%w: outcome must be succeeded/failed/cancelled/blocked: %q", ErrInvalidParameter, in.Outcome)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("workgraph complete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var projectID, planID, nodeID, ledgerID string
	var budgetUnits, nodeVersion int64
	err = tx.QueryRowContext(ctx, `
		SELECT ea.project_id::text, ea.plan_id::text, ea.node_id::text,
		       COALESCE(ea.budget_ledger_id::text, ''), ea.budget_units, n.node_version
		FROM execution_attempts ea
		JOIN work_nodes n ON n.id = ea.node_id
		WHERE ea.id = $1 AND ea.worker_id = $2 AND ea.connection_generation = $3 AND ea.status = 'running'
		FOR UPDATE OF ea`, in.AttemptID, in.WorkerID, in.ConnectionGeneration).
		Scan(&projectID, &planID, &nodeID, &ledgerID, &budgetUnits, &nodeVersion)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if lookupErr := tx.QueryRowContext(ctx,
			`SELECT 1 FROM execution_attempts WHERE id = $1`, in.AttemptID).Scan(&exists); errors.Is(lookupErr, sql.ErrNoRows) {
			return ErrAttemptNotFound
		} else if lookupErr != nil {
			return fmt.Errorf("workgraph complete: lookup: %w", lookupErr)
		}
		return ErrAttemptNotResumable
	}
	if err != nil {
		return fmt.Errorf("workgraph complete: fence: %w", err)
	}

	// Budget settlement: the reservation covered the whole attempt, so
	// the release returns all of it while the spend records the actual
	// (capped) usage — reserved nets to zero.
	if ledgerID != "" {
		usage := in.UsageUnits
		if usage < 0 {
			usage = 0
		}
		if usage > budgetUnits {
			usage = budgetUnits
		}
		if usage > 0 {
			if err := appendBudgetEntryTx(ctx, tx, ledgerID, budget.Spend, usage, "workgraph.complete"); err != nil {
				return err
			}
		}
		if err := appendBudgetEntryTx(ctx, tx, ledgerID, budget.Release, budgetUnits, "workgraph.complete"); err != nil {
			return err
		}
	}

	attemptStatus := map[string]string{
		AttemptOutcomeSucceeded: "succeeded",
		AttemptOutcomeFailed:    "failed",
		AttemptOutcomeCancelled: "cancelled",
		AttemptOutcomeBlocked:   "needs_human",
	}[in.Outcome]
	nodeStatus := map[string]string{
		AttemptOutcomeSucceeded: "validating",
		AttemptOutcomeFailed:    "failed",
		AttemptOutcomeCancelled: "cancelled",
		AttemptOutcomeBlocked:   "blocked",
	}[in.Outcome]

	outcome, err := json.Marshal(map[string]any{
		"outcome": in.Outcome, "summary": in.Summary, "usage_units": in.UsageUnits,
	})
	if err != nil {
		return fmt.Errorf("workgraph complete: marshal outcome: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE execution_attempts SET status = $2, outcome = $3::jsonb, ended_at = now()
		WHERE id = $1 AND status = 'running'`, in.AttemptID, attemptStatus, string(outcome)); err != nil {
		return fmt.Errorf("workgraph complete: attempt status: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE work_nodes SET status = $3, node_version = node_version + 1, updated_at = now()
		WHERE id = $1 AND plan_id = $2 AND node_version = $4`,
		nodeID, planID, nodeStatus, nodeVersion); err != nil {
		return fmt.Errorf("workgraph complete: node status: %w", err)
	}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.attempt.completed", ResourceType: "execution_attempt",
		ResourceID: in.AttemptID, Actor: in.Actor,
		OutboxType: "workgraph.attempt.completed",
		Payload: mustMarshal(map[string]any{
			"node_id": nodeID, "outcome": in.Outcome, "usage_units": in.UsageUnits}),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ExpireStaleWorkNodeAttempts sweeps running attempts past their
// lease expiry: the attempt is expired and the node returns to queued
// for a fresh attempt (WGM-INV-007: retry = new attempt; resume of the
// original binding is the Adapter's recovery path, documented in the
// scheduler doc).
func (s pgWorkGraphStore) ExpireStaleWorkNodeAttempts(ctx context.Context, actor string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("workgraph expire: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT id::text, node_id::text, project_id::text FROM execution_attempts
		WHERE status = 'running' AND lease_expires_at < now()
		FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return 0, fmt.Errorf("workgraph expire: select: %w", err)
	}
	type stale struct{ id, nodeID, projectID string }
	var stales []stale
	for rows.Next() {
		var st stale
		if err := rows.Scan(&st.id, &st.nodeID, &st.projectID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("workgraph expire: scan: %w", err)
		}
		stales = append(stales, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("workgraph expire: select: %w", err)
	}

	for _, st := range stales {
		if _, err := tx.ExecContext(ctx, `
			UPDATE execution_attempts SET status = 'expired', ended_at = now()
			WHERE id = $1 AND status = 'running'`, st.id); err != nil {
			return 0, fmt.Errorf("workgraph expire: attempt: %w", err)
		}
		// The dead attempt's reservation returns to the ledger so a
		// retry can reserve again.
		var ledgerID string
		var units int64
		lookupErr := tx.QueryRowContext(ctx, `
			SELECT COALESCE(budget_ledger_id::text, ''), budget_units
			FROM execution_attempts WHERE id = $1`, st.id).Scan(&ledgerID, &units)
		if lookupErr == nil && ledgerID != "" && units > 0 {
			if err := appendBudgetEntryTx(ctx, tx, ledgerID, budget.Release, units, "workgraph.expire"); err != nil {
				return 0, err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE work_nodes SET status = 'queued', node_version = node_version + 1, updated_at = now()
			WHERE id = $1 AND status = 'executing'`, st.nodeID); err != nil {
			return 0, fmt.Errorf("workgraph expire: node: %w", err)
		}
		if err := recordGovernanceEvent(ctx, tx, governanceEvent{
			ProjectID: st.projectID, Action: "workgraph.attempt.expired", ResourceType: "execution_attempt",
			ResourceID: st.id, Actor: actor, OutboxType: "workgraph.attempt.expired",
			Payload: mustMarshal(map[string]any{"node_id": st.nodeID, "attempt_id": st.id}),
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("workgraph expire: commit: %w", err)
	}
	return len(stales), nil
}

// GetExecutionAttempt loads one attempt row.
func (s pgWorkGraphStore) GetExecutionAttempt(ctx context.Context, attemptID string) (*ExecutionAttempt, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, plan_id::text, node_id::text, node_revision_id::text,
		       spec_digest, attempt_no, COALESCE(retry_of_attempt_id::text, ''),
		       principal, role, session_id, worker_id, worktree_path,
		       context_digest, context_set, COALESCE(budget_ledger_id::text, ''), budget_units,
		       lease_token::text, lease_epoch, lease_version, connection_generation,
		       lease_expires_at::text, idempotency_key, status,
		       COALESCE(outcome, 'null'), COALESCE(ended_at::text, ''), created_at::text
		FROM execution_attempts WHERE id = $1`, attemptID)
	return scanExecutionAttempt(row.Scan)
}

func scanExecutionAttempt(scan func(dest ...any) error) (*ExecutionAttempt, error) {
	var a ExecutionAttempt
	err := scan(&a.ID, &a.ProjectID, &a.PlanID, &a.NodeID, &a.NodeRevisionID,
		&a.SpecDigest, &a.AttemptNo, &a.RetryOfAttemptID,
		&a.Principal, &a.Role, &a.SessionID, &a.WorkerID, &a.WorktreePath,
		&a.ContextDigest, &a.ContextSet, &a.BudgetLedgerID, &a.BudgetUnits,
		&a.LeaseToken, &a.LeaseEpoch, &a.LeaseVersion, &a.ConnectionGeneration,
		&a.LeaseExpiresAt, &a.IdempotencyKey, &a.Status,
		&a.Outcome, &a.EndedAt, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAttemptNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: scan attempt: %w", err)
	}
	return &a, nil
}

// AggregateWorkPackageInput drives the pure aggregation for one
// parent node.
type AggregateWorkPackageInput struct {
	PlanID string
	NodeID string
	// IntegrationEvidence is the independent-evidence flag for the
	// PARENT scope (evaluation records / gate outcomes — wired by the
	// evaluation surface, J2c/H consume).
	IntegrationEvidence bool
	Actor               string
}

// AggregateWorkPackage projects one parent's status from its stored
// children outcomes and spec policies via the pure function, then CAS
// -updates the node. Re-running with unchanged children is a no-op
// (idempotent replay).
func (s pgWorkGraphStore) AggregateWorkPackage(ctx context.Context, in AggregateWorkPackageInput) (*WorkNode, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph aggregate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var projectID, nodeType, status string
	var nodeVersion int64
	var specJSON []byte
	err = tx.QueryRowContext(ctx, `
		SELECT p.project_id::text, n.node_type, n.status, n.node_version, nr.spec
		FROM work_nodes n
		JOIN work_plans p ON p.id = n.plan_id
		JOIN LATERAL (
			SELECT nr.spec FROM work_node_revisions nr
			JOIN plan_revisions pr ON pr.id = nr.plan_revision_id
			WHERE nr.node_id = n.id AND pr.plan_id = n.plan_id
			ORDER BY pr.revision_no DESC LIMIT 1
		) nr ON TRUE
		WHERE n.id = $1 AND n.plan_id = $2 FOR UPDATE OF n`,
		in.NodeID, in.PlanID).Scan(&projectID, &nodeType, &status, &nodeVersion, &specJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrWorkNodeNotFound, in.NodeID)
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph aggregate: node: %w", err)
	}
	if nodeType == workgraph.NodeTypeItem {
		return nil, fmt.Errorf("%w: aggregation targets work_package/gate nodes, not work items", ErrInvalidParameter)
	}

	var spec workgraph.NodeSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, fmt.Errorf("workgraph aggregate: decode spec: %w", err)
	}
	policies := workgraph.NodePolicies{FailurePolicy: spec.FailurePolicy, CancelPolicy: spec.CancelPolicy}
	if spec.SuccessThreshold != nil {
		policies.SuccessThreshold = *spec.SuccessThreshold
	}

	childRows, err := tx.QueryContext(ctx, `
		SELECT status FROM work_nodes WHERE parent_node_id = $1 AND plan_id = $2`,
		in.NodeID, in.PlanID)
	if err != nil {
		return nil, fmt.Errorf("workgraph aggregate: children: %w", err)
	}
	var children []workgraph.ChildState
	for childRows.Next() {
		var childStatus string
		if err := childRows.Scan(&childStatus); err != nil {
			childRows.Close()
			return nil, fmt.Errorf("workgraph aggregate: scan child: %w", err)
		}
		children = append(children, workgraph.ChildState{NodeID: "", Status: childOutcomeOf(childStatus)})
	}
	childRows.Close()
	if err := childRows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph aggregate: children: %w", err)
	}

	result := workgraph.Aggregate(workgraph.AggregateInput{
		Policies: policies, Children: children, IntegrationEvidence: in.IntegrationEvidence,
	})
	if result.Status == status || result.Status == workgraph.ParentAggregating && status == "aggregating" {
		// Replay no-op: the projection is a fixed point.
		_ = tx.Rollback()
		return s.GetWorkNode(ctx, in.PlanID, in.NodeID)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE work_nodes SET status = $3, node_version = node_version + 1, updated_at = now()
		WHERE id = $1 AND plan_id = $2 AND node_version = $4`,
		in.NodeID, in.PlanID, result.Status, nodeVersion); err != nil {
		return nil, fmt.Errorf("workgraph aggregate: node status: %w", err)
	}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.node.aggregated", ResourceType: "work_node",
		ResourceID: in.NodeID, Actor: in.Actor, OutboxType: "workgraph.node.aggregated",
		Payload: mustMarshal(map[string]any{
			"node_id": in.NodeID, "status": result.Status, "reason": result.Reason,
			"children": len(children)}),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph aggregate: commit: %w", err)
	}
	return s.GetWorkNode(ctx, in.PlanID, in.NodeID)
}

// childOutcomeOf maps a stored node status to the aggregation
// vocabulary: only gate/evaluation-passed outcomes count as succeeded.
func childOutcomeOf(nodeStatus string) string {
	switch nodeStatus {
	case "done", "satisfied":
		return workgraph.ChildSucceeded
	case "failed":
		return workgraph.ChildFailed
	case "cancelled":
		return workgraph.ChildCancelled
	case "needs_human", "blocked", "ready_for_human_merge":
		return workgraph.ChildNeedsHuman
	default:
		return workgraph.ChildRunning
	}
}

// CancelWorkNode cancels one node and projects the cancel_policy over
// its requires-reachable descendants (cancel set) and the
// optional-only fringe (detach set). Cancelled nodes are never
// scheduled again (WGS-RULE-005).
func (s pgWorkGraphStore) CancelWorkNode(ctx context.Context, planID, nodeID string, expectedNodeVersion int64, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("workgraph cancel: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var projectID, specCancelPolicy string
	var nodeVersion int64
	err = tx.QueryRowContext(ctx, `
		SELECT p.project_id::text, n.node_version, nr.spec->>'cancel_policy'
		FROM work_nodes n
		JOIN work_plans p ON p.id = n.plan_id
		JOIN LATERAL (
			SELECT nr.spec FROM work_node_revisions nr
			JOIN plan_revisions pr ON pr.id = nr.plan_revision_id
			WHERE nr.node_id = n.id AND pr.plan_id = n.plan_id
			ORDER BY pr.revision_no DESC LIMIT 1
		) nr ON TRUE
		WHERE n.id = $1 AND n.plan_id = $2 FOR UPDATE OF n`, nodeID, planID).
		Scan(&projectID, &nodeVersion, &specCancelPolicy)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrWorkNodeNotFound, nodeID)
	}
	if err != nil {
		return fmt.Errorf("workgraph cancel: node: %w", err)
	}
	if nodeVersion != expectedNodeVersion {
		return ErrNodeVersionMismatch
	}

	edgeRows, err := tx.QueryContext(ctx, `
		SELECT from_node_id::text, to_node_id::text, requirement FROM work_dependencies WHERE plan_id = $1`, planID)
	if err != nil {
		return fmt.Errorf("workgraph cancel: edges: %w", err)
	}
	var edges []workgraph.DescendantEdge
	for edgeRows.Next() {
		var e workgraph.DescendantEdge
		if err := edgeRows.Scan(&e.From, &e.To, &e.Requirement); err != nil {
			edgeRows.Close()
			return fmt.Errorf("workgraph cancel: scan edge: %w", err)
		}
		edges = append(edges, e)
	}
	edgeRows.Close()
	if err := edgeRows.Err(); err != nil {
		return fmt.Errorf("workgraph cancel: edges: %w", err)
	}

	cancelSet, detachSet := workgraph.CancellationProjection(nodeID, specCancelPolicy, edges)
	cancelSet = append(cancelSet, nodeID)

	for _, id := range cancelSet {
		if _, err := tx.ExecContext(ctx, `
			UPDATE work_nodes SET status = 'cancelled', node_version = node_version + 1, updated_at = now()
			WHERE id = $1 AND plan_id = $2 AND status <> 'cancelled'`, id, planID); err != nil {
			return fmt.Errorf("workgraph cancel: node %s: %w", id, err)
		}
	}
	for _, id := range detachSet {
		if err := recordGovernanceEvent(ctx, tx, governanceEvent{
			ProjectID: projectID, Action: "workgraph.node.detached", ResourceType: "work_node",
			ResourceID: id, Actor: actor, OutboxType: "workgraph.node.detached",
			Payload: mustMarshal(map[string]any{"node_id": id, "cancelled_ancestor": nodeID}),
		}); err != nil {
			return err
		}
	}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.node.cancelled", ResourceType: "work_node",
		ResourceID: nodeID, Actor: actor, OutboxType: "workgraph.node.cancelled",
		Payload: mustMarshal(map[string]any{
			"node_id": nodeID, "cancel_policy": specCancelPolicy,
			"cancelled": cancelSet, "detached": detachSet}),
	}); err != nil {
		return err
	}
	return tx.Commit()
}
