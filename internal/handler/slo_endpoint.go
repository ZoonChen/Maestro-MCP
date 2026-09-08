package handler

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ZoonChen/Maestro-MCP/internal/config"
	"github.com/ZoonChen/Maestro-MCP/internal/slo"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// The M4-REL-001/M4-OBS-001 snapshot surface: the frozen
// slo-status.schema.json wire evaluated from the telemetry aggregates
// (declared objectives) and the backup ledger (deployment
// objectives). The policy is explicit configuration — the evaluator
// has no defaults, and missing availability telemetry fails closed
// instead of inventing a number.

// MetricWindowSource reads the newest telemetry window per metric.
type MetricWindowSource interface {
	LatestMetricWindows(ctx context.Context, projectID string, metrics []string, notBefore, notAfter time.Time) ([]store.MetricWindow, error)
}

// BackupStatusSource reads the backup ledger for the deployment
// objectives (RPO anchor and run outcomes).
type BackupStatusSource interface {
	LatestVerifiedBackup(ctx context.Context, kind string, notAfter time.Time) (*store.BackupRun, error)
	CountBackupRuns(ctx context.Context, kind, status string, from, to time.Time) (int64, error)
}

// backupRunbookRef is the frozen runbook every deployment-objective
// alert links to (OBS-RULE-005: no alert without a runbook).
const backupRunbookRef = "runbooks/database-backup-restore"

// SLOSnapshotHandler serves GET /projects/:pid/slo-snapshot.
type SLOSnapshotHandler struct {
	policy        slo.Policy
	windowKind    string
	successMetric string
	totalMetric   string
	declared      []config.SLOObjectiveConfig
	backup        config.BackupConfig
	metrics       MetricWindowSource
	backups       BackupStatusSource
}

// NewSLOSnapshotHandler builds the declared policy from configuration
// and appends the deployment objectives whenever the frozen backup
// section carries targets (their at-risk line equals the target — the
// frozen contract defines no softer zone for them).
func NewSLOSnapshotHandler(cfg *config.SLOConfig, backup config.BackupConfig, metrics MetricWindowSource, backups BackupStatusSource) (*SLOSnapshotHandler, error) {
	policy := slo.Policy{
		Availability: slo.AvailabilityPolicy{
			AtRiskErrorBudgetRemainingPercent: cfg.Availability.AtRiskErrorBudgetRemainingPercent,
		},
	}
	for _, objective := range cfg.Objectives {
		policy.Objectives = append(policy.Objectives, slo.ObjectivePolicy{
			Kind: slo.ObjectiveKind(objective.Kind), Target: objective.Target,
			Unit: objective.Unit, LowerIsBetter: objective.LowerIsBetter,
			AtRiskThreshold: objective.AtRiskThreshold, RunbookRef: objective.RunbookRef,
		})
	}
	if backup.RPOMinutes > 0 {
		policy.Objectives = append(policy.Objectives, deploymentObjective(slo.ObjectiveRPOMinutes,
			float64(backup.RPOMinutes), "minutes", true))
	}
	if backup.RTOMinutes > 0 {
		policy.Objectives = append(policy.Objectives, deploymentObjective(slo.ObjectiveRTOMinutes,
			float64(backup.RTOMinutes), "minutes", true))
	}
	if backup != (config.BackupConfig{}) {
		policy.Objectives = append(policy.Objectives, deploymentObjective(slo.ObjectiveBackupSuccessRatePercent,
			100, "percent", false))
	}
	return &SLOSnapshotHandler{
		policy: policy, windowKind: cfg.Window,
		successMetric: cfg.Availability.SuccessMetric,
		totalMetric:   cfg.Availability.TotalMetric,
		declared:      cfg.Objectives, backup: backup,
		metrics: metrics, backups: backups,
	}, nil
}

func deploymentObjective(kind slo.ObjectiveKind, target float64, unit string, lowerIsBetter bool) slo.ObjectivePolicy {
	return slo.ObjectivePolicy{
		Kind: kind, Target: target, Unit: unit, LowerIsBetter: lowerIsBetter,
		AtRiskThreshold: target, RunbookRef: backupRunbookRef,
	}
}

