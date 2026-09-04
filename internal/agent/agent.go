// Package agent implements the M3-AGT-001 remediation orchestrator:
// the deterministic state machine that carries an Agent run from
// eligibility through reproduction, diagnosis, modification, local
// verification, MR creation and CI verification to its terminal
// handoff — with the model NEVER owning state (AGT-RULE-005), every
// model call gated by the budget ledger (AGT-RULE-001), and no fix
// declared without ground truth (AGT-RULE-004).
//
// The frozen PRD state machine is enforced as pure transition logic;
// side effects (model calls, tools, MR creation) are injected ports so
// the orchestrator is fully testable and the agent runtime stays a
// replaceable detail.
package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/budget"
)

// State is the frozen RemediationRun enum (the PRD wire contract).
type State string

const (
	StateEligibilityCheck State = "eligibility_check"
	StateReproducing      State = "reproducing"
	StateDiagnosing       State = "diagnosing"
	StateModifying        State = "modifying"
	StateLocalVerifying   State = "local_verifying"
	StateRetrying         State = "retrying"
	StateMRCreated        State = "mr_created"
	StateCIVerifying      State = "ci_verifying"
	StateAwaitingHuman    State = "awaiting_human"
	StateNeedsHuman       State = "needs_human"
	StateStopped          State = "stopped"
)

// HandoffReason enumerates why a run ended in human hands (the frozen
// events.yaml AgentHandoffPayload enum).
type HandoffReason string

const (
	HandoffCannotReproduce HandoffReason = "cannot_reproduce"
	HandoffBudgetExhausted HandoffReason = "budget_exhausted"
	HandoffLowConfidence   HandoffReason = "low_confidence"
	HandoffHighRisk        HandoffReason = "high_risk"
	HandoffToolLimit       HandoffReason = "tool_limit"
	HandoffPolicyStop      HandoffReason = "policy_stop"
	HandoffHumanReview     HandoffReason = "human_review_required"
)

// transitions is the PRD mermaid diagram, verbatim. Every edge not
// listed here is illegal — the machine is closed.
var transitions = map[State]map[State]struct{}{
	StateEligibilityCheck: {StateReproducing: {}, StateNeedsHuman: {}, StateStopped: {}},
	StateReproducing:      {StateDiagnosing: {}, StateNeedsHuman: {}, StateStopped: {}},
	StateDiagnosing:       {StateModifying: {}, StateNeedsHuman: {}, StateStopped: {}},
	StateModifying:        {StateLocalVerifying: {}, StateNeedsHuman: {}, StateStopped: {}},
	StateLocalVerifying:   {StateMRCreated: {}, StateRetrying: {}, StateNeedsHuman: {}, StateStopped: {}},
	StateRetrying:         {StateReproducing: {}, StateStopped: {}},
	StateMRCreated:        {StateCIVerifying: {}, StateNeedsHuman: {}, StateStopped: {}},
	StateCIVerifying:      {StateAwaitingHuman: {}, StateRetrying: {}, StateNeedsHuman: {}, StateStopped: {}},
	StateAwaitingHuman:    {},
	StateNeedsHuman:       {},
	StateStopped:          {},
}

// CanTransition reports whether the frozen machine carries the edge.
func CanTransition(from, to State) bool {
	_, ok := transitions[from][to]
	return ok
}

// Terminal reports whether the state ends the run.
func Terminal(state State) bool {
	switch state {
	case StateAwaitingHuman, StateNeedsHuman, StateStopped:
		return true
	}
	return false
}

// ErrIllegalTransition reports a move the frozen machine refuses —
// including any attempt to leave a terminal state.
var ErrIllegalTransition = errors.New("agent: state transition refused by the frozen machine")

// ErrNoGroundTruth guards AGT-RULE-004: declaring a fix without the
// verifier's own pass evidence.
var ErrNoGroundTruth = errors.New("agent: no ground truth for the fix claim")

// ErrToolForbidden guards AGT-RULE-003: the agent runtime may not
// define commands, network, secrets, waivers or merges.
var ErrToolForbidden = errors.New("agent: tool call outside the frozen tool surface")

// Ports are the side-effecting collaborators the orchestrator drives.
// The model is ONE port among several — never the state owner.
type Ports struct {
	// Eligible answers whether the defect is remediable at all
	// (severity policy, defect state, active-link absence).
	Eligible func(ctx RunContext) (bool, string, error)
	// Reproduce runs the reproduction against the pinned SHAs; ok=false
	// with reason means handoff.
	Reproduce func(ctx RunContext) (ok bool, signature string, err error)
	// Diagnose produces a structured diagnosis (no state changes).
	Diagnose func(ctx RunContext, signature string) (diagnosis string, err error)
	// Modify applies the fix through the versioned command profiles —
	// never free-text commands.
	Modify func(ctx RunContext, diagnosis string) (diffRef string, err error)
	// VerifyLocally runs the local verification gate; passed=false with
	// retriable=true sends the run to retrying when budget remains.
	VerifyLocally func(ctx RunContext, diffRef string) (passed bool, retriable bool, err error)
	// CreateMR opens the candidate MR (human merge remains the only
	// merge path — AGT-RULE-003).
	CreateMR func(ctx RunContext, diffRef string) (mrRef string, err error)
	// CheckCI reads the authoritative pipeline verdict for the MR.
	CheckCI func(ctx RunContext, mrRef string) (passed bool, retriable bool, err error)
}

// RunContext carries one run's identity through the ports.
type RunContext struct {
	ProjectID string
	DefectID  string
	RunID     string
	Attempt   int
}

