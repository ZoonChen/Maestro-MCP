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
// empty principal that authorizes nothing.
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
	return &model.PrincipalContext{
		PrincipalID:        user.ID,
		Type:               model.PrincipalTypeHuman,
		ProjectMemberships: scoped,
	}, nil
}
