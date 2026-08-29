package identity

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// PG-gated: the store-backed resolver maps verified identities to real
// registry principals with derived memberships.
func TestStoreResolverDerivesMemberships(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	db, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(context.Background(),
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;`)
	require.NoError(t, err)
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	registry, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	user, err := registry.Identities().GetOrCreateUser(ctx, "https://idp.example", "resolver-subject", "Resolver User")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f3000-0000-7000-8000-000000000001', 'resolver team')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ('018f3000-0000-7000-8000-000000000002', '018f3000-0000-7000-8000-000000000001', 'resolver-proj', 'Resolver Project', 'active')`)
	require.NoError(t, err)
	require.NoError(t, registry.Identities().CreateMembership(ctx, &model.TeamMembership{
		TeamID: "018f3000-0000-7000-8000-000000000001", UserID: user.ID, Role: "developer",
	}))

	resolver := NewStoreResolver(registry.Identities())
	principal, err := resolver.Resolve(ctx, "https://idp.example", "resolver-subject")
	require.NoError(t, err)
	assert.Equal(t, user.ID, principal.PrincipalID)
	assert.Equal(t, model.PrincipalTypeHuman, principal.Type)
	assert.Equal(t, map[string]string{
		"018f3000-0000-7000-8000-000000000002": "developer",
	}, principal.ProjectMemberships)

	// A memberless identity resolves to an empty principal that
	// authorizes nothing (the frozen policy denies every scope).
	_, err = registry.Identities().GetOrCreateUser(ctx, "https://idp.example", "lonely-subject", "Lonely")
	require.NoError(t, err)
	stranger, err := resolver.Resolve(ctx, "https://idp.example", "lonely-subject")
	require.NoError(t, err)
	assert.Empty(t, stranger.ProjectMemberships)
}
