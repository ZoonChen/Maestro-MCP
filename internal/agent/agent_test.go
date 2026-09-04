package agent

import (
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/budget"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The frozen machine, exhaustively: every PRD edge legal, every other
// edge refused, terminal states immutable.
func TestFrozenStateMachine(t *testing.T) {
	legal := [][2]State{
		{StateEligibilityCheck, StateReproducing},
		{StateEligibilityCheck, StateNeedsHuman},
		{StateReproducing, StateDiagnosing},
		{StateReproducing, StateNeedsHuman},
		{StateDiagnosing, StateModifying},
		{StateDiagnosing, StateNeedsHuman},
		{StateModifying, StateLocalVerifying},
		{StateModifying, StateNeedsHuman},
		{StateLocalVerifying, StateMRCreated},
		{StateLocalVerifying, StateRetrying},
		{StateLocalVerifying, StateNeedsHuman},
		{StateRetrying, StateReproducing},
		{StateMRCreated, StateCIVerifying},
		{StateMRCreated, StateNeedsHuman},
		{StateCIVerifying, StateAwaitingHuman},
		{StateCIVerifying, StateRetrying},
		{StateCIVerifying, StateNeedsHuman},
	}
	states := []State{StateEligibilityCheck, StateReproducing, StateDiagnosing,
		StateModifying, StateLocalVerifying, StateRetrying, StateMRCreated,
		StateCIVerifying, StateAwaitingHuman, StateNeedsHuman, StateStopped}
	legalSet := map[State]map[State]bool{}
	for _, edge := range legal {
		if legalSet[edge[0]] == nil {
			legalSet[edge[0]] = map[State]bool{}
		}
		legalSet[edge[0]][edge[1]] = true
	}
	for _, from := range states {
		for _, to := range states {
			edge := CanTransition(from, to)
			assert.Equal(t, legalSet[from][to] || (to == StateStopped && !Terminal(from)), edge,
				"%s -> %s", from, to)
		}
	}
	for _, state := range []State{StateAwaitingHuman, StateNeedsHuman, StateStopped} {
		assert.True(t, Terminal(state))
		assert.False(t, CanTransition(state, StateReproducing), "%s is immutable", state)
	}
}

type scripted struct {
	eligible    bool
	eligibleWhy string
	reproOK     bool
	verifyPass  bool
	verifyRetry bool
	ciPass      bool
	ciRetry     bool
}

func ports(s scripted) Ports {
	return Ports{
		Eligible:  func(RunContext) (bool, string, error) { return s.eligible, s.eligibleWhy, nil },
		Reproduce: func(RunContext) (bool, string, error) { return s.reproOK, "sig", nil },
		Diagnose:  func(RunContext, string) (string, error) { return "d", nil },
		Modify:    func(RunContext, string) (string, error) { return "diff-1", nil },
		VerifyLocally: func(RunContext, string) (bool, bool, error) {
			return s.verifyPass, s.verifyRetry, nil
		},
		CreateMR: func(RunContext, string) (string, error) { return "mr-1", nil },
		CheckCI:  func(RunContext, string) (bool, bool, error) { return s.ciPass, s.ciRetry, nil },
	}
}

func newOrch(s scripted, limits budget.Limits) *Orchestrator {
	ledger, err := budget.New("ag-l", limits)
	if err != nil {
		panic(err)
	}
	start := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	return &Orchestrator{
		Ports: ports(s), Ledger: ledger, MaxAttempts: limits.MaxAttempts,
		StartedAt: start, Now: func() time.Time { return start },
	}
}

var rc = RunContext{ProjectID: "p", DefectID: "d", RunID: "r", Attempt: 1}

func TestHappyPathParksForHumanReview(t *testing.T) {
	o := newOrch(scripted{eligible: true, reproOK: true, verifyPass: true, ciPass: true},
		budget.Limits{BudgetUnits: 1000, MaxAttempts: 3, WallTimeLimit: time.Hour})

	out, err := o.EligibilityGate(rc)
	require.NoError(t, err)
	assert.Equal(t, StateReproducing, out.To)

	out, err = o.ReproduceStep(rc)
	require.NoError(t, err)
	assert.Equal(t, StateDiagnosing, out.To)

	out, err = o.DiagnoseStep(rc, "sig")
	require.NoError(t, err)
	assert.Equal(t, StateModifying, out.To)

	out, err = o.ModifyStep(rc, "d")
	require.NoError(t, err)
	assert.Equal(t, StateLocalVerifying, out.To)

	out, err = o.LocalVerifyStep(rc, "diff-1")
	require.NoError(t, err)
	assert.Equal(t, StateMRCreated, out.To)

	// Green CI parks for HUMAN review — the agent never merges.
	out, err = o.CIVerifyStep(rc, "mr-1")
	require.NoError(t, err)
	assert.Equal(t, StateAwaitingHuman, out.To)
	assert.Equal(t, HandoffHumanReview, out.Reason)
	assert.True(t, out.Terminal)
}

func TestHandoffsAreHonest(t *testing.T) {
	t.Run("ineligible", func(t *testing.T) {
		o := newOrch(scripted{eligible: false, eligibleWhy: string(HandoffPolicyStop)},
			budget.Limits{BudgetUnits: 10, MaxAttempts: 3, WallTimeLimit: time.Hour})
		out, err := o.EligibilityGate(rc)
		require.NoError(t, err)
		assert.Equal(t, StateNeedsHuman, out.To)
		assert.Equal(t, HandoffPolicyStop, out.Reason)
	})

	t.Run("cannot reproduce", func(t *testing.T) {
		o := newOrch(scripted{eligible: true, reproOK: false},
			budget.Limits{BudgetUnits: 10, MaxAttempts: 3, WallTimeLimit: time.Hour})
		out, err := o.ReproduceStep(rc)
		require.NoError(t, err)
		assert.Equal(t, StateNeedsHuman, out.To)
		assert.Equal(t, HandoffCannotReproduce, out.Reason, "no guessed fixes")
	})

	t.Run("unretriable local failure is low confidence", func(t *testing.T) {
		o := newOrch(scripted{eligible: true, reproOK: true, verifyPass: false, verifyRetry: false},
			budget.Limits{BudgetUnits: 10, MaxAttempts: 3, WallTimeLimit: time.Hour})
		out, err := o.LocalVerifyStep(rc, "diff-1")
		require.NoError(t, err)
		assert.Equal(t, StateNeedsHuman, out.To)
		assert.Equal(t, HandoffLowConfidence, out.Reason)
	})

	t.Run("budget exhaustion ends the retry loop", func(t *testing.T) {
		o := newOrch(scripted{eligible: true, reproOK: true, verifyPass: false, verifyRetry: true},
			budget.Limits{BudgetUnits: 10, MaxAttempts: 3, WallTimeLimit: time.Hour})
		// Drain the budget: reserve the whole ceiling.
		res, err := o.Ledger.ReserveCall(10, "model", time.Now())
		require.NoError(t, err)
		_ = o.Ledger.RecordUsageMissing(res, time.Now())
		spent := o.Ledger.Snapshot().SpentUnits
		assert.Equal(t, int64(10), spent)

		out, err := o.LocalVerifyStep(rc, "diff-1")
		require.NoError(t, err)
		assert.Equal(t, StateNeedsHuman, out.To)
		assert.Equal(t, HandoffBudgetExhausted, out.Reason)
	})

	t.Run("retriable failure with budget loops to retrying", func(t *testing.T) {
		o := newOrch(scripted{eligible: true, reproOK: true, verifyPass: false, verifyRetry: true},
			budget.Limits{BudgetUnits: 1000, MaxAttempts: 3, WallTimeLimit: time.Hour})
		out, err := o.LocalVerifyStep(rc, "diff-1")
		require.NoError(t, err)
		assert.Equal(t, StateRetrying, out.To)
		assert.False(t, out.Terminal)
	})

	t.Run("attempt ceiling ends the loop", func(t *testing.T) {
		o := newOrch(scripted{verifyRetry: true},
			budget.Limits{BudgetUnits: 1000, MaxAttempts: 2, WallTimeLimit: time.Hour})
		last := RunContext{ProjectID: "p", DefectID: "d", RunID: "r", Attempt: 2}
		out, err := o.LocalVerifyStep(last, "diff-1")
		require.NoError(t, err)
		assert.Equal(t, StateNeedsHuman, out.To)
		assert.Equal(t, HandoffBudgetExhausted, out.Reason)
	})
}

func TestStopAndIllegalMoves(t *testing.T) {
	o := newOrch(scripted{}, budget.Limits{BudgetUnits: 10, MaxAttempts: 3, WallTimeLimit: time.Hour})

	out, err := o.Stop(rc, StateDiagnosing)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, out.To)
	assert.Equal(t, HandoffPolicyStop, out.Reason)

	// Terminal states refuse everything.
	_, err = o.Stop(rc, StateStopped)
	require.ErrorIs(t, err, ErrIllegalTransition)

	// Jumping states the machine does not carry.
	_, err = o.to(rc, StateReproducing, StateMRCreated, "")
	require.ErrorIs(t, err, ErrIllegalTransition)

	// awaiting_human only with its review reason.
	_, err = o.to(rc, StateCIVerifying, StateAwaitingHuman, HandoffLowConfidence)
	require.ErrorIs(t, err, ErrIllegalTransition)
}
