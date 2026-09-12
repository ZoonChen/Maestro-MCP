package identity

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// PG-gated: the store-backed resolver carries ACTIVE platform roles
// beside the project memberships, and a revocation or expiry
// propagates on the NEXT resolve — no cached authority (J5-2).

func newPlatformResolverDB(t *testing.T) *store.PostgresStore {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	admin, err := store.OpenPostgres(context.Background(), dsn)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_identity_platform_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_identity_platform_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_identity_platform_test WITH (FORCE)`)
		_ = admin.Close()
	})
	db, err := store.OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_identity_platform_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(context.Background(),
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;`)
	require.NoError(t, err)
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	registry, err := store.NewPostgresStore(db)
	require.NoError(t, err)
	return registry
}

func TestStoreResolverPlatformRolesAndRevocation(t *testing.T) {
	registry := newPlatformResolverDB(t)
	ctx := context.Background()
	identities := registry.Identities()

	// The platform admin deliberately holds NO membership: the frozen
	// memberships CHECK would refuse platform_admin, and the platform
	// path must not need one (CR-P5a-1).
	user, err := identities.GetOrCreateUser(ctx, "https://idp.example", "platform-resolver-subject", "Platform Resolver")
	require.NoError(t, err)
	require.NoError(t, identities.GrantPlatformRole(ctx, &store.PlatformGrant{
		UserID: user.ID, Role: store.PlatformRoleAdmin, SourceRef: "deed/platform-resolver-2026-09",
	}))

	resolver := NewStoreResolver(identities)

	// Both entry points resolve the platform authority beside an EMPTY
	// membership map.
	principal, err := resolver.Resolve(ctx, "https://idp.example", "platform-resolver-subject")
	require.NoError(t, err)
	assert.Empty(t, principal.ProjectMemberships)
	assert.Equal(t, []string{store.PlatformRoleAdmin}, principal.PlatformRoles)

	byID, err := resolver.ResolveByID(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{store.PlatformRoleAdmin}, byID.PlatformRoles)

	// The resolved principal authorizes the platform string with no
	// membership — the frozen matrix decides.
	policy, err := EmbeddedPolicy()
	require.NoError(t, err)
	decision := policy.Authorize(ctx, principal, "pilot.write",
		model.Resource{Type: "project", ProjectID: "018f3300-0000-7000-8000-000000000002"})
	assert.True(t, decision.Allow, decision.Reasons)
	assert.Equal(t, "platform:platform_admin", Authority(decision))

	// Revocation propagates on the very next resolve of either path.
	grants, err := identities.ListPlatformGrants(ctx, store.PlatformRoleAdmin)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.NoError(t, identities.RevokePlatformRole(ctx, grants[0].ID))

	principal, err = resolver.Resolve(ctx, "https://idp.example", "platform-resolver-subject")
	require.NoError(t, err)
	assert.Empty(t, principal.PlatformRoles, "revocation must drop the authority on the next resolve")
	decision = policy.Authorize(ctx, principal, "pilot.write",
		model.Resource{Type: "project", ProjectID: "018f3300-0000-7000-8000-000000000002"})
	assert.False(t, decision.Allow, "the revoked grant authorizes nothing")

	byID, err = resolver.ResolveByID(ctx, user.ID)
	require.NoError(t, err)
	assert.Empty(t, byID.PlatformRoles)
}
