package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated agent-run persistence: find-or-resume on (project, defect,
// attempt), version-guarded settles, idempotent re-settle.

func newAgentFixture(t *testing.T) (*PostgresStore, string) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_agt_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_agt_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_agt_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_agt_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-000000000001', 'agt')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7e00-0000-7000-8000-000000000001', 'agt', 'AGT', 'active')`, m3Project)
	require.NoError(t, err)
	return pg, m3Project
}

func TestAgentRunPersistence(t *testing.T) {
	pg, projectID := newAgentFixture(t)
	ctx := context.Background()
	run := agent.RunContext{
		ProjectID: projectID,
		DefectID:  "018f7e00-0000-7000-8000-0000000000d1",
		RunID:     "018f7e00-0000-7000-8000-0000000000e1",
		Attempt:   1,
	}

	// Create lands at eligibility_check.
	state, created, err := pg.AgentRuns().CreateRun(ctx, run, 1)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, agent.StateEligibilityCheck, state)

	// A crashed creator's replay resumes the SAME row.
	replay := agent.RunContext{ProjectID: projectID,
		DefectID: run.DefectID, RunID: "018f7e00-0000-7000-8000-0000000000e2", Attempt: 1}
	state, created, err = pg.AgentRuns().CreateRun(ctx, replay, 1)
	require.NoError(t, err)
	assert.False(t, created, "one live run per defect+attempt")
	assert.Equal(t, agent.StateEligibilityCheck, state)

	// Settle forward under the version guard; re-settle same is
	// idempotent; settling from a stale state conflicts.
	require.NoError(t, pg.AgentRuns().Settle(ctx, run, agent.StateEligibilityCheck, agent.StateReproducing, ""))
	require.NoError(t, pg.AgentRuns().Settle(ctx, run, agent.StateReproducing, agent.StateDiagnosing, ""))
	require.NoError(t, pg.AgentRuns().Settle(ctx, run, agent.StateReproducing, agent.StateDiagnosing, ""))
	err = pg.AgentRuns().Settle(ctx, run, agent.StateReproducing, agent.StateNeedsHuman, agent.HandoffCannotReproduce)
	require.ErrorIs(t, err, ErrAgentRunConflict)

	// The handoff lands with its reason.
	require.NoError(t, pg.AgentRuns().Settle(ctx, run, agent.StateDiagnosing, agent.StateNeedsHuman, agent.HandoffCannotReproduce))
	var persisted, reason string
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT state, COALESCE(handoff_reason, '') FROM agent_runs WHERE id = $1`, run.RunID).
		Scan(&persisted, &reason))
	assert.Equal(t, string(agent.StateNeedsHuman), persisted)
	assert.Equal(t, string(agent.HandoffCannotReproduce), reason)

	// Unknown runs answer the miss sentinel.
	ghost := agent.RunContext{ProjectID: projectID, DefectID: run.DefectID, RunID: "00000000-0000-7000-8000-00000000dead"}
	_, _, found, err := pg.AgentRuns().LoadState(ctx, ghost)
	require.NoError(t, err)
	assert.False(t, found)
	require.ErrorIs(t, pg.AgentRuns().Settle(ctx, ghost, agent.StateDiagnosing, agent.StateModifying, ""), ErrAgentRunNotFound)
}
