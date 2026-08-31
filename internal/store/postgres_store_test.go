package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPostgresStore gates on the dedicated service DSN wired by the
// m1-runtime workflow; each test starts from a freshly migrated schema.
func testPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	db, err := OpenPostgres(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	_, err = db.ExecContext(ctx,
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;`)
	require.NoError(t, err)
	_, err = ApplyPostgresMigrations(ctx, db)
	require.NoError(t, err)

	store, err := NewPostgresStore(db)
	require.NoError(t, err)
	return store
}

// seedIdentityFixture inserts a user, team and project directly so FK-bound
// store paths have minimal referential data.
func seedIdentityFixture(t *testing.T, store *PostgresStore) (userID, teamID, projectID string) {
	t.Helper()
	ctx := context.Background()
	user, err := store.Identities().GetOrCreateUser(ctx,
		"https://idp.example", "subject-1", "Fixture User")
	require.NoError(t, err)

	teamID = pgNewUUID()
	projectID = pgNewUUID()
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO teams (id, name) VALUES ($1, 'fixture-team')`, teamID)
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO projects (id, team_id, key, name, status)
		VALUES ($1, $2, 'fixture-project', 'Fixture Project', 'active')`, projectID, teamID)
	require.NoError(t, err)
	return user.ID, teamID, projectID
}

func TestPGIdentityStoreUserLifecycle(t *testing.T) {
	store := testPostgresStore(t)
	ctx := context.Background()
	identities := store.Identities()

	first, err := identities.GetOrCreateUser(ctx, "https://idp.example", "subject-a", "First Name")
	require.NoError(t, err)
	assert.Equal(t, "active", first.Status)

	// Same identity maps to the same row idempotently; the display name
	// refreshes.
	again, err := identities.GetOrCreateUser(ctx, "https://idp.example", "subject-a", "Renamed")
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, "Renamed", again.DisplayName)

	fetched, err := identities.GetUser(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", fetched.DisplayName)

	_, err = identities.GetUser(ctx, pgNewUUID())
	assert.ErrorIs(t, err, ErrUserNotFound)

	require.NoError(t, identities.UpdateUserStatus(ctx, first.ID, "active", "suspended"))
	require.ErrorIs(t, identities.UpdateUserStatus(ctx, first.ID, "active", "removed"),
		ErrUserNotFound, "CAS on a stale status must fail")
}

func TestPGIdentityStoreMembershipDerivation(t *testing.T) {
	store := testPostgresStore(t)
	ctx := context.Background()
	userID, teamID, projectID := seedIdentityFixture(t, store)
	identities := store.Identities()

	require.NoError(t, identities.CreateMembership(ctx, testMembership(teamID, userID, "developer")))
	// Re-creating the same membership is a no-op conflict sentinel.
	require.ErrorIs(t, identities.CreateMembership(ctx, testMembership(teamID, userID, "verifier")),
		ErrMembershipNotFound)

	memberships, err := identities.ListMembershipsByUser(ctx, userID, "")
	require.NoError(t, err)
	require.Len(t, memberships, 1)
	assert.Equal(t, "developer", memberships[0].Role)

	views, err := identities.ListProjectMemberships(ctx, userID)
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, projectID, views[0].ProjectID)
	assert.Equal(t, "developer", views[0].Role)
}

func TestPGRunnerRegistryEnrollmentSingleUse(t *testing.T) {
	store := testPostgresStore(t)
	ctx := context.Background()
	userID, _, projectID := seedIdentityFixture(t, store)
	registry := store.RunnerRegistry()

	enrollment := testEnrollment(projectID, userID, time.Now().UTC().Add(10*time.Minute))
	require.NoError(t, registry.CreateEnrollment(ctx, enrollment))

	// Wrong code hash never consumes the enrollment.
	require.ErrorIs(t, registry.ConsumeEnrollment(ctx, enrollment.ID, "sha256:wrong"), ErrEnrollmentInvalid)
	require.NoError(t, registry.ConsumeEnrollment(ctx, enrollment.ID, enrollment.CodeHash))
	require.ErrorIs(t, registry.ConsumeEnrollment(ctx, enrollment.ID, enrollment.CodeHash),
		ErrEnrollmentConsumed, "a one-time code must burn on first use")

	expired := testEnrollment(projectID, userID, time.Now().UTC().Add(-time.Minute))
	require.NoError(t, registry.CreateEnrollment(ctx, expired))
	require.ErrorIs(t, registry.ConsumeEnrollment(ctx, expired.ID, expired.CodeHash), ErrEnrollmentExpired)

	require.ErrorIs(t, registry.ConsumeEnrollment(ctx, pgNewUUID(), "sha256:x"), ErrEnrollmentInvalid)
}

func TestPGRunnerRegistryLifecycleAndFencing(t *testing.T) {
	store := testPostgresStore(t)
	ctx := context.Background()
	_, _, projectID := seedIdentityFixture(t, store)
	registry := store.RunnerRegistry()

	runner := testRunner("fixture-runner", "sha256:keyhash")
	binding := &runnerBindingFixture{ProjectID: projectID}
	require.NoError(t, registry.CreateRunner(ctx, runner, bindingFixture(binding)))
	fetched, err := registry.GetRunner(ctx, runner.ID)
	require.NoError(t, err)
	assert.Equal(t, "pending_approval", fetched.Status)
	assert.Equal(t, int64(1), fetched.Generation)

	require.NoError(t, registry.UpdateRunnerStatus(ctx, runner.ID, "pending_approval", "approved"))
	require.ErrorIs(t, registry.UpdateRunnerStatus(ctx, runner.ID, "pending_approval", "approved"),
		ErrRunnerStatusInvalid, "stale status CAS must fail")

	require.NoError(t, registry.UpdateRunnerHeartbeat(ctx, runner.ID))

	generation, err := registry.BumpRunnerGeneration(ctx, runner.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), generation)

	// Revocation is terminal, idempotent, and blocks generation fencing.
	require.NoError(t, registry.RevokeRunner(ctx, runner.ID))
	require.NoError(t, registry.RevokeRunner(ctx, runner.ID))
	_, err = registry.BumpRunnerGeneration(ctx, runner.ID)
	require.ErrorIs(t, err, ErrRunnerRevoked)
	require.ErrorIs(t, registry.UpdateRunnerHeartbeat(ctx, runner.ID), ErrRunnerStatusInvalid)
	require.ErrorIs(t, registry.UpdateRunnerStatus(ctx, runner.ID, "revoked", "approved"), ErrRunnerStatusInvalid)

	// A second device stays listable per project binding.
	other := testRunner("fixture-runner-2", "sha256:keyhash2")
	require.NoError(t, registry.CreateRunner(ctx, other, bindingFixture(binding)))
	runners, err := registry.ListRunnersByProject(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, runners, 2)
}

func TestPGOutboxClaimExclusivityAndLifecycle(t *testing.T) {
	store := testPostgresStore(t)
	ctx := context.Background()
	outbox := store.Outbox()

	events := make([]*model.OutboxEvent, 4)
	for i := range events {
		events[i] = testOutboxEvent(fmt.Sprintf("evt-%d", i))
		require.NoError(t, outbox.Enqueue(ctx, events[i]))
	}
	// The same durable event identity can never enqueue twice, even with a
	// different payload.
	dup := *events[0]
	dup.Payload = []byte(`{"other":true}`)
	require.ErrorIs(t, outbox.Enqueue(ctx, &dup), ErrDuplicateEvent)

	claimedA, err := outbox.ClaimPending(ctx, 10, "dispatcher-a", "")
	require.NoError(t, err)
	require.Len(t, claimedA, 4, "one dispatcher claims the whole batch")

	claimedB, err := outbox.ClaimPending(ctx, 10, "dispatcher-b", "")
	require.NoError(t, err)
	assert.Empty(t, claimedB, "SKIP LOCKED keeps competing dispatchers disjoint")

	for _, event := range claimedA {
		require.ErrorIs(t, outbox.MarkDelivered(ctx, event.EventID, "dispatcher-b"), ErrOutboxClaimMismatch)
	}
	for _, event := range claimedA[:3] {
		require.NoError(t, outbox.MarkDelivered(ctx, event.EventID, "dispatcher-a"))
	}

	// The fourth event retries into the future and is unclaimable until its
	// backoff window elapses (simulated by backdating available_at).
	stale := claimedA[3]
	require.NoError(t, outbox.MarkRetry(ctx, stale.EventID, "dispatcher-a", 1,
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339)))
	retriedNow, err := outbox.ClaimPending(ctx, 10, "dispatcher-b", "")
	require.NoError(t, err)
	assert.Empty(t, retriedNow, "future available_at must block claiming")

	_, err = store.db.ExecContext(ctx, `
		UPDATE outbox_events SET available_at = now() - interval '1 minute'
		WHERE event_id = $1`, stale.EventID)
	require.NoError(t, err)
	retriedLater, err := outbox.ClaimPending(ctx, 10, "dispatcher-b", "")
	require.NoError(t, err)
	require.Len(t, retriedLater, 1)
	require.NoError(t, outbox.MarkDeadLetter(ctx, retriedLater[0].EventID, "dispatcher-b"))

	var deliveredCount, deadLetterCount int
	require.NoError(t, store.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE status = 'delivered'),
		       count(*) FILTER (WHERE status = 'dead_letter')
		FROM outbox_events`).Scan(&deliveredCount, &deadLetterCount))
	assert.Equal(t, 3, deliveredCount)
	assert.Equal(t, 1, deadLetterCount)
}

