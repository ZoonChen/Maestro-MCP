// Package slo implements the M4-REL-001 evaluation core: the SLO
// snapshot per the frozen slo-status.schema.json (availability with
// error budget, the named objective set, alert-to-runbook linkage and
// the degradation mode). Every threshold is an explicit policy input —
// the evaluator refuses hidden constants, so the at-risk line can only
// come from the caller's approved policy; the only number baked in is
// the frozen availability target (reliability-and-recovery.md §11).
//
// The package is pure over inputs; the telemetry-backed measurement
// lives in internal/store.
package slo

import (
	"errors"
	"fmt"
)

// SchemaVersion is the frozen wire version of the snapshot.
const SchemaVersion = "3.0"

// AvailabilityTargetPercent is the frozen monthly availability
// objective; the wire schema pins it as a const.
const AvailabilityTargetPercent = 99.5

// State is the snapshot state vocabulary. Availability has no no_data
// state — without an availability measurement the evaluation refuses
// to run (fail-closed), it never invents a number.
type State string

const (
	StateHealthy  State = "healthy"
	StateAtRisk   State = "at_risk"
	StateBreached State = "breached"
	StateNoData   State = "no_data"
)

// AlertSeverity is the frozen severity vocabulary.
type AlertSeverity string

const (
	SeverityWarning  AlertSeverity = "warning"
	SeverityCritical AlertSeverity = "critical"
)

// ObjectiveKind enumerates the frozen objective set.
type ObjectiveKind string

const (
	ObjectiveAPIP95LatencyMS           ObjectiveKind = "api_p95_latency_ms"
	ObjectiveWebhookIngestP95LatencyMS ObjectiveKind = "webhook_ingest_p95_latency_ms"
	ObjectiveInboxLagP95Seconds        ObjectiveKind = "inbox_lag_p95_seconds"
	ObjectiveGateEvalP95LatencyMS      ObjectiveKind = "gate_eval_p95_latency_ms"
	ObjectiveRPOMinutes                ObjectiveKind = "rpo_minutes"
	ObjectiveRTOMinutes                ObjectiveKind = "rto_minutes"
	ObjectiveBackupSuccessRatePercent  ObjectiveKind = "backup_success_rate_percent"
)

var objectiveKinds = map[ObjectiveKind]bool{
	ObjectiveAPIP95LatencyMS:           true,
	ObjectiveWebhookIngestP95LatencyMS: true,
	ObjectiveInboxLagP95Seconds:        true,
	ObjectiveGateEvalP95LatencyMS:      true,
	ObjectiveRPOMinutes:                true,
	ObjectiveRTOMinutes:                true,
	ObjectiveBackupSuccessRatePercent:  true,
}

// DegradationMode is the frozen degradation vocabulary; the zero value
// marshals as the schema's null mode.
type DegradationMode string

const (
	DegradationReadOnly    DegradationMode = "read_only"
	DegradationWebhookOnly DegradationMode = "webhook_only"
	DegradationDraining    DegradationMode = "draining"
)

var degradationModes = map[DegradationMode]bool{
	DegradationReadOnly:    true,
	DegradationWebhookOnly: true,
	DegradationDraining:    true,
}

// Sentinels.
var (
	ErrPolicyInvalid          = errors.New("slo: policy invalid")
	ErrAvailabilityInvalid    = errors.New("slo: availability input invalid")
	ErrNoAvailabilityData     = errors.New("slo: availability has no measurement")
	ErrObjectiveWithoutPolicy = errors.New("slo: objective input has no policy")
)

// AvailabilityPolicy carries the explicit at-risk line: remaining
// error budget at or below it classifies at_risk. It is deliberately
// not defaulted — an invented constant would be a hidden policy.
type AvailabilityPolicy struct {
	AtRiskErrorBudgetRemainingPercent float64
}

// ObjectivePolicy is one objective's thresholds. LowerIsBetter covers
// latency/lag/RPO/RTO (healthy means measured at or under the
// thresholds); backup success rate is higher-is-better.
type ObjectivePolicy struct {
	Kind ObjectiveKind
	// Target is the objective boundary: crossing it breaches.
	Target        float64
	Unit          string
	LowerIsBetter bool
	// AtRiskThreshold uses the same direction as Target: lower-is-better
	// goes at_risk above it, higher-is-better below it.
	AtRiskThreshold float64
	// RunbookRef rides every firing alert — an alert without a runbook
	// is refused up front (OBS-RULE-005).
	RunbookRef string
}

// Policy is the complete, explicit threshold set for one snapshot.
type Policy struct {
	Availability AvailabilityPolicy
	Objectives   []ObjectivePolicy
}

