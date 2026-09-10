package workgraph

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The full enumeration of illegal decompositions: every stable
// violation code has at least one proposal shape that triggers exactly
// it (J2b-1 acceptance). Codes are wire contract — when a code's
// meaning changes the test changes with it.

var validLimits = ProposalLimits{
	MaxNodes:            10,
	MaxContainmentDepth: 4,
	MaxFanOut:           4,
	BudgetCeilingUnits:  100_000,
	Now:                 time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
}

var validCtx = ProposalContext{
	Nodes: map[string]NodeFact{
		"11111111-1111-1111-1111-111111111111": {Exists: true, NodeType: NodeTypePackage, Depth: 0},
		"22222222-2222-2222-2222-222222222222": {Exists: true, NodeType: NodeTypeItem, Depth: 1},
	},
	ChildCount: map[string]int{},
	SlotsUnder: map[string]map[string]bool{},
	HumanCodes: map[string]bool{"MST-WI-00999": true},
	Requires:   []EdgeFact{},
	Consumes:   map[string]map[string]bool{},
	Assets: map[string]AssetFact{
		"ART-hld-001@2": {Exists: true, Status: "approved", Digest: "sha256:" + repeat("ab", 32)},
	},
	PatternActive: true,
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

// itemNode builds a minimal valid work_item node.
func itemNode(localID, parent, slot, code string) ProposalNode {
	prio := 2
	return ProposalNode{
		LocalID: localID, Parent: parent, NodeType: NodeTypeItem,
		SlotKey: slot, HumanCode: code,
		Spec: NodeSpec{
			Title: "Contract API", AcceptanceCriteria: []string{"schema frozen"},
			Repo: "peixun-java", BaselineSHA: repeat("a1", 20),
			WorkspacePaths: []string{"src/main/java"}, OwningCapability: "backend.java",
			BudgetUnits: 100, Priority: &prio,
			FailurePolicy: FailureCollectAll, CancelPolicy: CancelNone,
		},
	}
}

func baseProposal() DecompositionProposal {
	return DecompositionProposal{
		PlanID:               "plan-1",
		WorkPattern:          WorkPatternRef{PatternID: "11111111-aaaa-1111-1111-111111111111", Version: 1},
		ExpectedGraphVersion: 1,
		Nodes: []ProposalNode{
			itemNode("n1", "ext:11111111-1111-1111-1111-111111111111", "api", "MST-WI-00101"),
		},
	}
}

func codesOf(vs []Violation) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Code)
	}
	return out
}

func TestValidateProposalAcceptsLegalDecomposition(t *testing.T) {
	p := baseProposal()
	p.Nodes = append(p.Nodes, ProposalNode{
		LocalID: "pkg", Parent: "ext:11111111-1111-1111-1111-111111111111",
		NodeType: NodeTypePackage, SlotKey: "sub", HumanCode: "MST-WP-00201",
		Spec: NodeSpec{
			Title: "Sub package", AcceptanceCriteria: []string{"children done"},
			BudgetUnits: 500, SuccessThreshold: &SuccessThreshold{Kind: ThresholdAll},
			FailurePolicy: FailureFailFast, CancelPolicy: CancelDetachOptional,
		},
	}, itemNode("n2", "pkg", "impl", "MST-WI-00102"))
	p.Dependencies = []ProposalEdge{{From: "n2", To: "n1"}}
	p.Flows = []ProposalFlow{{Node: "n2", Direction: "consumes", AssetRef: "ART-hld-001@2", PortKey: "design"}}

	violations := ValidateProposal(p, validLimits, validCtx)
	assert.Empty(t, violations, "legal decomposition must validate: %v", violations)
}

