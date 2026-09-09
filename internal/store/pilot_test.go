package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pure M4-PILOT-001 semantics: the stage lifecycle matrix, the effect
// resolution consumers apply, and the deterministic gray bucketing. No
// database involved — the PG-gated behavior lives in
// postgres_pilot_test.go.

func TestPilotCanTransitionMatrix(t *testing.T) {
	cases := []struct {
		from, to string
		legal    bool
	}{
		// The lifecycle: off → shadow → gray → full, rollback terminal.
		{PilotStageOff, PilotStageShadow, true},
		{PilotStageShadow, PilotStageGray, true},
		{PilotStageGray, PilotStageFull, true},
		{PilotStageShadow, PilotStageRolledBack, true},
		{PilotStageGray, PilotStageRolledBack, true},
		{PilotStageFull, PilotStageRolledBack, true},
		// Shadow cannot be skipped and off has nothing to roll back.
		{PilotStageOff, PilotStageGray, false},
		{PilotStageOff, PilotStageFull, false},
		{PilotStageOff, PilotStageRolledBack, false},
		{PilotStageShadow, PilotStageFull, false},
		// No de-escalation back to earlier stages; rollback instead.
		{PilotStageShadow, PilotStageOff, false},
		{PilotStageGray, PilotStageShadow, false},
		{PilotStageFull, PilotStageGray, false},
		{PilotStageFull, PilotStageOff, false},
		// rolled_back is terminal.
		{PilotStageRolledBack, PilotStageShadow, false},
		{PilotStageRolledBack, PilotStageOff, false},
		{PilotStageRolledBack, PilotStageRolledBack, false},
		// Unknown stages never transition.
		{"paused", PilotStageShadow, false},
		{PilotStageShadow, "paused", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.legal, PilotCanTransition(tc.from, tc.to),
			"%s → %s", tc.from, tc.to)
	}
}

func TestPilotEffectOf(t *testing.T) {
	assert.Equal(t, PilotEffectOff, PilotEffectOf(PilotStageOff))
	assert.Equal(t, PilotEffectShadow, PilotEffectOf(PilotStageShadow))
	assert.Equal(t, PilotEffectGray, PilotEffectOf(PilotStageGray))
	assert.Equal(t, PilotEffectFull, PilotEffectOf(PilotStageFull))
	assert.Equal(t, PilotEffectOff, PilotEffectOf(PilotStageRolledBack),
		"the terminal rollback stage is inert")
	assert.Equal(t, PilotEffectOff, PilotEffectOf("paused"),
		"unknown stages fail inert")
}

func TestPilotGrayBucketing(t *testing.T) {
	// Determinism: the same subject never flips buckets.
	assert.Equal(t, PilotGrayBucket("agent_autofix", "repo-1"), PilotGrayBucket("agent_autofix", "repo-1"))
	assert.Less(t, PilotGrayBucket("agent_autofix", "repo-1"), 100)
	assert.GreaterOrEqual(t, PilotGrayBucket("agent_autofix", "repo-1"), 0)

	// Percent bounds: 0 disables everyone, 100 enables everyone,
	// out-of-range fails closed.
	assert.False(t, PilotGrayActive("f", "any", 0))
	assert.True(t, PilotGrayActive("f", "any", 100))
	assert.False(t, PilotGrayActive("f", "any", 101))
	assert.False(t, PilotGrayActive("f", "any", -1))

	// Flags bucket the same subjects independently: the migration
	// shadow face and the agent-remediation face must not share buckets.
	shared := 0
	for index := range 200 {
		subject := "subject-" + string(rune('a'+index%26)) + string(rune('0'+index%10))
		if PilotGrayActive("flag-a", subject, 50) == PilotGrayActive("flag-b", subject, 50) {
			shared++
		}
	}
	assert.Less(t, shared, 200, "two flags should not always agree")

	// Distribution sanity: 25% keeps roughly a quarter of subjects.
	enabled := 0
	for index := range 1000 {
		if PilotGrayActive("agent_autofix", "repo-"+string(rune('0'+index%10))+"-"+string(rune('a'+index%26)), 25) {
			enabled++
		}
	}
	assert.InDelta(t, 250, enabled, 60, "25%% gray should hold roughly a quarter (got %d)", enabled)
}

func TestPilotValidateDecisionFields(t *testing.T) {
	base := func() PilotDecision {
		return PilotDecision{Stage: PilotStageShadow, Actor: "po-1", Reason: "shadow phase begins now"}
	}
	current := PilotFlagState{Exists: false}

	check, err := PilotValidateDecision(current, base())
	require.NoError(t, err)
	assert.Equal(t, PilotDecisionApply, check)

	invalid := []func(PilotDecision) PilotDecision{
		func(d PilotDecision) PilotDecision { d.Stage = "paused"; return d },
		func(d PilotDecision) PilotDecision { d.Stage = ""; return d },
		func(d PilotDecision) PilotDecision { d.Actor = ""; return d },
		func(d PilotDecision) PilotDecision { d.Reason = "too short"; return d },
		func(d PilotDecision) PilotDecision { d.Reason = strings.Repeat("r", 2001); return d },
		func(d PilotDecision) PilotDecision { d.GrayPercent = 101; return d },
		func(d PilotDecision) PilotDecision { d.GrayPercent = -1; return d },
		// Percentage only applies to gray.
		func(d PilotDecision) PilotDecision { d.Stage = PilotStageShadow; d.GrayPercent = 25; return d },
	}
	for _, mutate := range invalid {
		check, err := PilotValidateDecision(current, mutate(base()))
		assert.Equal(t, PilotDecisionReject, check)
		assert.ErrorIs(t, err, ErrPilotDecisionInvalid)
	}
}