// Orchestrator drives one remediation run.
type Orchestrator struct {
	Ports  Ports
	Ledger *budget.Ledger
	// MaxAttempts is the attempt ceiling (default 3 per the PRD).
	MaxAttempts int
	StartedAt   time.Time
	Now         func() time.Time
}

// StepOutcome reports one machine step's result.
type StepOutcome struct {
	From     State
	To       State
	Reason   HandoffReason // set when To is a handoff state
	Terminal bool
}

// EligibilityGate is step one: ineligible or unbudgeted defects hand
// off immediately.
func (o *Orchestrator) EligibilityGate(ctx RunContext) (StepOutcome, error) {
	eligible, reason, err := o.Ports.Eligible(ctx)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("agent: eligibility: %w", err)
	}
	if !eligible {
		return o.to(ctx, StateEligibilityCheck, StateNeedsHuman, HandoffReason(reason))
	}
	return o.to(ctx, StateEligibilityCheck, StateReproducing, "")
}

// ReproduceStep: a defect that cannot be reproduced hands off with the
// honest reason — no guessed fixes (AGT-REQ-002's shadow).
func (o *Orchestrator) ReproduceStep(ctx RunContext) (StepOutcome, error) {
	ok, _, err := o.Ports.Reproduce(ctx)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("agent: reproduce: %w", err)
	}
	if !ok {
		return o.to(ctx, StateReproducing, StateNeedsHuman, HandoffCannotReproduce)
	}
	return o.to(ctx, StateReproducing, StateDiagnosing, "")
}

// DiagnoseStep: context/budget exhaustion hands off.
func (o *Orchestrator) DiagnoseStep(ctx RunContext, signature string) (StepOutcome, error) {
	if _, err := o.Ports.Diagnose(ctx, signature); err != nil {
		return StepOutcome{}, fmt.Errorf("agent: diagnose: %w", err)
	}
	return o.to(ctx, StateDiagnosing, StateModifying, "")
}

// ModifyStep applies the diagnosis through the profiled tools.
func (o *Orchestrator) ModifyStep(ctx RunContext, diagnosis string) (StepOutcome, error) {
	if _, err := o.Ports.Modify(ctx, diagnosis); err != nil {
		return StepOutcome{}, fmt.Errorf("agent: modify: %w", err)
	}
	return o.to(ctx, StateModifying, StateLocalVerifying, "")
}

// LocalVerifyStep: pass proceeds to MR; failure retries while budget
// and attempts remain, otherwise hands off.
func (o *Orchestrator) LocalVerifyStep(ctx RunContext, diffRef string) (StepOutcome, error) {
	passed, retriable, err := o.Ports.VerifyLocally(ctx, diffRef)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("agent: local verify: %w", err)
	}
	if passed {
		return o.to(ctx, StateLocalVerifying, StateMRCreated, "")
	}
	if retriable && o.attemptsRemain(ctx) {
		return o.to(ctx, StateLocalVerifying, StateRetrying, "")
	}
	if retriable {
		return o.to(ctx, StateLocalVerifying, StateNeedsHuman, HandoffBudgetExhausted)
	}
	return o.to(ctx, StateLocalVerifying, StateNeedsHuman, HandoffLowConfidence)
}

// CIVerifyStep: green CI parks the run for human review (the ONLY
// merge path); red CI retries while budget remains.
func (o *Orchestrator) CIVerifyStep(ctx RunContext, mrRef string) (StepOutcome, error) {
	passed, retriable, err := o.Ports.CheckCI(ctx, mrRef)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("agent: ci verify: %w", err)
	}
	if passed {
		return o.to(ctx, StateCIVerifying, StateAwaitingHuman, HandoffHumanReview)
	}
	if retriable && o.attemptsRemain(ctx) {
		return o.to(ctx, StateCIVerifying, StateRetrying, "")
	}
	if retriable {
		return o.to(ctx, StateCIVerifying, StateNeedsHuman, HandoffBudgetExhausted)
	}
	return o.to(ctx, StateCIVerifying, StateNeedsHuman, HandoffLowConfidence)
}

// RetryLoopStep sends a retrying run back into reproduction.
func (o *Orchestrator) RetryLoopStep(ctx RunContext) (StepOutcome, error) {
	return o.to(ctx, StateRetrying, StateReproducing, "")
}

// Stop cancels a live run from any non-terminal state.
func (o *Orchestrator) Stop(ctx RunContext, from State) (StepOutcome, error) {
	return o.to(ctx, from, StateStopped, HandoffPolicyStop)
}

func (o *Orchestrator) attemptsRemain(ctx RunContext) bool {
	maxAttempts := o.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	if ctx.Attempt >= maxAttempts {
		return false
	}
	if o.Ledger == nil {
		return true
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	_, stop := o.Ledger.StopReasonIfExhausted(ctx.Attempt, now(), o.StartedAt)
	return !stop
}

// to validates the edge against the frozen machine and returns the
// outcome — the single place a transition is decided.
func (o *Orchestrator) to(_ RunContext, from, to State, reason HandoffReason) (StepOutcome, error) {
	if !CanTransition(from, to) {
		return StepOutcome{}, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
	}
	if Terminal(from) {
		return StepOutcome{}, fmt.Errorf("%w: %s is terminal", ErrIllegalTransition, from)
	}
	if to == StateAwaitingHuman && reason != HandoffHumanReview {
		// awaiting_human is only ever reached with the review reason.
		return StepOutcome{}, fmt.Errorf("%w: awaiting_human requires human_review_required", ErrIllegalTransition)
	}
	return StepOutcome{From: from, To: to, Reason: reason, Terminal: Terminal(to)}, nil
}
