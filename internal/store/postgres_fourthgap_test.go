package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated fourth sweep: the cheap, deterministic branch families the
// earlier sweeps left — instance/mapping validation, runner status
// transitions and enrollment lifecycles, identity upserts and
// memberships, work-item claim guards, and event optionality edge
// shapes.

func newFourthGapFixture(t *testing.T) (*PostgresStore, string) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_fourthgap_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_fourthgap_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_fourthgap_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_fourthgap_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7d00-0000-7000-8000-000000000001', 'fourth')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ('018f7d00-0000-7000-8000-000000000002', '018f7d00-0000-7000-8000-000000000001', 'fourth', 'Fourth', 'active')`)
	require.NoError(t, err)
	return pg, "018f7d00-0000-7000-8000-000000000002"
}

func TestInstanceAndMappingValidationBranches(t *testing.T) {
	pg, projectID := newFourthGapFixture(t)
	ctx := context.Background()

	// Blank secret refs are rejected.
	_, err := pg.Instances().CreateInstance(ctx, "https://gitlab.fourth.example", "n", "", "env:B")
	require.Error(t, err)

	// The mapping guards: malformed branch, numeric floor, unknown or
	// non-active instance.
	valid, err := pg.Instances().CreateInstance(ctx, "https://gitlab.fourth.example", "fourth", "env:A", "env:B")
	require.NoError(t, err)

	_, err = pg.Instances().PutMapping(ctx, projectID, valid.ID, 9001, "bad branch~x", 0)
	require.Error(t, err)
	_, err = pg.Instances().PutMapping(ctx, projectID, valid.ID, 0, "main", 0)
	require.Error(t, err)
	_, err = pg.Instances().PutMapping(ctx, projectID, "018f7d00-0000-7000-8000-00000000dead", 9001, "main", 0)
	require.Error(t, err, "unknown instances never bind")

	// A suspended instance stops binding new mappings.
	_, err = pg.DB().ExecContext(ctx, `UPDATE gitlab_instances SET status='suspended' WHERE id = $1`, valid.ID)
	require.NoError(t, err)
	_, err = pg.Instances().PutMapping(ctx, projectID, valid.ID, 9001, "main", 0)
	require.Error(t, err)
	_, err = pg.DB().ExecContext(ctx, `UPDATE gitlab_instances SET status='active' WHERE id = $1`, valid.ID)
	require.NoError(t, err)

	// Create then replace through the CAS; wrong versions conflict.
	created, err := pg.Instances().PutMapping(ctx, projectID, valid.ID, 9001, "main", 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Version)
	replaced, err := pg.Instances().PutMapping(ctx, projectID, valid.ID, 9002, "release", created.Version)
	require.NoError(t, err)
	assert.Equal(t, int64(2), replaced.Version)
	_, err = pg.Instances().PutMapping(ctx, projectID, valid.ID, 9003, "main", 99)
	assert.ErrorIs(t, err, ErrMappingConflict)

	// Duplicate instance registration conflicts.
	_, err = pg.Instances().CreateInstance(ctx, "https://gitlab.fourth.example", "dupe", "env:A", "env:B")
	assert.ErrorIs(t, err, ErrInstanceExists)

	listing, err := pg.Instances().ListInstances(ctx)
	require.NoError(t, err)
	require.Len(t, listing, 1)
}

func TestRunnerStatusAndEnrollmentBranches(t *testing.T) {
	pg, projectID := newFourthGapFixture(t)
	ctx := context.Background()

	device := &model.RunnerDevice{DisplayName: "fg", DeviceKeyHash: "sha256:fg",
		Status: model.RunnerStatusPendingApproval, Capabilities: []byte(`["a","b","c"]`)}
	require.NoError(t, pg.RunnerRegistry().CreateRunner(ctx, device, &model.RunnerBinding{ProjectID: projectID}))
	listed, err := pg.RunnerRegistry().ListRunnersByProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	id := listed[0].ID

	// Revoked is not a status-transition target.
	require.ErrorIs(t, pg.RunnerRegistry().UpdateRunnerStatus(ctx, id,
		model.RunnerStatusPendingApproval, model.RunnerStatusRevoked), ErrRunnerStatusInvalid)
	// A wrong expected status is rejected with the mismatch sentinel.
	require.ErrorIs(t, pg.RunnerRegistry().UpdateRunnerStatus(ctx, id,
		model.RunnerStatusApproved, model.RunnerStatusOnline), ErrRunnerStatusInvalid)
	// The legal transition lands; generation bumps are monotonic.
	require.NoError(t, pg.RunnerRegistry().UpdateRunnerStatus(ctx, id,
		model.RunnerStatusPendingApproval, model.RunnerStatusApproved))
	first, err := pg.RunnerRegistry().BumpRunnerGeneration(ctx, id)
	require.NoError(t, err)
	second, err := pg.RunnerRegistry().BumpRunnerGeneration(ctx, id)
	require.NoError(t, err)
	assert.Greater(t, second, first)
	require.NoError(t, pg.RunnerRegistry().UpdateRunnerHeartbeat(ctx, id))
	// Unknown devices fail generation bumps and heartbeats.
	_, err = pg.RunnerRegistry().BumpRunnerGeneration(ctx, "018f7d00-0000-7000-8000-00000000dead")
	require.ErrorIs(t, err, ErrRunnerNotFound)
	require.ErrorIs(t, pg.RunnerRegistry().UpdateRunnerHeartbeat(ctx, "018f7d00-0000-7000-8000-00000000dead"), ErrRunnerNotFound)

	// Enrollment lifecycle: create, resolve by hash, consume twice.
	enrollment := &model.RunnerEnrollment{
		ID: "018f7d00-0000-7000-8000-0000000000e1", ProjectID: projectID,
		CodeHash:  "sha256:fg-code",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		CreatedBy: firstUser(t, pg),
	}
	require.NoError(t, pg.RunnerRegistry().CreateEnrollment(ctx, enrollment))
	found, projectOf, err := pg.RunnerRegistry().EnrollmentByCodeHash(ctx, "sha256:fg-code")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, projectID, projectOf)
	_, _, err = pg.RunnerRegistry().EnrollmentByCodeHash(ctx, "sha256:absent")
	require.ErrorIs(t, err, ErrEnrollmentInvalid)
	require.NoError(t, pg.RunnerRegistry().ConsumeEnrollment(ctx, found.ID, "sha256:fg-code"))
	require.ErrorIs(t, pg.RunnerRegistry().ConsumeEnrollment(ctx, found.ID, "sha256:fg-code"), ErrEnrollmentConsumed)

	// A second device lands in the revoked terminal state and refuses
	// further transitions.
	second2 := &model.RunnerDevice{DisplayName: "fg2", DeviceKeyHash: "sha256:fg2",
		Status: model.RunnerStatusPendingApproval, Capabilities: []byte(`["a","b","c"]`)}
	require.NoError(t, pg.RunnerRegistry().CreateRunner(ctx, second2, &model.RunnerBinding{ProjectID: projectID}))
	listed, err = pg.RunnerRegistry().ListRunnersByProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.NoError(t, pg.RunnerRegistry().RevokeRunner(ctx, listed[1].ID))
	require.ErrorIs(t, pg.RunnerRegistry().UpdateRunnerStatus(ctx, listed[1].ID,
		model.RunnerStatusRevoked, model.RunnerStatusApproved), ErrRunnerStatusInvalid)
}

