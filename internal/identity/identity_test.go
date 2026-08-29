package identity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// The decision point is matrix-driven: every assertion reads the FROZEN
// permissions.yaml embedded in the binary, so a matrix edit that breaks a
// guarantee fails here before any transport sees it.

func policy(t *testing.T) *Policy {
	t.Helper()
	loaded, err := EmbeddedPolicy()
	require.NoError(t, err)
	return loaded
}

func principalWith(projectID, role string) *model.PrincipalContext {
	return &model.PrincipalContext{
		PrincipalID:        "principal-test",
		Type:               model.PrincipalTypeHuman,
		ProjectMemberships: map[string]string{projectID: role},
	}
}

func TestEmbeddedPolicyIsFrozenV3(t *testing.T) {
	loaded := policy(t)
	assert.Equal(t, "3.0", loaded.Version)
	assert.Equal(t, "deny", loaded.DefaultEffect)
	assert.NotEmpty(t, loaded.Roles)
	assert.Contains(t, loaded.Roles, "project_admin")
	assert.Contains(t, loaded.Service, "runner_device")
	assert.False(t, loaded.Delegation.ServiceInheritHuman)
	assert.False(t, loaded.Delegation.AgentSelfReviewWaive)
}

func TestAuthorizeMatrixAllowAndDeny(t *testing.T) {
	p := policy(t)
	ctx := context.Background()
	project := "project-a"

	allowed := []struct{ role, action string }{
		{"developer", "work_item.claim"},
		{"developer", "work_item.submit"},
		{"coordinator", "work_item.create"},
		{"project_admin", "runner.approve"},
		{"verifier", "work_item.read"},
		{"viewer", "project.read"},
	}
	for _, test := range allowed {
		decision := p.Authorize(ctx, principalWith(project, test.role), test.action,
			model.Resource{Type: "work_item", ProjectID: project})
		assert.True(t, decision.Allow, "%s/%s: %v", test.role, test.action, decision.Reasons)
	}

	denied := p.Authorize(ctx, principalWith(project, "developer"), "runner.approve",
		model.Resource{Type: "runner", ProjectID: project})
	assert.False(t, denied.Allow)

	denied = p.Authorize(ctx, principalWith(project, "platform_admin"), "project.code.read_by_role_only",
		model.Resource{Type: "project", ProjectID: project})
	assert.False(t, denied.Allow)
}

func TestAuthorizeProjectScopeIsolation(t *testing.T) {
	p := policy(t)
	ctx := context.Background()

	decision := p.Authorize(ctx, principalWith("project-a", "project_admin"), "project.read",
		model.Resource{Type: "project", ProjectID: "project-b"})
	assert.False(t, decision.Allow)
	assert.Contains(t, decision.Reasons[0], "no membership")

	decision = p.Authorize(ctx, nil, "project.read", model.Resource{Type: "project", ProjectID: "project-a"})
	assert.False(t, decision.Allow)

	decision = p.Authorize(ctx, principalWith("p", "viewer"), "project.read", model.Resource{})
	assert.False(t, decision.Allow)
	assert.Contains(t, decision.Reasons[0], "invalid resource")
}

func TestAuthorizeProtectedActionsNeverAllowed(t *testing.T) {
	p := policy(t)
	ctx := context.Background()

	for _, action := range []string{"protected_branch.merge", "gitlab.merge"} {
		decision := p.Authorize(ctx, principalWith("project-a", "project_admin"), action,
			model.Resource{Type: "merge", ProjectID: "project-a"})
		assert.False(t, decision.Allow, action)
		assert.Contains(t, decision.Reasons[0], "human-only")
	}
}

func TestAuthorizeDelegationRestrictions(t *testing.T) {
	p := policy(t)
	ctx := context.Background()
	project := "project-a"

	agent := principalWith(project, "developer")
	agent.DelegationID = "delegation-1"
	decision := p.Authorize(ctx, agent, "work_item.claim", model.Resource{Type: "work_item", ProjectID: project})
	assert.True(t, decision.Allow)

	delegatedVerifier := principalWith(project, "verifier")
	delegatedVerifier.DelegationID = "delegation-2"
	decision = p.Authorize(ctx, delegatedVerifier, "verification.submit",
		model.Resource{Type: "work_item", ProjectID: project})
	assert.False(t, decision.Allow)
	assert.Contains(t, decision.Reasons[0], "delegated principals")

	human := principalWith(project, "verifier")
	decision = p.Authorize(ctx, human, "verification.submit", model.Resource{Type: "work_item", ProjectID: project})
	assert.True(t, decision.Allow)
}

func TestAuthorizeServiceIdentities(t *testing.T) {
	p := policy(t)
	ctx := context.Background()

	decision := p.AuthorizeService(ctx, "runner_device", "runner.lease.claim",
		model.Resource{Type: "lease", ProjectID: "project-a"})
	assert.True(t, decision.Allow)

	decision = p.AuthorizeService(ctx, "runner_device", "runner.approve",
		model.Resource{Type: "runner", ProjectID: "project-a"})
	assert.False(t, decision.Allow)

	decision = p.AuthorizeService(ctx, "gitlab_bot", "git.repository.push",
		model.Resource{Type: "repository", ProjectID: "project-a"})
	assert.False(t, decision.Allow)

	decision = p.AuthorizeService(ctx, "mystery", "project.read",
		model.Resource{Type: "project", ProjectID: "project-a"})
	assert.False(t, decision.Allow)
}

func TestFunctionalApproversStaticGrants(t *testing.T) {
	p := policy(t)

	assert.True(t, p.AllowFunctionalRole(context.Background(), "security_owner", "waiver.approve"))
	assert.True(t, p.AllowFunctionalRole(context.Background(), "qa_owner", "waiver.approve"))
	assert.False(t, p.AllowFunctionalRole(context.Background(), "developer", "waiver.approve"))
	assert.False(t, p.AllowFunctionalRole(context.Background(), "security_owner", "work_item.claim"))
}

func TestLoadPolicyFailsClosedOnDrift(t *testing.T) {
	_, err := LoadPolicy([]byte("version: \"2.0\"\ndefault_effect: deny\n"))
	assert.Error(t, err, "unsupported version must fail")

	_, err = LoadPolicy([]byte("version: \"3.0\"\ndefault_effect: allow\n"))
	assert.Error(t, err, "default allow must fail")

	_, err = LoadPolicy([]byte("version: \"3.0\"\ndefault_effect: deny\nroles: {}\n"))
	assert.Error(t, err, "roleless matrix must fail")

	overlap := []byte(`
version: "3.0"
default_effect: deny
roles:
  tester:
    allow: [action.one, action.both]
    deny: [action.both]
`)
	loaded, err := LoadPolicy(overlap)
	require.NoError(t, err)
	denied := loaded.Authorize(context.Background(), principalWith("p", "tester"), "action.both",
		model.Resource{Type: "thing", ProjectID: "p"})
	assert.False(t, denied.Allow, "deny must override allow for the same role")
	allowed := loaded.Authorize(context.Background(), principalWith("p", "tester"), "action.one",
		model.Resource{Type: "thing", ProjectID: "p"})
	assert.True(t, allowed.Allow)
}