// AvailabilityInput carries the raw SLI counts.
type AvailabilityInput struct {
	Success int64
	Total   int64
}

// ObjectiveInput is one objective's measurement; a nil Measured
// classifies no_data (the frozen wire still requires a measured value,
// which is emitted as 0 — the state carries the semantics).
type ObjectiveInput struct {
	Kind     ObjectiveKind
	Measured *float64
	// Since is the observation timestamp carried onto a firing alert.
	Since string
}

// DegradationInput is the runtime degradation state passed through to
// the snapshot; the zero value omits the wire object.
type DegradationInput struct {
	Active bool
	Mode   DegradationMode
	Since  string
}

// Window is the frozen snapshot window.
type Window struct {
	Kind string `json:"kind"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Window kinds.
const (
	WindowRolling30D    = "rolling_30d"
	WindowRolling7D     = "rolling_7d"
	WindowCalendarMonth = "calendar_month"
)

// Availability is the availability block of the wire snapshot.
type Availability struct {
	TargetPercent               float64 `json:"target_percent"`
	MeasuredPercent             float64 `json:"measured_percent"`
	State                       State   `json:"state"`
	ErrorBudgetRemainingPercent float64 `json:"error_budget_remaining_percent"`
}

// Alert is a firing objective alert with its runbook linkage.
type Alert struct {
	Firing     bool          `json:"firing"`
	Severity   AlertSeverity `json:"severity,omitempty"`
	RunbookRef string        `json:"runbook_ref,omitempty"`
	Since      string        `json:"since,omitempty"`
}

// Objective is one objective in the wire snapshot.
type Objective struct {
	Kind     ObjectiveKind `json:"kind"`
	Target   float64       `json:"target"`
	Measured float64       `json:"measured"`
	Unit     string        `json:"unit,omitempty"`
	State    State         `json:"state"`
	Alert    *Alert        `json:"alert,omitempty"`
}

// Degradation is the degradation block; a nil Mode marshals as null
// per the frozen enum.
type Degradation struct {
	Active bool             `json:"active"`
	Mode   *DegradationMode `json:"mode"`
	Since  string           `json:"since,omitempty"`
}

// Snapshot is the wire shape of slo-status.schema.json; field names
// and presence must stay exactly on the frozen contract
// (additionalProperties is closed).
type Snapshot struct {
	SchemaVersion string       `json:"schema_version"`
	Window        Window       `json:"window"`
	Availability  Availability `json:"availability"`
	Objectives    []Objective  `json:"objectives"`
	Degradation   *Degradation `json:"degradation,omitempty"`
	GeneratedAt   string       `json:"generated_at"`
}

// Evaluate assembles one snapshot. It refuses to run without an
// availability measurement, without a policy for every input, and
// without a runbook for any objective that can alert.
func Evaluate(policy Policy, availability AvailabilityInput, inputs []ObjectiveInput, window Window, degradation DegradationInput, generatedAt string) (Snapshot, error) {
	if err := validatePolicy(policy); err != nil {
		return Snapshot{}, err
	}
	if availability.Total <= 0 {
		return Snapshot{}, ErrNoAvailabilityData
	}
	if availability.Success < 0 || availability.Success > availability.Total {
		return Snapshot{}, fmt.Errorf("%w: success=%d total=%d", ErrAvailabilityInvalid, availability.Success, availability.Total)
	}
	byKind := make(map[ObjectiveKind]ObjectiveInput, len(inputs))
	for _, input := range inputs {
		if input.Kind == "" {
			return Snapshot{}, fmt.Errorf("%w: empty objective kind", ErrObjectiveWithoutPolicy)
		}
		if _, has := policyIndex(policy, input.Kind); !has {
			return Snapshot{}, fmt.Errorf("%w: %s", ErrObjectiveWithoutPolicy, input.Kind)
		}
		if _, duplicate := byKind[input.Kind]; duplicate {
			return Snapshot{}, fmt.Errorf("%w: duplicate input %s", ErrPolicyInvalid, input.Kind)
		}
		byKind[input.Kind] = input
	}

	snapshot := Snapshot{
		SchemaVersion: SchemaVersion,
		Window:        window,
		Availability:  evaluateAvailability(policy, availability),
		GeneratedAt:   generatedAt,
	}
	if degradation.Active || degradation.Mode != "" || degradation.Since != "" {
		if degradation.Mode != "" && !degradationModes[degradation.Mode] {
			return Snapshot{}, fmt.Errorf("%w: unknown degradation mode %q", ErrPolicyInvalid, degradation.Mode)
		}
		mode := degradation.Mode
		snapshot.Degradation = &Degradation{
			Active: degradation.Active,
			Mode:   &mode,
			Since:  degradation.Since,
		}
	}
	for _, objectivePolicy := range policy.Objectives {
		input := byKind[objectivePolicy.Kind]
		snapshot.Objectives = append(snapshot.Objectives, evaluateObjective(objectivePolicy, input))
	}
	return snapshot, nil
}

func validatePolicy(policy Policy) error {
	if policy.Availability.AtRiskErrorBudgetRemainingPercent < 0 || policy.Availability.AtRiskErrorBudgetRemainingPercent > 100 {
		return fmt.Errorf("%w: at-risk error budget remaining %v outside 0..100",
			ErrPolicyInvalid, policy.Availability.AtRiskErrorBudgetRemainingPercent)
	}
	seen := make(map[ObjectiveKind]bool, len(policy.Objectives))
	for _, objective := range policy.Objectives {
		if !objectiveKinds[objective.Kind] {
			return fmt.Errorf("%w: objective kind %q not in the frozen set", ErrPolicyInvalid, objective.Kind)
		}
		if seen[objective.Kind] {
			return fmt.Errorf("%w: duplicate objective %s", ErrPolicyInvalid, objective.Kind)
		}
		seen[objective.Kind] = true
		if objective.Target < 0 || objective.AtRiskThreshold < 0 {
			return fmt.Errorf("%w: negative threshold for %s", ErrPolicyInvalid, objective.Kind)
		}
		if objective.LowerIsBetter && objective.AtRiskThreshold > objective.Target {
			return fmt.Errorf("%w: %s at-risk threshold %v must be <= target %v",
				ErrPolicyInvalid, objective.Kind, objective.AtRiskThreshold, objective.Target)
		}
		if !objective.LowerIsBetter && objective.AtRiskThreshold < objective.Target {
			return fmt.Errorf("%w: %s at-risk threshold %v must be >= target %v",
				ErrPolicyInvalid, objective.Kind, objective.AtRiskThreshold, objective.Target)
		}
		if objective.RunbookRef == "" {
			return fmt.Errorf("%w: %s has no runbook ref — an alert without a runbook is refused (OBS-RULE-005)",
				ErrPolicyInvalid, objective.Kind)
		}
	}
	return nil
}

func policyIndex(policy Policy, kind ObjectiveKind) (int, bool) {
	for index, objective := range policy.Objectives {
		if objective.Kind == kind {
			return index, true
		}
	}
	return 0, false
}

func evaluateAvailability(policy Policy, availability AvailabilityInput) Availability {
	measured := float64(availability.Success) / float64(availability.Total) * 100
	budgetTotal := 100 - AvailabilityTargetPercent
	remaining := 0.0
	if measured > AvailabilityTargetPercent {
		remaining = (measured - AvailabilityTargetPercent) / budgetTotal * 100
	}
	state := StateHealthy
	switch {
	case measured < AvailabilityTargetPercent:
		state = StateBreached
	case remaining <= policy.Availability.AtRiskErrorBudgetRemainingPercent:
		state = StateAtRisk
	}
	return Availability{
		TargetPercent:               AvailabilityTargetPercent,
		MeasuredPercent:             measured,
		State:                       state,
		ErrorBudgetRemainingPercent: remaining,
	}
}

func evaluateObjective(objective ObjectivePolicy, input ObjectiveInput) Objective {
	if input.Measured == nil {
		// The frozen wire requires a measured value; 0 plus the no_data
		// state carries the honest semantics.
		return Objective{Kind: objective.Kind, Target: objective.Target, Unit: objective.Unit, State: StateNoData}
	}
	measured := *input.Measured
	state := StateHealthy
	switch {
	case breaches(objective.LowerIsBetter, measured, objective.Target):
		state = StateBreached
	case breaches(objective.LowerIsBetter, measured, objective.AtRiskThreshold):
		state = StateAtRisk
	}
	entry := Objective{Kind: objective.Kind, Target: objective.Target, Measured: measured, Unit: objective.Unit, State: state}
	if state == StateAtRisk || state == StateBreached {
		severity := SeverityWarning
		if state == StateBreached {
			severity = SeverityCritical
		}
		entry.Alert = &Alert{Firing: true, Severity: severity, RunbookRef: objective.RunbookRef, Since: input.Since}
	}
	return entry
}

// breaches reports whether measured crosses the boundary in the
// objective's direction: above it for lower-is-better, below for
// higher-is-better. Equality never breaches.
func breaches(lowerIsBetter bool, measured, boundary float64) bool {
	if lowerIsBetter {
		return measured > boundary
	}
	return measured < boundary
}
