package identity

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// PG-gated: the store-backed resolver carries ACTIVE functional roles
// beside the project memberships, and a revocation or expiry propagates
// on the NEXT resolve — no cached authority (J1-2).

func newFunctionalResolverDB(t *testing.T) (*store.PostgresStore, *sql.DB) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	admin, err := store.OpenPostgres(context.Background(), dsn)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_identity_functional_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_identity_functional_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_identity_functional_test WITH (FORCE)`)
		_ = admin.Close()
	})
	db, err := store.OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_identity_functional_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(context.Background(),
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;`)
	require.NoError(t, err)
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	registry, err := store.NewPostgresStore(db)
	require.NoError(t, err)
	return registry, db
}

func TestStoreResolverFunctionalRolesAndRevocation(t *testing.T) {
	registry, db := newFunctionalResolverDB(t)
	ctx := context.Background()
	identities := registry.Identities()

	user, err := identities.GetOrCreateUser(ctx, "https://idp.example", "functional-resolver-subject", "Functional Resolver")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f3300-0000-7000-8000-000000000001', 'functional resolver team')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ('018f3300-0000-7000-8000-000000000002', '018f3300-0000-7000-8000-000000000001', 'func-proj', 'Functional Project', 'active')`)
	require.NoError(t, err)
	require.NoError(t, identities.CreateMembership(ctx, &model.TeamMembership{
		TeamID: "018f3300-0000-7000-8000-000000000001", UserID: user.ID, Role: "viewer",
	}))
	require.NoError(t, identities.GrantFunctionalRole(ctx, &store.FunctionalPrincipal{
		UserID: user.ID, Function: store.FunctionalRoleSecurityOwner, SourceRef: "deed/resolver-2026-09",
	}))

	resolver := NewStoreResolver(identities)

	// Both entry points resolve the functional authority beside the
	// project membership.
	principal, err := resolver.Resolve(ctx, "https://idp.example", "functional-resolver-subject")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"018f3300-0000-7000-8000-000000000002": "viewer"}, principal.ProjectMemberships)
	assert.Equal(t, []string{"security_owner"}, principal.FunctionalRoles)

	byID, err := resolver.ResolveByID(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"security_owner"}, byID.FunctionalRoles)

	// Revocation propagates on the very next resolve of either path.
	grants, err := identities.ListFunctionalPrincipals(ctx, store.FunctionalRoleSecurityOwner)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.NoError(t, identities.RevokeFunctionalRole(ctx, grants[0].ID))

	principal, err = resolver.Resolve(ctx, "https://idp.example", "functional-resolver-subject")
	require.NoError(t, err)
	assert.Empty(t, principal.FunctionalRoles, "revocation must drop the authority on the next resolve")
	assert.NotEmpty(t, principal.ProjectMemberships, "the project membership is untouched")

	byID, err = resolver.ResolveByID(ctx, user.ID)
	require.NoError(t, err)
	assert.Empty(t, byID.FunctionalRoles)
}
