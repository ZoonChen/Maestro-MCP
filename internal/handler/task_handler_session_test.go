package handler

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureNamedSessionUsesBoundedCapacityAndStopsOnIdentityConflict(t *testing.T) {
	database, err := store.NewSQLiteDB(":memory:")
	require.NoError(t, err)
	require.NoError(t, database.Init(context.Background()))
	t.Cleanup(func() { _ = database.Close() })
	db := database.DB()
	_, err = db.Exec(`INSERT INTO projects(id, name, workspace_path, status, config)
		VALUES ('project-handler', 'Handler Test', '/tmp/handler-test', 'active', '{}')`)
	require.NoError(t, err)

	sessions := store.NewSessionStore(db)
	sessionService := service.NewSessionService(
		sessions,
		store.NewWorkerStore(db),
		store.NewSQLiteTaskStore(db),
		store.NewWorktreeStore(db),
		store.NewAuditLogStore(db),
		nil,
	)
	handler := NewTaskHandler(nil, nil, sessionService)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	require.NoError(t, handler.ensureNamedSession(ctx, "project-handler", "rest-session", model.RoleFrontend))
	session, err := sessions.GetByID(context.Background(), "project-handler", "rest-session")
	require.NoError(t, err)
	assert.Equal(t, 5, session.Capacity)
	require.NoError(t, handler.ensureNamedSession(ctx, "project-handler", "rest-session", model.RoleFrontend))
	err = handler.ensureNamedSession(ctx, "project-handler", "rest-session", model.RoleBackend)
	require.True(t, errors.Is(err, store.ErrIdempotencyConflict))

	// The endpoint must return the stable conflict before reaching the claim
	// service; a failed identity bootstrap is never logged and ignored.
	router := gin.New()
	router.POST("/projects/:id/tasks/next", handler.GetNextTask)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost,
		"/projects/project-handler/tasks/next?role=backend&session_id=rest-session&worker_id=worker-1", nil)
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Contains(t, response.Body.String(), "IDEMPOTENCY_CONFLICT")
}

func TestBlockTaskUsesDurableAssignmentWhenSessionIDIsOmitted(t *testing.T) {
	fixture := newBlockTaskHandlerFixture(t, true)
	response := performBlockTaskRequest(fixture.handler, fixture.taskID,
		`{"reason":"waiting for dependency"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	task, err := fixture.taskStore.GetByID(context.Background(), fixture.projectID, fixture.taskID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusBlocked, task.Status)
	var syntheticAPISessions int
	require.NoError(t, fixture.db.QueryRow(`SELECT COUNT(*) FROM agent_sessions
		WHERE project_id = ? AND COALESCE(external_id, id) = 'api'`, fixture.projectID).Scan(&syntheticAPISessions))
	assert.Zero(t, syntheticAPISessions, "block must not manufacture a privileged compatibility session")
}

func TestBlockTaskRejectsExplicitSessionThatDoesNotOwnLease(t *testing.T) {
	fixture := newBlockTaskHandlerFixture(t, true)
	response := performBlockTaskRequest(fixture.handler, fixture.taskID,
		`{"reason":"forged blocker","session_id":"attacker-session"}`)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "TASK_NOT_OWNED")

	task, err := fixture.taskStore.GetByID(context.Background(), fixture.projectID, fixture.taskID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusExecuting, task.Status)
	var activeLeases int
	require.NoError(t, fixture.db.QueryRow(`SELECT COUNT(*) FROM task_leases
		WHERE project_id = ? AND task_id = ? AND status = 'active'`, fixture.projectID, fixture.taskID).Scan(&activeLeases))
	assert.Equal(t, 1, activeLeases)
}

func TestBlockTaskRequiresSessionWhenTaskHasNoDurableAssignment(t *testing.T) {
	fixture := newBlockTaskHandlerFixture(t, false)
	response := performBlockTaskRequest(fixture.handler, fixture.taskID,
		`{"reason":"missing identity"}`)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "INVALID_PARAMETER")
}

func TestHeartbeatTaskRESTDerivesDurableOwnerAndEnforcesLeaseVersion(t *testing.T) {
	fixture := newBlockTaskHandlerFixture(t, true)
	leaseID := uuid.NewString()
	expiresAt := time.Now().UTC().Add(15 * time.Second).Format("2006-01-02 15:04:05")
	_, err := fixture.db.Exec(`UPDATE task_leases SET id = ?, expires_at = ?
		WHERE project_id = ? AND task_id = ?`, leaseID, expiresAt, fixture.projectID, fixture.taskID)
	require.NoError(t, err)
	_, err = fixture.db.Exec(`UPDATE tasks SET active_lease_id = ?, lease_expires_at = ?
		WHERE project_id = ? AND id = ?`, leaseID, expiresAt, fixture.projectID, fixture.taskID)
	require.NoError(t, err)

	router := gin.New()
	router.POST("/projects/:id/tasks/:tid/heartbeat", fixture.handler.HeartbeatTask)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost,
		"/projects/"+fixture.projectID+"/tasks/"+fixture.taskID+"/heartbeat",
		bytes.NewBufferString(`{"lease_id":"`+leaseID+`","lease_version":1}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "heartbeat-handler-0001")
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"version":2`)
	assert.Contains(t, response.Body.String(), `"session_id":"owner-session"`)

	// A different operation using the consumed version is rejected with a
	// stable concurrency code; it cannot silently renew the Lease again.
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost,
		"/projects/"+fixture.projectID+"/tasks/"+fixture.taskID+"/heartbeat",
		bytes.NewBufferString(`{"lease_id":"`+leaseID+`","lease_version":1,"idempotency_key":"heartbeat-handler-0002"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "LEASE_VERSION_MISMATCH")
}

