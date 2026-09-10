package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PostgreSQL persistence for the migration 0017 work graph (J2a). Every
// structural mutation runs inside one transaction that: (1) locks the
// plan row and compares expected_graph_version, (2) rejects writes once
// the current plan revision is sealed, (3) writes the business rows,
// (4) bumps the graph version, and (5) commits the audit row and the
// outbox event together with the change (WGM-INV-012).

type pgWorkGraphStore struct{ db *sql.DB }

// WorkGraph returns the work graph store bound to the pool.
func (s *PostgresStore) WorkGraph() pgWorkGraphStore { return pgWorkGraphStore{db: s.db} }

// governanceEvent is one atomic audit + outbox pair.
type governanceEvent struct {
	ProjectID     string
	Action        string
	ResourceType  string
	ResourceID    string
	Actor         string
	Reason        string
	CorrelationID string
	OutboxType    string
	Payload       []byte
}

// recordGovernanceEvent writes the audit_events row and the matching
// outbox_events row on the caller's transaction: either both commit with
// the state change or neither does.
func recordGovernanceEvent(ctx context.Context, tx *sql.Tx, event governanceEvent) error {
	correlation := event.CorrelationID
	if correlation == "" {
		correlation = "wgm-" + uuid.Must(uuid.NewV7()).String()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events
			(actor_principal, project_id, action, resource_type, resource_id, decision, reason, correlation_id)
		VALUES ($1, $2, $3, $4, $5, 'allow', $6, $7)`,
		event.Actor, event.ProjectID, event.Action, event.ResourceType,
		event.ResourceID, event.Reason, correlation); err != nil {
		return fmt.Errorf("governance event audit %s: %w", event.Action, err)
	}
	digest, err := SpecDigest(event.Payload)
	if err != nil {
		return fmt.Errorf("governance event payload digest: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_events
			(event_id, event_type, event_version, source, project_id, subject,
			 occurred_at, correlation_id, causation_id, payload_digest, sensitivity, payload, status)
		VALUES ($1, $2, 1, 'control-plane', $3, $4, $5, $6, $6, $7, 'internal', $8::jsonb, 'pending')`,
		uuid.Must(uuid.NewV7()).String(), event.OutboxType, event.ProjectID, event.ResourceID,
		time.Now().UTC(), correlation, digest, string(event.Payload)); err != nil {
		return fmt.Errorf("governance event outbox %s: %w", event.OutboxType, err)
	}
	return nil
}

// CreateWorkPlanInput creates a plan with its root node and revision 1
// (draft) atomically.
type CreateWorkPlanInput struct {
	ProjectID     string
	Title         string
	HumanCode     string
	RootSlotKey   string
	RootSpec      []byte
	Actor         string
	CorrelationID string
}

