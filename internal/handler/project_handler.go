package handler

import (
	"encoding/json"
	"net/http"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ProjectHandler handles REST API endpoints for project CRUD.
type ProjectHandler struct {
	projectService *service.ProjectService
}

// NewProjectHandler creates a new ProjectHandler with the given service dependency.
func NewProjectHandler(ps *service.ProjectService) *ProjectHandler {
	return &ProjectHandler{
		projectService: ps,
	}
}

// ListProjects handles GET /api/v1/projects.
// Query param: include_archived (default "false").
func (h *ProjectHandler) ListProjects(c *gin.Context) {
	includeArchived := c.Query("include_archived") == "true"

	projects, err := h.projectService.ListProjects(c.Request.Context(), includeArchived)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": projects})
}

// CreateProject handles POST /api/v1/projects.
// Body: {name, workspace_path, description?, config?}.
func (h *ProjectHandler) CreateProject(c *gin.Context) {
	var body struct {
		Name          string              `json:"name" binding:"required"`
		WorkspacePath string              `json:"workspace_path" binding:"required"`
		Description   string              `json:"description"`
		Config        model.ProjectConfig `json:"config"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	p := &model.Project{
		ID:            "P-" + uuid.New().String()[:8],
		Name:          body.Name,
		WorkspacePath: body.WorkspacePath,
		Description:   body.Description,
		Status:        model.ProjectStatusActive,
	}

	// Marshal config to JSON if provided.
	if body.Config.DefaultTestCommand != nil || body.Config.DefaultCoverageFormat != nil {
		configBytes, _ := json.Marshal(body.Config) //nolint:errchkjson // ProjectConfig is a safe struct
		p.Config = configBytes
	}

	if err := h.projectService.CreateProject(c.Request.Context(), p); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": p})
}

// GetProject handles GET /api/v1/projects/:id.
func (h *ProjectHandler) GetProject(c *gin.Context) {
	id := c.Param("id")

	p, err := h.projectService.GetProject(c.Request.Context(), id)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": p})
}

// UpdateProject handles PATCH /api/v1/projects/:id.
// Body: {name?, description?, config?}.
func (h *ProjectHandler) UpdateProject(c *gin.Context) {
	id := c.Param("id")

	p, err := h.projectService.GetProject(c.Request.Context(), id)
	if err != nil {
		errorReply(c, err)
		return
	}

	var body struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Config      json.RawMessage `json:"config"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if body.Name != "" {
		p.Name = body.Name
	}
	if body.Description != "" {
		p.Description = body.Description
	}
	if len(body.Config) > 0 {
		p.Config = body.Config
	}

	if err := h.projectService.UpdateProject(c.Request.Context(), p); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": p})
}

// ArchiveProject handles POST /api/v1/projects/:id/archive.
func (h *ProjectHandler) ArchiveProject(c *gin.Context) {
	id := c.Param("id")

	if err := h.projectService.ArchiveProject(c.Request.Context(), id); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": id, "status": model.ProjectStatusArchived}})
}

// RestoreProject handles POST /api/v1/projects/:id/restore.
func (h *ProjectHandler) RestoreProject(c *gin.Context) {
	id := c.Param("id")

	if err := h.projectService.RestoreProject(c.Request.Context(), id); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": id, "status": model.ProjectStatusActive}})
}
