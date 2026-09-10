package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Work Graph storage types and pure validation (J2a, ADR-009 approved
// 2026-09-10; authority: docs/technical/work-graph-model.md). The
// PostgreSQL layer keeps every structural mutation behind the plan's
// graph_version CAS and every node status change behind the node's
// node_version CAS; the 0017 triggers are the tamper-evidence backstop,
// these helpers are the stable-error front line.

var (
	ErrWorkPlanNotFound     = errors.New("work plan not found")
	ErrWorkNodeNotFound     = errors.New("work node not found")
	ErrGraphVersionMismatch = errors.New("work graph version mismatch, replay on latest graph")
	ErrNodeVersionMismatch  = errors.New("work node version mismatch, replay on latest node")
	ErrSlotKeyInvalid       = errors.New("slot key must match ^[a-z][a-z0-9.]{0,63}$")
	ErrRevisionSealed       = errors.New("plan revision sealed; structural changes need a new revision (J2b replan protocol)")
	ErrSealRejected         = errors.New("plan revision cannot be sealed")
)

// Work node types (0017 work_nodes.node_type).
const (
	WorkNodeTypePackage = "work_package"
	WorkNodeTypeItem    = "work_item"
	WorkNodeTypeGate    = "gate"
)

// Dependency requirement kinds (0017 work_dependencies.requirement).
const (
	DependencyRequired = "required"
	DependencyOptional = "optional"
)

// Lineage kinds (0017 node_lineage.kind); retry_of lands with the J2b
// execution-attempt tables, deliberately not a node-level kind.
const (
	LineageFollowupOf    = "followup_of"
	LineageReplacementOf = "replacement_of"
)

// Artifact flow directions (0017 node_artifact_flows.direction).
const (
	FlowConsumes = "consumes"
	FlowProduces = "produces"
)

var (
	slotKeyPattern    = regexp.MustCompile(`^[a-z][a-z0-9.]{0,63}$`)
	portKeyPattern    = regexp.MustCompile(`^[a-z][a-z0-9.]{0,63}$`)
	planCodePattern   = regexp.MustCompile(`^MST-WP-[0-9]{5}$`)
	nodeCodePattern   = regexp.MustCompile(`^MST-(WP|WI)-[0-9]{5}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	assetIDPattern    = regexp.MustCompile(`^ART-[a-z-]+-[0-9]{3,}$`)
	supersedesPattern = regexp.MustCompile(`^(ART-[a-z-]+-[0-9]{3,})@([0-9]+)$`)
)

// ValidateSlotKey enforces the WGM-007 slot grammar.
func ValidateSlotKey(key string) error {
	if !slotKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrSlotKeyInvalid, key)
	}
	return nil
}

// ValidatePortKey enforces the same grammar for artifact flow ports.
func ValidatePortKey(key string) error {
	if !portKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrSlotKeyInvalid, key)
	}
	return nil
}

// ValidatePlanHumanCode checks the MST-WP-xxxxx shape (project-unique).
func ValidatePlanHumanCode(code string) error {
	if !planCodePattern.MatchString(code) {
		return fmt.Errorf("%w: plan code must match MST-WP-xxxxx: %q", ErrInvalidParameter, code)
	}
	return nil
}

// ValidateNodeHumanCode checks the MST-WP-xxxxx / MST-WI-xxxxx shape.
func ValidateNodeHumanCode(code string) error {
	if !nodeCodePattern.MatchString(code) {
		return fmt.Errorf("%w: node code must match MST-(WP|WI)-xxxxx: %q", ErrInvalidParameter, code)
	}
	return nil
}

// ValidateSpecDigestFormat checks the sha256:<64hex> wire shape.
func ValidateSpecDigestFormat(digest string) error {
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("%w: digest must be sha256:<64hex>: %q", ErrInvalidParameter, digest)
	}
	return nil
}

// CanonicalJSON re-encodes any JSON document with sorted object keys and
// no insignificant whitespace, so equal specs always digest equal.
func CanonicalJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("canonical json: %w", err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("canonical json: %w", err)
	}
	return canonical, nil
}

// SpecDigest canonicalizes the spec document and returns its sha256
// digest in the wire shape sha256:<64hex> (WGM-007: stale judgment only,
// never an entity id).
func SpecDigest(spec []byte) (string, error) {
	canonical, err := CanonicalJSON(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// WorkPlan is one stored work_plans row.
type WorkPlan struct {
	ID           string
	ProjectID    string
	Title        string
	HumanCode    string
	RootNodeID   string
	GraphVersion int64
	Status       string
	CreatedAt    string
	UpdatedAt    string
}

// WorkNode is one stored work_nodes row (contains adjacency).
type WorkNode struct {
	ID           string
	PlanID       string
	ParentNodeID string
	NodeType     string
	SlotKey      string
	HumanCode    string
	Status       string
	NodeVersion  int64
	Depth        int
	CreatedAt    string
	UpdatedAt    string
}

// PlanRevision is one stored plan_revisions row; SealedAt is empty while
// the revision is still a draft.
type PlanRevision struct {
	ID           string
	PlanID       string
	RevisionNo   int
	Status       string
	SpecDigest   string
	NodeManifest []byte
	SealedAt     string
	CreatedAt    string
}

// WorkNodeRevision is one append-only work_node_revisions row.
type WorkNodeRevision struct {
	ID             string
	NodeID         string
	PlanRevisionID string
	SpecDigest     string
	Spec           []byte
	CreatedAt      string
}

// WorkDependency is one requires edge.
type WorkDependency struct {
	ID          string
	PlanID      string
	FromNodeID  string
	ToNodeID    string
	Requirement string
	CreatedAt   string
}

// NodeLineage is one followup_of / replacement_of edge.
type NodeLineage struct {
	ID                string
	PlanID            string
	NodeID            string
	PredecessorNodeID string
	Kind              string
	CreatedAt         string
}

// NodeArtifactFlow is one consumes/produces edge to a ledger asset.
type NodeArtifactFlow struct {
	ID           string
	PlanID       string
	NodeID       string
	Direction    string
	AssetID      string
	AssetVersion int
	PortKey      string
	CreatedAt    string
}