// TestIllegalDecompositionCatalog walks every stable violation code.
func TestIllegalDecompositionCatalog(t *testing.T) {
	cases := []struct {
		code string
		mutate func(*DecompositionProposal, *ProposalContext, *ProposalLimits)
	}{
		{CodeSlotKeyGrammar, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].SlotKey = "1Bad"
		}},
		{CodeSlotKeyDuplicate, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes = append(p.Nodes, itemNode("n2", "ext:11111111-1111-1111-1111-111111111111", "api", "MST-WI-00102"))
		}},
		{CodeSlotKeyDuplicate, func(p *DecompositionProposal, ctx *ProposalContext, _ *ProposalLimits) {
			ctx.SlotsUnder["11111111-1111-1111-1111-111111111111"] = map[string]bool{"api": true}
		}},
		{CodeHumanCodeGrammar, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].HumanCode = "MST-WI-1"
		}},
		{CodeHumanCodeDuplicate, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].HumanCode = "MST-WI-00999" // already in stored plan
		}},
		{CodeHumanCodeDuplicate, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes = append(p.Nodes, itemNode("n2", "ext:11111111-1111-1111-1111-111111111111", "api2", "MST-WI-00101"))
		}},
		{CodeNodeTypeInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].NodeType = "epic"
		}},
		{CodeTitleLength, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.Title = ""
		}},
		{CodeNodesLimit, func(p *DecompositionProposal, _ *ProposalContext, l *ProposalLimits) {
			l.MaxNodes = 0 // defaults to 50
			for i := range 51 {
				p.Nodes = append(p.Nodes, itemNode(
					legalLocalID(i), "ext:11111111-1111-1111-1111-111111111111",
					legalSlot(i), legalCode(i)))
			}
		}},
		{CodeDepthExceeded, func(p *DecompositionProposal, _ *ProposalContext, l *ProposalLimits) {
			l.MaxContainmentDepth = 1
			p.Nodes[0].Parent = "ext:22222222-2222-2222-2222-222222222222" // stored depth 1 + 1 = 2 > 1
		}},
		{CodeFanOutExceeded, func(p *DecompositionProposal, _ *ProposalContext, l *ProposalLimits) {
			l.MaxFanOut = 1
			p.Nodes = append(p.Nodes, itemNode("n2", "ext:11111111-1111-1111-1111-111111111111", "api2", "MST-WI-00102"))
		}},
		{CodeFanOutExceeded, func(p *DecompositionProposal, ctx *ProposalContext, l *ProposalLimits) {
			l.MaxFanOut = 1
			ctx.ChildCount["11111111-1111-1111-1111-111111111111"] = 1
		}},
		{CodeBudgetNonPositive, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.BudgetUnits = 0
		}},
		{CodeDeadlineInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.Deadline = "2026-09-01T00:00:00Z" // before Now
		}},
		{CodeDeadlineInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.Deadline = "tomorrow"
		}},
		{CodeAcceptanceEmpty, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.AcceptanceCriteria = nil
		}},
		{CodeParentUnknown, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Parent = "ext:99999999-9999-9999-9999-999999999999"
		}},
		{CodeParentUnknown, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Parent = "ghost"
		}},
		{CodeContainsCycle, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Parent = "n2"
			p.Nodes = append(p.Nodes, func() ProposalNode {
				n := itemNode("n2", "n1", "impl", "MST-WI-00102")
				return n
			}())
		}},
		{CodeRequiresSelf, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Dependencies = []ProposalEdge{{From: "n1", To: "n1"}}
		}},
		{CodeRequiresCycle, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes = append(p.Nodes, itemNode("n2", "ext:11111111-1111-1111-1111-111111111111", "impl", "MST-WI-00102"))
			p.Dependencies = []ProposalEdge{{From: "n1", To: "n2"}, {From: "n2", To: "n1"}}
		}},
		{CodeRequiresCycle, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			// Mixed cycle through a stored node: n1 -> stored(2222) -> n1.
			// Stored edges can never reference proposal nodes, so both
			// legs live in the proposal.
			p.Nodes[0].Parent = "ext:22222222-2222-2222-2222-222222222222"
			p.Dependencies = []ProposalEdge{
				{From: "n1", To: "ext:22222222-2222-2222-2222-222222222222"},
				{From: "ext:22222222-2222-2222-2222-222222222222", To: "n1"},
			}
		}},
		{CodeRequiresNonExec, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Dependencies = []ProposalEdge{{From: "n1", To: "ext:11111111-1111-1111-1111-111111111111"}}
		}},
		{CodeRequiresNonExec, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes = append(p.Nodes, ProposalNode{
				LocalID: "pkg", Parent: "ext:11111111-1111-1111-1111-111111111111",
				NodeType: NodeTypePackage, SlotKey: "sub", HumanCode: "MST-WP-00201",
				Spec: NodeSpec{Title: "P", AcceptanceCriteria: []string{"x"}, BudgetUnits: 500},
			})
			p.Dependencies = []ProposalEdge{{From: "pkg", To: "n1"}}
		}},
		{CodeEndpointUnknown, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Dependencies = []ProposalEdge{{From: "n1", To: "ghost"}}
		}},
		{CodeEndpointUnknown, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Dependencies = []ProposalEdge{{From: "ext:99999999-9999-9999-9999-999999999999", To: "n1"}}
		}},
		{CodeBudgetSumExceeded, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			pkg := ProposalNode{
				LocalID: "pkg", Parent: "ext:11111111-1111-1111-1111-111111111111",
				NodeType: NodeTypePackage, SlotKey: "sub", HumanCode: "MST-WP-00201",
				Spec: NodeSpec{Title: "P", AcceptanceCriteria: []string{"x"}, BudgetUnits: 100},
			}
			p.Nodes = append(p.Nodes, pkg,
				itemNode("n2", "pkg", "a", "MST-WI-00102"), itemNode("n3", "pkg", "b", "MST-WI-00103"))
		}},
		{CodeBudgetCeiling, func(p *DecompositionProposal, _ *ProposalContext, l *ProposalLimits) {
			l.BudgetCeilingUnits = 50
		}},
		{CodePatternUnavailable, func(p *DecompositionProposal, ctx *ProposalContext, _ *ProposalLimits) {
			ctx.PatternActive = false
		}},
		{CodePolicyInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.SuccessThreshold = &SuccessThreshold{Kind: "most"}
		}},
		{CodePolicyInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.SuccessThreshold = &SuccessThreshold{Kind: ThresholdQuorum, K: 0}
		}},
		{CodePolicyInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.FailurePolicy = "ignore"
		}},
		{CodePolicyInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.CancelPolicy = "yolo"
		}},
		{CodePolicyInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Dependencies = []ProposalEdge{{From: "n1", To: "ext:22222222-2222-2222-2222-222222222222", Requirement: "maybe"}}
		}},
		{CodePolicyInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Flows = []ProposalFlow{{Node: "n1", Direction: "both", AssetRef: "ART-hld-001@2", PortKey: "design"}}
		}},
		{CodePortGrammar, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.RequiredInputs = []string{"1design"}
			p.Flows = []ProposalFlow{{Node: "n1", Direction: "consumes", AssetRef: "ART-hld-001@2", PortKey: "1design"}}
		}},
		{CodePortUnbound, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.RequiredInputs = []string{"design"}
		}},
		{CodePortUnbound, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.RequiredInputs = []string{"design"}
			p.Flows = []ProposalFlow{{Node: "n1", Direction: "produces", AssetRef: "ART-hld-001@2", PortKey: "design"}}
		}},
		{CodePortUnbound, func(p *DecompositionProposal, ctx *ProposalContext, _ *ProposalLimits) {
			// Bound port lives on the STORED node, not the proposal node.
			p.Nodes[0].Spec.RequiredInputs = []string{"design"}
			ctx.Consumes["11111111-1111-1111-1111-111111111111"] = map[string]bool{"design": true}
		}},
		{CodeAssetRefInvalid, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.RequiredInputs = []string{"design"}
			p.Flows = []ProposalFlow{{Node: "n1", Direction: "consumes", AssetRef: "hld-2", PortKey: "design"}}
		}},
		{CodeAssetUnknown, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.RequiredInputs = []string{"design"}
			p.Flows = []ProposalFlow{{Node: "n1", Direction: "consumes", AssetRef: "ART-hld-009@7", PortKey: "design"}}
		}},
		{CodeBaselineMissing, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.BaselineSHA = "HEAD"
		}},
		{CodeBaselineMissing, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.BaselineSHA = ""
		}},
		{CodeAtomicityRepository, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.Repo = ""
		}},
		{CodeWorkspaceMissing, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.WorkspacePaths = nil
		}},
		{CodeCapabilityMissing, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].Spec.OwningCapability = ""
		}},
		{CodeLocalIDGrammar, func(p *DecompositionProposal, _ *ProposalContext, _ *ProposalLimits) {
			p.Nodes[0].LocalID = "1n"
		}},
	}

	seen := map[string]bool{}
	for _, tc := range cases {
		p := baseProposal()
		ctx := cloneCtx(validCtx)
		limits := validLimits
		tc.mutate(&p, &ctx, &limits)
		violations := ValidateProposal(p, limits, ctx)
		codes := codesOf(violations)
		require.NotEmpty(t, violations, "case %s must produce violations", tc.code)
		assert.Contains(t, codes, tc.code, "violations: %v", violations)
		seen[tc.code] = true
	}

	// The catalog is exhaustive over the frozen code list.
	for _, code := range []string{
		CodeSlotKeyGrammar, CodeSlotKeyDuplicate, CodeHumanCodeGrammar, CodeHumanCodeDuplicate,
		CodeNodeTypeInvalid, CodeTitleLength, CodeNodesLimit, CodeDepthExceeded, CodeFanOutExceeded,
		CodeBudgetNonPositive, CodeDeadlineInvalid, CodeAcceptanceEmpty, CodeParentUnknown,
		CodeContainsCycle, CodeRequiresSelf, CodeRequiresCycle, CodeRequiresNonExec, CodeEndpointUnknown,
		CodeBudgetSumExceeded, CodeBudgetCeiling,
		CodePatternUnavailable, CodePolicyInvalid, CodePortGrammar, CodePortUnbound, CodeAssetRefInvalid, CodeAssetUnknown,
		CodeBaselineMissing, CodeWorkspaceMissing, CodeCapabilityMissing, CodeAtomicityRepository,
		CodeLocalIDGrammar,
	} {
		assert.True(t, seen[code], "catalog must cover code %s", code)
	}
}

