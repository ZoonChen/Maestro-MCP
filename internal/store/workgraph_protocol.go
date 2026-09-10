package store

import (
	"errors"

	"github.com/ZoonChen/Maestro-MCP/internal/workgraph"
)

// J2b protocol storage types (authority: internal/workgraph for the
// domain logic; this file is the row-shape mirror of migration 0019).

var (
	// ErrProposalRejected reports a decided rejection (the violations
	// travel on the record, not the error).
	ErrProposalRejected = errors.New("decomposition proposal rejected")
	// ErrReplanRejected reports a replan attempt against a plan whose
	// current revision is not sealed.
	ErrReplanRejected = errors.New("replan requires the current revision to be sealed")
	// ErrAttemptNotFound reports a missing execution attempt.
	ErrAttemptNotFound = errors.New("execution attempt not found")
	// ErrAttemptNotResumable reports a completion against an attempt
	// that is no longer running.
	ErrAttemptNotResumable = errors.New("execution attempt is not running")
)

// BusinessProblem is one business_problems row (Intent layer).
type BusinessProblem struct {
	ID        string
	ProjectID string
	Title     string
	Statement string
	Status    string
	CreatedBy string
}

// OutcomeContract is one outcome_contracts row.
type OutcomeContract struct {
	ID              string
	ProblemID       string
	Version         int
	SuccessCriteria []byte
	ConstraintsDoc  []byte
}

// Capability is one capabilities row.
type Capability struct {
	ID          string
	ProjectID   string
	CapKey      string
	Description string
}

// WorkPlanIntent is the primary problem binding of one plan.
type WorkPlanIntent struct {
	PlanID             string
	ProblemID          string
	OutcomeContractID  string
	AttachedBy         string
}

// WorkPattern is one versioned decomposition template row.
type WorkPattern struct {
	ID        string
	ProjectID string
	Name      string
	Version   int
	Status    string
	Body      []byte
	CreatedBy string
}

// DecompositionProposalRecord is one decomposition_proposals row; a
// decided proposal is an immutable protocol fact.
type DecompositionProposalRecord struct {
	ID                   string
	ProjectID            string
	PlanID               string
	WorkPatternID        string
	ExpectedGraphVersion int64
	Payload              []byte
	IdempotencyKey       string
	Status               string // submitted | applied | rejected
	Violations           []byte
	AppliedNodeIDs       []byte
	SubmittedBy          string
	DecidedAt            string
	CreatedAt            string
}

// ExecutionAttempt is one execution_attempts row: the immutable
// five-way binding plus lease fencing bookkeeping.
type ExecutionAttempt struct {
	ID                  string
	ProjectID           string
	PlanID              string
	NodeID              string
	NodeRevisionID      string
	SpecDigest          string
	AttemptNo           int
	RetryOfAttemptID    string
	Principal           string
	Role                string
	SessionID           string
	WorkerID            string
	WorktreePath        string
	ContextDigest       string
	ContextSet          []byte
	BudgetLedgerID      string
	BudgetUnits         int64
	LeaseToken          string
	LeaseEpoch          int64
	LeaseVersion        int64
	ConnectionGeneration string
	LeaseExpiresAt      string
	IdempotencyKey      string
	Status              string
	Outcome             []byte
	EndedAt             string
	CreatedAt           string
}

// WorkNodeClaim is the graph-path dispatch outcome (the envelope plus
// the row identities the caller needs for follow-up calls).
type WorkNodeClaim struct {
	Envelope workgraph.ExecutionEnvelope
}

// Attempt completion outcomes.
const (
	AttemptOutcomeSucceeded = "succeeded"
	AttemptOutcomeFailed    = "failed"
	AttemptOutcomeCancelled = "cancelled"
	AttemptOutcomeBlocked   = "blocked"
)
