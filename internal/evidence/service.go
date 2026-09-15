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

// ControlPlaneFactSource is the optional store capability that grounds
// the D1 self-attestation in persisted facts: the stored project
// policy row's digest integrity and the work item's execution records
// (registered runner + approved profile). Stores without it keep the
// legacy, non-self-attesting evaluation path.
type ControlPlaneFactSource interface {
	ControlPlaneFacts(ctx context.Context, projectID, workItemID string) (*ControlPlaneFacts, error)
}

// ControlPlaneEvidenceAppender persists minted self-attestation
// records append-only (identity collapse makes re-appends idempotent).
// It matches the store's AppendEvidence surface so the quality store
// satisfies it without adaptation.
type ControlPlaneEvidenceAppender interface {
	AppendEvidence(ctx context.Context, record *Record) error
}

// ReadyMarker is the optional gate-driven state writer (W6-1, S2C-A1):
// when the store implements it, a Ready verdict moves a VALIDATING
// work item to ready_for_human_merge — the state the merged fact's
// done edge requires. Stores without the writer stay append-only
// (verdicts persist; the state machine is untouched).
type ReadyMarker interface {
	MarkWorkItemReadyFromGates(ctx context.Context, projectID, workItemID, actor, reason string) (bool, error)
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
//
// A tuple with an EMPTY PolicyVersion is bound to the resolved policy
// in force right now (the live path). A NON-EMPTY PolicyVersion is
// honored as a replay pin (D1-4): the tuple re-runs under the version
// it was originally evaluated with, and the self-attested
// baseline_freshness gate judges the pin against the active version —
// drift fails closed instead of silently re-baselining the tuple.
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
	// A pre-set version is the replay pin and is kept verbatim.
	if tup.PolicyVersion == "" {
		tup.PolicyVersion = resolved.Policy.Version
	}
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

	// D1 self-attestation: when the store grounds the judgments in
	// persisted facts, the three engine-oracle gates self-attest.
	// Stores without the fact source evaluate exactly as before.
	facts, err := s.controlPlaneFacts(ctx, overlay, tup)
	if err != nil {
		return nil, fmt.Errorf("evaluate service: %w", err)
	}

	verdict, err := Evaluate(tup, resolved, facts, records, waivers, now())
	if err != nil {
		return nil, fmt.Errorf("evaluate service: %w", err)
	}
	if len(verdict.ControlPlane) > 0 {
		appender, ok := s.Store.(ControlPlaneEvidenceAppender)
		if !ok {
			return nil, fmt.Errorf("evaluate service: store cannot persist control-plane evidence")
		}
		for _, minted := range verdict.ControlPlane {
			record := minted
			if err := appender.AppendEvidence(ctx, &record); err != nil {
				return nil, fmt.Errorf("evaluate service: control-plane evidence: %w", err)
			}
		}
	}
	if err := s.Store.PersistVerdict(ctx, verdict); err != nil {
		return nil, fmt.Errorf("evaluate service: persist: %w", err)
	}
	// W6-1: a Ready verdict on the exact tuple is the machine-legal
	// signal that validation finished — drive validating →
	// ready_for_human_merge so the human merge and its merged fact can
	// complete the done chain. The guarded writer no-ops on any other
	// state, so replays and out-of-order verdicts stay inert.
	if verdict.Ready {
		if marker, ok := s.Store.(ReadyMarker); ok {
			reason := fmt.Sprintf("all required gates passed or waived (policy %s)", resolved.Policy.Version)
			if _, err := marker.MarkWorkItemReadyFromGates(ctx, tup.ProjectID, tup.WorkItemID, "control-plane", reason); err != nil {
				return verdict, fmt.Errorf("evaluate service: ready transition: %w", err)
			}
		}
	}
	return verdict, nil
}

// controlPlaneFacts assembles the self-attestation ground truth: the
// digests of the documents this evaluation actually resolved plus the
// store's persisted facts. A nil result (store without the fact
// source) disables self-attestation for this evaluation; a fact-source
// failure is a system error and fails the evaluation.
func (s *Service) controlPlaneFacts(ctx context.Context, overlay *Policy, tup Tuple) (*ControlPlaneFacts, error) {
	source, ok := s.Store.(ControlPlaneFactSource)
	if !ok {
		return nil, nil
	}
	facts, err := source.ControlPlaneFacts(ctx, tup.ProjectID, tup.WorkItemID)
	if err != nil {
		return nil, fmt.Errorf("control-plane facts: %w", err)
	}
	if facts == nil {
		facts = &ControlPlaneFacts{}
	}
	companyDigest, err := s.Company.Digest()
	if err != nil {
		return nil, fmt.Errorf("control-plane facts: company digest: %w", err)
	}
	facts.CompanyDigest = companyDigest
	if overlay != nil {
		projectDigest, digestErr := overlay.Digest()
		if digestErr != nil {
			return nil, fmt.Errorf("control-plane facts: overlay digest: %w", digestErr)
		}
		facts.ProjectDigest = projectDigest
	}
	return facts, nil
}
