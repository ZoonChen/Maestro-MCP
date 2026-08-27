package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/publicerror"
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
		invalidRequestReply(c)
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
		if err := service.ValidateTaskTestRequirements([]byte(body.TestRequirements)); err != nil {
			errorReply(c, &service.ValidationError{Code: "VALIDATION_INPUT_INVALID", Cause: err})
			return
		}
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

// GetNextTask handles POST /api/v1/projects/:id/tasks/next?role=&worker_id=.
// This is the atomic claim endpoint: finds and atomically claims the next queued task.
func (h *TaskHandler) GetNextTask(c *gin.Context) {
	pid := c.Param("id")
	role := c.Query("role")
	workerID := c.Query("worker_id")
	sessionID := c.Query("session_id")

	if workerID == "" {
		workerID = "default"
	}
	if sessionID == "" {
		sessionID = "api-" + role
	}

	// M0 has a shared development principal, but a claim session must still be
	// bound to the requested task role. M1 replaces this compatibility path with
	// the authenticated Runner/session context.
	if err := h.ensureNamedSession(c, pid, sessionID, role); err != nil {
		errorReply(c, err)
		return
	}

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
		invalidRequestReply(c)
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
		invalidRequestReply(c)
		return
	}

	// Bind a newly created compatibility session to the task's required role;
	// the removed literal "worker" was not a valid domain role and made the
	// real REST claim path unusable on a fresh database.
	taskToClaim, err := h.taskService.GetTask(c.Request.Context(), pid, tid)
	if err != nil {
		errorReply(c, err)
		return
	}
	if err := h.ensureNamedSession(c, pid, body.SessionID, taskToClaim.Role); err != nil {
		errorReply(c, err)
		return
	}

	task, err := h.taskService.ClaimTask(c.Request.Context(), pid, tid, body.SessionID, body.WorkerID)
	if err != nil {
		errorReply(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": task})
}

// HeartbeatTask handles POST /api/v1/projects/:id/tasks/:tid/heartbeat.
// The Lease ID, Lease version, and idempotency key are explicit concurrency
// inputs. M0 may derive Session/Worker from the durable task assignment; the
// service still proves the exact owner, epoch, live deadline, and Worker CAS.
func (h *TaskHandler) HeartbeatTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")
	var body struct {
		SessionID      string `json:"session_id"`
		WorkerID       string `json:"worker_id"`
		LeaseID        string `json:"lease_id" binding:"required"`
		LeaseVersion   int64  `json:"lease_version" binding:"required"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		invalidRequestReply(c)
		return
	}
	headerKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	bodyKey := strings.TrimSpace(body.IdempotencyKey)
	if headerKey != "" && bodyKey != "" && headerKey != bodyKey {
		errorReply(c, fmt.Errorf("HeartbeatTask: header/body idempotency key mismatch: %w", store.ErrInvalidParameter))
		return
	}
	if bodyKey == "" {
		bodyKey = headerKey
	}

	sessionID := strings.TrimSpace(body.SessionID)
	workerID := strings.TrimSpace(body.WorkerID)
	if sessionID == "" || workerID == "" {
		task, err := h.taskService.GetTask(c.Request.Context(), pid, tid)
		if err != nil {
			errorReply(c, err)
			return
		}
		if sessionID == "" && task.AssignedSessionID != nil {
			sessionID = *task.AssignedSessionID
		}
		if workerID == "" && task.AssignedWorkerID != nil {
			workerID = *task.AssignedWorkerID
		}
	}
	lease, err := h.taskService.HeartbeatTask(c.Request.Context(), pid, tid,
		sessionID, workerID, body.LeaseID, body.LeaseVersion, bodyKey)
	if err != nil {
		errorReply(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": lease})
}

// SubmitTask handles POST /api/v1/projects/:id/tasks/:tid/submit.
// Body: {summary?, session_id?}.
// Runs zero-trust validation. Missing worktree/evidence is a hard failure and
// never falls back to an unvalidated state transition.
func (h *TaskHandler) SubmitTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	var body struct {
		Summary   *string `json:"summary"`
		SessionID string  `json:"session_id"`
		WorkerID  string  `json:"worker_id"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		invalidRequestReply(c)
		return
	}

	// Missing compatibility identifiers are derived from the durable Task
	// assignment. A caller cannot make an unowned submission pass by inventing
	// another session/worker pair.
	task, err := h.taskService.GetTask(c.Request.Context(), pid, tid)
	if err != nil {
		errorReply(c, err)
		return
	}
	sessionID := body.SessionID
	if sessionID == "" && task.AssignedSessionID != nil {
		sessionID = *task.AssignedSessionID
	}
	workerID := body.WorkerID
	if workerID == "" && task.AssignedWorkerID != nil {
		workerID = *task.AssignedWorkerID
	}

	err = h.validationService.SubmitAndValidate(c.Request.Context(), pid, tid, sessionID, workerID, body.Summary)
	if err != nil {
		errorReply(c, err)
		return
	}

	task, _ = h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// BlockTask handles POST /api/v1/projects/:id/tasks/:tid/block.
// Body: {reason, session_id?}. If session_id is omitted, the durable task
// assignment is used. ReportBlocker still validates the exact live Lease.
func (h *TaskHandler) BlockTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	var body struct {
		Reason    string `json:"reason" binding:"required"`
		SessionID string `json:"session_id"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		invalidRequestReply(c)
		return
	}

	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		task, err := h.taskService.GetTask(c.Request.Context(), pid, tid)
		if err != nil {
			errorReply(c, err)
			return
		}
		if task.AssignedSessionID == nil || strings.TrimSpace(*task.AssignedSessionID) == "" {
			errorReply(c, fmt.Errorf("BlockTask: session_id is required when the task has no durable assignment: %w", store.ErrInvalidParameter))
			return
		}
		sessionID = *task.AssignedSessionID
	}

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
		invalidRequestReply(c)
		return
	}

	// Ensure the "api" session exists — ResolveBlocker logs activity using
	// the task's assigned_session_id which references agent_sessions.
	if err := h.ensureAPISession(c, pid); err != nil {
		errorReply(c, err)
		return
	}

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
		invalidRequestReply(c)
		return
	}

	// Ensure the verifier session exists before it is used in activity_log / verified_by.
	if err := h.ensureNamedSession(c, pid, body.SessionID, model.RoleVerifier); err != nil {
		errorReply(c, err)
		return
	}

	if err := h.taskService.SubmitVerification(c.Request.Context(), pid, body.SessionID, body.WorkerID, tid, body.Passed, body.Notes); err != nil {
		errorReply(c, err)
		return
	}

	task, _ := h.taskService.GetTask(c.Request.Context(), pid, tid) //nolint:errcheck // mutation already succeeded; best-effort read for response
	c.JSON(http.StatusOK, gin.H{"data": task})
}

// GetNextVerificationTask handles POST /api/v1/projects/:id/tasks/next-verification.
// Atomically creates an independent verification Lease for the next validating task.
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
	if err := h.ensureNamedSession(c, pid, verifierSessionID, model.RoleVerifier); err != nil {
		errorReply(c, err)
		return
	}

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
		invalidRequestReply(c)
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
// Admin escape hatch: rolls back an eligible task to queued.
func (h *TaskHandler) ForceRollbackTask(c *gin.Context) {
	pid := c.Param("id")
	tid := c.Param("tid")

	sessionID := "admin"
	if err := h.ensureAPISession(c, pid); err != nil {
		errorReply(c, err)
		return
	}

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
		invalidRequestReply(c)
		return
	}

	sessionID := "api"

	// Ensure the "api" session exists before writing activity_log with session_id FK.
	if err := h.ensureAPISession(c, pid); err != nil {
		errorReply(c, err)
		return
	}

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
func (h *TaskHandler) ensureAPISession(c *gin.Context, projectID string) error {
	return h.ensureNamedSession(c, projectID, "api", apiSessionRole)
}

// ensureNamedSession lazily creates a session with the given ID for the given
// project if it does not already exist. The session is created with the
// specified role and a "rest-api" client type.
func (h *TaskHandler) ensureNamedSession(c *gin.Context, projectID, sessionID, role string) error {
	sess := &model.AgentSession{
		ID:         sessionID,
		ProjectID:  projectID,
		Role:       role,
		ClientType: "rest-api",
		Capacity:   5,
		Status:     model.SessionStatusOnline,
	}
	_, err := h.sessionService.EnsureSession(c.Request.Context(), projectID, sess)
	if err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Error mapping helpers
// ---------------------------------------------------------------------------

// errorReply is the only service/store error-to-wire boundary for REST.
func errorReply(c *gin.Context, err error) {
	public := publicerror.Classify(err)
	publicerror.Log(err, public)
	c.JSON(public.HTTPStatus, gin.H{
		"error":          public.Message,
		"error_code":     public.Code,
		"correlation_id": public.CorrelationID,
	})
}

func invalidRequestReply(c *gin.Context) {
	errorReply(c, store.ErrInvalidParameter)
}

func staticErrorReply(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"error": message, "error_code": code,
		"correlation_id": publicerror.NewCorrelationID(),
	})
}
