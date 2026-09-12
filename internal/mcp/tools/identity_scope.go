package tools

import (
	"context"
	"errors"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// Identity-bound call scope (W5-1). Two MCP transports exist:
//
//   - the local stdio Runner's DELEGATED context: the host-injected
//     single-project TransportBinding is the authorization (Guard nil),
//     and the audit actor is the bound session;
//   - the server's Streamable HTTP transport: the engine-wide identity
//     layer has already resolved the caller's principal (memberships +
//     functional + platform roles) — the helpers below resolve the
//     project scope and the audit actor from THAT server-side
//     principal, never from any tool payload.
//
// The project rule for identity-bound calls is deliberately narrow:
// exactly ONE active project membership. A principal with zero or
// several memberships fails closed — the MCP protocol carries no
// project selector, and guessing one would silently retarget writes.

var errIdentityScopeAmbiguous = errors.New(
	"identity-bound MCP calls require exactly one active project membership; the MCP protocol carries no project selector")

// identityPrincipal returns the request principal when the call rides
// an identity-bound transport, nil for the delegated context.
func identityPrincipal(ctx context.Context) *model.PrincipalContext {
	return identity.RequestPrincipalFrom(ctx)
}

// identityProject resolves the single-membership project scope for an
// identity-bound call; empty string with nil error means the transport
// is delegated (fall back to the TransportBinding).
func identityProject(ctx context.Context) (string, error) {
	principal := identityPrincipal(ctx)
	if principal == nil {
		return "", nil
	}
	if len(principal.ProjectMemberships) != 1 {
		return "", errIdentityScopeAmbiguous
	}
	for projectID := range principal.ProjectMemberships {
		return projectID, nil
	}
	return "", errIdentityScopeAmbiguous
}

// identityActor derives the audit actor for identity-bound writes: the
// authenticated user principal, distinct from every delegated session
// actor (the separation-of-duties comparisons stay sound).
func identityActor(ctx context.Context) (string, error) {
	principal := identityPrincipal(ctx)
	if principal == nil {
		return "", nil
	}
	return "user:" + principal.PrincipalID, nil
}

// identityFunctionalRoles returns the caller's active functional roles
// for asset signoffs (W5-2); nil for delegated sessions, which never
// carry functional authority.
func identityFunctionalRoles(ctx context.Context) []string {
	principal := identityPrincipal(ctx)
	if principal == nil || len(principal.FunctionalRoles) == 0 {
		return nil
	}
	roles := make([]string, len(principal.FunctionalRoles))
	copy(roles, principal.FunctionalRoles)
	return roles
}

// callProject resolves the project scope for one tool call: the
// identity principal's single membership on identity-bound transports,
// otherwise the delegated TransportBinding.
func (s *Services) callProject(ctx context.Context) (string, error) {
	if projectID, err := identityProject(ctx); err != nil {
		return "", err
	} else if projectID != "" {
		return projectID, nil
	}
	projectID, _, _, err := s.Binding.scope()
	return projectID, err
}

// callActor resolves the audit actor for one tool call: the
// authenticated user principal on identity-bound transports, otherwise
// the delegated session actor.
func (s *Services) callActor(ctx context.Context) (string, error) {
	if actor, err := identityActor(ctx); err != nil {
		return "", err
	} else if actor != "" {
		return actor, nil
	}
	return s.sessionActor()
}
