package identity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// J5: the merged decision point over project-role grants, frozen
// functional grants and frozen platform grants. Every assertion reads
// the FROZEN permissions.yaml embedded in the binary — the platform
// allow list is the matrix's platform_admin role block, never an
// invented grant.

// platformPrincipal builds a principal holding ACTIVE platform grants
// beside the optional single-membership map (nil = no membership at
// all — the CR-P5a-1 reality: platform_admin can never be a
// membership role).
func platformPrincipal(roles ...string) *model.PrincipalContext {
	return &model.PrincipalContext{
		PrincipalID:   "user-platform",
		Type:          model.PrincipalTypeHuman,
		PlatformRoles: roles,
	}
}

func TestAuthorizePlatformGrants(t *testing.T) {
	p := policy(t)
	ctx := context.Background()
	project := "project-a"

	t.Run("platform grant reaches pilot.write without any membership", func(t *testing.T) {
		decision := p.Authorize(ctx, platformPrincipal("platform_admin"),
			"pilot.write", model.Resource{Type: "project", ProjectID: project})
		assert.True(t, decision.Allow, decision.Reasons)
		assert.Equal(t, "3.0", decision.PolicyVersion)
		assert.Equal(t, "platform:platform_admin", Authority(decision))
	})

	t.Run("platform grant reaches the scopeless platform resources", func(t *testing.T) {
		for _, action := range []string{
			"gitlab_instance.configure", "platform.configure", "oidc.configure",
			"company_policy.manage", "security.emergency_stop", "audit.export",
			"pilot.read", "pilot.write",
		} {
			decision := p.Authorize(ctx, platformPrincipal("platform_admin"),
				action, model.Resource{Type: "platform"})
			assert.True(t, decision.Allow, "%s: %v", action, decision.Reasons)
		}
	})

	t.Run("platform grants do not stack project permissions", func(t *testing.T) {
		// platform_admin grants only the frozen platform allow list: no
		// project read/update, no work-item actions — those stay with
		// project roles (and the frozen deny list keeps
		// project.code.read_by_role_only out).
		for _, action := range []string{"project.read", "project.update", "work_item.create", "waiver.approve"} {
			decision := p.Authorize(ctx, platformPrincipal("platform_admin"),
				action, model.Resource{Type: "project", ProjectID: project})
			assert.False(t, decision.Allow, action)
		}
	})

	t.Run("no membership hides the project once the platform grant does not cover the action", func(t *testing.T) {
		// Outside the platform allow list, a membership-free principal
		// gets the resource-hiding deny (transport maps to 404, never
		// 403) — the frozen semantics are unchanged by the platform
		// path.
		decision := p.Authorize(ctx, platformPrincipal("platform_admin"),
			"project.read", model.Resource{Type: "project", ProjectID: project})
		assert.False(t, decision.Allow)
		assert.Contains(t, decision.Reasons[0], "no membership")
	})

	t.Run("project roles never reach the platform strings", func(t *testing.T) {
		for _, role := range []string{"project_admin", "coordinator", "developer", "verifier", "viewer"} {
			decision := p.Authorize(ctx, principalWith(project, role),
				"pilot.write", model.Resource{Type: "project", ProjectID: project})
			assert.False(t, decision.Allow, role)
		}
	})

	t.Run("an unknown platform role denies without granting", func(t *testing.T) {
		// Membership-free the deny collapses to the resource-hiding
		// reason (the transport needs the "no membership" prefix);
		// with a membership the diagnostic surfaces: the unknown
		// platform grant added nothing.
		principal := principalWith(project, "viewer")
		principal.PlatformRoles = []string{"superuser"}
		decision := p.Authorize(ctx, principal,
			"pilot.write", model.Resource{Type: "project", ProjectID: project})
		assert.False(t, decision.Allow)
		assert.Contains(t, decision.Reasons[0], "unknown platform role")

		membershipFree := p.Authorize(ctx, platformPrincipal("superuser"),
			"pilot.write", model.Resource{Type: "project", ProjectID: project})
		assert.False(t, membershipFree.Allow)
		assert.Contains(t, membershipFree.Reasons[0], "no membership")
	})

	t.Run("delegated principals keep the frozen veto through the platform path", func(t *testing.T) {
		agent := platformPrincipal("platform_admin")
		agent.DelegationID = "delegation-9"
		// pilot.write is not on the frozen delegation veto list — the
		// intersection semantics decide it; a vetoed action through the
		// platform class is still refused.
		vetoed := p.Authorize(ctx, agent, "waiver.approve",
			model.Resource{Type: "waiver", ProjectID: project})
		assert.False(t, vetoed.Allow)
		assert.Contains(t, vetoed.Reasons[0], "delegated principals")
	})

	t.Run("platform authority coexists with a project membership", func(t *testing.T) {
		// A platform admin who is ALSO a project member keeps both
		// classes: platform for pilot.write, the membership for project
		// actions.
		principal := principalWith(project, "viewer")
		principal.PlatformRoles = []string{"platform_admin"}
		writeDecision := p.Authorize(ctx, principal, "pilot.write",
			model.Resource{Type: "project", ProjectID: project})
		assert.True(t, writeDecision.Allow, writeDecision.Reasons)
		readDecision := p.Authorize(ctx, principal, "project.read",
			model.Resource{Type: "project", ProjectID: project})
		assert.True(t, readDecision.Allow, readDecision.Reasons)
		assert.Equal(t, "project:viewer", Authority(readDecision))
	})
}

func TestAuthorityDistinguishesPlatformClass(t *testing.T) {
	p := policy(t)
	ctx := context.Background()

	platform := p.Authorize(ctx, platformPrincipal("platform_admin"),
		"pilot.write", model.Resource{Type: "project", ProjectID: "project-a"})
	assert.Equal(t, "platform:platform_admin", Authority(platform))
}

func TestStaticResolverCarriesPlatformRoles(t *testing.T) {
	resolver := &StaticResolver{
		Memberships: map[string]map[string]string{
			"plat-1": {"project-a": "viewer"},
		},
		Platform: map[string][]string{
			"plat-1": {"platform_admin"},
		},
	}
	principal, err := resolver.Resolve(context.Background(), "https://idp.example", "plat-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"platform_admin"}, principal.PlatformRoles)

	// A resolver without the platform map resolves principals with no
	// platform authority — the pre-J5 behavior is unchanged.
	bare := &StaticResolver{Memberships: map[string]map[string]string{
		"plat-1": {"project-a": "viewer"},
	}}
	principal, err = bare.Resolve(context.Background(), "https://idp.example", "plat-1")
	require.NoError(t, err)
	assert.Empty(t, principal.PlatformRoles)
}
