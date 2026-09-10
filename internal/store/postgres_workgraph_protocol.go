package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
	"github.com/google/uuid"
)

// PostgreSQL implementation of the J2b decomposition protocol and
// execution binding (migration 0019). Every state change keeps the
// house discipline: one transaction, CAS first, business rows, audit
// row + outbox event, commit (WGM-INV-012). The domain logic itself
// (validation classes, aggregation, cancellation projection) lives in
// internal/workgraph and is consumed here — never re-implemented.

var workPatternNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

// ---------------------------------------------------------------------------
// Intent layer
// ---------------------------------------------------------------------------

// CreateBusinessProblem inserts one Intent-layer problem.
func (s pgWorkGraphStore) CreateBusinessProblem(ctx context.Context, in BusinessProblem) (*BusinessProblem, error) {
	if l := len([]rune(in.Title)); l < 1 || l > 120 {
		return nil, fmt.Errorf("%w: problem title must be 1-120 characters", ErrInvalidParameter)
	}
	if l := len([]rune(in.Statement)); l < 1 || l > 2000 {
		return nil, fmt.Errorf("%w: problem statement must be 1-2000 characters", ErrInvalidParameter)
	}
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO business_problems (id, project_id, title, statement, created_by)
		VALUES ($1, $2, $3, $4, $5)`,
		id, in.ProjectID, in.Title, in.Statement, in.CreatedBy); err != nil {
		return nil, fmt.Errorf("workgraph: create problem: %w", err)
	}
	problem := in
	problem.ID = id
	problem.Status = "active"
	return &problem, nil
}

// CreateOutcomeContract appends one contract version to a problem.
func (s pgWorkGraphStore) CreateOutcomeContract(ctx context.Context, in OutcomeContract) (*OutcomeContract, error) {
	if len(in.SuccessCriteria) == 0 {
		return nil, fmt.Errorf("%w: success criteria must be a non-empty array", ErrInvalidParameter)
	}
	criteria, err := workgraph.CanonicalJSON(in.SuccessCriteria)
	if err != nil {
		return nil, fmt.Errorf("%w: success criteria must be JSON: %v", ErrInvalidParameter, err)
	}
	doc, err := workgraph.CanonicalJSON(in.ConstraintsDoc)
	if err != nil {
		return nil, fmt.Errorf("%w: constraints must be JSON: %v", ErrInvalidParameter, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var next int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(version), 0) + 1 FROM outcome_contracts WHERE problem_id = $1`,
		in.ProblemID).Scan(&next); err != nil {
		return nil, fmt.Errorf("workgraph: contract version: %w", err)
	}
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outcome_contracts (id, problem_id, version, success_criteria, constraints_doc)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb)`,
		id, in.ProblemID, next, string(criteria), string(doc)); err != nil {
		return nil, fmt.Errorf("workgraph: create contract: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph: commit: %w", err)
	}
	contract := in
	contract.ID = id
	contract.Version = next
	return &contract, nil
}

// CreateCapability registers one project capability (capability
// routing target; roles are not capabilities — WGS-RULE-004).
func (s pgWorkGraphStore) CreateCapability(ctx context.Context, in Capability) (*Capability, error) {
	if !workPatternNamePattern.MatchString(strings.ReplaceAll(in.CapKey, "-", ".")) && !capabilityKeyPattern.MatchString(in.CapKey) {
		return nil, fmt.Errorf("%w: capability key must be lowercase dotted/dashed", ErrInvalidParameter)
	}
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO capabilities (id, project_id, cap_key, description)
		VALUES ($1, $2, $3, $4)`,
		id, in.ProjectID, in.CapKey, in.Description); err != nil {
		return nil, fmt.Errorf("workgraph: create capability: %w", err)
	}
	capability := in
	capability.ID = id
	return &capability, nil
}

var capabilityKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9.-]{1,63}$`)

// LinkProblemCapability records which capabilities a problem involves.
func (s pgWorkGraphStore) LinkProblemCapability(ctx context.Context, problemID, capabilityID string) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO problem_capability_links (problem_id, capability_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, problemID, capabilityID); err != nil {
		return fmt.Errorf("workgraph: link capability: %w", err)
	}
	return nil
}

// AttachWorkPlanIntent binds a plan to its primary business problem
// and outcome contract (WGP-REQ-001: exactly one primary binding).
func (s pgWorkGraphStore) AttachWorkPlanIntent(ctx context.Context, in WorkPlanIntent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var projectID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id::text FROM work_plans WHERE id = $1`, in.PlanID).Scan(&projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrWorkPlanNotFound
		}
		return fmt.Errorf("workgraph: plan lookup: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO work_plan_intents (plan_id, problem_id, outcome_contract_id, attached_by)
		VALUES ($1, $2, $3, $4)`, in.PlanID, in.ProblemID, in.OutcomeContractID, in.AttachedBy); err != nil {
		return fmt.Errorf("workgraph: attach intent: %w", err)
	}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.plan.intent_attached", ResourceType: "work_plan",
		ResourceID: in.PlanID, Actor: in.AttachedBy, OutboxType: "workgraph.plan.intent_attached",
		Payload: mustMarshal(map[string]any{"plan_id": in.PlanID, "problem_id": in.ProblemID}),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Work patterns
// ---------------------------------------------------------------------------

// CreateWorkPattern registers one template version (draft or active).
func (s pgWorkGraphStore) CreateWorkPattern(ctx context.Context, in WorkPattern) (*WorkPattern, error) {
	if !workPatternNamePattern.MatchString(in.Name) {
		return nil, fmt.Errorf("%w: pattern name must match ^[a-z][a-z0-9-]{1,63}$", ErrInvalidParameter)
	}
	if in.Version < 1 {
		return nil, fmt.Errorf("%w: pattern version must be positive", ErrInvalidParameter)
	}
	if in.Status != "draft" && in.Status != "active" {
		return nil, fmt.Errorf("%w: pattern status must be draft/active at creation", ErrInvalidParameter)
	}
	body, err := workgraph.CanonicalJSON(in.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: pattern body must be JSON: %v", ErrInvalidParameter, err)
	}
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO work_patterns (id, project_id, name, version, status, body, created_by)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`,
		id, in.ProjectID, in.Name, in.Version, in.Status, string(body), in.CreatedBy); err != nil {
		return nil, fmt.Errorf("workgraph: create pattern: %w", err)
	}
	pattern := in
	pattern.ID = id
	pattern.Body = body
	return &pattern, nil
}

