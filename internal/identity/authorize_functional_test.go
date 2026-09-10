package identity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// J1: the merged decision point over project-role grants and frozen
// functional grants. Every assertion reads the FROZEN permissions.yaml
// embedded in the binary — the functional allow lists are the matrix's
// functional_approvers blocks, never an invented grant.

func functionalPrincipal(projectID, role string, functions ...string) *model.PrincipalContext {
	principal := principalWith(projectID, role)
	principal.FunctionalRoles = functions
	return principal
}

func TestAuthorizeFunctionalRoles(t *testing.T) {
	p := policy(t)
	ctx := context.Background()
	project := "project-a"

	t.Run("security_owner over viewer membership reaches waiver.approve", func(t *testing.T) {
		decision := p.Authorize(ctx, functionalPrincipal(project, "viewer", "security_owner"),
			"waiver.approve", model.Resource{Type: "waiver", ProjectID: project})
		assert.True(t, decision.Allow, decision.Reasons)
		assert.Equal(t, "3.0", decision.PolicyVersion)
		assert.Equal(t, "functional:security_owner", Authority(decision))
	})

	t.Run("qa_owner reaches the category actions", func(t *testing.T) {
		for _, action := range []string{"waiver.approve", "waiver.approve.quality", "remediation_run.create"} {
			decision := p.Authorize(ctx, functionalPrincipal(project, "viewer", "qa_owner"),
				action, model.Resource{Type: "waiver", ProjectID: project})
			assert.True(t, decision.Allow, "%s: %v", action, decision.Reasons)
		}
	})

	t.Run("functional grants still require a project membership", func(t *testing.T) {
		principal := functionalPrincipal("other-project", "viewer", "security_owner")
		decision := p.Authorize(ctx, principal, "waiver.approve",
			model.Resource{Type: "waiver", ProjectID: project})
		assert.False(t, decision.Allow)
		assert.Contains(t, decision.Reasons[0], "no membership")
	})

	t.Run("project roles alone never approve", func(t *testing.T) {
		for _, role := range []string{"project_admin", "coordinator", "developer", "verifier", "viewer"} {
			decision := p.Authorize(ctx, principalWith(project, role), "waiver.approve",
				model.Resource{Type: "waiver", ProjectID: project})
			assert.False(t, decision.Allow, role)
		}
	})

	t.Run("functional roles do not stack project permissions", func(t *testing.T) {
		// security_owner grants only the frozen functional_approvers
		// actions: no project update, no policy strengthen, no ticket
		// creation — the viewer membership decides the project path.
		for _, action := range []string{"project.update", "project_policy.strengthen", "work_item.create"} {
			decision := p.Authorize(ctx, functionalPrincipal(project, "viewer", "security_owner"),
				action, model.Resource{Type: "project", ProjectID: project})
			assert.False(t, decision.Allow, action)
		}
	})

	t.Run("delegated principals never approve through a functional grant", func(t *testing.T) {
		agent := functionalPrincipal(project, "viewer", "security_owner")
		agent.DelegationID = "delegation-9"
		decision := p.Authorize(ctx, agent, "waiver.approve",
			model.Resource{Type: "waiver", ProjectID: project})
		assert.False(t, decision.Allow)
		assert.Contains(t, decision.Reasons[0], "delegated principals")
	})

	t.Run("bindable functions without frozen grants authorize nothing", func(t *testing.T) {
		// All five functions are bindable (migration 0016 enum); for the
		// waiver actions only security_owner and qa_owner carry frozen
		// grants — the J4 asset/workgraph planes do not widen waivers.
		for _, function := range []string{"operations_owner", "product_owner", "technical_lead"} {
			decision := p.Authorize(ctx, functionalPrincipal(project, "viewer", function),
				"waiver.approve", model.Resource{Type: "waiver", ProjectID: project})
			assert.False(t, decision.Allow, function)
			assert.Contains(t, decision.Reasons[0], "does not grant")
		}
	})

	t.Run("independent grant classes: a project deny does not veto the functional authority", func(t *testing.T) {
		// The organization granted the functional authority separately
		// (functional_principals + authorization deed); the developer
		// role's deny constrains the project path only. The frozen
		// separation-of-duties conditions (approver ≠ requester, ≠
		// change author) are enforced by the calling surface and the
		// store, not by class stacking.
		decision := p.Authorize(ctx, functionalPrincipal(project, "developer", "security_owner"),
			"waiver.approve", model.Resource{Type: "waiver", ProjectID: project})
		assert.True(t, decision.Allow, decision.Reasons)
	})

	t.Run("audit.export reaches a functional approver", func(t *testing.T) {
		decision := p.Authorize(ctx, functionalPrincipal(project, "viewer", "security_owner"),
			"audit.export", model.Resource{Type: "project", ProjectID: project})
		assert.True(t, decision.Allow, decision.Reasons)
	})
}

func TestAuthorityDistinguishesGrantClasses(t *testing.T) {
	p := policy(t)
	ctx := context.Background()
	project := "project-a"

	functional := p.Authorize(ctx, functionalPrincipal(project, "viewer", "qa_owner"),
		"waiver.approve", model.Resource{Type: "waiver", ProjectID: project})
	assert.Equal(t, "functional:qa_owner", Authority(functional))

	projectRole := p.Authorize(ctx, principalWith(project, "project_admin"),
		"waiver.request", model.Resource{Type: "waiver", ProjectID: project})
	assert.Equal(t, "project:project_admin", Authority(projectRole))

	denied := p.Authorize(ctx, principalWith(project, "viewer"),
		"work_item.create", model.Resource{Type: "work_item", ProjectID: project})
	assert.False(t, denied.Allow)
	assert.Equal(t, "", Authority(denied), "a deny never reports an authority")
}

func TestStaticResolverCarriesFunctionalRoles(t *testing.T) {
	resolver := &StaticResolver{
		Memberships: map[string]map[string]string{
			"sec-1": {"project-a": "viewer"},
		},
		Functional: map[string][]string{
			"sec-1": {"security_owner"},
		},
	}
	principal, err := resolver.Resolve(context.Background(), "https://idp.example", "sec-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"security_owner"}, principal.FunctionalRoles)

	_, err = resolver.Resolve(context.Background(), "https://idp.example", "viewer-only-subject")
	require.Error(t, err, "unknown subject fails closed")

	// A resolver without the functional map resolves principals with no
	// functional authority — the pre-J1 behavior is unchanged.
	bare := &StaticResolver{Memberships: map[string]map[string]string{
		"sec-1": {"project-a": "viewer"},
	}}
	principal, err = bare.Resolve(context.Background(), "https://idp.example", "sec-1")
	require.NoError(t, err)
	assert.Empty(t, principal.FunctionalRoles)
}