// Deterministic replay: identical inputs yield identical violations.
func TestValidateProposalDeterministicReplay(t *testing.T) {
	p := baseProposal()
	p.Nodes = append(p.Nodes, itemNode("n2", "ext:11111111-1111-1111-1111-111111111111", "API", "MST-WI-00101"))
	first := ValidateProposal(p, validLimits, validCtx)
	second := ValidateProposal(p, validLimits, validCtx)
	require.Equal(t, first, second)
	raw, err := json.Marshal(first)
	require.NoError(t, err)
	assert.NotEmpty(t, raw)
}

func legalLocalID(i int) string {
	return "n" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
}

// cloneCtx deep-copies the shared fixture so one case's context
// mutation cannot leak into the next.
func cloneCtx(in ProposalContext) ProposalContext {
	out := ProposalContext{
		Nodes:        map[string]NodeFact{},
		ChildCount:   map[string]int{},
		SlotsUnder:   map[string]map[string]bool{},
		HumanCodes:   map[string]bool{},
		Requires:     append([]EdgeFact(nil), in.Requires...),
		Consumes:     map[string]map[string]bool{},
		Assets:       map[string]AssetFact{},
		PatternActive: in.PatternActive,
	}
	for k, v := range in.Nodes {
		out.Nodes[k] = v
	}
	for k, v := range in.ChildCount {
		out.ChildCount[k] = v
	}
	for k, v := range in.SlotsUnder {
		out.SlotsUnder[k] = cloneBoolSet(v)
	}
	for k := range in.HumanCodes {
		out.HumanCodes[k] = true
	}
	for k, v := range in.Consumes {
		out.Consumes[k] = cloneBoolSet(v)
	}
	for k, v := range in.Assets {
		out.Assets[k] = v
	}
	return out
}

func legalSlot(i int) string  { return "slot" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) }
func legalCode(i int) string {
	digits := [5]string{"0", "0", "0", "0", "0"}
	n := i
	for d := 4; d >= 0; d-- {
		digits[d] = string(rune('0' + n%10))
		n /= 10
	}
	return "MST-WI-002" + digits[2] + digits[3] + digits[4]
}
