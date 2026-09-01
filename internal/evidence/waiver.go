package evidence

import (
	"errors"
	"fmt"
	"time"
)

// Waiver states (the frozen machine: requested → approved/rejected →
// active → expired/revoked). 'active' is reserved for a future external
// activation step; an approved, unexpired waiver already waives.
const (
	WaiverRequested = "requested"
	WaiverApproved  = "approved"
	WaiverRejected  = "rejected"
	WaiverActive    = "active"
	WaiverExpired   = "expired"
	WaiverRevoked   = "revoked"
)

// Waiver lifecycle violations surfaced to callers as stable conditions.
var (
	ErrWaiverNonWaivable = errors.New("check is not waivable")
	ErrWaiverTooLong     = errors.New("waiver exceeds the maximum lifetime")
	ErrWaiverSelfApprove = errors.New("waiver approver must differ from the requester")
	ErrWaiverState       = errors.New("waiver state does not allow this operation")
)

// Waiver mirrors the stored waiver row relevant to evaluation.
type Waiver struct {
	ID              string
	GateID          string
	Check           string
	SourceSHA       string
	MergeRequestIID int64
	State           string
	Requester       string
	Approver        string
	Reason          string
	ExpiresAt       time.Time
	Version         int64
}

// WaiverRequestInput is a validated creation request.
type WaiverRequestInput struct {
	GateID          string
	Check           string
	SourceSHA       string
	MergeRequestIID int64
	Requester       string
	Reason          string
	ExpiresAt       time.Time
}

// maxWaiverLifetime is the frozen seven-day ceiling (waiver.max_days
// const 7); the policy value is asserted equal at resolve time.
const maxWaiverLifetime = 7 * 24 * time.Hour

// NewWaiver validates a waiver request against the effective policy
// (QUAL-QUALITY-POLICY §10): bounded lifetime, non-waivable principles
// rejected outright, and a substantive reason.
func NewWaiver(policy *EffectivePolicy, input WaiverRequestInput, now time.Time) (*Waiver, error) {
	if policy == nil || policy.Policy == nil {
		return nil, fmt.Errorf("waiver: effective policy is required")
	}
	for _, principle := range policy.Policy.Waiver.NonWaivableGates {
		if principle == input.Check {
			return nil, fmt.Errorf("%w: %s", ErrWaiverNonWaivable, input.Check)
		}
	}
	if input.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("waiver: expires_at is required")
	}
	if input.ExpiresAt.After(now.Add(maxWaiverLifetime + time.Second)) {
		return nil, fmt.Errorf("%w: %s exceeds 7 days from now", ErrWaiverTooLong, input.ExpiresAt.Format(time.RFC3339))
	}
	if !input.ExpiresAt.After(now) {
		return nil, fmt.Errorf("waiver: expires_at must be in the future")
	}
	if len(input.Reason) < 16 || len(input.Reason) > 4000 {
		return nil, fmt.Errorf("waiver: reason must be 16-4000 chars")
	}
	if input.Requester == "" || input.Check == "" || !shaPattern.MatchString(input.SourceSHA) {
		return nil, fmt.Errorf("waiver: requester, check and source SHA are required")
	}
	if input.MergeRequestIID < 1 {
		return nil, fmt.Errorf("waiver: merge_request_iid is required")
	}
	return &Waiver{
		GateID:          input.GateID,
		Check:           input.Check,
		SourceSHA:       input.SourceSHA,
		MergeRequestIID: input.MergeRequestIID,
		State:           WaiverRequested,
		Requester:       input.Requester,
		Reason:          input.Reason,
		ExpiresAt:       input.ExpiresAt,
	}, nil
}

// Approve records the distinct approver's decision. The approver must
// differ from the requester (QUAL §10:申请人、变更作者、Agent 和 Verifier
// 均不能批准自身相关豁免 — the engine enforces the requester split; the
// author/agent/verifier splits are enforced by the authorization layer
// that knows those identities).
func (w *Waiver) Approve(approverID string) error {
	if w.State != WaiverRequested {
		return fmt.Errorf("%w: cannot approve from %s", ErrWaiverState, w.State)
	}
	if approverID == "" {
		return fmt.Errorf("waiver: approver is required")
	}
	if approverID == w.Requester {
		return ErrWaiverSelfApprove
	}
	w.State = WaiverApproved
	w.Approver = approverID
	return nil
}

// Reject records a distinct approver's rejection (terminal).
func (w *Waiver) Reject(approverID string) error {
	if w.State != WaiverRequested {
		return fmt.Errorf("%w: cannot reject from %s", ErrWaiverState, w.State)
	}
	if approverID == w.Requester {
		return ErrWaiverSelfApprove
	}
	w.State = WaiverRejected
	w.Approver = approverID
	return nil
}

// Revoke cancels an approved waiver (terminal).
func (w *Waiver) Revoke() error {
	switch w.State {
	case WaiverApproved, WaiverActive, WaiverRequested:
		w.State = WaiverRevoked
		return nil
	default:
		return fmt.Errorf("%w: cannot revoke from %s", ErrWaiverState, w.State)
	}
}

// Expire marks a waiver past its deadline (terminal). Lazy evaluation:
// call sites sweep or check IsValid before use.
func (w *Waiver) Expire(now time.Time) bool {
	if now.After(w.ExpiresAt) && (w.State == WaiverApproved || w.State == WaiverActive) {
		w.State = WaiverExpired
		return true
	}
	return false
}

// IsValid reports whether the waiver currently waives its bound gate.
func (w *Waiver) IsValid(now time.Time) bool {
	return (w.State == WaiverApproved || w.State == WaiverActive) && !now.After(w.ExpiresAt)
}

// applicableWaiver finds the valid waiver bound to this exact gate
// identity and check. The gate identity embeds the SHA tuple and policy
// version, so a waiver for an old SHA or old policy simply stops
// matching; the stored source_sha is re-checked as a second guard so an
// inconsistent row can never waive a drifted tuple (QG-RULE-003).
func applicableWaiver(waivers []Waiver, tup Tuple, check string, now time.Time) (Waiver, bool) {
	gateID := StableGateID(tup, check)
	for index, waiver := range waivers {
		if waiver.GateID == gateID && waiver.Check == check &&
			waiver.SourceSHA == tup.SourceSHA && waiver.IsValid(now) {
			return waivers[index], true
		}
	}
	return Waiver{}, false
}
