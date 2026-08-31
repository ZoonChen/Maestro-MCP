package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
)

// GitLab registry REST surface of the frozen control-plane.yaml GitLab
// tag (M2-GL-001): the approved HTTPS host registry and the project
// numeric mapping. Secrets enter only as references and never leave
// the sanitized projections; the connector's outbound API client and
// reconciliation land with the next slice.

// GitLabStore is the handler's persistence contract.
type GitLabStore interface {
	ListInstances(ctx context.Context) ([]store.InstanceView, error)
	CreateInstance(ctx context.Context, baseURL, displayName, botCredentialRef, webhookSecretRef string) (store.InstanceView, error)
	GetMapping(ctx context.Context, projectID string) (*store.MappingView, error)
	PutMapping(ctx context.Context, projectID, instanceID string, gitlabProjectID int64, targetBranch string, expectedVersion int64) (*store.MappingView, error)
	MergeRequestProjection(ctx context.Context, projectID string, mrIID int64) (map[string]any, bool, error)
	ProjectExists(ctx context.Context, projectID string) (bool, error)
}

// GitLabHandler serves the registry endpoints.
type GitLabHandler struct {
	store GitLabStore
}

// NewGitLabHandler builds the handler.
func NewGitLabHandler(gitlabStore GitLabStore) *GitLabHandler {
	return &GitLabHandler{store: gitlabStore}
}

type instanceCreateBody struct {
	BaseURL          string `json:"base_url"`
	BotSecretRef     string `json:"bot_secret_ref"`
	WebhookSecretRef string `json:"webhook_secret_ref"`
	DisplayName      string `json:"display_name"`
}

// ListInstances returns the sanitized registry.
func (h *GitLabHandler) ListInstances(c *gin.Context) {
	instances, err := h.store.ListInstances(c.Request.Context())
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Instances could not be listed")
		return
	}
	c.JSON(http.StatusOK, instances)
}

// CreateInstance registers an approved HTTPS host
// (createGitLabInstance: If-None-Match required, 409 on duplicates).
func (h *GitLabHandler) CreateInstance(c *gin.Context) {
	if c.GetHeader("Idempotency-Key") == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A valid Idempotency-Key is required")
		return
	}
	if c.GetHeader("If-None-Match") != "*" {
		staticErrorReply(c, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED", "If-None-Match:* is required")
		return
	}
	var body instanceCreateBody
	if err := c.ShouldBindJSON(&body); err != nil || body.BaseURL == "" ||
		body.BotSecretRef == "" || body.WebhookSecretRef == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Instance body does not match the contract")
		return
	}
	displayName := body.DisplayName
	if displayName == "" {
		displayName = body.BaseURL
	}
	view, err := h.store.CreateInstance(c.Request.Context(), body.BaseURL, displayName, body.BotSecretRef, body.WebhookSecretRef)
	switch {
	case errors.Is(err, store.ErrInstanceExists):
		staticErrorReply(c, http.StatusConflict, "INSTANCE_EXISTS", "An instance with this base_url already exists")
		return
	case err != nil:
		staticErrorReply(c, http.StatusUnprocessableEntity, "INSTANCE_INVALID", err.Error())
		return
	}
	c.JSON(http.StatusCreated, view)
}

// GetMapping reads the project's current mapping (404 when absent).
func (h *GitLabHandler) GetMapping(c *gin.Context) {
	view, err := h.store.GetMapping(c.Request.Context(), c.Param("pid"))
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Mapping could not be read")
		return
	}
	if view == nil {
		staticErrorReply(c, http.StatusNotFound, "MAPPING_NOT_FOUND", "No GitLab mapping exists for this project")
		return
	}
	c.Header("ETag", quoteVersion(view.Version))
	c.JSON(http.StatusOK, view)
}

type mappingPutBody struct {
	GitLabInstanceID string `json:"gitlab_instance_id"`
	GitLabProjectID  int64  `json:"gitlab_project_numeric_id"`
	TargetBranch     string `json:"target_branch"`
}

// PutMapping creates or replaces the mapping
// (putGitLabProjectMapping: If-None-Match:* creates, If-Match replaces).
func (h *GitLabHandler) PutMapping(c *gin.Context) {
	projectID := c.Param("pid")
	if c.GetHeader("Idempotency-Key") == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A valid Idempotency-Key is required")
		return
	}
	var body mappingPutBody
	if err := c.ShouldBindJSON(&body); err != nil || body.GitLabInstanceID == "" ||
		body.GitLabProjectID < 1 || body.TargetBranch == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Mapping body does not match the contract")
		return
	}

	created := false
	var expected int64
	if noneMatch := c.GetHeader("If-None-Match"); noneMatch == "*" {
		created = true
	} else if match, ok := parseIfMatch(c.GetHeader("If-Match")); ok {
		expected = match
	} else {
		staticErrorReply(c, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED", "If-Match or If-None-Match:* is required")
		return
	}
	if !created {
		current, err := h.store.GetMapping(c.Request.Context(), projectID)
		if err != nil {
			staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Mapping could not be read")
			return
		}
		if current == nil {
			staticErrorReply(c, http.StatusPreconditionFailed, "PRECONDITION_FAILED", "No mapping exists to replace")
			return
		}
	}

	view, err := h.store.PutMapping(c.Request.Context(), projectID, body.GitLabInstanceID, body.GitLabProjectID, body.TargetBranch, expected)
	switch {
	case errors.Is(err, store.ErrMappingConflict):
		staticErrorReply(c, http.StatusPreconditionFailed, "PRECONDITION_FAILED", "Mapping row version mismatch")
		return
	case err != nil:
		staticErrorReply(c, http.StatusUnprocessableEntity, "MAPPING_INVALID", err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.Header("ETag", quoteVersion(view.Version))
	c.JSON(status, view)
}

// GetMergeRequest reads the cached projection last verified against
// GitLab facts (getMergeRequestProjection).
func (h *GitLabHandler) GetMergeRequest(c *gin.Context) {
	iid, ok := parseMergeRequestIID(c.Param("iid"))
	if !ok {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "merge_request_iid must be a positive integer")
		return
	}
	projection, found, err := h.store.MergeRequestProjection(c.Request.Context(), c.Param("pid"), iid)
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Projection could not be read")
		return
	}
	if !found {
		staticErrorReply(c, http.StatusNotFound, "MERGE_REQUEST_NOT_FOUND", "Merge request is unknown in this project")
		return
	}
	c.JSON(http.StatusOK, projection)
}

func parseMergeRequestIID(raw string) (int64, bool) {
	var iid int64
	if raw == "" {
		return 0, false
	}
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		iid = iid*10 + int64(ch-'0')
		if iid > 1<<31 {
			return 0, false
		}
	}
	return iid, iid >= 1
}
