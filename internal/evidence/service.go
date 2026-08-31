package evidence

import (
	"context"
	"fmt"
	"time"
)

// EvalStore is the persistence surface the evaluation trigger needs.
type EvalStore interface {
	GetProjectPolicy(ctx context.Context, projectID string) (*Policy, int64, error)
	ListEvidenceForWorkItem(ctx context.Context, projectID, workItemID string) ([]Record, error)
	ListWaiversForWorkItem(ctx context.Context, projectID, workItemID string) ([]Waiver, error)
	PersistVerdict(ctx context.Context, verdict *Verdict) error
}

// Service is the evaluation trigger: whenever facts change for a work
// item's exact SHA tuple (evidence appended, waiver approved, MR tuple
// completed), the engine re-evaluates deterministically and persists
// the verdict — idempotent upserts with drift-driven staling
// (QUAL-QUALITY-POLICY section 11: evidence settled within 30s of the
// last event; here the trigger is synchronous with the event).
type Service struct {
	Company *Policy
	Store   EvalStore
	Now     func() time.Time
}

// EvaluateWorkItem resolves the effective policy, loads the work
// item's evidence and waivers, evaluates the exact tuple, and persists
// the verdict. The caller owns the tuple (the MR projection's SHA
// pair); an incomplete tuple is a caller error, not an empty pass.
func (s *Service) EvaluateWorkItem(ctx context.Context, tup Tuple) (*Verdict, error) {
	if s.Company == nil {
		return nil, fmt.Errorf("evaluate service: company baseline is required")
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	overlay, _, err := s.Store.GetProjectPolicy(ctx, tup.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("evaluate service: policy: %w", err)
	}
	resolved, err := ResolveEffective(s.Company, overlay)
	if err != nil {
		return nil, fmt.Errorf("evaluate service: %w", err)
	}
	// The evaluation tuple carries the RESOLVED policy version: the
	// overlay's semver when present, otherwise the company baseline's.
	// Callers pass SHA facts only; validation runs with the version set.
	tup.PolicyVersion = resolved.Policy.Version
	if err := validateTuple(tup); err != nil {
		return nil, err
	}

	records, err := s.Store.ListEvidenceForWorkItem(ctx, tup.ProjectID, tup.WorkItemID)
	if err != nil {
		return nil, fmt.Errorf("evaluate service: evidence: %w", err)
	}
	waivers, err := s.Store.ListWaiversForWorkItem(ctx, tup.ProjectID, tup.WorkItemID)
	if err != nil {
		return nil, fmt.Errorf("evaluate service: waivers: %w", err)
	}
	verdict, err := Evaluate(tup, resolved, records, waivers, now())
	if err != nil {
		return nil, fmt.Errorf("evaluate service: %w", err)
	}
	if err := s.Store.PersistVerdict(ctx, verdict); err != nil {
		return nil, fmt.Errorf("evaluate service: persist: %w", err)
	}
	return verdict, nil
}
