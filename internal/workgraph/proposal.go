// Package workgraph holds the J2b decomposition protocol: the
// DecompositionProposal wire contract with its four validation classes
// (scope, cycles, budget, resources), the execution ContextSet/Envelope
// shapes and the pure aggregation projection. The package depends on
// nothing but the standard library so internal/store can consume it as
// the domain authority while keeping SQL out of the protocol.
//
// Authority: docs/prd/work-planning-and-orchestration.md (draft
// vocabulary), docs/technical/work-graph-model.md (WGM invariants) and
// ADR-009 §4-§7. Coordinator sessions only ever submit proposals; every
// decision below is server-side and deterministic.
package workgraph

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Node types (mirror the 0017 work_nodes.node_type catalog).
const (
	NodeTypePackage = "work_package"
	NodeTypeItem    = "work_item"
	NodeTypeGate    = "gate"
)

// Dependency requirement kinds (0017 work_dependencies.requirement).
const (
	DependencyRequired = "required"
	DependencyOptional = "optional"
)

// Policy enums (PRD work-planning §5; frozen vocabulary).
const (
	ThresholdAll = "all"
	ThresholdAny = "any"
	ThresholdQuorum = "quorum"

	FailureFailFast    = "fail_fast"
	FailureCollectAll  = "collect_all"
	FailureNeedsHuman  = "needs_human"

	CancelCascadeRequired = "cascade_required"
	CancelDetachOptional  = "detach_optional"
	CancelNone            = "none"
)

// DefaultProposalLimits are the conservative envelopes used when the
// caller does not carry a project policy (WGS §7: unknown policy means
// the most conservative default).
const (
	DefaultMaxNodes            = 50
	DefaultMaxContainmentDepth = 8
	DefaultMaxFanOut           = 12
	DefaultBudgetCeilingUnits  = 1_000_000
)