// GetSLOSnapshot evaluates one project's snapshot at request time.
func (h *SLOSnapshotHandler) GetSLOSnapshot(c *gin.Context) {
	ctx := c.Request.Context()
	projectID := c.Param("pid")
	asOf := time.Now().UTC()
	from := h.windowStart(asOf)

	names := make([]string, 0, len(h.declared)+2)
	for _, objective := range h.declared {
		names = append(names, objective.Metric)
	}
	names = append(names, h.successMetric, h.totalMetric)
	windows, err := h.metrics.LatestMetricWindows(ctx, projectID, names, from, asOf)
	if err != nil {
		staticErrorReply(c, 500, "INTERNAL_ERROR", "Telemetry could not be read")
		return
	}
	byMetric := make(map[string]store.MetricWindow, len(windows))
	for _, window := range windows {
		byMetric[window.Metric] = window
	}

	success, hasSuccess := byMetric[h.successMetric]
	total, hasTotal := byMetric[h.totalMetric]
	if !hasSuccess || !hasTotal || total.Sum <= 0 {
		// The frozen snapshot has no no_data state for availability —
		// refuse rather than fabricate 100%.
		staticErrorReply(c, 503, "SLO_AVAILABILITY_UNMEASURED",
			"No availability telemetry in the window; the snapshot refuses to invent a number")
		return
	}

	var inputs []slo.ObjectiveInput
	for _, objective := range h.declared {
		window, ok := byMetric[objective.Metric]
		if !ok || window.P95 == nil {
			continue // policy without input classifies no_data
		}
		inputs = append(inputs, slo.ObjectiveInput{
			Kind: slo.ObjectiveKind(objective.Kind), Measured: window.P95,
			Since: window.WindowStart.UTC().Format(time.RFC3339),
		})
	}
	if !h.appendDeploymentInputs(c, &inputs, from, asOf) {
		return
	}

	snapshot, err := slo.Evaluate(h.policy,
		slo.AvailabilityInput{Success: int64(success.Sum), Total: int64(total.Sum)},
		inputs, slo.Window{Kind: h.windowKind, From: from.Format(time.RFC3339), To: asOf.Format(time.RFC3339)},
		slo.DegradationInput{}, asOf.Format(time.RFC3339))
	if err != nil {
		staticErrorReply(c, 503, "SLO_AVAILABILITY_UNMEASURED", "The availability SLI failed validation")
		return
	}
	c.JSON(200, snapshot)
}

// appendDeploymentInputs measures the ledger-backed objectives: RPO
// from the newest verified backup's completion, and the backup
// success rate over the runs that happened in the window (verified vs
// failed-or-still-unverified; no runs means no_data, never 100%).
// appendDeploymentInputs reports success; false means the response
// was already written (fail-closed ledger read).
func (h *SLOSnapshotHandler) appendDeploymentInputs(c *gin.Context, inputs *[]slo.ObjectiveInput, from, asOf time.Time) bool {
	ctx := c.Request.Context()
	if h.backup.RPOMinutes > 0 {
		if latest, err := h.backups.LatestVerifiedBackup(ctx, "full", asOf); err == nil && latest != nil && latest.FinishedAt != nil {
			rpoMinutes := asOf.Sub(*latest.FinishedAt).Minutes()
			*inputs = append(*inputs, slo.ObjectiveInput{
				Kind: slo.ObjectiveRPOMinutes, Measured: &rpoMinutes,
				Since: latest.FinishedAt.UTC().Format(time.RFC3339),
			})
		}
	}
	if h.backup == (config.BackupConfig{}) {
		return true
	}
	verified, verifiedErr := h.backups.CountBackupRuns(ctx, "full", "verified", from, asOf)
	failed, failedErr := h.backups.CountBackupRuns(ctx, "full", "failed", from, asOf)
	completed, completedErr := h.backups.CountBackupRuns(ctx, "full", "completed", from, asOf)
	if verifiedErr != nil || failedErr != nil || completedErr != nil {
		staticErrorReply(c, 500, "INTERNAL_ERROR", "The backup ledger could not be read")
		return false
	}
	total := verified + failed + completed
	if total > 0 {
		rate := float64(verified) / float64(total) * 100
		*inputs = append(*inputs, slo.ObjectiveInput{
			Kind: slo.ObjectiveBackupSuccessRatePercent, Measured: &rate,
		})
	}
	return true
}

func (h *SLOSnapshotHandler) windowStart(asOf time.Time) time.Time {
	switch h.windowKind {
	case "rolling_7d":
		return asOf.Add(-7 * 24 * time.Hour)
	case "calendar_month":
		year, month, _ := asOf.Date()
		return time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	default:
		return asOf.Add(-30 * 24 * time.Hour)
	}
}
