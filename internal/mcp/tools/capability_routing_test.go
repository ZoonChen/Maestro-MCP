package tools

import (
	"context"
	"testing"

	mcpspec "github.com/ZoonChen/Maestro-MCP/docs/specs/mcp"
	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Capability-routing contract tests (J2c-3, ADR-009 §2): the frozen
// declaration must cover exactly the six J2c tools, the plane classes
// must agree with the embedded permission catalog, the server-exclusive
// operations must stay off the MCP surface, and the WorkPattern plane
// must enforce owning-capability routing through the J2b validator.

func j2cToolNames(t *testing.T) map[string]string {
	t.Helper()
	permissions, err := mcpspec.ToolPermissions()
	require.NoError(t, err)
	j2c := map[string]string{}
	for _, name := range []string{
		"worktree_graph_query", "decomposition_propose", "asset_register",
		"asset_review", "asset_approve", "asset_query",
	} {
		permission, ok := permissions[name]
		require.True(t, ok, "J2c tool %q missing from the embedded catalog", name)
		j2c[name] = permission
	}
	return j2c
}

func TestCapabilityRoutingDeclarationCoversExactlyTheJ2cTools(t *testing.T) {
	j2c := j2cToolNames(t)
	require.Len(t, workGraphCapabilityRoutes, len(j2c))
	for name := range j2c {
		require.Contains(t, workGraphCapabilityRoutes, name)
	}
}

// TestCapabilityRoutingPlanesMatchPermissions pins the plane class ↔
// frozen-permission agreement (J4 family): the query planes read on the
// read family, the proposal/ledger-write planes carry the
// developer-level write grants, and the functional planes map onto the
// frozen functional approver grants only.
func TestCapabilityRoutingPlanesMatchPermissions(t *testing.T) {
	j2c := j2cToolNames(t)
	planePermissions := map[string]map[string]string{
		PlaneQuery: {
			"worktree_graph_query": "workgraph.read",
			"asset_query":          "asset.read",
		},
		PlaneProposal:          {"decomposition_propose": "workgraph.propose"},
		PlaneLedgerWrite:       {"asset_register": "asset.register"},
		PlaneFunctionalReview:  {"asset_review": "asset.review"},
		PlaneFunctionalRelease: {"asset_approve": "asset.approve"},
	}
	for name, permission := range j2c {
		route, ok := workGraphCapabilityRoutes[name]
		require.True(t, ok, "tool %q has no capability route", name)
		expected, ok := planePermissions[route.Plane][name]
		require.True(t, ok, "tool %q has no pinned plane permission", name)
		assert.Equal(t, expected, permission, "%s: plane %s permission mismatch", name, route.Plane)
	}
}

// TestCapabilityRoutingDeveloperLevelPlanes asserts the J4 boundary
// where it is expressible in the frozen matrix: the proposal and
// registration grants (workgraph.propose / asset.register) belong to
// exactly the developer-level project roles — developer and
// coordinator — and to no other role.
func TestCapabilityRoutingDeveloperLevelPlanes(t *testing.T) {
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	for _, permission := range []string{"workgraph.propose", "asset.register"} {
		granted := map[string]bool{}
		for role, grants := range policy.Roles {
			if _, ok := grants.Allow[permission]; ok {
				granted[role] = true
			}
		}
		assert.Equal(t, map[string]bool{"coordinator": true, "developer": true}, granted,
			"%s must stay on the developer-level project roles", permission)
	}
}

// TestCapabilityRoutingServerExclusiveOperationsStayOffTheCatalog is
// the negative surface: sealing, structural mutation, aggregation and
// replan are server-exclusive (ADR-009 §2) and must never be MCP tools.
func TestCapabilityRoutingServerExclusiveOperationsStayOffTheCatalog(t *testing.T) {
	permissions, err := mcpspec.ToolPermissions()
	require.NoError(t, err)
	for _, operation := range serverExclusiveOperations {
		_, leaked := permissions[operation]
		assert.False(t, leaked, "server-exclusive operation %q leaked into the MCP catalog", operation)
	}
}

// TestCapabilityRoutingReleasePlaneVetoesDelegatedPrincipals asserts
// the release plane's boundary end to end: a delegated (agent)
// principal holding the qa_owner functional grant is still vetoed by
// the frozen delegation table — an agent never self-releases. The J4
// family keeps the veto on asset.approve (the transitional
// waiver.approve string moves over with the mapping).
func TestCapabilityRoutingReleasePlaneVetoesDelegatedPrincipals(t *testing.T) {
	policy, err := identity.EmbeddedPolicy()
	require.NoError(t, err)
	human := &model.PrincipalContext{
		PrincipalID:        "user:qa",
		ProjectMemberships: map[string]string{"proj-wg": "viewer"},
		FunctionalRoles:    []string{"qa_owner"},
	}
	decision := policy.Authorize(context.Background(), human, "asset.approve", model.Resource{Type: "work_item", ProjectID: "proj-wg"})
	assert.True(t, decision.Allow, "functional qa_owner must release: %v", decision.Reasons)
	assert.Equal(t, "functional:qa_owner", identity.Authority(decision))

	agent := &model.PrincipalContext{
		PrincipalID:        "user:qa",
		DelegationID:       "delegation-agent",
		ProjectMemberships: map[string]string{"proj-wg": "viewer"},
		FunctionalRoles:    []string{"qa_owner"},
	}
	decision = policy.Authorize(context.Background(), agent, "asset.approve", model.Resource{Type: "work_item", ProjectID: "proj-wg"})
	assert.False(t, decision.Allow, "delegated principals must never release assets")
}

// TestCapabilityRoutingWorkPatternPlaneRequiresOwningCapability pins
// the WorkPattern-side routing: the J2b proposal validator rejects a
// work item without its owning capability (WGP-RS-009), so every
// executable decomposition routes onto the capability plane.
func TestCapabilityRoutingWorkPatternPlaneRequiresOwningCapability(t *testing.T) {
	missing := workgraph.DecompositionProposal{
		PlanID: "plan-1", ExpectedGraphVersion: 1,
		WorkPattern: workgraph.WorkPatternRef{PatternID: "pattern-1", Version: 1},
		Nodes: []workgraph.ProposalNode{{
			LocalID: "api", Parent: "root", NodeType: "work_item",
			SlotKey: "api", HumanCode: "MST-WI-00101",
			Spec: workgraph.NodeSpec{
				Title: "no capability", Repo: "peixun-java",
				BaselineSHA:    "0123456789012345678901234567890123456789",
				WorkspacePaths: []string{"services/api"},
			},
		}},
	}
	violations := workgraph.ValidateProposal(missing, workgraph.ProposalLimits{}, workgraph.ProposalContext{})
	found := false
	for _, violation := range violations {
		if violation.Code == workgraph.CodeCapabilityMissing {
			found = true
		}
	}
	assert.True(t, found, "work item without owning_capability must be rejected (WGP-RS-009), got %v", violations)
}