type blockTaskHandlerFixture struct {
	handler   *TaskHandler
	db        *sql.DB
	taskStore store.TaskStore
	projectID string
	taskID    string
}

func newBlockTaskHandlerFixture(t *testing.T, assigned bool) blockTaskHandlerFixture {
	t.Helper()
	database, err := store.NewSQLiteDB("")
	require.NoError(t, err)
	require.NoError(t, database.Init(context.Background()))
	t.Cleanup(func() { _ = database.Close() })
	db := database.DB()
	projectID, featureID, taskID := "project-block-handler", "feature-block-handler", "T-block-handler"
	_, err = db.Exec(`INSERT INTO projects(id, name, workspace_path, status, config)
		VALUES (?, 'Block Handler', '/tmp/block-handler', 'active', '{}')`, projectID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO features(id, project_id, title, description, status)
		VALUES (?, ?, 'Block Handler', 'fixture', 'active')`, featureID, projectID)
	require.NoError(t, err)

	taskStore := store.NewSQLiteTaskStore(db)
	task := &model.Task{
		ID: taskID, ProjectID: projectID, FeatureID: featureID,
		Title: "Block handler task", Description: "fixture", Role: model.RoleBackend,
		Status: model.TaskStatusQueued, AllowedDirectories: `["src/"]`,
		Dependencies: []byte(`[]`), Priority: model.PriorityNormal,
		CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if assigned {
		sessionID, workerID, leaseID := "owner-session", "owner-worker", "owner-lease"
		expiresAt := time.Now().UTC().Add(10 * time.Minute).Format("2006-01-02 15:04:05")
		_, err = db.Exec(`INSERT INTO agent_sessions
			(id, project_id, role, client_type, capacity, status, last_heartbeat)
			VALUES (?, ?, ?, 'test', 1, 'online', datetime('now'))`, sessionID, projectID, model.RoleBackend)
		require.NoError(t, err)
		task.Status = model.TaskStatusExecuting
		task.Version = 2
		task.LeaseEpoch = 1
		task.AssignedSessionID = &sessionID
		task.AssignedWorkerID = &workerID
		task.ActiveLeaseID = &leaseID
		task.LeaseExpiresAt = &expiresAt
		require.NoError(t, taskStore.Create(context.Background(), projectID, task))
		_, err = db.Exec(`INSERT INTO agent_workers
			(id, session_id, project_id, current_task_id, status, tasks_completed, version, last_active)
			VALUES (?, ?, ?, ?, 'busy', 0, 1, datetime('now'))`, workerID, sessionID, projectID, taskID)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO task_leases
			(id, project_id, task_id, session_id, worker_id, epoch, status, version, expires_at)
			VALUES (?, ?, ?, ?, ?, 1, 'active', 1, ?)`, leaseID, projectID, taskID, sessionID, workerID, expiresAt)
		require.NoError(t, err)
	} else {
		require.NoError(t, taskStore.Create(context.Background(), projectID, task))
	}

	taskService := service.NewTaskService(
		taskStore, nil, nil, nil, nil, nil, nil, nil, nil, nil, db, nil,
	)
	return blockTaskHandlerFixture{
		handler: NewTaskHandler(taskService, nil, nil), db: db, taskStore: taskStore,
		projectID: projectID, taskID: taskID,
	}
}

func performBlockTaskRequest(handler *TaskHandler, taskID, body string) *httptest.ResponseRecorder {
	router := gin.New()
	router.POST("/projects/:id/tasks/:tid/block", handler.BlockTask)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost,
		"/projects/project-block-handler/tasks/"+taskID+"/block", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	return response
}