// CreateWorkPlan inserts work_plans + root work_nodes + plan_revisions
// (revision 1, draft) + the root's work_node_revisions row in one
// transaction and audits workgraph.plan.created.
func (s pgWorkGraphStore) CreateWorkPlan(ctx context.Context, in CreateWorkPlanInput) (*WorkPlan, error) {
	if err := ValidatePlanHumanCode(in.HumanCode); err != nil {
		return nil, err
	}
	if l := len([]rune(in.Title)); l < 1 || l > 120 {
		return nil, fmt.Errorf("%w: plan title must be 1-120 characters", ErrInvalidParameter)
	}
	slot := in.RootSlotKey
	if slot == "" {
		slot = "root"
	}
	if err := ValidateSlotKey(slot); err != nil {
		return nil, err
	}
	rootDigest, err := SpecDigest(in.RootSpec)
	if err != nil {
		return nil, err
	}
	rootSpec, err := CanonicalJSON(in.RootSpec)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	planID := uuid.Must(uuid.NewV7()).String()
	rootID := uuid.Must(uuid.NewV7()).String()
	revisionID := uuid.Must(uuid.NewV7()).String()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO work_plans (id, project_id, title, human_code, graph_version, status)
		VALUES ($1, $2, $3, $4, 1, 'draft')`,
		planID, in.ProjectID, in.Title, in.HumanCode); err != nil {
		return nil, fmt.Errorf("workgraph: create plan: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO work_nodes (id, plan_id, parent_node_id, node_type, slot_key, human_code, node_version, depth)
		VALUES ($1, $2, NULL, $3, $4, $5, 1, 0)`,
		rootID, planID, WorkNodeTypePackage, slot, in.HumanCode); err != nil {
		return nil, fmt.Errorf("workgraph: create root node: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO plan_revisions (id, plan_id, revision_no, status, spec_digest)
		VALUES ($1, $2, 1, 'draft', $3)`,
		revisionID, planID, rootDigest); err != nil {
		return nil, fmt.Errorf("workgraph: create revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO work_node_revisions (id, node_id, plan_revision_id, spec_digest, spec)
		VALUES ($1, $2, $3, $4, $5::jsonb)`,
		uuid.Must(uuid.NewV7()).String(), rootID, revisionID, rootDigest, string(rootSpec)); err != nil {
		return nil, fmt.Errorf("workgraph: create root node revision: %w", err)
	}
	// The root pointer is set exactly once (0017 trigger blocks re-point).
	if _, err := tx.ExecContext(ctx, `
		UPDATE work_plans SET root_node_id = $2, updated_at = now() WHERE id = $1`,
		planID, rootID); err != nil {
		return nil, fmt.Errorf("workgraph: point root: %w", err)
	}

	payload := mustMarshal(map[string]any{"plan_id": planID, "human_code": in.HumanCode})
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: in.ProjectID, Action: "workgraph.plan.created", ResourceType: "work_plan",
		ResourceID: planID, Actor: in.Actor, CorrelationID: in.CorrelationID,
		OutboxType: "workgraph.plan.created", Payload: payload,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph: commit: %w", err)
	}
	return s.GetWorkPlan(ctx, planID)
}

// AddWorkNodeInput appends one node under an existing parent.
type AddWorkNodeInput struct {
	PlanID               string
	ParentNodeID         string
	NodeType             string
	SlotKey              string
	HumanCode            string
	Spec                 []byte
	ExpectedGraphVersion int64
	Actor                string
	CorrelationID        string
}

// AddWorkNode inserts the node plus its first node revision behind the
// graph CAS and the draft-revision guard.
func (s pgWorkGraphStore) AddWorkNode(ctx context.Context, in AddWorkNodeInput) (*WorkNode, error) {
	switch in.NodeType {
	case WorkNodeTypePackage, WorkNodeTypeItem, WorkNodeTypeGate:
	default:
		return nil, fmt.Errorf("%w: node type must be work_package/work_item/gate: %q", ErrInvalidParameter, in.NodeType)
	}
	if err := ValidateSlotKey(in.SlotKey); err != nil {
		return nil, err
	}
	if err := ValidateNodeHumanCode(in.HumanCode); err != nil {
		return nil, err
	}
	digest, err := SpecDigest(in.Spec)
	if err != nil {
		return nil, err
	}
	spec, err := CanonicalJSON(in.Spec)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	projectID, revisionID, err := lockPlanForStructure(ctx, tx, in.PlanID, in.ExpectedGraphVersion)
	if err != nil {
		return nil, err
	}
	var parentDepth int
	err = tx.QueryRowContext(ctx, `
		SELECT depth FROM work_nodes WHERE id = $1 AND plan_id = $2 FOR UPDATE`,
		in.ParentNodeID, in.PlanID).Scan(&parentDepth)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: parent %s", ErrWorkNodeNotFound, in.ParentNodeID)
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: lock parent: %w", err)
	}

	nodeID := uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO work_nodes (id, plan_id, parent_node_id, node_type, slot_key, human_code, node_version, depth)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7)`,
		nodeID, in.PlanID, in.ParentNodeID, in.NodeType, in.SlotKey, in.HumanCode, parentDepth+1); err != nil {
		return nil, fmt.Errorf("workgraph: insert node: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO work_node_revisions (id, node_id, plan_revision_id, spec_digest, spec)
		VALUES ($1, $2, $3, $4, $5::jsonb)`,
		uuid.Must(uuid.NewV7()).String(), nodeID, revisionID, digest, string(spec)); err != nil {
		return nil, fmt.Errorf("workgraph: insert node revision: %w", err)
	}
	if err := bumpGraphVersion(ctx, tx, in.PlanID); err != nil {
		return nil, err
	}
	payload := mustMarshal(map[string]any{"plan_id": in.PlanID, "node_id": nodeID, "slot_key": in.SlotKey})
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.node.added", ResourceType: "work_node",
		ResourceID: nodeID, Actor: in.Actor, CorrelationID: in.CorrelationID,
		OutboxType: "workgraph.node.added", Payload: payload,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph: commit: %w", err)
	}
	node, err := s.GetWorkNode(ctx, in.PlanID, nodeID)
	if err != nil {
		return nil, err
	}
	return node, nil
}