func TestPilotValidateDecisionLifecycle(t *testing.T) {
	decision := func(stage string, percent int) PilotDecision {
		return PilotDecision{Stage: stage, GrayPercent: percent, Actor: "po-1",
			Reason: "one recorded rollout decision"}
	}

	// Creation: register inert or start shadow — nothing else.
	for _, stage := range []string{PilotStageOff, PilotStageShadow} {
		check, err := PilotValidateDecision(PilotFlagState{}, decision(stage, 0))
		require.NoError(t, err, stage)
		assert.Equal(t, PilotDecisionApply, check, stage)
	}
	for _, stage := range []string{PilotStageGray, PilotStageFull, PilotStageRolledBack} {
		check, err := PilotValidateDecision(PilotFlagState{}, decision(stage, 0))
		assert.Equal(t, PilotDecisionReject, check, stage)
		assert.ErrorIs(t, err, ErrPilotTransitionInvalid, stage)
	}
	check, err := PilotValidateDecision(PilotFlagState{}, decision(PilotStageGray, 25))
	assert.Equal(t, PilotDecisionReject, check)
	assert.ErrorIs(t, err, ErrPilotTransitionInvalid, "gray with percent is still an illegal opening move")

	// Lifecycle walk: off → shadow → gray(25) → gray(50) → full →
	// rolled_back, then terminal.
	steps := []struct {
		current PilotFlagState
		target  PilotDecision
		expect  PilotDecisionCheck
	}{
		{PilotFlagState{Stage: PilotStageOff, Exists: true}, decision(PilotStageShadow, 0), PilotDecisionApply},
		{PilotFlagState{Stage: PilotStageShadow, Exists: true}, decision(PilotStageGray, 25), PilotDecisionApply},
		{PilotFlagState{Stage: PilotStageGray, GrayPercent: 25, Exists: true}, decision(PilotStageGray, 50), PilotDecisionApply},
		{PilotFlagState{Stage: PilotStageGray, GrayPercent: 50, Exists: true}, decision(PilotStageFull, 0), PilotDecisionApply},
		{PilotFlagState{Stage: PilotStageFull, Exists: true}, decision(PilotStageRolledBack, 0), PilotDecisionApply},
	}
	for _, step := range steps {
		check, err := PilotValidateDecision(step.current, step.target)
		require.NoError(t, err, "%s → %s", step.current.Stage, step.target.Stage)
		assert.Equal(t, step.expect, check, "%s → %s", step.current.Stage, step.target.Stage)
	}
	terminal := PilotFlagState{Stage: PilotStageRolledBack, Exists: true}
	for _, stage := range []string{PilotStageOff, PilotStageShadow, PilotStageGray, PilotStageFull, PilotStageRolledBack} {
		check, err := PilotValidateDecision(terminal, decision(stage, 0))
		if stage == PilotStageRolledBack {
			// Identical target on the terminal state is a no-op, not an
			// error: state stays, nothing is re-audited.
			assert.Equal(t, PilotDecisionNoop, check)
			assert.NoError(t, err)
			continue
		}
		assert.Equal(t, PilotDecisionReject, check, "rolled_back → %s", stage)
		assert.ErrorIs(t, err, ErrPilotTransitionInvalid, "rolled_back → %s", stage)
	}

	// Illegal jumps.
	jumps := []struct {
		current PilotFlagState
		target  PilotDecision
	}{
		{PilotFlagState{Stage: PilotStageOff, Exists: true}, decision(PilotStageGray, 25)},
		{PilotFlagState{Stage: PilotStageOff, Exists: true}, decision(PilotStageRolledBack, 0)},
		{PilotFlagState{Stage: PilotStageShadow, Exists: true}, decision(PilotStageFull, 0)},
		{PilotFlagState{Stage: PilotStageGray, GrayPercent: 25, Exists: true}, decision(PilotStageShadow, 0)},
		{PilotFlagState{Stage: PilotStageFull, Exists: true}, decision(PilotStageGray, 10)},
	}
	for _, jump := range jumps {
		check, err := PilotValidateDecision(jump.current, jump.target)
		assert.Equal(t, PilotDecisionReject, check, "%s → %s", jump.current.Stage, jump.target.Stage)
		assert.ErrorIs(t, err, ErrPilotTransitionInvalid, "%s → %s", jump.current.Stage, jump.target.Stage)
	}
}

func TestPilotValidateDecisionNoopReplays(t *testing.T) {
	decision := func(stage string, percent int) PilotDecision {
		return PilotDecision{Stage: stage, GrayPercent: percent, Actor: "po-1",
			Reason: "identical decision replayed"}
	}
	// Identical targets on every stage are idempotent no-ops — the
	// replay of a recorded decision never audits twice.
	states := []PilotFlagState{
		{Stage: PilotStageOff, Exists: true},
		{Stage: PilotStageShadow, Exists: true},
		{Stage: PilotStageGray, GrayPercent: 30, Exists: true},
		{Stage: PilotStageFull, Exists: true},
		{Stage: PilotStageRolledBack, Exists: true},
	}
	for _, state := range states {
		check, err := PilotValidateDecision(state, decision(state.Stage, state.GrayPercent))
		assert.Equal(t, PilotDecisionNoop, check, state.Stage)
		assert.NoError(t, err, state.Stage)
	}
}
