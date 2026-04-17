package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TaskHandler handles REST API endpoints for task lifecycle management.
type TaskHandler struct {
	taskService       *service.TaskService
	validationService *service.ValidationService
	sessionService    *service.SessionService
}

// NewTaskHandler creates a new TaskHandler with the given service dependencies.
func NewTaskHandler(ts *service.TaskService, vs *service.ValidationService, ss *service.SessionService) *TaskHandler {
	return &TaskHandler{
		taskService:       ts,
		validationService: vs,
		sessionService:    ss,
	}
}

// CreateTask handles POST /api/v1/projects/:id/tasks.
// Body: {feature_id, title, description, role, allowed_directories, forbidden_patterns?,
//
//	required_apis?, dependencies?, test_requirements?, priority?}
func (h *TaskHandler) CreateTask(c *gin.Context) {
	pid := c.Param("id")

	var body struct {
		FeatureID          string `json:"feature_id" binding:"required"`
		Title              string `json:"title" binding:"required"`
		Description        string `json:"description" binding:"required"`
		Role               string `json:"role" binding:"required"`
		AllowedDirectories string `json:"allowed_directories" binding:"required"`
		ForbiddenPatterns  string `json:"forbidden_patterns"`
		RequiredAPIs       string `json:"required_apis"`
		Dependencies       string `json:"dependencies"`
		TestRequirements   string `json:"test_requirements"`
		Priority           string `json:"priority"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	t := &model.Task{
		ID:                 "T-" + uuid.New().String()[:8],
		ProjectID:          pid,
		FeatureID:          body.FeatureID,
		Title:              body.Title,
		Description:        body.Description,
		Role:               body.Role,
		AllowedDirectories: body.AllowedDirectories,
		Priority:           body.Priority,
	}

	if body.ForbiddenPatterns != "" {
		t.ForbiddenPatterns = []byte(body.ForbiddenPatterns)
	} else {
		t.ForbiddenPatterns = []byte("[]")
	}

	if body.RequiredAPIs != "" {
		t.RequiredAPIs = []byte(body.RequiredAPIs)
	} else {
		t.RequiredAPIs = []byte("[]")
	}

	if body.Dependencies != "" {
		t.Dependencies = []byte(body.Dependencies)
	} else {
		t.Dependencies = []byte("[]")
	}

	if body.TestRequirements != "" {
		t.TestRequirements = []byte(body.TestRequirements)
	}

	if err := h.taskService.CreateTask(c.Request.Context(), pid, t); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": t})
}

// ListTasks handles GET /api/v1/projects/:id/tasks?status=&role=&feature_id=.
func (h *TaskHandler) ListTasks(c *gin.Context) {
	pid := c.Param("id")

	filter := store.TaskFilter{
		Status:    c.Query("status"),
		Role:      c.Query("role"),
		FeatureID: c.Query("feature_id"),
	}

	tasks, err := h.taskService.ListTasks(c.Request.Context(), pid, filter)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": tasks})
}

// GetNextTask handles GET /api/v1/projects/:id/tasks/next?role=&worker_id=.
// This is the atomic claim endpoint: finds and atomically claims the next pending task.
func (h *TaskHandler) GetNextTask(c *gin.Context) {
	pid := c.Param("id")
	role := c.Query("role")
	workerID := c.Query("worker_id")

	if workerID == "" {
		workerID = "default"
	}

	// The session_id for REST API calls defaults to "api".
	// In production, this would be extracted from auth middleware.
	sessionID := "api"

	// Ensure the "api" session exists in the database before claiming.
	// The tasks table has a foreign key on assigned_session_id referencing agent_sessions,
	// so the session must exist for the UPDATE to succeed.
	h.ensureAPISession(c, pid)

	task, err := h.taskService.GetNextTask(c.Request.Context(), pid, sessionID, role, workerID)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": task})
}

// GetTask handles GET /api/v1/projects/:id/tasks/:tid.
func (h *TaskHandler) GetTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	task, err := h.taskService.GetTask(c.Request.Context(), pid, tid)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": task})
}

// UpdateTask handles PATCH /api/v1/projects/:id/tasks/:tid.
// Body: {title?, description?, allowed_directories?, priority?, dependencies?}.
func (h *TaskHandler) UpdateTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	task, err := h.taskService.GetTask(c.Request.Context(), pid, tid)
	if err != nil {
		errorReply(c, err)
		return
	}

	var body struct {
		Title              string `json:"title"`
		Description        string `json:"description"`
		AllowedDirectories string `json:"allowed_directories"`
		Priority           string `json:"priority"`
		Dependencies       string `json:"dependencies"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if body.Title != "" {
		task.Title = body.Title
	}
	if body.Description != "" {
		task.Description = body.Description
	}
	if body.AllowedDirectories != "" {
		task.AllowedDirectories = body.AllowedDirectories
	}
	if body.Priority != "" {
		task.Priority = body.Priority
	}
	if body.Dependencies != "" {
		task.Dependencies = json.RawMessage(body.Dependencies)
	}

	if err := h.taskService.UpdateTask(c.Request.Context(), pid, task); err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": task})
}

// ClaimTask handles POST /api/v1/projects/:id/tasks/:tid/claim.
// Body: {session_id, worker_id}.
func (h *TaskHandler) ClaimTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	var body struct {
		SessionID string `json:"session_id" binding:"required"`
		WorkerID  string `json:"worker_id" binding:"required"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Ensure the session exists (FK constraint on assigned_session_id).
	h.ensureNamedSession(c, pid, body.SessionID, "worker")

	task, err := h.taskService.ClaimTask(c.Request.Context(), pid, tid, body.SessionID, body.WorkerID)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": task})
}

// SubmitTask handles POST /api/v1/projects/:id/tasks/:tid/submit.
// Body: {summary?, session_id?}.
// Tries zero-trust validation (git diff + test execution). Falls back to a simplified
// submit (in_progress -> submitted) if the task has no worktree (e.g., during testing).
func (h *TaskHandler) SubmitTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	var body struct {
		Summary   *string `json:"summary"`
		SessionID string  `json:"session_id"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Use provided session_id, or default to "api" for REST API calls.
	sessionID := "api"
	if body.SessionID != "" {
		sessionID = body.SessionID
	}
	workerID := "api"

	// Try zero-trust validation first. If worktree is not available
	// (e.g., no git repo in test environments), fall back to simplified submit.
	err := h.validationService.SubmitAndValidate(c.Request.Context(), pid, tid, sessionID, workerID, body.Summary)
	if err != nil {
		if errors.Is(err, store.ErrWorktreeNotFound) {
			// No worktree — simplified submit without validation.
			result := &model.TaskResult{
				ID:      tid,
				Summary: body.Summary,
			}
			if err := h.taskService.SubmitTaskResult(c.Request.Context(), pid, tid, sessionID, workerID, result); err != nil {
				errorReply(c, err)
				return
			}
		} else {
			errorReply(c, err)
			return
		}
	}

	task, _ := h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// BlockTask handles POST /api/v1/projects/:id/tasks/:tid/block.
// Body: {reason}.
func (h *TaskHandler) BlockTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	var body struct {
		Reason string `json:"reason" binding:"required"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sessionID := "api"

	// Ensure the "api" session exists before writing activity_log with session_id FK.
	h.ensureAPISession(c, pid)

	if err := h.taskService.ReportBlocker(c.Request.Context(), pid, tid, sessionID, body.Reason); err != nil {
		errorReply(c, err)
		return
	}

	task, _ := h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// ResolveBlocker handles POST /api/v1/projects/:id/tasks/:tid/resolve.
// Body: {reassign?}.
func (h *TaskHandler) ResolveBlocker(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	var body struct {
		Reassign bool `json:"reassign"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Ensure the "api" session exists — ResolveBlocker logs activity using
	// the task's assigned_session_id which references agent_sessions.
	h.ensureAPISession(c, pid)

	if err := h.taskService.ResolveBlocker(c.Request.Context(), pid, tid, body.Reassign, ""); err != nil {
		errorReply(c, err)
		return
	}

	task, _ := h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// VerifyTask handles POST /api/v1/projects/:id/tasks/:tid/verify.
// Body: {session_id, worker_id, passed, notes?}.
func (h *TaskHandler) VerifyTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	var body struct {
		SessionID string `json:"session_id" binding:"required"`
		WorkerID  string `json:"worker_id" binding:"required"`
		Passed    bool   `json:"passed"`
		Notes     string `json:"notes"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Ensure the verifier session exists before it is used in activity_log / verified_by.
	h.ensureNamedSession(c, pid, body.SessionID, "verifier")

	if err := h.taskService.SubmitVerification(c.Request.Context(), pid, body.SessionID, body.WorkerID, tid, body.Passed, body.Notes); err != nil {
		errorReply(c, err)
		return
	}

	task, _ := h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// MergeTask handles POST /api/v1/projects/:id/tasks/:tid/merge.
// Transitions a ready_to_merge task to done via the service layer.
func (h *TaskHandler) MergeTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	sessionID := "api"

	// Ensure the "api" session exists before writing activity_log with session_id FK.
	h.ensureAPISession(c, pid)

	if err := h.taskService.MergeTask(c.Request.Context(), pid, tid, sessionID); err != nil {
		errorReply(c, err)
		return
	}

	task, _ := h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// GetNextVerificationTask handles GET /api/v1/projects/:id/tasks/next-verification.
// Atomically claims the next submitted task for verification (submitted -> verifying).
func (h *TaskHandler) GetNextVerificationTask(c *gin.Context) {
	pid := c.Param("id")

	verifierSessionID := c.Query("session_id")
	if verifierSessionID == "" {
		verifierSessionID = "verifier-api"
	}
	verifierWorkerID := c.Query("worker_id")
	if verifierWorkerID == "" {
		verifierWorkerID = "verifier-api"
	}

	// Ensure the verifier session exists before it is used in activity_log.
	h.ensureNamedSession(c, pid, verifierSessionID, "verifier")

	task, err := h.taskService.GetVerificationTask(c.Request.Context(), pid, verifierSessionID, verifierWorkerID)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": task})
}

// ResolveMergeConflict handles POST /api/v1/projects/:id/tasks/:tid/resolve-merge-conflict.
// Body: {action, reason?}.
func (h *TaskHandler) ResolveMergeConflict(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	var body struct {
		Action string `json:"action" binding:"required"`
		Reason string `json:"reason"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.taskService.ResolveMergeConflict(c.Request.Context(), pid, tid, body.Action, body.Reason); err != nil {
		errorReply(c, err)
		return
	}

	task, _ := h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// GetValidationHistory handles GET /api/v1/projects/:id/tasks/:tid/validation.
// Returns validation run history for a task.
func (h *TaskHandler) GetValidationHistory(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	runs, err := h.validationService.GetValidationHistory(c.Request.Context(), pid, tid)
	if err != nil {
		errorReply(c, err)
		return
	}

	// Ensure non-nil slice serializes as [] not null.
	if runs == nil {
		runs = []*model.ValidationRun{}
	}

	c.JSON(http.StatusOK, gin.H{"data": runs})
}

// GetTaskResult handles GET /api/v1/projects/:id/tasks/:tid/result.
// Returns the current (latest) task result.
func (h *TaskHandler) GetTaskResult(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	result, err := h.taskService.GetTaskResult(c.Request.Context(), pid, tid)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": result})
}

// GetTaskDiff handles GET /api/v1/projects/:id/tasks/:tid/diff.
// Returns the git diff for the task's worktree relative to the base commit.
func (h *TaskHandler) GetTaskDiff(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	diff, err := h.taskService.GetTaskDiff(c.Request.Context(), pid, tid)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"task_id": tid, "diff": diff}})
}

// ForceRollbackTask handles POST /api/v1/projects/:id/tasks/:tid/force-rollback.
// Admin escape hatch: rolls back a task from any non-terminal state to pending.
func (h *TaskHandler) ForceRollbackTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	sessionID := "admin"
	h.ensureAPISession(c, pid)

	if err := h.taskService.ForceRollback(c.Request.Context(), pid, tid, sessionID); err != nil {
		errorReply(c, err)
		return
	}

	task, _ := h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// CancelTask handles POST /api/v1/projects/:id/tasks/:tid/cancel.
// Body: {reason}.
func (h *TaskHandler) CancelTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	var body struct {
		Reason string `json:"reason" binding:"required"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sessionID := "api"

	// Ensure the "api" session exists before writing activity_log with session_id FK.
	h.ensureAPISession(c, pid)

	if err := h.taskService.CancelTask(c.Request.Context(), pid, tid, sessionID, body.Reason); err != nil {
		errorReply(c, err)
		return
	}

	task, _ := h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// ---------------------------------------------------------------------------
// REST API helpers
// ---------------------------------------------------------------------------

// apiSessionRole is the role used for the implicit "api" session created
// for REST API calls that need to claim or submit tasks.
const apiSessionRole = "coordinator"

// ensureAPISession lazily creates the "api" session for the given project
// if it does not already exist. This is needed because the tasks table has
// a foreign key on assigned_session_id referencing agent_sessions.
func (h *TaskHandler) ensureAPISession(c *gin.Context, projectID string) {
	h.ensureNamedSession(c, projectID, "api", apiSessionRole)
}

// ensureNamedSession lazily creates a session with the given ID for the given
// project if it does not already exist. The session is created with the
// specified role and a "rest-api" client type.
func (h *TaskHandler) ensureNamedSession(c *gin.Context, projectID, sessionID, role string) {
	sess := &model.AgentSession{
		ID:         sessionID,
		ProjectID:  projectID,
		Role:       role,
		ClientType: "rest-api",
		Capacity:   100,
		Status:     model.SessionStatusOnline,
	}
	created, err := h.sessionService.EnsureSession(c.Request.Context(), projectID, sess)
	if err != nil {
		slog.Error("ensureNamedSession: failed to ensure session", "session_id", sessionID, "error", err)
	} else if created {
		slog.Debug("ensureNamedSession: created new session", "session_id", sessionID, "project_id", projectID)
	}
}

// ---------------------------------------------------------------------------
// Error mapping helpers
// ---------------------------------------------------------------------------

// mapErrorToHTTP maps a service/store error to an appropriate HTTP status code.
// Uses errors.Is to correctly match wrapped errors from the service layer
// (e.g., fmt.Errorf("SubmitTaskResult: %w", store.ErrTaskStateInvalid)).
func mapErrorToHTTP(err error) int {
	if errors.Is(err, store.ErrTaskNotFound) || errors.Is(err, store.ErrProjectNotFound) ||
		errors.Is(err, store.ErrFeatureNotFound) || errors.Is(err, store.ErrSessionNotFound) ||
		errors.Is(err, store.ErrNoAvailableTask) || errors.Is(err, store.ErrWorktreeNotFound) ||
		errors.Is(err, store.ErrWorkerNotFound) || errors.Is(err, store.ErrContractNotFound) {
		return http.StatusNotFound
	}
	// 409 Conflict: state conflicts
	if errors.Is(err, store.ErrTaskStateInvalid) || errors.Is(err, store.ErrTaskAlreadyCancelled) {
		return http.StatusConflict
	}
	// 403 Forbidden: ownership/authorization violations
	if errors.Is(err, store.ErrTaskNotOwned) {
		return http.StatusForbidden
	}
	// 422 Unprocessable Entity: semantic validation failures
	if errors.Is(err, store.ErrBoundaryViolation) || errors.Is(err, store.ErrCoverageBelowMin) ||
		errors.Is(err, store.ErrCircularDependency) || errors.Is(err, store.ErrValidationFailed) {
		return http.StatusUnprocessableEntity
	}
	// 429 Too Many Requests: capacity limits
	if errors.Is(err, store.ErrSessionCapacityFull) {
		return http.StatusTooManyRequests
	}
	// 400 Bad Request: input/validation errors
	if errors.Is(err, store.ErrInvalidParameter) || errors.Is(err, store.ErrFeatureStatusInvalid) ||
		errors.Is(err, store.ErrDependencyNotReady) || errors.Is(err, store.ErrProjectAlreadyExists) ||
		errors.Is(err, store.ErrProjectNotBound) || errors.Is(err, store.ErrProjectAmbiguous) {
		return http.StatusBadRequest
	}
	// 412 Precondition Failed: dependency unmet
	if errors.Is(err, store.ErrTaskDependencyUnmet) {
		return http.StatusPreconditionFailed
	}
	// 500 Internal Server Error: infrastructure failures (explicit mapping for clarity)
	if errors.Is(err, store.ErrWorktreeCreateFailed) || errors.Is(err, store.ErrWorktreeCleanFailed) {
		return http.StatusInternalServerError
	}
	if errors.Is(err, store.ErrConcurrentConflict) || errors.Is(err, store.ErrMergeConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, store.ErrProjectArchived) {
		return http.StatusForbidden
	}
	if errors.Is(err, store.ErrTestExecutionFailed) {
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

// mapErrorCode maps a service/store error to a machine-readable error code string.
func mapErrorCode(err error) string {
	codes := []struct {
		err  error
		code string
	}{
		{store.ErrTaskNotFound, "TASK_NOT_FOUND"},
		{store.ErrProjectNotFound, "PROJECT_NOT_FOUND"},
		{store.ErrFeatureNotFound, "FEATURE_NOT_FOUND"},
		{store.ErrSessionNotFound, "SESSION_NOT_FOUND"},
		{store.ErrNoAvailableTask, "NO_AVAILABLE_TASK"},
		{store.ErrWorktreeNotFound, "WORKTREE_NOT_FOUND"},
		{store.ErrWorkerNotFound, "WORKER_NOT_FOUND"},
		{store.ErrContractNotFound, "CONTRACT_NOT_FOUND"},
		{store.ErrTaskStateInvalid, "TASK_STATE_INVALID"},
		{store.ErrTaskAlreadyCancelled, "TASK_ALREADY_CANCELLED"},
		{store.ErrTaskNotOwned, "TASK_NOT_OWNED"},
		{store.ErrBoundaryViolation, "BOUNDARY_VIOLATION"},
		{store.ErrCoverageBelowMin, "COVERAGE_BELOW_MIN"},
		{store.ErrCircularDependency, "CIRCULAR_DEPENDENCY"},
		{store.ErrValidationFailed, "VALIDATION_FAILED"},
		{store.ErrSessionCapacityFull, "SESSION_CAPACITY_FULL"},
		{store.ErrInvalidParameter, "INVALID_PARAMETER"},
		{store.ErrFeatureStatusInvalid, "FEATURE_STATUS_INVALID"},
		{store.ErrDependencyNotReady, "DEPENDENCY_NOT_READY"},
		{store.ErrProjectAlreadyExists, "PROJECT_ALREADY_EXISTS"},
		{store.ErrProjectNotBound, "PROJECT_NOT_BOUND"},
		{store.ErrProjectAmbiguous, "PROJECT_AMBIGUOUS"},
		{store.ErrTaskDependencyUnmet, "TASK_DEPENDENCY_UNMET"},
		{store.ErrWorktreeCreateFailed, "WORKTREE_CREATE_FAILED"},
		{store.ErrWorktreeCleanFailed, "WORKTREE_CLEAN_FAILED"},
		{store.ErrConcurrentConflict, "CONCURRENT_CONFLICT"},
		{store.ErrMergeConflict, "MERGE_CONFLICT"},
		{store.ErrProjectArchived, "PROJECT_ARCHIVED"},
		{store.ErrTestExecutionFailed, "TEST_EXECUTION_FAILED"},
	}
	for _, c := range codes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return "INTERNAL_ERROR"
}

// errorReply sends a structured JSON error response with status, message, and error_code.
func errorReply(c *gin.Context, err error) {
	code := mapErrorCode(err)
	status := mapErrorToHTTP(err)
	c.JSON(status, gin.H{
		"error":      err.Error(),
		"error_code": code,
	})
}
