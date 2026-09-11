package identity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// J4 (CR-1 terminal state): the asset.*/workgraph.* permission families.
// Every assertion reads the FROZEN permissions.yaml embedded in the
// binary; the tables below are exhaustive per string — each new
// permission is checked against every project role (positive AND
// negative) and against the functional planes the matrix grants.

// j4ProjectRoles is every frozen project-role grant class.
var j4ProjectRoles = []string{
	"project_admin", "coordinator", "developer", "verifier", "viewer", "platform_admin",
}

func TestAssetWorkGraphFamiliesPerProjectRole(t *testing.T) {
	p := policy(t)
	ctx := context.Background()
	const project = "project-j4"

	// allowedByRole: the exact project-role allow sets for the seven
	// family strings (everything not listed denies).
	allowedByRole := map[string]map[string]bool{
		"asset.read": {
			"project_admin": true, "coordinator": true, "developer": true,
			"verifier": true, "viewer": true, // viewer-level read floor
			"platform_admin": false, // platform plane never widens project reads
		},
		"workgraph.read": {
			"project_admin": true, "coordinator": true, "developer": true,
			"verifier": true, "viewer": true,
			"platform_admin": false,
		},
		"asset.register": {
			"developer": true, "coordinator": true, // developer-level write
			"project_admin": false, "verifier": false, "viewer": false, "platform_admin": false,
		},
		"workgraph.propose": {
			"developer": true, "coordinator": true,
			"project_admin": false, "verifier": false, "viewer": false, "platform_admin": false,
		},
		"asset.review":  {}, // functional planes only
		"asset.approve": {},
		"workgraph.seal": {
			// project roles never seal: the technical_lead functional plane owns it
			"project_admin": false, "coordinator": false, "developer": false,
			"verifier": false, "viewer": false, "platform_admin": false,
		},
	}

	for permission, allows := range allowedByRole {
		for _, role := range j4ProjectRoles {
			decision := p.Authorize(ctx, principalWith(project, role), permission,
				model.Resource{Type: "work_item", ProjectID: project})
			expected := allows[role]
			assert.Equal(t, expected, decision.Allow,
				"%s over project role %s: %v", permission, role, decision.Reasons)
		}
	}
}

func TestAssetWorkGraphFamiliesPerFunctionalRole(t *testing.T) {
	p := policy(t)
	ctx := context.Background()
	const project = "project-j4"

	// The functional allow sets: review is technical_lead+qa_owner;
	// approve is the four functional owners; seal is technical_lead.
	// Reads need no functional grant — every project membership (the
	// viewer floor below) already carries them, so the read rows for
	// security_owner assert the project path, never a functional one.
	functionalAllows := map[string]map[string]bool{
		"asset.read": {
			"technical_lead": true, "qa_owner": true, "product_owner": true,
			"operations_owner": true, "security_owner": true,
		},
		"workgraph.read": {
			"technical_lead": true, "qa_owner": true, "product_owner": true,
			"operations_owner": true, "security_owner": true,
		},
		"asset.review": {
			"technical_lead": true, "qa_owner": true,
			"product_owner": false, "operations_owner": false, "security_owner": false,
		},
		"asset.approve": {
			"technical_lead": true, "qa_owner": true, "product_owner": true,
			"operations_owner": true, "security_owner": false,
		},
		"workgraph.seal": {
			"technical_lead": true,
			"qa_owner":       false, "product_owner": false, "operations_owner": false, "security_owner": false,
		},
		"asset.register": {
			// functional planes never widen into project writes
			"technical_lead": false, "qa_owner": false, "product_owner": false,
			"operations_owner": false, "security_owner": false,
		},
		"workgraph.propose": {
			"technical_lead": false, "qa_owner": false, "product_owner": false,
			"operations_owner": false, "security_owner": false,
		},
	}

	for permission, allows := range functionalAllows {
		for function, expected := range allows {
			decision := p.Authorize(ctx, functionalPrincipal(project, "viewer", function),
				permission, model.Resource{Type: "work_item", ProjectID: project})
			assert.Equal(t, expected, decision.Allow,
				"%s over functional %s: %v", permission, function, decision.Reasons)
			if expected && permission == "asset.read" || expected && permission == "workgraph.read" {
				// Reads ride the viewer membership's project grant, never
				// a functional one (security_owner carries no read grant).
				assert.Equal(t, "project:viewer", Authority(decision),
					"%s read must ride the project path for %s", permission, function)
				continue
			}
			if expected {
				assert.Equal(t, "functional:"+function, Authority(decision),
					"%s allow must report the functional authority", permission)
			}
		}
	}
}

