package agent

import "time"

// Store is the persistence contract for agent runs: the orchestrator's
// decisions land durably with version guards so a crash resumes from
// the last recorded state instead of replaying side effects
// (the PRD's crash-recovery rule).
type Store interface {
	// CreateRun opens a run for one defect+work-item pair under one
	// attempt; returns the persisted current state for resume.
	CreateRun(ctx RunContext, attempt int) (state State, created bool, err error)

	// LoadState reads the durable state.
	LoadState(ctx RunContext) (state State, attempt int, found bool, err error)

	// Settle records a validated transition (the machine validated the
	// edge BEFORE this call); expectedState guards against concurrent
	// drivers — a mismatch refuses.
	Settle(ctx RunContext, from, to State, reason HandoffReason) error
}

// RunRecord is the durable row shape for reporting.
type RunRecord struct {
	ID           string
	ProjectID    string
	DefectID     string
	WorkItemID   string
	Attempt      int
	State        State
	UsedTokens   int64
	BudgetTokens int64
	UpdatedAt    time.Time
}