// AddWorkDependencyInput adds one requires edge.
type AddWorkDependencyInput struct {
	PlanID               string
	FromNodeID           string
	ToNodeID             string
	Requirement          string
	ExpectedGraphVersion int64
	Actor                string
	CorrelationID        string
}

// AddWorkDependency enforces the executable-only rule (WGM-INV-003) and
// rejects edges that would close a cycle; PostgreSQL cannot express
// DAG acyclicity as a constraint, so this application-side check is the
// authority (known boundary, model doc §6).
func (s pgWorkGraphStore) AddWorkDependency(ctx context.Context, in AddWorkDependencyInput) (*WorkDependency, error) {
	requirement := in.Requirement
	if requirement == "" {
		requirement = DependencyRequired
	}
	if requirement != DependencyRequired && requirement != DependencyOptional {
		return nil, fmt.Errorf("%w: requirement must be required/optional: %q", ErrInvalidParameter, in.Requirement)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	projectID, _, err := lockPlanForStructure(ctx, tx, in.PlanID, in.ExpectedGraphVersion)
	if err != nil {
		return nil, err
	}
	for _, nodeID := range []string{in.FromNodeID, in.ToNodeID} {
		var nodeType string
		err := tx.QueryRowContext(ctx, `
			SELECT node_type FROM work_nodes WHERE id = $1 AND plan_id = $2`, nodeID, in.PlanID).Scan(&nodeType)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrWorkNodeNotFound, nodeID)
		}
		if err != nil {
			return nil, fmt.Errorf("workgraph: check node: %w", err)
		}
		if nodeType == WorkNodeTypePackage {
			return nil, fmt.Errorf("%w: work_package %s cannot enter the requires graph (WGM-INV-003)",
				ErrInvalidParameter, nodeID)
		}
	}

	// Cycle check: edge (from -> to) reads "from requires to", so adding
	// it is cyclic iff "to" already transitively requires "from" through
	// existing edges.
	var cyclic bool
	err = tx.QueryRowContext(ctx, `
		WITH RECURSIVE reach(node_id) AS (
			SELECT $3::uuid
			UNION
			SELECT wd.to_node_id
			FROM work_dependencies wd
			JOIN reach r ON wd.from_node_id = r.node_id
			WHERE wd.plan_id = $1
		)
		SELECT EXISTS(SELECT 1 FROM reach WHERE node_id = $2::uuid)`,
		in.PlanID, in.FromNodeID, in.ToNodeID).Scan(&cyclic)
	if err != nil {
		return nil, fmt.Errorf("workgraph: cycle check: %w", err)
	}
	if cyclic {
		return nil, fmt.Errorf("%w: %s -> %s would close a cycle", ErrCircularDependency, in.FromNodeID, in.ToNodeID)
	}

	edgeID := uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO work_dependencies (id, plan_id, from_node_id, to_node_id, requirement)
		VALUES ($1, $2, $3, $4, $5)`,
		edgeID, in.PlanID, in.FromNodeID, in.ToNodeID, requirement); err != nil {
		return nil, fmt.Errorf("workgraph: insert dependency: %w", err)
	}
	if err := bumpGraphVersion(ctx, tx, in.PlanID); err != nil {
		return nil, err
	}
	payload := mustMarshal(map[string]any{"plan_id": in.PlanID, "from": in.FromNodeID, "to": in.ToNodeID})
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.dependency.added", ResourceType: "work_dependency",
		ResourceID: edgeID, Actor: in.Actor, CorrelationID: in.CorrelationID,
		OutboxType: "workgraph.dependency.added", Payload: payload,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph: commit: %w", err)
	}
	return &WorkDependency{ID: edgeID, PlanID: in.PlanID, FromNodeID: in.FromNodeID,
		ToNodeID: in.ToNodeID, Requirement: requirement}, nil
}

// AddNodeLineageInput records one followup_of / replacement_of edge.
type AddNodeLineageInput struct {
	PlanID               string
	NodeID               string
	PredecessorNodeID    string
	Kind                 string
	ExpectedGraphVersion int64
	Actor                string
	CorrelationID        string
}

// AddNodeLineage inserts a lineage edge behind the graph CAS.
func (s pgWorkGraphStore) AddNodeLineage(ctx context.Context, in AddNodeLineageInput) error {
	if in.Kind != LineageFollowupOf && in.Kind != LineageReplacementOf {
		return fmt.Errorf("%w: lineage kind must be followup_of/replacement_of: %q", ErrInvalidParameter, in.Kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	projectID, _, err := lockPlanForStructure(ctx, tx, in.PlanID, in.ExpectedGraphVersion)
	if err != nil {
		return err
	}
	for _, nodeID := range []string{in.NodeID, in.PredecessorNodeID} {
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM work_nodes WHERE id = $1 AND plan_id = $2`, nodeID, in.PlanID).Scan(&count); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrWorkNodeNotFound, nodeID)
			}
			return fmt.Errorf("workgraph: check lineage node: %w", err)
		}
	}
	edgeID := uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_lineage (id, plan_id, node_id, predecessor_node_id, kind)
		VALUES ($1, $2, $3, $4, $5)`,
		edgeID, in.PlanID, in.NodeID, in.PredecessorNodeID, in.Kind); err != nil {
		return fmt.Errorf("workgraph: insert lineage: %w", err)
	}
	if err := bumpGraphVersion(ctx, tx, in.PlanID); err != nil {
		return err
	}
	payload := mustMarshal(map[string]any{"plan_id": in.PlanID, "node": in.NodeID, "kind": in.Kind})
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.lineage.added", ResourceType: "node_lineage",
		ResourceID: edgeID, Actor: in.Actor, CorrelationID: in.CorrelationID,
		OutboxType: "workgraph.lineage.added", Payload: payload,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workgraph: commit: %w", err)
	}
	return nil
}

// AddNodeArtifactFlowInput binds one consumes/produces port to a ledger
// asset version.
type AddNodeArtifactFlowInput struct {
	PlanID               string
	NodeID               string
	Direction            string
	AssetID              string
	AssetVersion         int
	PortKey              string
	ExpectedGraphVersion int64
	Actor                string
	CorrelationID        string
}

// AddNodeArtifactFlow inserts a flow edge behind the graph CAS; the
// composite foreign key to assets (asset_id, version) makes unregistered
// targets impossible.
func (s pgWorkGraphStore) AddNodeArtifactFlow(ctx context.Context, in AddNodeArtifactFlowInput) error {
	if in.Direction != FlowConsumes && in.Direction != FlowProduces {
		return fmt.Errorf("%w: direction must be consumes/produces: %q", ErrInvalidParameter, in.Direction)
	}
	if err := ValidatePortKey(in.PortKey); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	projectID, _, err := lockPlanForStructure(ctx, tx, in.PlanID, in.ExpectedGraphVersion)
	if err != nil {
		return err
	}
	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM work_nodes WHERE id = $1 AND plan_id = $2`,
		in.NodeID, in.PlanID).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrWorkNodeNotFound, in.NodeID)
	}
	if err != nil {
		return fmt.Errorf("workgraph: check flow node: %w", err)
	}

	flowID := uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_artifact_flows (id, plan_id, node_id, direction, asset_id, asset_version, port_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		flowID, in.PlanID, in.NodeID, in.Direction, in.AssetID, in.AssetVersion, in.PortKey); err != nil {
		return fmt.Errorf("workgraph: insert flow: %w", err)
	}
	if err := bumpGraphVersion(ctx, tx, in.PlanID); err != nil {
		return err
	}
	payload := mustMarshal(map[string]any{"plan_id": in.PlanID, "node": in.NodeID,
		"direction": in.Direction, "asset": FormatAssetRef(in.AssetID, in.AssetVersion), "port": in.PortKey})
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.flow.added", ResourceType: "node_artifact_flow",
		ResourceID: flowID, Actor: in.Actor, CorrelationID: in.CorrelationID,
		OutboxType: "workgraph.flow.added", Payload: payload,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workgraph: commit: %w", err)
	}
	return nil
}

