package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// Identity-bound call scope (W5-1, W6-4). Two MCP transports exist:
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
// Scope selection (W6-4, S2C-A4): a principal with exactly ONE active
// membership needs no parameter (the pre-W6 behavior, unchanged). A
// principal with SEVERAL memberships must name one through the tool
// call's `project` argument — the selection is constrained to the
// caller's own memberships (the payload selects among pre-authorized
// scopes, it never grants one), and a missing or foreign selection is
// rejected as INVALID_PARAMETER with an explicit message. Zero
// memberships stays fail-closed.

// identityScopeKey carries the guard-resolved project scope into the
// tool handler context, so authorization and handler share one scope.
type identityScopeKey struct{}

// errIdentityScopeRequired reports a scope that could not be resolved
// (missing selection on multi-membership, a foreign selection, or no
// membership at all). The guard maps it to INVALID_PARAMETER.
var errIdentityScopeRequired = errors.New(
	"identity-bound MCP calls require an explicit project scope: pass the 'project' argument naming one of the caller's active project memberships")

// identityPrincipal returns the request principal when the call rides
// an identity-bound transport, nil for the delegated context.
func identityPrincipal(ctx context.Context) *model.PrincipalContext {
	return identity.RequestPrincipalFrom(ctx)
}

// resolveIdentityScope picks the call's project scope for an
// identity-bound transport: the explicit `project` argument when the
// principal holds several memberships (the argument must name one of
// them), otherwise the sole membership. Empty string with nil error
// means the transport is delegated (fall back to the TransportBinding).
func resolveIdentityScope(ctx context.Context, requested string) (string, error) {
	principal := identityPrincipal(ctx)
	if principal == nil {
		return "", nil
	}
	switch len(principal.ProjectMemberships) {
	case 1:
		sole := ""
		for projectID := range principal.ProjectMemberships {
			sole = projectID
		}
		if requested != "" && requested != sole {
			return "", fmt.Errorf("%w; the caller's only membership is a different project", errIdentityScopeRequired)
		}
		return sole, nil
	case 0:
		return "", fmt.Errorf("%w; the caller holds no active project membership", errIdentityScopeRequired)
	default:
		if requested == "" {
			return "", fmt.Errorf("%w; the caller holds %d active project memberships",
				errIdentityScopeRequired, len(principal.ProjectMemberships))
		}
		if _, ok := principal.ProjectMemberships[requested]; !ok {
			return "", fmt.Errorf("%w; the requested project is not among the caller's %d memberships",
				errIdentityScopeRequired, len(principal.ProjectMemberships))
		}
		return requested, nil
	}
}

// identityProject resolves the project scope for an identity-bound
// call; empty string with nil error means the transport is delegated
// (fall back to the TransportBinding). The guard-stashed scope (when
// the call carried an explicit selection) wins.
func identityProject(ctx context.Context) (string, error) {
	if scoped, ok := ctx.Value(identityScopeKey{}).(string); ok && scoped != "" {
		return scoped, nil
	}
	principal := identityPrincipal(ctx)
	if principal == nil {
		return "", nil
	}
	if len(principal.ProjectMemberships) == 1 {
		for projectID := range principal.ProjectMemberships {
			return projectID, nil
		}
	}
	return "", errIdentityScopeRequired
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
// identity principal's resolved scope on identity-bound transports,
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