func TestPGInboxExactlyOnce(t *testing.T) {
	store := testPostgresStore(t)
	ctx := context.Background()
	inbox := store.Inbox()

	event := testInboxEvent("inbox-1")
	created, err := inbox.Record(ctx, event)
	require.NoError(t, err)
	assert.True(t, created)

	created, err = inbox.Record(ctx, event)
	require.NoError(t, err)
	assert.False(t, created, "duplicate event identity records once")

	require.NoError(t, inbox.ClaimProcessing(ctx, event.EventID))
	require.ErrorIs(t, inbox.ClaimProcessing(ctx, event.EventID), ErrDuplicateEvent)
	require.NoError(t, inbox.MarkProcessed(ctx, event.EventID))
	require.ErrorIs(t, inbox.MarkProcessed(ctx, event.EventID), ErrDuplicateEvent)
}

func TestPGIdempotencyReplayVsConflict(t *testing.T) {
	store := testPostgresStore(t)
	ctx := context.Background()
	idem := store.APIIdempotency()

	record := &IdempotencyRecord{
		PrincipalID:    "principal-1",
		ProjectID:      "project-1",
		Operation:      "work_item.create",
		Key:            "key-1234567890abcdef",
		RequestHash:    "sha256:aaa",
		ResponseStatus: 202,
	}
	replayed, existing, err := idem.LookupOrCreate(ctx, record)
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.Nil(t, existing)

	replayed, existing, err = idem.LookupOrCreate(ctx, record)
	require.NoError(t, err)
	assert.True(t, replayed)
	require.NotNil(t, existing)
	assert.Equal(t, 202, existing.ResponseStatus)

	conflict := *record
	conflict.RequestHash = "sha256:bbb"
	_, _, err = idem.LookupOrCreate(ctx, &conflict)
	assert.ErrorIs(t, err, ErrIdempotencyConflict)
}

