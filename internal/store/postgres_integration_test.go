package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/integration"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated IntegrationRun lifecycle: exact-combination replay, terminal
// immutability, idempotent re-settle.

func newIntegrationFixture(t *testing.T) (*PostgresStore, string) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_int_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_int_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_int_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_int_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-000000000001', 'int')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7e00-0000-7000-8000-000000000001', 'int', 'INT', 'active')`, m3Project)
	require.NoError(t, err)
	return pg, m3Project
}

func intManifest() integration.Manifest {
	return integration.Manifest{
		Revisions: []integration.RepositoryRevision{
			{RepositoryMappingID: "m-web", SHA: strings.Repeat("a", 40)},
			{RepositoryMappingID: "m-api", SHA: strings.Repeat("b", 40)},
		},
		ContractHash:       "sha256:" + strings.Repeat("c", 64),
		SuiteVersion:       "suite-2",
		EnvironmentProfile: "staging-1",
		FixtureVersion:     "fx-3",
		PolicyVersion:      "pol-1",
		TTL:                time.Hour,
	}
}

func manifestHash(t *testing.T, manifest integration.Manifest) string {
	t.Helper()
	digest, err := manifest.CombinationDigest()
	require.NoError(t, err)
	return digest
}

func TestIntegrationRunLifecycle(t *testing.T) {
	pg, projectID := newIntegrationFixture(t)
	ctx := context.Background()
	manifest := intManifest()
	hash := manifestHash(t, manifest)

	// Create + start.
	runID, state, created, err := pg.IntegrationRuns().StartRun(ctx, projectID, manifest, hash)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, "running", state)

	// The EXACT same combination replays onto the same run.
	replayID, _, created, err := pg.IntegrationRuns().StartRun(ctx, projectID, manifest, hash)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, runID, replayID, "the digest is the identity")

	// A different combination is a new run.
	variant := intManifest()
	variant.Revisions[0].SHA = strings.Repeat("d", 40)
	variantHash := manifestHash(t, variant)
	otherID, _, created, err := pg.IntegrationRuns().StartRun(ctx, projectID, variant, variantHash)
	require.NoError(t, err)
	require.True(t, created)
	assert.NotEqual(t, runID, otherID)

	// Terminal settle; the same verdict re-settles idempotently; a
	// DIFFERENT verdict on a settled run refuses.
	require.NoError(t, pg.IntegrationRuns().SettleRun(ctx, projectID, runID, "passed", "complete"))
	require.NoError(t, pg.IntegrationRuns().SettleRun(ctx, projectID, runID, "passed", "complete"))
	require.ErrorIs(t, pg.IntegrationRuns().SettleRun(ctx, projectID, runID, "failed", "complete"), ErrRunStateConflict)

	// The passed verdict reads back bound to the exact digest.
	state, _, digest, found, err := pg.IntegrationRuns().Run(ctx, projectID, runID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "passed", state)
	assert.Equal(t, hash, digest, "passed is only valid against its exact combination")

	// Non-terminal settles are refused outright.
	require.Error(t, pg.IntegrationRuns().SettleRun(ctx, projectID, otherID, "running", "executing"))

	// Unknown runs answer the miss sentinel.
	_, _, _, found, err = pg.IntegrationRuns().Run(ctx, projectID, "00000000-0000-7000-8000-00000000dead")
	require.NoError(t, err)
	assert.False(t, found)
}