// UpdateWorkNodeStatus CAS-advances one node's status (expected_node_version).
func (s pgWorkGraphStore) UpdateWorkNodeStatus(ctx context.Context, planID, nodeID string, expectedVersion int64, newStatus string, actor string) (*WorkNode, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var projectID string
	err = tx.QueryRowContext(ctx, `SELECT project_id FROM work_plans WHERE id = $1`, planID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: plan lookup: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE work_nodes SET status = $4, node_version = node_version + 1, updated_at = now()
		WHERE id = $1 AND plan_id = $2 AND node_version = $3`,
		nodeID, planID, expectedVersion, newStatus)
	if err != nil {
		return nil, fmt.Errorf("workgraph: node status cas: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var exists int
		if lookupErr := tx.QueryRowContext(ctx,
			`SELECT 1 FROM work_nodes WHERE id = $1 AND plan_id = $2`, nodeID, planID).Scan(&exists); lookupErr == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: %s", ErrWorkNodeNotFound, nodeID)
		} else if lookupErr != nil {
			return nil, fmt.Errorf("workgraph: node lookup: %w", lookupErr)
		}
		return nil, ErrNodeVersionMismatch
	}
	payload := mustMarshal(map[string]any{"node_id": nodeID, "status": newStatus})
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.node.status_changed", ResourceType: "work_node",
		ResourceID: nodeID, Actor: actor, OutboxType: "workgraph.node.status_changed", Payload: payload,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph: commit: %w", err)
	}
	return s.GetWorkNode(ctx, planID, nodeID)
}

// SealPlanRevision freezes the current draft revision: required input
// ports must be bound (spec key "required_inputs"), the node manifest is
// snapshotted, the revision digest recorded and the plan marked sealed.
// From here the 0017 triggers reject any mutation of the revision rows.
func (s pgWorkGraphStore) SealPlanRevision(ctx context.Context, planID string, expectedGraphVersion int64, actor string) (*PlanRevision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("workgraph: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	projectID, revisionID, err := lockPlanForStructure(ctx, tx, planID, expectedGraphVersion)
	if err != nil {
		return nil, err
	}
	var revisionNo int
	var revisionStatus string
	err = tx.QueryRowContext(ctx, `
		SELECT id, revision_no, status FROM plan_revisions
		WHERE plan_id = $1 ORDER BY revision_no DESC LIMIT 1 FOR UPDATE`,
		planID).Scan(&revisionID, &revisionNo, &revisionStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: plan %s has no revision", ErrSealRejected, planID)
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: lock revision: %w", err)
	}
	if revisionStatus != "draft" {
		return nil, fmt.Errorf("%w: revision %d already sealed", ErrRevisionSealed, revisionNo)
	}

	// Required-input port binding (WGM-INV-004, J2a scope: specs may
	// declare "required_inputs"; the J2b protocol tightens this to typed
	// contracts).
	rows, err := tx.QueryContext(ctx, `
		SELECT n.id, nr.spec
		FROM work_nodes n
		JOIN work_node_revisions nr ON nr.node_id = n.id AND nr.plan_revision_id = $2
		WHERE n.plan_id = $1`, planID, revisionID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: load specs: %w", err)
	}
	type nodeSpecRow struct {
		id   string
		spec map[string]any
	}
	specs := []nodeSpecRow{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("workgraph: scan spec: %w", err)
		}
		var decoded map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &decoded); err != nil {
				rows.Close()
				return nil, fmt.Errorf("%w: node %s spec is not a JSON object", ErrSealRejected, id)
			}
		}
		specs = append(specs, nodeSpecRow{id: id, spec: decoded})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: load specs: %w", err)
	}

	boundPorts := map[string]map[string]bool{}
	portRows, err := tx.QueryContext(ctx, `
		SELECT node_id, port_key FROM node_artifact_flows WHERE plan_id = $1 AND direction = 'consumes'`, planID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: load ports: %w", err)
	}
	for portRows.Next() {
		var nodeID, portKey string
		if err := portRows.Scan(&nodeID, &portKey); err != nil {
			portRows.Close()
			return nil, fmt.Errorf("workgraph: scan port: %w", err)
		}
		if boundPorts[nodeID] == nil {
			boundPorts[nodeID] = map[string]bool{}
		}
		boundPorts[nodeID][portKey] = true
	}
	portRows.Close()
	if err := portRows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: load ports: %w", err)
	}

	for _, spec := range specs {
		rawInputs, ok := spec.spec["required_inputs"]
		if !ok {
			continue
		}
		inputs, ok := rawInputs.([]any)
		if !ok {
			return nil, fmt.Errorf("%w: node %s required_inputs must be a list", ErrSealRejected, spec.id)
		}
		for _, rawInput := range inputs {
			port, ok := rawInput.(string)
			if !ok {
				return nil, fmt.Errorf("%w: node %s required_inputs must be strings", ErrSealRejected, spec.id)
			}
			if !boundPorts[spec.id][port] {
				return nil, fmt.Errorf("%w: node %s required input port %q is unbound (WGM-INV-004)",
					ErrSealRejected, spec.id, port)
			}
		}
	}

	manifest, err := buildPlanManifest(ctx, tx, planID, revisionID)
	if err != nil {
		return nil, err
	}
	manifestDigest, err := SpecDigest(manifest)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE plan_revisions SET status = 'sealed', sealed_at = now(), spec_digest = $2, node_manifest = $3::jsonb
		WHERE id = $1 AND status = 'draft'`,
		revisionID, manifestDigest, string(manifest)); err != nil {
		return nil, fmt.Errorf("workgraph: seal revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE work_plans SET status = 'sealed', updated_at = now() WHERE id = $1`, planID); err != nil {
		return nil, fmt.Errorf("workgraph: seal plan: %w", err)
	}
	if err := bumpGraphVersion(ctx, tx, planID); err != nil {
		return nil, err
	}
	payload := mustMarshal(map[string]any{"plan_id": planID, "revision_no": revisionNo, "spec_digest": manifestDigest})
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "workgraph.plan.sealed", ResourceType: "plan_revision",
		ResourceID: revisionID, Actor: actor, OutboxType: "workgraph.plan.sealed", Payload: payload,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("workgraph: commit: %w", err)
	}
	revision, err := s.GetPlanRevision(ctx, revisionID)
	if err != nil {
		return nil, err
	}
	return revision, nil
}