func TestPGTxStoresRollbackAndCommit(t *testing.T) {
	store := testPostgresStore(t)
	ctx := context.Background()

	rolled := testOutboxEvent("tx-rollback")
	tx, err := store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.Outbox().Enqueue(ctx, rolled))
	require.NoError(t, tx.Rollback())
	var count int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox_events`).Scan(&count))
	assert.Equal(t, 0, count, "rolled-back enqueue must not persist")

	committed := testOutboxEvent("tx-commit")
	tx, err = store.BeginTx(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.Outbox().Enqueue(ctx, committed))
	require.NoError(t, tx.Commit())
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox_events`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestPGMigrationTamperDetectionAndRevertChain(t *testing.T) {
	db := testPostgresDB(t)
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS maestro_meta CASCADE;`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP DATABASE IF EXISTS maestro_store_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE DATABASE maestro_store_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DROP DATABASE IF EXISTS maestro_store_test WITH (FORCE)`)
	})

	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	dsn = dsn[:strings.LastIndex(dsn, "/")+1] + "maestro_store_test"
	isolated, err := OpenPostgres(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { isolated.Close() })

	// A fresh database applies every embedded migration.
	applied, err := ApplyPostgresMigrations(ctx, isolated)
	require.NoError(t, err)
	migrations, err := ParsePostgresMigrations()
	require.NoError(t, err)
	require.Equal(t, len(migrations), applied)

	// Digest tampering on ANY row fails closed on apply AND validate.
	_, err = isolated.ExecContext(ctx, `UPDATE maestro_meta.schema_migrations SET digest = 'sha256:deadbeef'`)
	require.NoError(t, err)
	require.ErrorIs(t, ValidatePostgresSchema(ctx, isolated), ErrPostgresMigrationIntegrity)
	_, err = ApplyPostgresMigrations(ctx, isolated)
	require.ErrorIs(t, err, ErrPostgresMigrationIntegrity)

	// Restoring digests then reverting everything leaves zero objects.
	for _, migration := range migrations {
		_, err = isolated.ExecContext(ctx,
			`UPDATE maestro_meta.schema_migrations SET digest = $1 WHERE version = $2`,
			migration.Digest, migration.Version)
		require.NoError(t, err)
	}
	reverted, err := RevertPostgresMigrations(ctx, isolated, len(migrations))
	require.NoError(t, err)
	require.Equal(t, len(migrations), reverted)
	var objects int
	require.NoError(t, isolated.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&objects))
	require.Zero(t, objects)
}