func firstUser(t *testing.T, pg *PostgresStore) string {
	t.Helper()
	user, err := pg.Identities().GetOrCreateUser(context.Background(), "https://idp.fourth", "enroller", "Enroller")
	require.NoError(t, err)
	return user.ID
}

func TestIdentityUpsertAndMembershipBranches(t *testing.T) {
	pg, _ := newFourthGapFixture(t)
	ctx := context.Background()

	first, err := pg.Identities().GetOrCreateUser(ctx, "https://idp.fourth", "sub-1", "First")
	require.NoError(t, err)
	// Re-upsert updates the display name, same identity.
	again, err := pg.Identities().GetOrCreateUser(ctx, "https://idp.fourth", "sub-1", "Renamed")
	require.NoError(t, err)
	assert.Equal(t, first.ID, again.ID)
	assert.Equal(t, "Renamed", again.DisplayName)

	teamID := "018f7d00-0000-7000-8000-000000000001"
	require.NoError(t, pg.Identities().CreateMembership(ctx, &model.TeamMembership{
		TeamID: teamID, UserID: first.ID, Role: "project_admin",
	}))
	var memberCount int
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM memberships WHERE user_id = $1`, first.ID).Scan(&memberCount))
	require.Equal(t, 1, memberCount, "the membership row landed")
	// The DB clock is the comparison domain: client-clock skew can make
	// a whole-nanosecond client "now" land before valid_from.
	members, err := pg.Identities().ListMembershipsByUser(ctx, first.ID, "")
	require.NoError(t, err)
	assert.Len(t, members, 1)
	projects, err := pg.Identities().ListProjectMemberships(ctx, first.ID)
	require.NoError(t, err)
	require.Len(t, projects, 1)

	// User status transitions guard the same way.
	require.NoError(t, pg.Identities().UpdateUserStatus(ctx, first.ID, "active", "suspended"))
	require.Error(t, pg.Identities().UpdateUserStatus(ctx, first.ID, "active", "suspended"))
}

func TestWorkItemAndEventEdgeBranches(t *testing.T) {
	pg, projectID := newFourthGapFixture(t)
	ctx := context.Background()
	workID := "018f7d00-0000-7000-8000-000000000003"
	_, err := pg.DB().ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, version)
		VALUES ($1, $2, 'fourth task', 'queued', 1)`, workID, projectID)
	require.NoError(t, err)

	// The claim leases the item to one runner; a competing claim finds
	// the queue empty while the lease holds.
	device := &model.RunnerDevice{DisplayName: "wi", DeviceKeyHash: "sha256:wi",
		Status: model.RunnerStatusApproved, Capabilities: []byte(`["a","b","c"]`)}
	require.NoError(t, pg.RunnerRegistry().CreateRunner(ctx, device, &model.RunnerBinding{ProjectID: projectID}))
	runners, err := pg.RunnerRegistry().ListRunnersByProject(ctx, projectID)
	require.NoError(t, err)
	require.NotEmpty(t, runners)
	claimed, err := pg.ClaimNextWorkItem(ctx, runners[0].ID, "gen-1", 0, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	newVersion, err := pg.RunnerLeaseHeartbeat(ctx, claimed.LeaseID, runners[0].ID, "gen-1", claimed.LeaseVersion, time.Minute)
	require.NoError(t, err)
	assert.Greater(t, newVersion, claimed.LeaseVersion)
	_, err = pg.RunnerLeaseHeartbeat(ctx, claimed.LeaseID, runners[0].ID, "gen-1", claimed.LeaseVersion, time.Minute)
	require.ErrorIs(t, err, ErrLeaseVersionMismatch)
}
