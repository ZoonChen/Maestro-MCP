package slo

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pure M4-REL-001 core: availability/error-budget math, objective
// classification, alert-runbook linkage and wire conformance to the
// frozen slo-status.schema.json.

func ptr(value float64) *float64 { return &value }

func fullPolicy() Policy {
	return Policy{
		Availability: AvailabilityPolicy{AtRiskErrorBudgetRemainingPercent: 25},
		Objectives: []ObjectivePolicy{
			{Kind: ObjectiveAPIP95LatencyMS, Target: 500, Unit: "ms", LowerIsBetter: true,
				AtRiskThreshold: 400, RunbookRef: "runbooks/webhook-pipeline-failure"},
			{Kind: ObjectiveBackupSuccessRatePercent, Target: 100, Unit: "percent", LowerIsBetter: false,
				AtRiskThreshold: 100, RunbookRef: "runbooks/database-backup-restore"},
		},
	}
}

func TestEvaluateAvailabilityStates(t *testing.T) {
	policy := fullPolicy()

	cases := []struct {
		name             string
		success, total   int64
		state            State
		remainingPercent float64
	}{
		{"perfect run stays healthy at full budget", 1000000, 1000000, StateHealthy, 100},
		{"healthy with 40 percent budget left", 9970, 10000, StateHealthy, 40},
		{"at risk when remaining hits the line", 9959, 10000, StateAtRisk, 18},
		{"breached below target keeps zero budget", 9900, 10000, StateBreached, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot, err := Evaluate(policy, AvailabilityInput{Success: testCase.success, Total: testCase.total},
				nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "2026-09-07T00:00:00Z")
			require.NoError(t, err)
			assert.Equal(t, testCase.state, snapshot.Availability.State)
			assert.InDelta(t, testCase.remainingPercent, snapshot.Availability.ErrorBudgetRemainingPercent, 1e-9)
			assert.Equal(t, AvailabilityTargetPercent, snapshot.Availability.TargetPercent)
		})
	}
}

func TestEvaluateRefusesMissingOrInvalidAvailability(t *testing.T) {
	_, err := Evaluate(fullPolicy(), AvailabilityInput{}, nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrNoAvailabilityData)

	_, err = Evaluate(fullPolicy(), AvailabilityInput{Success: 11, Total: 10}, nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrAvailabilityInvalid)

	_, err = Evaluate(fullPolicy(), AvailabilityInput{Success: -1, Total: 10}, nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrAvailabilityInvalid)
}

func TestEvaluateObjectiveStates(t *testing.T) {
	policy := fullPolicy()

	cases := []struct {
		name     string
		kind     ObjectiveKind
		measured *float64
		state    State
		severity AlertSeverity
	}{
		{"api latency healthy under the at-risk line", ObjectiveAPIP95LatencyMS, ptr(350), StateHealthy, ""},
		{"api latency at risk above the line", ObjectiveAPIP95LatencyMS, ptr(450), StateAtRisk, SeverityWarning},
		{"api latency breached", ObjectiveAPIP95LatencyMS, ptr(650), StateBreached, SeverityCritical},
		{"backup rate healthy at full success", ObjectiveBackupSuccessRatePercent, ptr(100), StateHealthy, ""},
		{"backup rate breached on a miss", ObjectiveBackupSuccessRatePercent, ptr(0), StateBreached, SeverityCritical},
		{"missing telemetry is no data not zero", ObjectiveAPIP95LatencyMS, nil, StateNoData, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot, err := Evaluate(policy, AvailabilityInput{Success: 9995, Total: 10000},
				[]ObjectiveInput{{Kind: testCase.kind, Measured: testCase.measured, Since: "2026-09-07T01:00:00Z"}},
				Window{Kind: WindowRolling30D}, DegradationInput{}, "2026-09-07T00:00:00Z")
			require.NoError(t, err)
			entry, ok := snapshot.objective(testCase.kind)
			require.True(t, ok, "policy objectives are always emitted")
			assert.Equal(t, testCase.state, entry.State)
			if testCase.state == StateNoData {
				assert.Equal(t, float64(0), entry.Measured, "no_data still emits the required measured value as 0")
				assert.Nil(t, entry.Alert)
			}
			if testCase.severity != "" {
				require.NotNil(t, entry.Alert)
				assert.True(t, entry.Alert.Firing)
				assert.Equal(t, testCase.severity, entry.Alert.Severity)
				assert.NotEmpty(t, entry.Alert.RunbookRef, "firing alerts always carry the runbook")
				assert.Equal(t, "2026-09-07T01:00:00Z", entry.Alert.Since)
			}
		})
	}
}

func TestEvaluatePolicyValidation(t *testing.T) {
	base := fullPolicy()

	base.Objectives[0].Kind = "made_up_metric"
	_, err := Evaluate(base, AvailabilityInput{Success: 1, Total: 1}, nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrPolicyInvalid)

	base = fullPolicy()
	base.Objectives[1].Kind = base.Objectives[0].Kind
	_, err = Evaluate(base, AvailabilityInput{Success: 1, Total: 1}, nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrPolicyInvalid)

	base = fullPolicy()
	base.Objectives[0].AtRiskThreshold = 600 // above the target for lower-is-better
	_, err = Evaluate(base, AvailabilityInput{Success: 1, Total: 1}, nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrPolicyInvalid)

	base = fullPolicy()
	base.Objectives[1].AtRiskThreshold = 90 // below the target for higher-is-better
	_, err = Evaluate(base, AvailabilityInput{Success: 1, Total: 1}, nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrPolicyInvalid)

	base = fullPolicy()
	base.Objectives[0].RunbookRef = ""
	_, err = Evaluate(base, AvailabilityInput{Success: 1, Total: 1}, nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrPolicyInvalid, "alerts without a runbook are refused up front")

	base = fullPolicy()
	base.Availability.AtRiskErrorBudgetRemainingPercent = 101
	_, err = Evaluate(base, AvailabilityInput{Success: 1, Total: 1}, nil, Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrPolicyInvalid)
}