// buildPlanManifest snapshots nodes, specs, edges, lineage and flows of
// one revision as canonical JSON (the sealed replay basis).
func buildPlanManifest(ctx context.Context, tx *sql.Tx, planID, revisionID string) ([]byte, error) {
	manifest := map[string]any{"plan_id": planID, "revision": revisionID}
	var nodes []map[string]any
	rows, err := tx.QueryContext(ctx, `
		SELECT n.id, n.parent_node_id, n.node_type, n.slot_key, n.human_code, nr.spec_digest
		FROM work_nodes n
		JOIN work_node_revisions nr ON nr.node_id = n.id AND nr.plan_revision_id = $2
		WHERE n.plan_id = $1 ORDER BY n.human_code`, planID, revisionID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: manifest nodes: %w", err)
	}
	for rows.Next() {
		var id, nodeType, slot, code, digest string
		var parent *string
		if err := rows.Scan(&id, &parent, &nodeType, &slot, &code, &digest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("workgraph: manifest node scan: %w", err)
		}
		node := map[string]any{"id": id, "type": nodeType, "slot": slot, "code": code, "spec_digest": digest}
		if parent != nil {
			node["parent"] = *parent
		}
		nodes = append(nodes, node)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: manifest nodes: %w", err)
	}
	manifest["nodes"] = nodes

	var dependencies []map[string]any
	rows, err = tx.QueryContext(ctx, `
		SELECT from_node_id, to_node_id, requirement FROM work_dependencies WHERE plan_id = $1 ORDER BY from_node_id, to_node_id`, planID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: manifest deps: %w", err)
	}
	for rows.Next() {
		var from, to, requirement string
		if err := rows.Scan(&from, &to, &requirement); err != nil {
			rows.Close()
			return nil, fmt.Errorf("workgraph: manifest dep scan: %w", err)
		}
		dependencies = append(dependencies, map[string]any{"from": from, "to": to, "requirement": requirement})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: manifest deps: %w", err)
	}
	manifest["dependencies"] = dependencies

	var lineage []map[string]any
	rows, err = tx.QueryContext(ctx, `
		SELECT node_id, predecessor_node_id, kind FROM node_lineage WHERE plan_id = $1 ORDER BY node_id`, planID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: manifest lineage: %w", err)
	}
	for rows.Next() {
		var node, predecessor, kind string
		if err := rows.Scan(&node, &predecessor, &kind); err != nil {
			rows.Close()
			return nil, fmt.Errorf("workgraph: manifest lineage scan: %w", err)
		}
		lineage = append(lineage, map[string]any{"node": node, "predecessor": predecessor, "kind": kind})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: manifest lineage: %w", err)
	}
	manifest["lineage"] = lineage

	var flows []map[string]any
	rows, err = tx.QueryContext(ctx, `
		SELECT node_id, direction, asset_id, asset_version, port_key
		FROM node_artifact_flows WHERE plan_id = $1 ORDER BY node_id, direction, port_key`, planID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: manifest flows: %w", err)
	}
	for rows.Next() {
		var node, direction, assetID, port string
		var version int
		if err := rows.Scan(&node, &direction, &assetID, &version, &port); err != nil {
			rows.Close()
			return nil, fmt.Errorf("workgraph: manifest flow scan: %w", err)
		}
		flows = append(flows, map[string]any{"node": node, "direction": direction,
			"asset": FormatAssetRef(assetID, version), "port": port})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workgraph: manifest flows: %w", err)
	}
	manifest["flows"] = flows

	return CanonicalJSON(mustMarshal(manifest))
}