func TestAllowFunctionalRoleWalksTheNewPlanes(t *testing.T) {
	p := policy(t)

	assert.True(t, p.AllowFunctionalRole(context.Background(), "technical_lead", "workgraph.seal"))
	assert.True(t, p.AllowFunctionalRole(context.Background(), "technical_lead", "asset.review"))
	assert.True(t, p.AllowFunctionalRole(context.Background(), "qa_owner", "asset.review"))
	assert.True(t, p.AllowFunctionalRole(context.Background(), "qa_owner", "asset.approve"))
	assert.True(t, p.AllowFunctionalRole(context.Background(), "product_owner", "asset.approve"))
	assert.True(t, p.AllowFunctionalRole(context.Background(), "operations_owner", "asset.approve"))

	assert.False(t, p.AllowFunctionalRole(context.Background(), "qa_owner", "workgraph.seal"))
	assert.False(t, p.AllowFunctionalRole(context.Background(), "product_owner", "asset.review"))
	assert.False(t, p.AllowFunctionalRole(context.Background(), "operations_owner", "workgraph.seal"))
	assert.False(t, p.AllowFunctionalRole(context.Background(), "security_owner", "asset.approve"))
	assert.False(t, p.AllowFunctionalRole(context.Background(), "viewer", "asset.approve"), "project roles never reach the functional helper")
	assert.False(t, p.AllowFunctionalRole(context.Background(), "unknown_function", "asset.read"))
}

func TestDelegatedPrincipalAssetApproveVeto(t *testing.T) {
	p := policy(t)
	ctx := context.Background()
	const project = "project-j4"

	// The asset release stays human-only: a delegated principal with the
	// functional grant still cannot approve (the transitional
	// waiver.approve veto carries over to the family string).
	agent := functionalPrincipal(project, "viewer", "technical_lead")
	agent.DelegationID = "delegation-j4"
	decision := p.Authorize(ctx, agent, "asset.approve", model.Resource{Type: "work_item", ProjectID: project})
	assert.False(t, decision.Allow)
	assert.Contains(t, decision.Reasons[0], "delegated principals")

	// Agents may still register and review: the frozen delegation veto
	// list blocks self-approve/waive/merge, and review stays open (the
	// pre-J4 quality_policy.review path was never vetoed either).
	review := p.Authorize(ctx, agent, "asset.review", model.Resource{Type: "work_item", ProjectID: project})
	assert.True(t, review.Allow, review.Reasons)

	// A delegated developer never seals (grant absence; seal is the
	// console-only HITL plane).
	worker := principalWith(project, "developer")
	worker.DelegationID = "delegation-j4"
	seal := p.Authorize(ctx, worker, "workgraph.seal", model.Resource{Type: "work_item", ProjectID: project})
	assert.False(t, seal.Allow)
}

func TestNewFamilyGrantsStillRequireMembership(t *testing.T) {
	p := policy(t)
	ctx := context.Background()

	for _, permission := range []string{
		"asset.read", "asset.register", "asset.review", "asset.approve", "workgraph.read", "workgraph.propose", "workgraph.seal",
	} {
		outside := p.Authorize(ctx, principalWith("other-project", "project_admin"), permission,
			model.Resource{Type: "work_item", ProjectID: "project-j4"})
		assert.False(t, outside.Allow, "%s must stay project-scoped", permission)

		functionalOutside := p.Authorize(ctx, functionalPrincipal("other-project", "viewer", "technical_lead"),
			permission, model.Resource{Type: "work_item", ProjectID: "project-j4"})
		assert.False(t, functionalOutside.Allow, "%s functional grant must stay project-scoped", permission)
		assert.Contains(t, functionalOutside.Reasons[0], "no membership")
	}
}
