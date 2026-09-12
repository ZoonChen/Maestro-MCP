package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated: the platform_grants lifecycle — fail-closed grant
// validation, the one-active-grant invariant, terminal revocation and
// the active-role view the resolver reads (J5-1).

// newPlatformStoreDB provisions the dedicated schema-migrated test
// database and hands back the identity store and the raw pool for
// out-of-band writes.
func newPlatformStoreDB(t *testing.T) (IdentityStore, *sql.DB) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_platform_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_platform_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_platform_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_platform_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(context.Background(),
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;`)
	require.NoError(t, err)
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	return registry.Identities(), db
}

func TestPlatformGrantValidationFailsClosed(t *testing.T) {
	identities, _ := newPlatformStoreDB(t)
	ctx := context.Background()

	user, err := identities.GetOrCreateUser(ctx, "https://idp.example", "platform-subject", "Platform User")
	require.NoError(t, err)

	// Unknown role: rejected before SQL, and the schema CHECK is the
	// second wall for out-of-band writers.
	err = identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: user.ID, Role: "superuser", SourceRef: "deed-1",
	})
	assert.ErrorIs(t, err, ErrPlatformGrantInvalid)

	// Unknown user.
	err = identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: "018f3100-0000-7000-8000-00000000dead", Role: PlatformRoleAdmin, SourceRef: "deed-1",
	})
	assert.ErrorIs(t, err, ErrPlatformGrantInvalid)

	// Missing authorization-source reference.
	err = identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: user.ID, Role: PlatformRoleAdmin,
	})
	assert.ErrorIs(t, err, ErrPlatformGrantInvalid)

	// Inverted validity window.
	past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	err = identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: user.ID, Role: PlatformRoleAdmin,
		ValidTo: &past, ValidFrom: future, SourceRef: "deed-1",
	})
	assert.ErrorIs(t, err, ErrPlatformGrantInvalid)
}

func TestPlatformGrantSchemaCheckFailsClosed(t *testing.T) {
	identities, db := newPlatformStoreDB(t)
	ctx := context.Background()

	user, err := identities.GetOrCreateUser(ctx, "https://idp.example", "platform-schema-subject", "Platform Schema")
	require.NoError(t, err)

	// A direct INSERT with an unknown role must hit the 0020 CHECK —
	// the second wall behind the Go validation for out-of-band writers.
	_, err = db.ExecContext(ctx, `
		INSERT INTO platform_grants (id, user_id, role, source_ref)
		VALUES ('018f3400-0000-7000-8000-000000000001', $1, 'superuser', 'deed-x')`,
		user.ID)
	require.Error(t, err, "the schema CHECK must refuse unknown platform roles")
	assert.Contains(t, err.Error(), "platform_grants_role_check")
}

func TestPlatformGrantLifecycle(t *testing.T) {
	identities, _ := newPlatformStoreDB(t)
	ctx := context.Background()

	admin, err := identities.GetOrCreateUser(ctx, "https://idp.example", "platform-lifecycle", "Platform Lifecycle")
	require.NoError(t, err)

	// Grant → active.
	require.NoError(t, identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: admin.ID, Role: PlatformRoleAdmin, SourceRef: "deed/platform-2026-09",
	}))
	roles, err := identities.ActivePlatformRoles(ctx, admin.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{PlatformRoleAdmin}, roles)

	// A second ACTIVE grant of the same role conflicts.
	err = identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: admin.ID, Role: PlatformRoleAdmin, SourceRef: "deed/duplicate",
	})
	assert.ErrorIs(t, err, ErrPlatformGrantConflict)

	// Revoke → the authority disappears from the active view, the row
	// stays in the administration view.
	grants, err := identities.ListPlatformGrants(ctx, PlatformRoleAdmin)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.NoError(t, identities.RevokePlatformRole(ctx, grants[0].ID))
	roles, err = identities.ActivePlatformRoles(ctx, admin.ID)
	require.NoError(t, err)
	assert.Empty(t, roles)

	// Revocation is terminal: revoking again answers not-found.
	err = identities.RevokePlatformRole(ctx, grants[0].ID)
	assert.ErrorIs(t, err, ErrPlatformGrantNotFound)
	err = identities.RevokePlatformRole(ctx, "018f3400-0000-7000-8000-00000000dead")
	assert.ErrorIs(t, err, ErrPlatformGrantNotFound)

	// After revocation a fresh grant of the same role is possible.
	require.NoError(t, identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: admin.ID, Role: PlatformRoleAdmin, SourceRef: "deed/platform-2026-10",
	}))
	roles, err = identities.ActivePlatformRoles(ctx, admin.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{PlatformRoleAdmin}, roles)
}

func TestPlatformGrantExpiry(t *testing.T) {
	identities, _ := newPlatformStoreDB(t)
	ctx := context.Background()

	holder, err := identities.GetOrCreateUser(ctx, "https://idp.example", "platform-expiry", "Platform Expiry")
	require.NoError(t, err)

	// An already-expired grant is storable (history) but never active.
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	require.NoError(t, identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: holder.ID, Role: PlatformRoleAdmin,
		ValidFrom: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		ValidTo:   &past, SourceRef: "deed/expired",
	}))
	roles, err := identities.ActivePlatformRoles(ctx, holder.ID)
	require.NoError(t, err)
	assert.Empty(t, roles, "an expired grant authorizes nothing")

	// A future grant is not yet active either. The expired row still
	// holds the (user, role) slot until revoked — the partial unique
	// index keys on revocation, not validity (same semantics as
	// functional grants), so renewal after expiry revokes first.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	far := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	err = identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: holder.ID, Role: PlatformRoleAdmin,
		ValidFrom: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		ValidTo:   strPtr(time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)), SourceRef: "deed/future",
	})
	assert.ErrorIs(t, err, ErrPlatformGrantConflict, "an expired-but-unrevoked row still holds the slot")

	grants, err := identities.ListPlatformGrants(ctx, PlatformRoleAdmin)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.NoError(t, identities.RevokePlatformRole(ctx, grants[0].ID))
	require.NoError(t, identities.GrantPlatformRole(ctx, &PlatformGrant{
		UserID: holder.ID, Role: PlatformRoleAdmin,
		ValidFrom: future, ValidTo: &far, SourceRef: "deed/future",
	}))
	roles, err = identities.ActivePlatformRoles(ctx, holder.ID)
	require.NoError(t, err)
	assert.Empty(t, roles, "a not-yet-valid grant authorizes nothing")
}

// strPtr is a small local helper for the RFC3339 window literals.
func strPtr(value string) *string { return &value }