func mustMarshal(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}

// lockPlanForStructure takes the plan row lock, verifies the presented
// graph version and that the current revision is still a draft, and
// returns the owning project id plus the current draft revision id.
func lockPlanForStructure(ctx context.Context, tx *sql.Tx, planID string, expectedGraphVersion int64) (string, string, error) {
	var projectID string
	var graphVersion int64
	err := tx.QueryRowContext(ctx, `
		SELECT project_id::text, graph_version FROM work_plans WHERE id = $1 FOR UPDATE`,
		planID).Scan(&projectID, &graphVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrWorkPlanNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("workgraph: lock plan: %w", err)
	}
	if graphVersion != expectedGraphVersion {
		return "", "", ErrGraphVersionMismatch
	}
	revisionID, err := currentDraftRevision(ctx, tx, planID)
	if err != nil {
		return "", "", err
	}
	return projectID, revisionID, nil
}

// currentDraftRevision resolves the plan's newest revision and refuses
// structural writes once it is sealed.
func currentDraftRevision(ctx context.Context, tx *sql.Tx, planID string) (string, error) {
	var revisionID, status string
	err := tx.QueryRowContext(ctx, `
		SELECT id, status FROM plan_revisions
		WHERE plan_id = $1 ORDER BY revision_no DESC LIMIT 1`, planID).Scan(&revisionID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: plan %s has no revision", ErrSealRejected, planID)
	}
	if err != nil {
		return "", fmt.Errorf("workgraph: current revision: %w", err)
	}
	if status != "draft" {
		return "", ErrRevisionSealed
	}
	return revisionID, nil
}