// GetWorkPattern loads one template version.
func (s pgWorkGraphStore) GetWorkPattern(ctx context.Context, patternID string) (*WorkPattern, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, name, version, status, body, created_by
		FROM work_patterns WHERE id = $1`, patternID)
	var pattern WorkPattern
	if err := row.Scan(&pattern.ID, &pattern.ProjectID, &pattern.Name, &pattern.Version,
		&pattern.Status, &pattern.Body, &pattern.CreatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrInvalidParameter, patternID)
		}
		return nil, fmt.Errorf("workgraph: get pattern: %w", err)
	}
	return &pattern, nil
}

// ---------------------------------------------------------------------------
// Decomposition proposal submit
// ---------------------------------------------------------------------------

// SubmitDecompositionProposalInput carries one Coordinator proposal.
type SubmitDecompositionProposalInput struct {
	Proposal       workgraph.DecompositionProposal
	Payload        []byte // the wire document as received (audited verbatim)
	IdempotencyKey string
	SubmittedBy    string
	CorrelationID  string
	Limits         workgraph.ProposalLimits
}

// SubmitDecompositionProposal validates and decides one proposal:
// clean proposals apply atomically to the plan's draft revision
// (nodes + node revisions + requires edges + artifact flows behind the
// graph CAS); rejected proposals persist their stable violation codes.
// Replaying the idempotency key returns the decided record unchanged.
func (s pgWorkGraphStore) SubmitDecompositionProposal(ctx context.Context, in SubmitDecompositionProposalInput) (*DecompositionProposalRecord, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency key is required", ErrInvalidParameter)
	}
	// Idempotent replay: the decision is final, return it verbatim.
	if existing, err := s.GetDecompositionProposalByKey(ctx, in.IdempotencyKey); err == nil {
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Lock the plan and verify the graph CAS.
	var projectID string
	var graphVersion int64
	err = tx.QueryRowContext(ctx, `
		SELECT project_id::text, graph_version FROM work_plans WHERE id = $1 FOR UPDATE`,
		in.Proposal.PlanID).Scan(&projectID, &graphVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: lock plan: %w", err)
	}
	if graphVersion != in.Proposal.ExpectedGraphVersion {
		return nil, ErrGraphVersionMismatch
	}
	revisionID, err := currentDraftRevision(ctx, tx, in.Proposal.PlanID)
	if err != nil {
		return nil, err // sealed or missing: replan first (J2b-2)
	}

	// Build the validation context from the stored graph.
	validationCtx, err := loadProposalContext(ctx, tx, in.Proposal)
	if err != nil {
		return nil, err
	}
	violations := workgraph.ValidateProposal(in.Proposal, in.Limits, *validationCtx)

	payload := in.Payload
	if len(payload) == 0 {
		raw, err := json.Marshal(in.Proposal)
		if err != nil {
			return nil, fmt.Errorf("workgraph: marshal proposal: %w", err)
		}
		payload = raw
	}
	proposalID := uuid.Must(uuid.NewV7()).String()

	if len(violations) > 0 {
		encoded, err := json.Marshal(violations)
		if err != nil {
			return nil, fmt.Errorf("workgraph: marshal violations: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO decomposition_proposals
				(id, project_id, plan_id, work_pattern_id, expected_graph_version, payload,
				 idempotency_key, status, violations, submitted_by, decided_at)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, 'rejected', $8::jsonb, $9, now())`,
			proposalID, projectID, in.Proposal.PlanID, in.Proposal.WorkPattern.PatternID,
			in.Proposal.ExpectedGraphVersion, string(payload), in.IdempotencyKey,
			string(encoded), in.SubmittedBy); err != nil {
			return nil, fmt.Errorf("workgraph: record rejection: %w", err)
		}
		if err := recordGovernanceEvent(ctx, tx, governanceEvent{
			ProjectID: projectID, Action: "workgraph.proposal.rejected", ResourceType: "decomposition_proposal",
			ResourceID: proposalID, Actor: in.SubmittedBy, CorrelationID: in.CorrelationID,
			OutboxType: "workgraph.proposal.rejected",
			Payload:    mustMarshal(map[string]any{"plan_id": in.Proposal.PlanID, "violations": violations}),
		}); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("workgraph: commit: %w", err)
		}
		return s.GetDecompositionProposalByKey(ctx, in.IdempotencyKey)
	}

	// Clean proposal: apply everything in this transaction.
	appliedIDs, err := applyProposal(ctx, tx, in.Proposal, revisionID)
	if err != nil {
		return nil, err
	}
	encodedIDs, err := json.Marshal(appliedIDs)
	if err != nil {
		return nil, fmt.Errorf("workgraph: marshal applied ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO decomposition_proposals
			(id, project_id, plan_id, work_pattern_id, expected_graph_version, payload,
			 idempotency_key, status, applied_node_ids, submitted_by, decided_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, 'applied', $8::jsonb, $9, now())`,
		proposalID, projectID, in.Proposal.PlanID, in.Proposal.WorkPattern.PatternID,
		in.Proposal.ExpectedGraphVersion, string(payload), in.IdempotencyKey,
		string(encodedIDs), in.SubmittedBy); err != nil {
		return nil, fmt.Errorf("workgraph: record application: %w", err)
	}
	if err := bumpGraphVersion(ctx, tx, in.Proposal.PlanID); err != nil {
		return nil, err
	}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.proposal.applied", ResourceType: "decomposition_proposal",
		ResourceID: proposalID, Actor: in.SubmittedBy, CorrelationID: in.CorrelationID,
		OutboxType: "workgraph.proposal.applied",
		Payload:    mustMarshal(map[string]any{"plan_id": in.Proposal.PlanID, "nodes": appliedIDs}),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph: commit: %w", err)
	}
	return s.GetDecompositionProposalByKey(ctx, in.IdempotencyKey)
}

// GetDecompositionProposalByKey loads one proposal by idempotency key.
func (s pgWorkGraphStore) GetDecompositionProposalByKey(ctx context.Context, key string) (*DecompositionProposalRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, plan_id::text, work_pattern_id::text,
		       expected_graph_version, payload, idempotency_key, status,
		       COALESCE(violations, 'null'), COALESCE(applied_node_ids, 'null'),
		       submitted_by, COALESCE(decided_at::text, ''), created_at::text
		FROM decomposition_proposals WHERE idempotency_key = $1`, key)
	return scanDecompositionProposal(row.Scan)
}

