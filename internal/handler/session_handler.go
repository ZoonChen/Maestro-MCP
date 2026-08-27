package handler

import (
	"net/http"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/gin-gonic/gin"
)

// SessionHandler handles REST API endpoints for agent session management.
type SessionHandler struct {
	sessionService *service.SessionService
}

// NewSessionHandler creates a new SessionHandler with the given service dependency.
func NewSessionHandler(ss *service.SessionService) *SessionHandler {
	return &SessionHandler{
		sessionService: ss,
	}
}

// RegisterSession handles POST /api/v1/projects/:pid/sessions.
// Body: {id, role, client_type?, capacity?}.
func (h *SessionHandler) RegisterSession(c *gin.Context) {
	pid := c.Param("id")

	var body struct {
		ID         string `json:"id" binding:"required"`
		Role       string `json:"role" binding:"required"`
		ClientType string `json:"client_type"`
		Capacity   int    `json:"capacity"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		invalidRequestReply(c)
		return
	}

	sess := &model.AgentSession{
		ID:         body.ID,
		ProjectID:  pid,
		Role:       body.Role,
		ClientType: body.ClientType,
		Capacity:   body.Capacity,
	}

	if err := h.sessionService.RegisterSession(c.Request.Context(), pid, sess); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": sess})
}

// Heartbeat handles PUT /api/v1/projects/:pid/sessions/:sid/heartbeat.
func (h *SessionHandler) Heartbeat(c *gin.Context) {
	pid := c.Param("id")
	sid := c.Param("sid")

	if err := h.sessionService.UpdateHeartbeat(c.Request.Context(), pid, sid); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": sid, "status": "heartbeat_updated"}})
}

// ListSessions handles GET /api/v1/projects/:pid/sessions.
func (h *SessionHandler) ListSessions(c *gin.Context) {
	pid := c.Param("id")

	sessions, err := h.sessionService.ListSessions(c.Request.Context(), pid)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": sessions})
}

// GetSession handles GET /api/v1/projects/:pid/sessions/:sid.
func (h *SessionHandler) GetSession(c *gin.Context) {
	pid := c.Param("id")
	sid := c.Param("sid")

	sess, err := h.sessionService.GetSession(c.Request.Context(), pid, sid)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": sess})
}

// DisconnectSession handles DELETE /api/v1/projects/:pid/sessions/:sid.
func (h *SessionHandler) DisconnectSession(c *gin.Context) {
	pid := c.Param("id")
	sid := c.Param("sid")

	if err := h.sessionService.DisconnectSession(c.Request.Context(), pid, sid); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": sid, "status": model.SessionStatusOffline}})
}

// ForceReleaseSession handles POST /api/v1/projects/:pid/sessions/:sid/force-release.
// Admin escape hatch: forcefully releases a session and cleans up all workers/tasks.
func (h *SessionHandler) ForceReleaseSession(c *gin.Context) {
	pid := c.Param("id")
	sid := c.Param("sid")

	if err := h.sessionService.ForceRelease(c.Request.Context(), pid, sid); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": sid, "status": model.SessionStatusOffline}})
}

// ---------------------------------------------------------------------------
// Worker endpoints
// ---------------------------------------------------------------------------

// RegisterWorker handles POST /api/v1/projects/:pid/sessions/:sid/workers.
// Persists the worker to the database via SessionService.
func (h *SessionHandler) RegisterWorker(c *gin.Context) {
	pid := c.Param("id")
	sid := c.Param("sid")

	var body struct {
		ID     string `json:"id" binding:"required"`
		Status string `json:"status"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		invalidRequestReply(c)
		return
	}

	worker := &model.AgentWorker{
		ID:     body.ID,
		Status: "idle",
	}
	if body.Status != "" {
		worker.Status = body.Status
	}

	if err := h.sessionService.RegisterWorker(c.Request.Context(), pid, sid, worker); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": worker, "message": "worker registered"})
}

// ListWorkers handles GET /api/v1/projects/:pid/sessions/:sid/workers.
func (h *SessionHandler) ListWorkers(c *gin.Context) {
	pid := c.Param("id")
	sid := c.Param("sid")

	workers, err := h.sessionService.ListWorkers(c.Request.Context(), pid, sid)
	if err != nil {
		errorReply(c, err)
		return
	}

	if workers == nil {
		workers = []*model.AgentWorker{}
	}

	c.JSON(http.StatusOK, gin.H{"data": workers})
}

// RemoveWorker handles DELETE /api/v1/projects/:pid/sessions/:sid/workers/:wid.
func (h *SessionHandler) RemoveWorker(c *gin.Context) {
	pid := c.Param("id")
	sid := c.Param("sid")
	wid := c.Param("wid")

	if err := h.sessionService.ReleaseWorker(c.Request.Context(), pid, sid, wid); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": wid, "status": "removed"}})
}