func bumpGraphVersion(ctx context.Context, tx *sql.Tx, planID string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE work_plans SET graph_version = graph_version + 1, updated_at = now() WHERE id = $1`,
		planID); err != nil {
		return fmt.Errorf("workgraph: bump graph version: %w", err)
	}
	return nil
}

const workPlanColumns = `id, project_id::text, title, human_code, root_node_id, graph_version, status,
	created_at, updated_at`

func scanWorkPlan(scan func(dest ...any) error) (*WorkPlan, error) {
	plan := &WorkPlan{}
	var root sql.NullString
	var createdAt, updatedAt time.Time
	if err := scan(&plan.ID, &plan.ProjectID, &plan.Title, &plan.HumanCode, &root,
		&plan.GraphVersion, &plan.Status, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if root.Valid {
		plan.RootNodeID = root.String
	}
	plan.CreatedAt = pgTimeString(createdAt)
	plan.UpdatedAt = pgTimeString(updatedAt)
	return plan, nil
}

// GetWorkPlan returns the plan by id.
func (s pgWorkGraphStore) GetWorkPlan(ctx context.Context, planID string) (*WorkPlan, error) {
	plan, err := scanWorkPlan(func(dest ...any) error {
		return s.db.QueryRowContext(ctx,
			`SELECT `+workPlanColumns+` FROM work_plans WHERE id = $1`, planID).Scan(dest...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: get plan: %w", err)
	}
	return plan, nil
}

const workNodeColumns = `id, plan_id, parent_node_id, node_type, slot_key, human_code, status,
	node_version, depth, created_at, updated_at`

func scanWorkNode(scan func(dest ...any) error) (*WorkNode, error) {
	node := &WorkNode{}
	var parent sql.NullString
	var createdAt, updatedAt time.Time
	if err := scan(&node.ID, &node.PlanID, &parent, &node.NodeType, &node.SlotKey,
		&node.HumanCode, &node.Status, &node.NodeVersion, &node.Depth, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	if parent.Valid {
		node.ParentNodeID = parent.String
	}
	node.CreatedAt = pgTimeString(createdAt)
	node.UpdatedAt = pgTimeString(updatedAt)
	return node, nil
}

// GetWorkNode returns one node of a plan.
func (s pgWorkGraphStore) GetWorkNode(ctx context.Context, planID, nodeID string) (*WorkNode, error) {
	node, err := scanWorkNode(func(dest ...any) error {
		return s.db.QueryRowContext(ctx,
			`SELECT `+workNodeColumns+` FROM work_nodes WHERE id = $1 AND plan_id = $2`,
			nodeID, planID).Scan(dest...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrWorkNodeNotFound, nodeID)
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: get node: %w", err)
	}
	return node, nil
}

// ListWorkNodes returns the plan's nodes in human-code order.
func (s pgWorkGraphStore) ListWorkNodes(ctx context.Context, planID string) ([]*WorkNode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+workNodeColumns+` FROM work_nodes WHERE plan_id = $1 ORDER BY human_code`, planID)
	if err != nil {
		return nil, fmt.Errorf("workgraph: list nodes: %w", err)
	}
	defer rows.Close()
	nodes := []*WorkNode{}
	for rows.Next() {
		node, scanErr := scanWorkNode(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("workgraph: scan node: %w", scanErr)
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// GetPlanRevision returns one revision row.
func (s pgWorkGraphStore) GetPlanRevision(ctx context.Context, revisionID string) (*PlanRevision, error) {
	revision := &PlanRevision{}
	var sealedAt sql.NullTime
	var createdAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT id, plan_id, revision_no, status, spec_digest, node_manifest, sealed_at, created_at
		FROM plan_revisions WHERE id = $1`, revisionID).
		Scan(&revision.ID, &revision.PlanID, &revision.RevisionNo, &revision.Status,
			&revision.SpecDigest, &revision.NodeManifest, &sealedAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: revision %s", ErrSealRejected, revisionID)
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: get revision: %w", err)
	}
	if sealedAt.Valid {
		revision.SealedAt = pgTimeString(sealedAt.Time)
	}
	revision.CreatedAt = pgTimeString(createdAt)
	return revision, nil
}

// CurrentPlanRevision returns the plan's newest revision.
func (s pgWorkGraphStore) CurrentPlanRevision(ctx context.Context, planID string) (*PlanRevision, error) {
	var revisionID string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM plan_revisions WHERE plan_id = $1 ORDER BY revision_no DESC LIMIT 1`,
		planID).Scan(&revisionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkPlanNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workgraph: current revision id: %w", err)
	}
	return s.GetPlanRevision(ctx, revisionID)
}