var (
	localIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	externalRefPattern = regexp.MustCompile(`^ext:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	assetRefPattern = regexp.MustCompile(`^ART-[a-z-]+-[0-9]{3,}@[0-9]+$`)
	shaPattern      = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	slotKeyPattern  = regexp.MustCompile(`^[a-z][a-z0-9.]{0,63}$`)
	nodeCodePattern = regexp.MustCompile(`^MST-(WP|WI)-[0-9]{5}$`)
)

// WorkPatternRef pins the decomposition template the proposal applies.
// The referenced (id, version) must exist and be active server-side.
type WorkPatternRef struct {
	PatternID string `json:"pattern_id"`
	Version   int    `json:"version"`
}

// NodeSpec is the typed node specification carried on every proposal
// node and stored (as canonical JSON) on work_node_revisions.spec.
// Work-item atomicity fields (repo/baseline/workspace/capability) are
// mandatory for work_item nodes; policy fields are mandatory for
// aggregating (work_package/gate) nodes.
type NodeSpec struct {
	Title               string   `json:"title"`
	AcceptanceCriteria  []string `json:"acceptance_criteria,omitempty"`
	Repo                string   `json:"repo,omitempty"`
	BaselineSHA         string   `json:"baseline_sha,omitempty"`
	WorkspacePaths      []string `json:"workspace_paths,omitempty"`
	OwningCapability    string   `json:"owning_capability,omitempty"`
	BudgetUnits         int64    `json:"budget_units,omitempty"`
	Deadline            string   `json:"deadline,omitempty"` // RFC3339
	Priority            *int     `json:"priority,omitempty"`
	RequiredInputs      []string `json:"required_inputs,omitempty"`
	SuccessThreshold    *SuccessThreshold `json:"success_threshold,omitempty"`
	FailurePolicy       string   `json:"failure_policy,omitempty"`
	CancelPolicy        string   `json:"cancel_policy,omitempty"`
}

// SuccessThreshold is the JoinPolicy: all / any / quorum(k).
type SuccessThreshold struct {
	Kind string `json:"kind"`
	K    int    `json:"k,omitempty"`
}

// ProposalNode is one node offered by a proposal. LocalID addresses the
// node inside the proposal; Parent resolves either to another proposal
// LocalID or to an existing node as "ext:<uuid>".
type ProposalNode struct {
	LocalID   string    `json:"local_id"`
	Parent    string    `json:"parent"`
	NodeType  string    `json:"node_type"`
	SlotKey   string    `json:"slot_key"`
	HumanCode string    `json:"human_code"`
	Spec      NodeSpec  `json:"spec"`
}

// ProposalEdge is one requires edge; From requires To. Endpoints are
// proposal LocalIDs or "ext:<uuid>" references to existing plan nodes.
type ProposalEdge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Requirement string `json:"requirement,omitempty"`
}

// ProposalFlow is one consumes/produces edge to a ledger asset.
type ProposalFlow struct {
	Node      string `json:"node"`
	Direction string `json:"direction"`
	AssetRef  string `json:"asset_ref"`
	PortKey   string `json:"port_key"`
}

// DecompositionProposal is the Coordinator-submitted wire document
// (ADR-009 §4): one batch of nodes, requires edges and artifact flows
// applied atomically to the plan's current draft revision behind the
// graph CAS. It is untrusted input — every field is validated.
type DecompositionProposal struct {
	PlanID               string          `json:"plan_id"`
	WorkPattern          WorkPatternRef  `json:"work_pattern"`
	Nodes                []ProposalNode  `json:"nodes"`
	Dependencies         []ProposalEdge  `json:"dependencies,omitempty"`
	Flows                []ProposalFlow  `json:"flows,omitempty"`
	ExpectedGraphVersion int64           `json:"expected_graph_version"`
}

// Violation is one stable validation rejection. Code is wire-stable
// (J2c surfaces it verbatim); Class is the four-way taxonomy
// (scope/cycle/budget/resource); Node/Field locate the defect.
type Violation struct {
	Code    string `json:"code"`
	Class   string `json:"class"`
	Node    string `json:"node,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// Violation classes.
const (
	ClassScope    = "scope"
	ClassCycle    = "cycle"
	ClassBudget   = "budget"
	ClassResource = "resource"
)

// Stable violation codes (WGP-VS scope / WGP-CY cycles / WGP-BU budget
// / WGP-RS resources). Codes are frozen wire contract: renumbering or
// reusing a code for a different meaning is a breaking change.
const (
	CodeSlotKeyGrammar       = "WGP-VS-001"
	CodeSlotKeyDuplicate     = "WGP-VS-002"
	CodeHumanCodeGrammar     = "WGP-VS-003"
	CodeHumanCodeDuplicate   = "WGP-VS-004"
	CodeNodeTypeInvalid      = "WGP-VS-005"
	CodeTitleLength          = "WGP-VS-006"
	CodeNodesLimit           = "WGP-VS-007"
	CodeDepthExceeded        = "WGP-VS-008"
	CodeFanOutExceeded       = "WGP-VS-009"
	CodeBudgetNonPositive    = "WGP-VS-010"
	CodeDeadlineInvalid      = "WGP-VS-011"
	CodeAcceptanceEmpty      = "WGP-VS-012"
	CodeParentUnknown        = "WGP-VS-013"

	CodeContainsCycle        = "WGP-CY-001"
	CodeRequiresSelf         = "WGP-CY-002"
	CodeRequiresCycle        = "WGP-CY-003"
	CodeRequiresNonExec      = "WGP-CY-004"
	CodeEndpointUnknown      = "WGP-CY-005"

	CodeBudgetSumExceeded    = "WGP-BU-001"
	CodeBudgetCeiling        = "WGP-BU-002"

	CodePatternUnavailable   = "WGP-RS-001"
	CodePolicyInvalid        = "WGP-RS-002"
	CodePortGrammar          = "WGP-RS-003"
	CodePortUnbound          = "WGP-RS-004"
	CodeAssetRefInvalid      = "WGP-RS-005"
	CodeAssetUnknown         = "WGP-RS-006"
	CodeBaselineMissing      = "WGP-RS-007"
	CodeWorkspaceMissing     = "WGP-RS-008"
	CodeCapabilityMissing    = "WGP-RS-009"
	CodeAtomicityRepository  = "WGP-RS-010"
	CodeLocalIDGrammar       = "WGP-RS-011"
)

// ProposalLimits are the project-policy envelopes applied to a
// proposal. Zero fields fall back to the conservative defaults.
type ProposalLimits struct {
	MaxNodes            int
	MaxContainmentDepth int
	MaxFanOut           int
	BudgetCeilingUnits  int64
	Now                 time.Time
}

func (l ProposalLimits) withDefaults() ProposalLimits {
	if l.MaxNodes <= 0 {
		l.MaxNodes = DefaultMaxNodes
	}
	if l.MaxContainmentDepth <= 0 {
		l.MaxContainmentDepth = DefaultMaxContainmentDepth
	}
	if l.MaxFanOut <= 0 {
		l.MaxFanOut = DefaultMaxFanOut
	}
	if l.BudgetCeilingUnits <= 0 {
		l.BudgetCeilingUnits = DefaultBudgetCeilingUnits
	}
	if l.Now.IsZero() {
		l.Now = time.Now().UTC()
	}
	return l
}

// AssetFact is what the server knows about one referenced asset
// version at validation time.
type AssetFact struct {
	Exists bool
	Status string // ledger lifecycle: draft/reviewed/approved/superseded
	Digest string
}

// EdgeFact is one existing requires edge (both endpoints are stored
// node UUIDs).
type EdgeFact struct {
	From, To   string
	Requirement string
}

// NodeFact is what the server knows about one existing node referenced
// by an ext: reference.
type NodeFact struct {
	Exists   bool
	NodeType string
	Depth    int
}

// ProposalContext carries the DB-sourced facts the pure validator
// needs: existing nodes/slots/codes, the current requires edges, the
// consumes flows already bound and the referenced asset versions.
type ProposalContext struct {
	// Nodes maps "ext:<uuid>" (and bare uuid) to the stored node fact.
	Nodes map[string]NodeFact
	// ChildCount maps parent uuid to the number of stored children.
	ChildCount map[string]int
	// SlotsUnder maps parent uuid to the slot keys already taken.
	SlotsUnder map[string]map[string]bool
	// HumanCodes are the codes already used inside the plan.
	HumanCodes map[string]bool
	// Requires are the plan's current requires edges.
	Requires []EdgeFact
	// Consumes maps node uuid to its bound consume ports.
	Consumes map[string]map[string]bool
	// Assets maps "ART-x@v" to the ledger fact.
	Assets map[string]AssetFact
	// PatternActive reports whether the referenced WorkPattern version
	// exists and is active.
	PatternActive bool
}

// ValidateProposal runs the four validation classes over the proposal
// and returns every violation found (empty = applicable). It is a pure
// function of (proposal, limits, context): same inputs, same
// violations, same order — the replay contract for WGP-REQ-002.
func ValidateProposal(p DecompositionProposal, limits ProposalLimits, ctx ProposalContext) []Violation {
	limits = limits.withDefaults()
	var out []Violation
	add := func(code, class, node, field, format string, args ...any) {
		out = append(out, Violation{Code: code, Class: class, Node: node, Field: field,
			Message: fmt.Sprintf(format, args...)})
	}

	// ---- resource: pattern version reference ------------------------
	if !ctx.PatternActive {
		add(CodePatternUnavailable, ClassResource, "", "work_pattern",
			"referenced work pattern %s@%d is missing or not active", p.WorkPattern.PatternID, p.WorkPattern.Version)
	}

	if len(p.Nodes) == 0 {
		add(CodeNodesLimit, ClassScope, "", "nodes", "proposal carries no nodes")
		return out
	}
	if len(p.Nodes) > limits.MaxNodes {
		add(CodeNodesLimit, ClassScope, "", "nodes",
			"proposal carries %d nodes, limit is %d", len(p.Nodes), limits.MaxNodes)
	}

	// ---- scope: per-node grammar and shape --------------------------
	nodes := map[string]ProposalNode{}
	for _, n := range p.Nodes {
		if !localIDPattern.MatchString(n.LocalID) {
			add(CodeLocalIDGrammar, ClassResource, n.LocalID, "local_id",
				"local id %q must match ^[a-z][a-z0-9_.-]{0,63}$", n.LocalID)
			continue
		}
		if _, dup := nodes[n.LocalID]; dup {
			add(CodeLocalIDGrammar, ClassResource, n.LocalID, "local_id", "duplicate local id %q", n.LocalID)
		}
		nodes[n.LocalID] = n
	}

	resolveParent := func(ref string) (string, bool) { // returns canonical uuid key
		if externalRefPattern.MatchString(ref) {
			return ref[len("ext:"):], true
		}
		return "", false
	}

	for _, n := range p.Nodes {
		node := n
		switch node.NodeType {
		case NodeTypePackage, NodeTypeItem, NodeTypeGate:
		default:
			add(CodeNodeTypeInvalid, ClassScope, node.LocalID, "node_type",
				"node type %q must be work_package/work_item/gate", node.NodeType)
		}
		if !slotKeyPattern.MatchString(node.SlotKey) {
			add(CodeSlotKeyGrammar, ClassScope, node.LocalID, "slot_key",
				"slot_key %q must match ^[a-z][a-z0-9.]{0,63}$", node.SlotKey)
		}
		if !nodeCodePattern.MatchString(node.HumanCode) {
			add(CodeHumanCodeGrammar, ClassScope, node.LocalID, "human_code",
				"human code %q must match MST-(WP|WI)-xxxxx", node.HumanCode)
		}
		if l := len([]rune(node.Spec.Title)); l < 1 || l > 120 {
			add(CodeTitleLength, ClassScope, node.LocalID, "title",
				"title must be 1-120 characters, got %d", l)
		}
		if len(node.Spec.AcceptanceCriteria) == 0 {
			add(CodeAcceptanceEmpty, ClassScope, node.LocalID, "acceptance_criteria",
				"at least one decidable acceptance criterion is required")
		}
		if node.Spec.BudgetUnits < 1 {
			add(CodeBudgetNonPositive, ClassScope, node.LocalID, "budget_units",
				"budget must be a positive integer, got %d", node.Spec.BudgetUnits)
		}
		if node.Spec.Deadline != "" {
			deadline, err := time.Parse(time.RFC3339, node.Spec.Deadline)
			if err != nil || !deadline.After(limits.Now) {
				add(CodeDeadlineInvalid, ClassScope, node.LocalID, "deadline",
					"deadline %q must be RFC3339 and in the future", node.Spec.Deadline)
			}
		}
		// Parent resolution + containment depth + slot uniqueness base.
		if !externalRefPattern.MatchString(node.Parent) {
			if _, ok := nodes[node.Parent]; !ok {
				add(CodeParentUnknown, ClassScope, node.LocalID, "parent",
					"parent %q is neither a proposal local id nor ext:<uuid>", node.Parent)
			}
		} else if fact, ok := ctx.Nodes[node.Parent[len("ext:"):]]; !ok || !fact.Exists {
			add(CodeParentUnknown, ClassScope, node.LocalID, "parent",
				"parent %s does not exist in this plan", node.Parent)
		}
	}

	// slot_key uniqueness: within (parent, slot) across proposal + store.
	slotTaken := map[string]bool{}
	for parent, slots := range ctx.SlotsUnder {
		for slot := range slots {
			slotTaken[parent+"\x00"+slot] = true
		}
	}
	for _, n := range p.Nodes {
		parentKey := parentCanonicalKey(n)
		key := parentKey + "\x00" + n.SlotKey
		if slotTaken[key] {
			add(CodeSlotKeyDuplicate, ClassScope, n.LocalID, "slot_key",
				"slot_key %q is already taken under this parent (plan, parent, slot uniqueness)", n.SlotKey)
		}
		slotTaken[key] = true
	}

	// human_code uniqueness inside the plan.
	codes := map[string]bool{}
	for code := range ctx.HumanCodes {
		codes[code] = true
	}
	for _, n := range p.Nodes {
		if codes[n.HumanCode] {
			add(CodeHumanCodeDuplicate, ClassScope, n.LocalID, "human_code",
				"human code %q is already used in this plan", n.HumanCode)
		}
		codes[n.HumanCode] = true
	}

	// ---- cycle: containment forest ----------------------------------
	// A contains cycle exists iff following proposal parents from a
	// local node revisits it (stored nodes are already acyclic).
	parentOf := func(localID string) (string, bool) { // "" when ext
		n, ok := nodes[localID]
		if !ok {
			return "", false
		}
		if externalRefPattern.MatchString(n.Parent) {
			return "", true
		}
		return n.Parent, true
	}
	for _, n := range p.Nodes {
		seen := map[string]bool{n.LocalID: true}
		cur := n.LocalID
		for {
			next, ok := parentOf(cur)
			if !ok {
				break
			}
			if next == "" {
				break // reaches an existing node: stored forest is acyclic
			}
			if seen[next] {
				add(CodeContainsCycle, ClassCycle, n.LocalID, "parent",
					"containment parents cycle through %q", next)
				break
			}
			seen[next] = true
			cur = next
		}
	}

	// ---- cycle + budget + resources over edges ----------------------
	for _, e := range p.Dependencies {
		fromKey, fromOK := endpointKey(e.From, nodes)
		toKey, toOK := endpointKey(e.To, nodes)
		if !fromOK || !toOK {
			add(CodeEndpointUnknown, ClassCycle, e.From, "from",
				"dependency endpoint %q/%q is not a proposal node or ext:<uuid>", e.From, e.To)
			continue
		}
		if e.From == e.To {
			add(CodeRequiresSelf, ClassCycle, e.From, "to", "a node cannot require itself")
			continue
		}
		for _, ref := range []struct{ raw, key string }{{e.From, fromKey}, {e.To, toKey}} {
			if externalRefPattern.MatchString(ref.raw) {
				if fact, ok := ctx.Nodes[ref.key]; !ok || !fact.Exists {
					add(CodeEndpointUnknown, ClassCycle, ref.raw, "endpoint",
						"dependency endpoint %s does not exist in this plan", ref.raw)
				}
			}
		}
		if e.Requirement != "" && e.Requirement != DependencyRequired && e.Requirement != DependencyOptional {
			add(CodePolicyInvalid, ClassResource, e.From, "requirement",
				"requirement must be required/optional, got %q", e.Requirement)
		}
		for _, key := range []string{fromKey, toKey} {
			if node, ok := nodes[key]; ok && node.NodeType == NodeTypePackage {
				add(CodeRequiresNonExec, ClassCycle, key, "node_type",
					"work_package %q cannot enter the requires graph (WGM-INV-003)", key)
			}
			if fact, ok := ctx.Nodes[key]; ok && fact.Exists && fact.NodeType == NodeTypePackage {
				add(CodeRequiresNonExec, ClassCycle, "ext:"+key, "node_type",
					"stored work_package %s cannot enter the requires graph (WGM-INV-003)", key)
			}
		}
	}

	// requires DAG over locals + stored nodes (union graph).
	if cyclic := requiresCycle(p, nodes, ctx); cyclic != "" {
		add(CodeRequiresCycle, ClassCycle, cyclic, "dependencies",
			"requires edges close a cycle through %q", cyclic)
	}

	// ---- budget ------------------------------------------------------
	for _, n := range p.Nodes {
		if n.Spec.BudgetUnits > limits.BudgetCeilingUnits {
			add(CodeBudgetCeiling, ClassBudget, n.LocalID, "budget_units",
				"budget %d exceeds the project ceiling %d", n.Spec.BudgetUnits, limits.BudgetCeilingUnits)
		}
	}
	// Children budget envelope per proposal package: the sum of the
	// budgets of its proposal children must not exceed the package
	// budget (stored children's budgets are enforced by their own
	// proposals; the envelope is re-checked at seal time).
	childSum := map[string]int64{}
	for _, n := range p.Nodes {
		if parent, ok := parentOf(n.LocalID); ok && parent != "" {
			childSum[parent] += n.Spec.BudgetUnits
		}
	}
	for localID, sum := range childSum {
		if pkg, ok := nodes[localID]; ok && sum > pkg.Spec.BudgetUnits {
			add(CodeBudgetSumExceeded, ClassBudget, localID, "budget_units",
				"children budgets sum to %d, exceeding package budget %d", sum, pkg.Spec.BudgetUnits)
		}
	}

	// ---- resources: ports, assets, atomicity, policies ---------------
	consumedPorts := map[string]map[string]bool{}
	for uuid, ports := range ctx.Consumes {
		consumedPorts[uuid] = cloneBoolSet(ports)
	}
	for _, f := range p.Flows {
		nodeKey := f.Node
		if !externalRefPattern.MatchString(nodeKey) {
			if _, ok := nodes[nodeKey]; !ok {
				add(CodeEndpointUnknown, ClassCycle, nodeKey, "node",
					"flow node %q is not a proposal node or ext:<uuid>", nodeKey)
				continue
			}
		} else if fact, ok := ctx.Nodes[nodeKey[len("ext:"):]]; !ok || !fact.Exists {
			add(CodeEndpointUnknown, ClassCycle, nodeKey, "node",
				"flow node %s does not exist in this plan", nodeKey)
			continue
		}
		if f.Direction != "consumes" && f.Direction != "produces" {
			add(CodePolicyInvalid, ClassResource, nodeKey, "direction",
				"flow direction must be consumes/produces, got %q", f.Direction)
		}
		if !slotKeyPattern.MatchString(f.PortKey) {
			add(CodePortGrammar, ClassResource, nodeKey, "port_key",
				"port key %q must match ^[a-z][a-z0-9.]{0,63}$", f.PortKey)
		}
		if !assetRefPattern.MatchString(f.AssetRef) {
			add(CodeAssetRefInvalid, ClassResource, nodeKey, "asset_ref",
				"asset reference %q must look like ART-<type>-<seq>@<version>", f.AssetRef)
		}
		// Absent from the context is also unknown: the store populates
		// every referenced version, so a missing key fails closed.
		if fact, ok := ctx.Assets[f.AssetRef]; !ok || !fact.Exists {
			add(CodeAssetUnknown, ClassResource, nodeKey, "asset_ref",
				"asset version %s is not registered in the ledger", f.AssetRef)
		}
		if consumedPorts[nodeKey] == nil {
			consumedPorts[nodeKey] = map[string]bool{}
		}
		// Only consumes bindings can satisfy required input ports;
		// a produces edge on the same port key does not (WGM-INV-004).
		if f.Direction == "consumes" {
			consumedPorts[nodeKey][f.PortKey] = true
		}
	}
	for _, n := range p.Nodes {
		for _, port := range n.Spec.RequiredInputs {
			if !consumedPorts[n.LocalID][port] {
				add(CodePortUnbound, ClassResource, n.LocalID, "required_inputs",
					"required input port %q has no consumes binding (WGM-INV-004)", port)
			}
		}
		if n.NodeType != NodeTypeItem {
			continue
		}
		if n.Spec.BaselineSHA == "" || !shaPattern.MatchString(n.Spec.BaselineSHA) {
			add(CodeBaselineMissing, ClassResource, n.LocalID, "baseline_sha",
				"work item must pin one exact baseline SHA (40/64 hex), got %q", n.Spec.BaselineSHA)
		}
		if n.Spec.Repo == "" {
			add(CodeAtomicityRepository, ClassResource, n.LocalID, "repo",
				"work item atomicity: exactly one repository is required")
		}
		if len(n.Spec.WorkspacePaths) == 0 {
			add(CodeWorkspaceMissing, ClassResource, n.LocalID, "workspace_paths",
				"work item needs an explicit workspace boundary (at least one path)")
		}
		if n.Spec.OwningCapability == "" {
			add(CodeCapabilityMissing, ClassResource, n.LocalID, "owning_capability",
				"work item needs one owning capability (capability routing, WGS-RULE-004)")
		}
	}
	for _, n := range p.Nodes {
		if n.Spec.SuccessThreshold != nil {
			if err := validThreshold(n.Spec.SuccessThreshold); err != "" {
				add(CodePolicyInvalid, ClassResource, n.LocalID, "success_threshold", "%s", err)
			}
		}
		switch n.Spec.FailurePolicy {
		case "", FailureFailFast, FailureCollectAll, FailureNeedsHuman:
		default:
			add(CodePolicyInvalid, ClassResource, n.LocalID, "failure_policy",
				"failure_policy must be fail_fast/collect_all/needs_human, got %q", n.Spec.FailurePolicy)
		}
		switch n.Spec.CancelPolicy {
		case "", CancelCascadeRequired, CancelDetachOptional, CancelNone:
		default:
			add(CodePolicyInvalid, ClassResource, n.LocalID, "cancel_policy",
				"cancel_policy must be cascade_required/detach_optional/none, got %q", n.Spec.CancelPolicy)
		}
	}

	// ---- scope: fan-out + depth (need resolved structure) -----------
	fanOut := map[string]int{}
	for parent := range ctx.ChildCount {
		fanOut[parent] += 0
	}
	for _, n := range p.Nodes {
		if parent, ok := parentOf(n.LocalID); ok && parent != "" {
			fanOut[parent]++
		} else if uuid, isExt := resolveParent(n.Parent); isExt {
			fanOut[uuid]++
		}
	}
	for parent, count := range fanOut {
		if existing := ctx.ChildCount[parent]; existing+count > limits.MaxFanOut {
			add(CodeFanOutExceeded, ClassScope, parent, "fan_out",
				"parent %s would carry %d children (existing %d + proposed %d), limit %d",
				parent, existing+count, existing, count, limits.MaxFanOut)
		}
	}
	for _, n := range p.Nodes {
		if d := proposalDepth(n.LocalID, nodes, ctx); d > limits.MaxContainmentDepth {
			add(CodeDepthExceeded, ClassScope, n.LocalID, "depth",
				"containment depth %d exceeds the limit %d", d, limits.MaxContainmentDepth)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

func validThreshold(t *SuccessThreshold) string {
	switch t.Kind {
	case ThresholdAll, ThresholdAny:
		return ""
	case ThresholdQuorum:
		if t.K < 1 {
			return "quorum k must be a positive integer"
		}
		return ""
	default:
		return fmt.Sprintf("success_threshold kind must be all/any/quorum, got %q", t.Kind)
	}
}

func ctxNodeDepth(ctx ProposalContext, uuid string) int {
	if fact, ok := ctx.Nodes[uuid]; ok {
		return fact.Depth
	}
	return 0
}

// parentCanonicalKey returns the map key used for slot uniqueness:
// the ext uuid when the parent is stored, the local id otherwise.
func parentCanonicalKey(n ProposalNode) string {
	if externalRefPattern.MatchString(n.Parent) {
		return n.Parent[len("ext:"):]
	}
	return n.Parent
}

// endpointKey maps an edge endpoint to its canonical key.
func endpointKey(ref string, nodes map[string]ProposalNode) (string, bool) {
	if externalRefPattern.MatchString(ref) {
		return ref[len("ext:"):], true
	}
	if _, ok := nodes[ref]; ok {
		return ref, true
	}
	return "", false
}

// requiresCycle walks the union requires graph (proposal edges +
// stored edges) and returns one node on a cycle, or "".
func requiresCycle(p DecompositionProposal, nodes map[string]ProposalNode, ctx ProposalContext) string {
	adj := map[string][]string{}
	for _, e := range ctx.Requires {
		adj[e.From] = append(adj[e.From], e.To)
	}
	for _, e := range p.Dependencies {
		from, okFrom := endpointKey(e.From, nodes)
		to, okTo := endpointKey(e.To, nodes)
		if !okFrom || !okTo || from == to {
			continue // malformed edges are reported by the scope pass
		}
		adj[from] = append(adj[from], to)
	}
	// Iterative DFS three-color over the union graph.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	for start := range adj {
		if color[start] != white {
			continue
		}
		stack = append(stack, start)
		path := map[string]bool{}
		for len(stack) > 0 {
			node := stack[len(stack)-1]
			switch color[node] {
			case white:
				color[node] = grey
				path[node] = true
				for _, next := range adj[node] {
					if color[next] == grey && path[next] {
						return next
					}
					if color[next] == white {
						stack = append(stack, next)
					}
				}
			case grey:
				// finished this node
				color[node] = black
				delete(path, node)
				stack = stack[:len(stack)-1]
			default:
				stack = stack[:len(stack)-1]
			}
		}
	}
	return ""
}

// proposalDepth resolves the containment depth of a proposal node:
// stored-parent depth + steps to the local node.
func proposalDepth(localID string, nodes map[string]ProposalNode, ctx ProposalContext) int {
	steps := 0
	cur := localID
	for steps <= DefaultMaxContainmentDepth*2 {
		n, ok := nodes[cur]
		if !ok {
			return steps
		}
		if externalRefPattern.MatchString(n.Parent) {
			return ctxNodeDepth(ctx, n.Parent[len("ext:"):]) + steps + 1
		}
		cur = n.Parent
		steps++
		if _, ok := nodes[cur]; !ok {
			return steps
		}
	}
	return steps // a cycle; the cycle pass reports it
}

func cloneBoolSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// MarshalSpec renders a node spec as canonical JSON (sorted keys, no
// insignificant whitespace) so equal specs always digest equal.
func MarshalSpec(spec NodeSpec) ([]byte, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("workgraph: marshal spec: %w", err)
	}
	return CanonicalJSON(raw)
}