// GetDecompositionProposal loads one proposal by id.
func (s pgWorkGraphStore) GetDecompositionProposal(ctx context.Context, id string) (*DecompositionProposalRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, plan_id::text, work_pattern_id::text,
		       expected_graph_version, payload, idempotency_key, status,
		       COALESCE(violations, 'null'), COALESCE(applied_node_ids, 'null'),
		       submitted_by, COALESCE(decided_at::text, ''), created_at::text
		FROM decomposition_proposals WHERE id = $1`, id)
	return scanDecompositionProposal(row.Scan)
}

// ListDecompositionProposals returns every decided proposal of one plan,
// newest first (J2c read surface for the HITL review view).
func (s pgWorkGraphStore) ListDecompositionProposals(ctx context.Context, planID string) ([]*DecompositionProposalRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, project_id::text, plan_id::text, work_pattern_id::text,
		       expected_graph_version, payload, idempotency_key, status,
		       COALESCE(violations, 'null'), COALESCE(applied_node_ids, 'null'),
		       submitted_by, COALESCE(decided_at::text, ''), created_at::text
		FROM decomposition_proposals WHERE plan_id = $1::uuid
		ORDER BY created_at DESC`, planID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: list proposals: %w", err)
	}
	defer rows.Close()
	records := []*DecompositionProposalRecord{}
	for rows.Next() {
		record, scanErr := scanDecompositionProposal(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func scanDecompositionProposal(scan func(dest ...any) error) (*DecompositionProposalRecord, error) {
	var record DecompositionProposalRecord
	var violations, appliedIDs []byte
	err := scan(&record.ID, &record.ProjectID, &record.PlanID, &record.WorkPatternID,
		&record.ExpectedGraphVersion, &record.Payload, &record.IdempotencyKey, &record.Status,
		&violations, &appliedIDs, &record.SubmittedBy, &record.DecidedAt, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: scan proposal: %w", err)
	}
	record.Violations = violations
	record.AppliedNodeIDs = appliedIDs
	return &record, nil
}

// loadProposalContext gathers every stored fact the pure validator
// needs about the plan the proposal targets.
func loadProposalContext(ctx context.Context, tx *sql.Tx, p workgraph.DecompositionProposal) (*workgraph.ProposalContext, error) {
	out := &workgraph.ProposalContext{
		Nodes:         map[string]workgraph.NodeFact{},
		ChildCount:    map[string]int{},
		SlotsUnder:    map[string]map[string]bool{},
		HumanCodes:    map[string]bool{},
		Consumes:      map[string]map[string]bool{},
		Assets:        map[string]workgraph.AssetFact{},
		PatternActive: false,
	}

	// Pattern version reference.
	var patternStatus string
	err := tx.QueryRowContext(ctx, `
		SELECT status FROM work_patterns WHERE id = $1 AND version = $2`,
		p.WorkPattern.PatternID, p.WorkPattern.Version).Scan(&patternStatus)
	if err == nil {
		out.PatternActive = patternStatus == "active"
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("workgraph: pattern lookup: %w", err)
	}

	// Every ext: reference across parents, edges and flows.
	refs := map[string]bool{}
	collect := func(ref string) {
		if strings.HasPrefix(ref, "ext:") {
			refs[ref[len("ext:"):]] = true
		}
	}
	for _, n := range p.Nodes {
		collect(n.Parent)
	}
	for _, e := range p.Dependencies {
		collect(e.From)
		collect(e.To)
	}
	for _, f := range p.Flows {
		collect(f.Node)
	}
	for uuid := range refs {
		var fact workgraph.NodeFact
		err := tx.QueryRowContext(ctx, `
			SELECT node_type, depth FROM work_nodes WHERE id = $1 AND plan_id = $2`,
			uuid, p.PlanID).Scan(&fact.NodeType, &fact.Depth)
		if errors.Is(err, sql.ErrNoRows) {
			out.Nodes[uuid] = workgraph.NodeFact{Exists: false}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("workgraph: node fact: %w", err)
		}
		fact.Exists = true
		out.Nodes[uuid] = fact
	}

	// Stored children and slot occupancy per referenced parent.
	rows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(parent_node_id::text, ''), slot_key FROM work_nodes WHERE plan_id = $1`,
		p.PlanID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: plan nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var parent, slot string
		if err := rows.Scan(&parent, &slot); err != nil {
			return nil, fmt.Errorf("workgraph: scan plan node: %w", err)
		}
		if parent != "" {
			out.ChildCount[parent]++
			if out.SlotsUnder[parent] == nil {
				out.SlotsUnder[parent] = map[string]bool{}
			}
			out.SlotsUnder[parent][slot] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: plan nodes: %w", err)
	}

	// Human codes and requires edges of the plan.
	codeRows, err := tx.QueryContext(ctx, `
		SELECT human_code, id::text FROM work_nodes WHERE plan_id = $1`, p.PlanID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: plan codes: %w", err)
	}
	for codeRows.Next() {
		var code, id string
		if err := codeRows.Scan(&code, &id); err != nil {
			codeRows.Close()
			return nil, fmt.Errorf("workgraph: scan code: %w", err)
		}
		out.HumanCodes[code] = true
		if _, known := out.Nodes[id]; !known {
			out.Nodes[id] = workgraph.NodeFact{Exists: true}
		}
	}
	codeRows.Close()
	if err := codeRows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: plan codes: %w", err)
	}

	edgeRows, err := tx.QueryContext(ctx, `
		SELECT from_node_id::text, to_node_id::text, requirement
		FROM work_dependencies WHERE plan_id = $1`, p.PlanID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: plan edges: %w", err)
	}
	for edgeRows.Next() {
		var from, to, requirement string
		if err := edgeRows.Scan(&from, &to, &requirement); err != nil {
			edgeRows.Close()
			return nil, fmt.Errorf("workgraph: scan edge: %w", err)
		}
		out.Requires = append(out.Requires, workgraph.EdgeFact{From: from, To: to, Requirement: requirement})
	}
	edgeRows.Close()
	if err := edgeRows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: plan edges: %w", err)
	}

	// Existing consumes bindings.
	flowRows, err := tx.QueryContext(ctx, `
		SELECT node_id::text, port_key FROM node_artifact_flows
		WHERE plan_id = $1 AND direction = 'consumes'`, p.PlanID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: plan flows: %w", err)
	}
	for flowRows.Next() {
		var nodeID, port string
		if err := flowRows.Scan(&nodeID, &port); err != nil {
			flowRows.Close()
			return nil, fmt.Errorf("workgraph: scan flow: %w", err)
		}
		if out.Consumes[nodeID] == nil {
			out.Consumes[nodeID] = map[string]bool{}
		}
		out.Consumes[nodeID][port] = true
	}
	flowRows.Close()
	if err := flowRows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: plan flows: %w", err)
	}

	// Referenced asset versions.
	assetRefs := map[string]bool{}
	for _, f := range p.Flows {
		assetRefs[f.AssetRef] = true
	}
	for ref := range assetRefs {
		assetID, version, ok := splitAssetRef(ref)
		if !ok {
			out.Assets[ref] = workgraph.AssetFact{Exists: false}
			continue
		}
		var status string
		err := tx.QueryRowContext(ctx, `
			SELECT status FROM assets WHERE asset_id = $1 AND version = $2`,
			assetID, version).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			out.Assets[ref] = workgraph.AssetFact{Exists: false}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("workgraph: asset lookup: %w", err)
		}
		out.Assets[ref] = workgraph.AssetFact{Exists: true, Status: status}
	}
	return out, nil
}

func splitAssetRef(ref string) (string, int, bool) {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return "", 0, false
	}
	var version int
	if _, err := fmt.Sscanf(ref[at+1:], "%d", &version); err != nil || version < 1 {
		return "", 0, false
	}
	return ref[:at], version, true
}

// applyProposal inserts nodes, node revisions, requires edges and
// artifact flows of a clean proposal on the caller's transaction.
func applyProposal(ctx context.Context, tx *sql.Tx, p workgraph.DecompositionProposal, revisionID string) (map[string]string, error) {
	localToUUID := map[string]string{}
	depths := map[string]int{} // node uuid -> containment depth
	nodeByLocal := map[string]workgraph.ProposalNode{}

	// Stored ext parents carry their depth in the graph; local parents
	// resolve through the chain (validator guarantees a forest).
	storedDepth := map[string]int{}
	for _, n := range p.Nodes {
		nodeByLocal[n.LocalID] = n
		localToUUID[n.LocalID] = uuid.Must(uuid.NewV7()).String()
		if strings.HasPrefix(n.Parent, "ext:") {
			storedDepth[n.Parent[4:]] = 0
		}
	}
	for nodeUUID := range storedDepth {
		var depth int
		if err := tx.QueryRowContext(ctx, `
			SELECT depth FROM work_nodes WHERE id = $1 AND plan_id = $2`,
			nodeUUID, p.PlanID).Scan(&depth); err != nil {
			return nil, fmt.Errorf("workgraph: parent depth: %w", err)
		}
		storedDepth[nodeUUID] = depth
	}

	var depthOf func(localID string, visiting map[string]bool) int
	depthOf = func(localID string, visiting map[string]bool) int {
		if d, ok := depths[localToUUID[localID]]; ok {
			return d
		}
		n := nodeByLocal[localID]
		if strings.HasPrefix(n.Parent, "ext:") {
			d := storedDepth[n.Parent[4:]] + 1
			depths[localToUUID[localID]] = d
			return d
		}
		if visiting[localID] {
			return 0 // containment cycle: already rejected upstream
		}
		visiting[localID] = true
		d := depthOf(n.Parent, visiting) + 1
		depths[localToUUID[localID]] = d
		return d
	}

	resolveNode := func(ref string) (string, error) {
		if strings.HasPrefix(ref, "ext:") {
			return ref[4:], nil
		}
		if id, ok := localToUUID[ref]; ok {
			return id, nil
		}
		return "", fmt.Errorf("%w: unknown node reference %q", ErrInvalidParameter, ref)
	}

	for _, n := range p.Nodes {
		parentUUID, err := resolveNode(n.Parent)
		if err != nil {
			return nil, err
		}
		depth := depthOf(n.LocalID, map[string]bool{})

		spec, err := workgraph.MarshalSpec(n.Spec)
		if err != nil {
			return nil, err
		}
		specDigest, err := workgraph.SpecDigest(spec)
		if err != nil {
			return nil, err
		}
		status := "draft"
		if n.NodeType == workgraph.NodeTypeItem {
			status = "queued"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_nodes (id, plan_id, parent_node_id, node_type, slot_key, human_code, status, node_version, depth)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8)`,
			localToUUID[n.LocalID], p.PlanID, parentUUID, n.NodeType, n.SlotKey, n.HumanCode, status, depth); err != nil {
			return nil, fmt.Errorf("workgraph: apply node %s: %w", n.LocalID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_node_revisions (id, node_id, plan_revision_id, spec_digest, spec)
			VALUES ($1, $2, $3, $4, $5::jsonb)`,
			uuid.Must(uuid.NewV7()).String(), localToUUID[n.LocalID], revisionID, specDigest, string(spec)); err != nil {
			return nil, fmt.Errorf("workgraph: apply node revision %s: %w", n.LocalID, err)
		}
	}

	// Requires edges: executable endpoints + per-edge cycle guard.
	for _, e := range p.Dependencies {
		from, err := resolveNode(e.From)
		if err != nil {
			return nil, err
		}
		to, err := resolveNode(e.To)
		if err != nil {
			return nil, err
		}
		requirement := e.Requirement
		if requirement == "" {
			requirement = workgraph.DependencyRequired
		}
		for _, nodeUUID := range []string{from, to} {
			var nodeType string
			err := tx.QueryRowContext(ctx, `
				SELECT node_type FROM work_nodes WHERE id = $1 AND plan_id = $2`,
				nodeUUID, p.PlanID).Scan(&nodeType)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w: %s", ErrWorkNodeNotFound, nodeUUID)
			}
			if err != nil {
				return nil, fmt.Errorf("workgraph: endpoint check: %w", err)
			}
			if nodeType == workgraph.NodeTypePackage {
				return nil, fmt.Errorf("%w: work_package %s cannot enter the requires graph (WGM-INV-003)",
					ErrInvalidParameter, nodeUUID)
			}
		}
		var cyclic bool
		err = tx.QueryRowContext(ctx, `
			WITH RECURSIVE reach(node_id) AS (
				SELECT $3::uuid
				UNION
				SELECT wd.to_node_id FROM work_dependencies wd
				JOIN reach r ON wd.from_node_id = r.node_id
				WHERE wd.plan_id = $1
			)
			SELECT EXISTS(SELECT 1 FROM reach WHERE node_id = $2::uuid)`,
			p.PlanID, from, to).Scan(&cyclic)
		if err != nil {
			return nil, fmt.Errorf("workgraph: apply cycle check: %w", err)
		}
		if cyclic {
			return nil, fmt.Errorf("%w: %s -> %s would close a cycle", ErrCircularDependency, from, to)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO work_dependencies (id, plan_id, from_node_id, to_node_id, requirement)
			VALUES ($1, $2, $3, $4, $5)`,
			uuid.Must(uuid.NewV7()).String(), p.PlanID, from, to, requirement); err != nil {
			return nil, fmt.Errorf("workgraph: apply dependency: %w", err)
		}
	}

	// Artifact flows.
	for _, f := range p.Flows {
		nodeUUID, err := resolveNode(f.Node)
		if err != nil {
			return nil, err
		}
		assetID, version, ok := splitAssetRef(f.AssetRef)
		if !ok {
			return nil, fmt.Errorf("%w: asset ref %q", ErrInvalidParameter, f.AssetRef)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO node_artifact_flows (id, plan_id, node_id, direction, asset_id, asset_version, port_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.Must(uuid.NewV7()).String(), p.PlanID, nodeUUID, f.Direction, assetID, version, f.PortKey); err != nil {
			return nil, fmt.Errorf("workgraph: apply flow: %w", err)
		}
	}
	return localToUUID, nil
}