func TestEvaluateInputValidation(t *testing.T) {
	policy := fullPolicy()

	_, err := Evaluate(policy, AvailabilityInput{Success: 1, Total: 1},
		[]ObjectiveInput{{Kind: ObjectiveRPOMinutes, Measured: ptr(15)}},
		Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrObjectiveWithoutPolicy, "inputs without a policy fail closed")

	_, err = Evaluate(policy, AvailabilityInput{Success: 1, Total: 1},
		[]ObjectiveInput{{Kind: ObjectiveAPIP95LatencyMS}, {Kind: ObjectiveAPIP95LatencyMS}},
		Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.ErrorIs(t, err, ErrPolicyInvalid, "duplicate inputs are refused")

	_, err = Evaluate(policy, AvailabilityInput{Success: 1, Total: 1}, nil,
		Window{Kind: WindowRolling30D}, DegradationInput{Active: true, Mode: "sideloaded"}, "now")
	require.ErrorIs(t, err, ErrPolicyInvalid, "unknown degradation modes are refused")
}

func TestEvaluateDegradationPassthrough(t *testing.T) {
	zero, err := Evaluate(fullPolicy(), AvailabilityInput{Success: 1, Total: 1}, nil,
		Window{Kind: WindowRolling30D}, DegradationInput{}, "now")
	require.NoError(t, err)
	assert.Nil(t, zero.Degradation)

	active, err := Evaluate(fullPolicy(), AvailabilityInput{Success: 1, Total: 1}, nil,
		Window{Kind: WindowRolling30D}, DegradationInput{Active: true, Mode: DegradationReadOnly, Since: "2026-09-07T02:00:00Z"}, "now")
	require.NoError(t, err)
	require.NotNil(t, active.Degradation)
	assert.True(t, active.Degradation.Active)
	require.NotNil(t, active.Degradation.Mode)
	assert.Equal(t, DegradationReadOnly, *active.Degradation.Mode)
	assert.Equal(t, "2026-09-07T02:00:00Z", active.Degradation.Since)
}

// TestSnapshotWireConformance hand-checks the frozen contract in Go:
// the closed top-level field set, the required blocks, the const
// availability target and the enum vocabularies. The machine-side
// AJV validation lives in scripts/schema-check.rb over the schema's
// own examples; this guards the Go emission between freezes.
func TestSnapshotWireConformance(t *testing.T) {
	snapshot, err := Evaluate(fullPolicy(), AvailabilityInput{Success: 9955, Total: 10000},
		[]ObjectiveInput{{Kind: ObjectiveAPIP95LatencyMS, Measured: ptr(610), Since: "2026-09-07T01:00:00Z"}},
		Window{Kind: WindowRolling30D, From: "2026-08-08T00:00:00Z", To: "2026-09-07T00:00:00Z"},
		DegradationInput{}, "2026-09-07T00:00:00Z")
	require.NoError(t, err)

	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))

	assert.ElementsMatch(t,
		[]string{"schema_version", "window", "availability", "objectives", "generated_at"},
		keysOf(wire), "the closed field set of the frozen schema")
	assert.Equal(t, "3.0", wire["schema_version"])

	availability, ok := wire["availability"].(map[string]any)
	require.True(t, ok)
	assert.ElementsMatch(t,
		[]string{"target_percent", "measured_percent", "state", "error_budget_remaining_percent"}, keysOf(availability))
	assert.Equal(t, 99.5, availability["target_percent"])
	assert.Contains(t, []string{"healthy", "at_risk", "breached"}, availability["state"],
		"availability has no no_data state in the frozen enum")

	objectives, ok := wire["objectives"].([]any)
	require.True(t, ok)
	frozenKinds := []string{
		"api_p95_latency_ms", "webhook_ingest_p95_latency_ms", "inbox_lag_p95_seconds",
		"gate_eval_p95_latency_ms", "rpo_minutes", "rto_minutes", "backup_success_rate_percent"}
	for _, item := range objectives {
		entry, ok := item.(map[string]any)
		require.True(t, ok)
		assert.Subset(t, []string{"kind", "target", "measured", "unit", "state", "alert"}, keysOf(entry))
		assert.Contains(t, entry, "measured", "measured is required even for no_data")
		assert.Contains(t, frozenKinds, entry["kind"])
		assert.Contains(t, []string{"healthy", "at_risk", "breached", "no_data"}, entry["state"])
		if alert, has := entry["alert"]; has {
			alertMap, ok := alert.(map[string]any)
			require.True(t, ok)
			assert.Subset(t, []string{"firing", "severity", "runbook_ref", "since"}, keysOf(alertMap))
			assert.Contains(t, []string{"warning", "critical"}, alertMap["severity"])
			assert.NotEmpty(t, alertMap["runbook_ref"])
		}
	}
}

func keysOf(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}

func (s Snapshot) objective(kind ObjectiveKind) (Objective, bool) {
	for _, entry := range s.Objectives {
		if entry.Kind == kind {
			return entry, true
		}
	}
	return Objective{}, false
}
