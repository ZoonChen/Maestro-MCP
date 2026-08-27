package handler

import (
	"net/http"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// FeatureHandler handles REST API endpoints for feature CRUD within a project.
type FeatureHandler struct {
	featureService *service.FeatureService
}

// NewFeatureHandler creates a new FeatureHandler with the given service dependency.
func NewFeatureHandler(fs *service.FeatureService) *FeatureHandler {
	return &FeatureHandler{
		featureService: fs,
	}
}

// CreateFeature handles POST /api/v1/projects/:pid/features.
// Body: {title, description?, reference_urls?, status?}.
func (h *FeatureHandler) CreateFeature(c *gin.Context) {
	pid := c.Param("id")

	var body struct {
		Title         string `json:"title" binding:"required"`
		Description   string `json:"description"`
		ReferenceURLs string `json:"reference_urls"`
		Status        string `json:"status"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		invalidRequestReply(c)
		return
	}

	f := &model.Feature{
		ID:            "F-" + uuid.New().String()[:8],
		ProjectID:     pid,
		Title:         body.Title,
		Description:   body.Description,
		ReferenceURLs: body.ReferenceURLs,
		Status:        body.Status,
	}

	if f.Status == "" {
		f.Status = model.FeatureStatusPlanning
	}

	if err := h.featureService.CreateFeature(c.Request.Context(), pid, f); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": f})
}

// ListFeatures handles GET /api/v1/projects/:pid/features.
func (h *FeatureHandler) ListFeatures(c *gin.Context) {
	pid := c.Param("id")

	features, err := h.featureService.ListFeatures(c.Request.Context(), pid)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": features})
}

// GetFeature handles GET /api/v1/projects/:pid/features/:id.
func (h *FeatureHandler) GetFeature(c *gin.Context) {
	pid := c.Param("id")
	id := c.Param("fid")

	f, err := h.featureService.GetFeature(c.Request.Context(), pid, id)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": f})
}

// UpdateFeature handles PATCH /api/v1/projects/:pid/features/:id.
// Body: {title?, description?, reference_urls?, status?}.
func (h *FeatureHandler) UpdateFeature(c *gin.Context) {
	pid := c.Param("id")
	id := c.Param("fid")

	f, err := h.featureService.GetFeature(c.Request.Context(), pid, id)
	if err != nil {
		errorReply(c, err)
		return
	}

	var body struct {
		Title         string `json:"title"`
		Description   string `json:"description"`
		ReferenceURLs string `json:"reference_urls"`
		Status        string `json:"status"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		invalidRequestReply(c)
		return
	}

	if body.Title != "" {
		f.Title = body.Title
	}
	if body.Description != "" {
		f.Description = body.Description
	}
	if body.ReferenceURLs != "" {
		f.ReferenceURLs = body.ReferenceURLs
	}
	if body.Status != "" {
		f.Status = body.Status
	}

	if err := h.featureService.UpdateFeature(c.Request.Context(), pid, f); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": f})
}
