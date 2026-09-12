package identity

import (
	"context"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// StoreResolver adapts the PostgreSQL identity store to the
// PrincipalResolver contract: a VERIFIED (issuer, subject) pair is mapped
// to the server-side PrincipalContext with project memberships derived
// from the registry — never from request data.
type StoreResolver struct {
	identities store.IdentityStore
}

// NewStoreResolver binds the resolver to an identity store.
func NewStoreResolver(identities store.IdentityStore) *StoreResolver {
	return &StoreResolver{identities: identities}
}

// Resolve maps the verified identity to its principal. Unknown users are
// lazily registered (first login) — GetOrCreateUser is idempotent per
// issuer+subject — and a user with no active memberships resolves to an
// empty principal that authorizes nothing. Functional roles resolve
// beside the memberships (J1): only the frozen functional permissions
// ride them, never project permissions. Platform grants resolve the
// same way (J5): only the frozen platform-role permissions, no
// membership requirement.
func (r *StoreResolver) Resolve(ctx context.Context, issuer, subject string) (*model.PrincipalContext, error) {
	user, err := r.identities.GetOrCreateUser(ctx, issuer, subject, "")
	if err != nil {
		return nil, fmt.Errorf("identity: resolve user: %w", err)
	}
	memberships, err := r.identities.ListProjectMemberships(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("identity: derive memberships: %w", err)
	}
	scoped := make(map[string]string, len(memberships))
	for _, membership := range memberships {
		scoped[membership.ProjectID] = membership.Role
	}
	functional, err := r.identities.ActiveFunctionalRoles(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("identity: derive functional roles: %w", err)
	}
	platform, err := r.identities.ActivePlatformRoles(ctx, user.ID)
	if err != nil {
		return nil, fmt.Errorf("identity: derive platform roles: %w", err)
	}
	return &model.PrincipalContext{
		PrincipalID:        user.ID,
		Type:               model.PrincipalTypeHuman,
		ProjectMemberships: scoped,
		FunctionalRoles:    functional,
		PlatformRoles:      platform,
	}, nil
}

// ResolveByID rebuilds the principal for an established session user.
// A suspended/removed user or a store failure resolves to an error —
// never to a stale principal: the cookie path fails closed. Functional
// and platform grants re-resolve per request, so expiry and revocation
// propagate on the next call.
func (r *StoreResolver) ResolveByID(ctx context.Context, userID string) (*model.PrincipalContext, error) {
	user, err := r.identities.GetUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("identity: resolve session user: %w", err)
	}
	if user.Status != "active" {
		return nil, fmt.Errorf("identity: session user is %q", user.Status)
	}
	memberships, err := r.identities.ListProjectMemberships(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("identity: derive memberships: %w", err)
	}
	scoped := make(map[string]string, len(memberships))
	for _, membership := range memberships {
		scoped[membership.ProjectID] = membership.Role
	}
	functional, err := r.identities.ActiveFunctionalRoles(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("identity: derive functional roles: %w", err)
	}
	platform, err := r.identities.ActivePlatformRoles(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("identity: derive platform roles: %w", err)
	}
	return &model.PrincipalContext{
		PrincipalID:        user.ID,
		Type:               model.PrincipalTypeHuman,
		ProjectMemberships: scoped,
		FunctionalRoles:    functional,
		PlatformRoles:      platform,
	}, nil
}
