package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated M4-PILOT-001 storage: the guarded lifecycle walk against the
// real pilot_flags table, atomic decision auditing, and the effect
// lookup. The pure algebra is covered by pilot_test.go.

func TestPilotStoreLifecycle(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_pilot_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_pilot_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_pilot_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_pilot_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-000000000001', 'pilot')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7e00-0000-7000-8000-000000000001', 'pilot', 'PILOT', 'active')`, m3Project)
	require.NoError(t, err)

	pilot := pg.Pilot()
	decision := func(stage string, percent int, actor, reason string) PilotDecision {
		return PilotDecision{Stage: stage, GrayPercent: percent, Actor: actor, Reason: reason}
	}
	auditCount := func() int {
		var count int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_events WHERE project_id = $1 AND action = 'pilot.decision.recorded'`,
			m3Project).Scan(&count))
		return count
	}

	// Unknown flag reads hide behind the not-found sentinel and the
	// effect stays inert.
	_, err = pilot.Flag(ctx, m3Project, "agent_autofix")
	assert.ErrorIs(t, err, ErrPilotFlagNotFound)
	effect, err := pilot.Effect(ctx, m3Project, "agent_autofix")
	require.NoError(t, err)
	assert.Equal(t, PilotEffectOff, effect, "a flag without a row is inert")

	// The guarded walk: register → shadow → gray(25) → gray(50) → full
	// → rolled_back, each step audited in the same transaction.
	steps := []struct {
		decision PilotDecision
		created  bool
		stage    string
		percent  int
	}{
		{decision(PilotStageOff, 0, "po-1", "register agent autofix flag"), true, PilotStageOff, 0},
		{decision(PilotStageShadow, 0, "po-1", "shadow phase over two repos"), false, PilotStageShadow, 0},
		{decision(PilotStageGray, 25, "po-1", "grayscale at quarter exposure"), false, PilotStageGray, 25},
		{decision(PilotStageGray, 50, "po-1", "raise grayscale to half exposure"), false, PilotStageGray, 50},
		{decision(PilotStageFull, 0, "po-1", "grayscale converged to full"), false, PilotStageFull, 0},
		{decision(PilotStageRolledBack, 0, "po-1", "kill switch after budget overrun"), false, PilotStageRolledBack, 0},
	}
	for index, step := range steps {
		stored, created, err := pilot.PutFlag(ctx, m3Project, "agent_autofix", step.decision)
		require.NoError(t, err, "step %d (%s)", index, step.decision.Stage)
		assert.Equal(t, step.created, created, "step %d", index)
		assert.Equal(t, step.stage, stored.Stage, "step %d", index)
		assert.Equal(t, step.percent, stored.GrayPercent, "step %d", index)
		assert.Equal(t, "po-1", stored.ChangedBy, "step %d", index)
		assert.Equal(t, index+1, auditCount(), "step %d audits in lockstep", index)
	}

	// Effects follow the stored stage.
	effect, err = pilot.Effect(ctx, m3Project, "agent_autofix")
	require.NoError(t, err)
	assert.Equal(t, PilotEffectOff, effect, "rolled_back is inert")

	// The terminal state refuses every move.
	_, _, err = pilot.PutFlag(ctx, m3Project, "agent_autofix",
		decision(PilotStageShadow, 0, "po-1", "restart after rollback"))
	assert.ErrorIs(t, err, ErrPilotTransitionInvalid)
	assert.Equal(t, len(steps), auditCount(), "rejected decisions audit nothing")

	// Illegal jumps leave the row untouched.
	_, _, err = pilot.PutFlag(ctx, m3Project, "never_started",
		decision(PilotStageGray, 25, "po-1", "gray cannot open a lifecycle"))
	assert.ErrorIs(t, err, ErrPilotTransitionInvalid)
	var stageCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM pilot_flags WHERE project_id = $1 AND flag = 'never_started'`, m3Project).
		Scan(&stageCount))
	assert.Zero(t, stageCount, "the rejected opening move persists nothing")

	// Identical replays are no-ops: state kept, nothing re-audited.
	before := auditCount()
	stored, created, err := pilot.PutFlag(ctx, m3Project, "agent_autofix",
		decision(PilotStageRolledBack, 0, "po-1", "kill switch after budget overrun"))
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, PilotStageRolledBack, stored.Stage)
	assert.Equal(t, before, auditCount(), "no-op replays never audit")

	// Flag-name and cross-project isolation: another project's flag is
	// invisible here, and one project never sees another's rows.
	_, created, err = pilot.PutFlag(ctx, m3Project, "shadow_of_gitlab_mirror",
		decision(PilotStageShadow, 0, "po-2", "mirror pipeline shadowing"))
	require.NoError(t, err)
	assert.True(t, created)
	flags, err := pilot.ListFlags(ctx, m3Project)
	require.NoError(t, err)
	require.Len(t, flags, 2)
	assert.Equal(t, "agent_autofix", flags[0].Flag)
	assert.Equal(t, "shadow_of_gitlab_mirror", flags[1].Flag)
	assert.NotEqual(t, flags[0].Stage, flags[1].Stage)

	// Invalid fields never reach SQL.
	for name, bad := range map[string]PilotDecision{
		"short reason":     decision(PilotStageOff, 0, "po-1", "too short"),
		"percent off gray": decision(PilotStageShadow, 10, "po-1", "percent outside gray"),
		"bad stage":        decision("paused", 0, "po-1", "unknown stage rejected"),
		"no actor":         decision(PilotStageOff, 0, "", "actor comes from the server"),
	} {
		_, _, err := pilot.PutFlag(ctx, m3Project, "field_checks", bad)
		assert.ErrorIs(t, err, ErrPilotDecisionInvalid, name)
	}
	_, _, err = pilot.PutFlag(ctx, m3Project, strings.Repeat("f", 129),
		decision(PilotStageOff, 0, "po-1", "flag name over the table bound"))
	assert.ErrorIs(t, err, ErrPilotDecisionInvalid, "flag name bound mirrors the table CHECK")
}
