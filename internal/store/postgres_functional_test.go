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

// PG-gated: the functional_principals lifecycle — fail-closed grant
// validation, the one-active-grant invariant, terminal revocation and
// the active-role view the resolver reads (J1-1/J1-3).

func newFunctionalStore(t *testing.T) IdentityStore {
	t.Helper()
	db := newFunctionalStoreDB(t)
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	return registry.Identities()
}

// newFunctionalStoreDB provisions the dedicated schema-migrated test
// database and hands back the raw pool for out-of-band writes.
func newFunctionalStoreDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_functional_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_functional_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_functional_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_functional_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.ExecContext(context.Background(),
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;`)
	require.NoError(t, err)
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	return db
}

func TestFunctionalGrantValidationFailsClosed(t *testing.T) {
	identities := newFunctionalStore(t)
	ctx := context.Background()

	user, err := identities.GetOrCreateUser(ctx, "https://idp.example", "functional-subject", "Functional User")
	require.NoError(t, err)

	// Unknown function: rejected before SQL, and the schema CHECK is
	// the second wall for out-of-band writers.
	err = identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: user.ID, Function: "chief_wizard", SourceRef: "deed-1",
	})
	assert.ErrorIs(t, err, ErrFunctionalGrantInvalid)

	// Unknown user.
	err = identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: "018f3100-0000-7000-8000-00000000dead", Function: FunctionalRoleSecurityOwner, SourceRef: "deed-1",
	})
	assert.ErrorIs(t, err, ErrFunctionalGrantInvalid)

	// Missing authorization-source reference.
	err = identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: user.ID, Function: FunctionalRoleSecurityOwner,
	})
	assert.ErrorIs(t, err, ErrFunctionalGrantInvalid)

	// Inverted validity window.
	past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	err = identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: user.ID, Function: FunctionalRoleSecurityOwner,
		ValidTo: &past, ValidFrom: future, SourceRef: "deed-1",
	})
	assert.ErrorIs(t, err, ErrFunctionalGrantInvalid)
}

func TestFunctionalGrantSchemaCheckFailsClosed(t *testing.T) {
	db := newFunctionalStoreDB(t)
	ctx := context.Background()
	registry, err := NewPostgresStore(db)
	require.NoError(t, err)
	user, err := registry.Identities().GetOrCreateUser(ctx, "https://idp.example", "schema-check-subject", "Schema Check")
	require.NoError(t, err)

	// A direct INSERT with an unknown function must hit the 0016 CHECK —
	// the second wall behind the Go validation for out-of-band writers.
	_, err = db.ExecContext(ctx, `
		INSERT INTO functional_principals (id, user_id, function, source_ref)
		VALUES ('018f3200-0000-7000-8000-000000000001', $1, 'chief_wizard', 'deed-x')`,
		user.ID)
	require.Error(t, err, "the schema CHECK must refuse unknown functions")
	assert.Contains(t, err.Error(), "functional_principals_function_check")
}

func TestFunctionalPrincipalLifecycle(t *testing.T) {
	identities := newFunctionalStore(t)
	ctx := context.Background()

	approver, err := identities.GetOrCreateUser(ctx, "https://idp.example", "lifecycle-approver", "Lifecycle Approver")
	require.NoError(t, err)

	// Grant → active.
	require.NoError(t, identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: approver.ID, Function: FunctionalRoleSecurityOwner, SourceRef: "deed/security-2026-09",
	}))
	roles, err := identities.ActiveFunctionalRoles(ctx, approver.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{FunctionalRoleSecurityOwner}, roles)

	// A second ACTIVE grant of the same function conflicts.
	err = identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: approver.ID, Function: FunctionalRoleSecurityOwner, SourceRef: "deed/duplicate",
	})
	assert.ErrorIs(t, err, ErrFunctionalGrantConflict)

	// A different function coexists.
	require.NoError(t, identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: approver.ID, Function: FunctionalRoleQAOwner, SourceRef: "deed/qa-2026-09",
	}))
	roles, err = identities.ActiveFunctionalRoles(ctx, approver.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{FunctionalRoleQAOwner, FunctionalRoleSecurityOwner}, roles)

	// Revoke → the authority disappears from the active view, the row
	// stays in the administration view.
	grants, err := identities.ListFunctionalPrincipals(ctx, FunctionalRoleSecurityOwner)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.NoError(t, identities.RevokeFunctionalRole(ctx, grants[0].ID))
	roles, err = identities.ActiveFunctionalRoles(ctx, approver.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{FunctionalRoleQAOwner}, roles)

	// Revocation is terminal: revoking again answers not-found.
	err = identities.RevokeFunctionalRole(ctx, grants[0].ID)
	assert.ErrorIs(t, err, ErrFunctionalGrantNotFound)
	err = identities.RevokeFunctionalRole(ctx, "018f3200-0000-7000-8000-00000000dead")
	assert.ErrorIs(t, err, ErrFunctionalGrantNotFound)

	// After revocation a fresh grant of the same function is possible.
	require.NoError(t, identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: approver.ID, Function: FunctionalRoleSecurityOwner, SourceRef: "deed/security-2026-10",
	}))
	roles, err = identities.ActiveFunctionalRoles(ctx, approver.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{FunctionalRoleQAOwner, FunctionalRoleSecurityOwner}, roles)
}

func TestFunctionalGrantExpiry(t *testing.T) {
	identities := newFunctionalStore(t)
	ctx := context.Background()

	qa, err := identities.GetOrCreateUser(ctx, "https://idp.example", "expiry-subject", "Expiry Subject")
	require.NoError(t, err)

	// An already-expired grant is storable (history) but never active.
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	require.NoError(t, identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: qa.ID, Function: FunctionalRoleQAOwner,
		ValidFrom: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		ValidTo:   &past, SourceRef: "deed/expired",
	}))
	roles, err := identities.ActiveFunctionalRoles(ctx, qa.ID)
	require.NoError(t, err)
	assert.Empty(t, roles, "an expired grant authorizes nothing")

	// A future grant is not yet active either.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	far := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	err = identities.GrantFunctionalRole(ctx, &FunctionalPrincipal{
		UserID: qa.ID, Function: FunctionalRoleOperationsOwner,
		ValidFrom: future, ValidTo: &far, SourceRef: "deed/future",
	})
	// (user, function) is free: the expired QA grant is a different
	// function, and operations_owner has no active row yet.
	require.NoError(t, err)
	roles, err = identities.ActiveFunctionalRoles(ctx, qa.ID)
	require.NoError(t, err)
	assert.Empty(t, roles, "a not-yet-valid grant authorizes nothing")
}
