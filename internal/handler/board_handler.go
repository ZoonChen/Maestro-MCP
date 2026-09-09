package handler

import (
	"net/http"
	"strconv"

	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
)

// BoardHandler handles REST API endpoints for the dashboard/board view.
type BoardHandler struct {
	taskService     *service.TaskService
	featureService  *service.FeatureService
	activityStore   store.ActivityLogStore
	projectService  *service.ProjectService
	worktreeService *service.WorktreeService
	sessionService  *service.SessionService
	platformDepths  PlatformDepthsProvider
}

// PlatformDepthsProvider exposes the platform backlog gauges the
// telemetry producer samples (M4-OBS-001 / G2): webhook inbox
// backlog, DLQ depth and outbox pending. Wired into
// GET /api/v1/metrics by the composition root; nil omits the section.
type PlatformDepthsProvider interface {
	LatestPlatformDepths() store.PlatformDepths
}

// SetPlatformDepths mounts the telemetry producer's latest depth
// snapshot into GET /api/v1/metrics (additive; existing keys intact).
func (h *BoardHandler) SetPlatformDepths(provider PlatformDepthsProvider) {
	h.platformDepths = provider
}

// NewBoardHandler creates a new BoardHandler with the given service dependencies.
func NewBoardHandler(ts *service.TaskService, fs *service.FeatureService, als store.ActivityLogStore, ps *service.ProjectService, ws *service.WorktreeService, ss *service.SessionService) *BoardHandler {
	return &BoardHandler{
		taskService:     ts,
		featureService:  fs,
		activityStore:   als,
		projectService:  ps,
		worktreeService: ws,
		sessionService:  ss,
	}
}

// GetBoard handles GET /api/v1/projects/:pid/board.
// Returns task counts grouped by status plus the feature list.
func (h *BoardHandler) GetBoard(c *gin.Context) {
	pid := c.Param("id")

	// Get task counts by status.
	counts, err := h.taskService.ListTasks(c.Request.Context(), pid, store.TaskFilter{})
	if err != nil {
		errorReply(c, err)
		return
	}

	statusCounts := make(map[string]int)
	for _, t := range counts {
		statusCounts[t.Status]++
	}

	// Get feature list.
	features, err := h.featureService.ListFeatures(c.Request.Context(), pid)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"task_counts":    statusCounts,
			"total_tasks":    len(counts),
			"features":       features,
			"total_features": len(features),
		},
	})
}

// GetActivity handles GET /api/v1/projects/:pid/board/activity?limit=&since=.
// Returns recent activity log entries for the project.
func (h *BoardHandler) GetActivity(c *gin.Context) {
	pid := c.Param("id")

	limit := 50 // default
	if l := c.Query("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	since := c.Query("since")

	logs, err := h.activityStore.List(c.Request.Context(), pid, limit, since)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": logs})
}

// GetOverview handles GET /api/v1/overview.
// Returns a global overview aggregating data across all projects with task counts.
func (h *BoardHandler) GetOverview(c *gin.Context) {
	projects, err := h.projectService.ListProjects(c.Request.Context(), false)
	if err != nil {
		errorReply(c, err)
		return
	}

	type ProjectSummary struct {
		ID         string         `json:"id"`
		Name       string         `json:"name"`
		Status     string         `json:"status"`
		TaskCounts map[string]int `json:"task_counts"`
	}

	summaries := make([]ProjectSummary, 0, len(projects))
	for _, p := range projects {
		taskCounts := make(map[string]int)
		tasks, err := h.taskService.ListTasks(c.Request.Context(), p.ID, store.TaskFilter{})
		if err == nil {
			for _, t := range tasks {
				taskCounts[t.Status]++
			}
		}
		summaries = append(summaries, ProjectSummary{
			ID:         p.ID,
			Name:       p.Name,
			Status:     p.Status,
			TaskCounts: taskCounts,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"total_projects": len(summaries),
			"projects":       summaries,
		},
	})
}

// GetMetrics handles GET /api/v1/metrics.
// Returns system-wide performance metrics and statistics.
func (h *BoardHandler) GetMetrics(c *gin.Context) {
	ctx := c.Request.Context()

	projects, err := h.projectService.ListProjects(ctx, false)
	if err != nil {
		errorReply(c, err)
		return
	}

	totalTasks := 0
	tasksByStatus := make(map[string]int)
	totalSessions := 0
	activeSessions := 0

	for _, p := range projects {
		// Task stats
		tasks, err := h.taskService.ListTasks(ctx, p.ID, store.TaskFilter{})
		if err == nil {
			totalTasks += len(tasks)
			for _, t := range tasks {
				tasksByStatus[t.Status]++
			}
		}

		// Session stats
		sessions, err := h.sessionService.ListSessions(ctx, p.ID)
		if err == nil {
			totalSessions += len(sessions)
			for _, s := range sessions {
				if s.Status == "online" {
					activeSessions++
				}
			}
		}
	}

	payload := gin.H{
		"total_projects":  len(projects),
		"total_tasks":     totalTasks,
		"tasks_by_status": tasksByStatus,
		"total_sessions":  totalSessions,
		"active_sessions": activeSessions,
	}
	if h.platformDepths != nil {
		payload["platform"] = platformDepthsPayload(h.platformDepths.LatestPlatformDepths())
	}
	c.JSON(http.StatusOK, gin.H{"data": payload})
}

// platformDepthsPayload projects the sampled platform gauges onto the
// metrics wire. Lag percentiles are null until a non-empty backlog
// produced one — never a zero that would read as "no lag".
func platformDepthsPayload(depths store.PlatformDepths) gin.H {
	lag := func(value *float64) any {
		if value == nil {
			return nil
		}
		return *value
	}
	return gin.H{
		"webhook_inbox_depth":    depths.InboxBacklog,
		"webhook_dlq_depth":      depths.DLQDepth,
		"webhook_outbox_pending": depths.OutboxPending,
		"inbox_lag_p95_seconds":  lag(depths.InboxLagP95Seconds),
		"outbox_lag_p95_seconds": lag(depths.OutboxLagP95Seconds),
	}
}

// TriggerWorktreeGC handles POST /api/v1/projects/:id/worktrees/gc.
// Triggers manual garbage collection of abandoned/stale worktrees.
func (h *BoardHandler) TriggerWorktreeGC(c *gin.Context) {
	pid := c.Param("id")

	if h.worktreeService == nil {
		errorReply(c, store.ErrRecoveryIntegrity)
		return
	}

	if err := h.worktreeService.GCWorktrees(c.Request.Context(), pid); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"project_id": pid, "status": "gc_completed"}})
}
