package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/eval"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
)

// Evaluation read surface (EV-2 / G3-E2): the observation plane over
// the CLI-imported evaluation_records. G3 registered the gap as "评测
// 面无 HTTP API——观察/裁决面缺位"; this endpoint is that read face,
// mapped to the frozen quality.read permission like every other
// quality observation route. The trial-disambiguation projection of
// `maestro eval-import` (same (run, case, layer) key → "#t<k>" case-id
// suffixes) is part of the stored truth: run summaries count disambiguated
// trials as distinct case rows, mirroring the store's uniqueness plane.

// EvaluationStore is the handler's persistence contract, satisfied by
// the PostgreSQL evaluation store.
type EvaluationStore interface {
	RunsByProject(ctx context.Context, projectID string) ([]store.EvaluationRunSummary, error)
}

// EvaluationHandler serves the evaluation read endpoints.
type EvaluationHandler struct {
	store EvaluationStore
}

// NewEvaluationHandler builds the read-only evaluation handler.
func NewEvaluationHandler(evaluationStore EvaluationStore) *EvaluationHandler {
	return &EvaluationHandler{store: evaluationStore}
}

// ListProjectEvaluations answers the project's evaluation runs with
// their aggregated layer/verdict counts (listProjectEvaluations).
// Read-only: ingestion stays on the eval-import CLI (append-only,
// fail-closed file semantics that an HTTP surface must not weaken).
func (h *EvaluationHandler) ListProjectEvaluations(c *gin.Context) {
	projectID := c.Param("pid")
	summaries, err := h.store.RunsByProject(c.Request.Context(), projectID)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "evaluation runs list failed", "error", err.Error())
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Evaluations could not be listed")
		return
	}
	reply := make([]gin.H, 0, len(summaries))
	for index := range summaries {
		reply = append(reply, evaluationRunReply(&summaries[index]))
	}
	c.JSON(http.StatusOK, reply)
}

// evaluationRunReply shapes one run row. Digest fields are null when a
// run mixed values (a run binds exactly one dataset/adapter digest per
// the authority §3 run-freeze rule; a mixed read-back is surfaced, not
// silently flattened).
func evaluationRunReply(summary *store.EvaluationRunSummary) gin.H {
	return gin.H{
		"run_id":              summary.RunID,
		"dataset_digest":      nullableString(summary.DatasetDigest),
		"model_config_digest": nullableString(summary.ModelConfigDigest),
		"records":             summary.Records,
		"layer_counts":        layerCountsReply(summary.LayerCounts),
		"verdict_counts":      verdictCountsReply(summary.VerdictCounts),
		"first_record_at":     summary.FirstRecordAt.UTC().Format(time.RFC3339),
		"last_record_at":      summary.LastRecordAt.UTC().Format(time.RFC3339),
	}
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func layerCountsReply(counts map[eval.Layer]int64) gin.H {
	return gin.H{
		"quality":    counts[eval.LayerQuality],
		"trajectory": counts[eval.LayerTrajectory],
		"security":   counts[eval.LayerSecurity],
		"capability": counts[eval.LayerCapability],
	}
}

func verdictCountsReply(counts map[eval.Verdict]int64) gin.H {
	return gin.H{
		"pass":    counts[eval.VerdictPass],
		"fail":    counts[eval.VerdictFail],
		"error":   counts[eval.VerdictError],
		"blocked": counts[eval.VerdictBlocked],
		"skipped": counts[eval.VerdictSkipped],
	}
}
