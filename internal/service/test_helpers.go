package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/require"
)

// testStores holds all store instances needed for service tests.
type testStores struct {
	projectStore    store.ProjectStore
	featureStore    store.FeatureStore
	taskStore       store.TaskStore
	resultStore     store.TaskResultStore
	validationStore store.ValidationRunStore
	sessionStore    store.SessionStore
	workerStore     store.WorkerStore
	worktreeStore   store.WorktreeStore
	activityStore   store.ActivityLogStore
	auditStore      store.AuditLogStore
	contractStore   store.ContractStore
	db              *sql.DB
}

// testServices holds all service instances needed for tests.
type testServices struct {
	stores   *testStores
	taskSvc  *TaskService
	sessSvc  *SessionService
	validSvc *ValidationService
	featSvc  *FeatureService
	projSvc  *ProjectService
}

// setupTestEnv creates an in-memory SQLite database, initializes all stores and
// services, and seeds a test project + feature. Returns a testServices struct
// with everything wired together. Each call creates a fresh, isolated DB.
func setupTestEnv(t *testing.T) *testServices {
	t.Helper()

	// Create in-memory DB.
	db, err := store.NewSQLiteDB(":memory:")
	require.NoError(t, err)
	err = db.Init(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	sqlDB := db.DB()

	// Create all stores.
	stores := &testStores{
		projectStore:    store.NewSQLiteProjectStore(sqlDB),
		featureStore:    store.NewSQLiteFeatureStore(sqlDB),
		taskStore:       store.NewSQLiteTaskStore(sqlDB),
		resultStore:     store.NewSQLiteTaskResultStore(sqlDB),
		validationStore: store.NewSQLiteValidationRunStore(sqlDB),
		sessionStore:    store.NewSessionStore(sqlDB),
		workerStore:     store.NewWorkerStore(sqlDB),
		worktreeStore:   store.NewWorktreeStore(sqlDB),
		activityStore:   store.NewActivityLogStore(sqlDB),
		auditStore:      store.NewAuditLogStore(sqlDB),
		contractStore:   store.NewContractStore(sqlDB),
		db:              sqlDB,
	}

	// Create all services.
	svc := &testServices{stores: stores}

	svc.taskSvc = NewTaskService(
		stores.taskStore, stores.resultStore, stores.validationStore,
		stores.sessionStore, stores.workerStore, stores.worktreeStore,
		stores.activityStore, stores.auditStore, stores.projectStore,
		stores.featureStore, sqlDB, &noopEmitter{},
	)

	svc.sessSvc = NewSessionService(
		stores.sessionStore, stores.workerStore, stores.taskStore,
		stores.worktreeStore, stores.auditStore, &noopEmitter{},
	)

	svc.validSvc = NewValidationService(
		stores.taskStore, stores.resultStore, stores.validationStore,
		stores.worktreeStore, stores.activityStore, stores.projectStore,
		sqlDB, &noopEmitter{}, TestExecutionConfig{},
	)

	svc.featSvc = NewFeatureService(
		stores.featureStore, stores.taskStore, &noopEmitter{},
	)

	svc.projSvc = NewProjectService(
		stores.projectStore, stores.auditStore,
	)

	// Seed a test project and feature.
	seedTestProject(t, stores)
	seedTestFeature(t, stores)

	return svc
}

// Constants for seeded test entities.
const (
	testProjectID = "proj-test-001"
	testFeatureID = "feat-test-001"
)

// seedTestProject inserts a test project row.
func seedTestProject(t *testing.T, s *testStores) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, name, workspace_path, description, status, config, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		testProjectID, "Test Project", "/tmp/test-workspace", "Test project for service tests",
		model.ProjectStatusActive, "{}", now, now,
	)
	require.NoError(t, err)
}

// seedTestFeature inserts a test feature row.
func seedTestFeature(t *testing.T, s *testStores) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO features (id, project_id, title, description, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		testFeatureID, testProjectID, "Test Feature", "Feature for service tests",
		model.FeatureStatusActive, now, now,
	)
	require.NoError(t, err)
}

// seedTestSession inserts a test session row and returns the session ID.
func seedTestSession(t *testing.T, s *testStores, sessionID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_sessions (id, project_id, role, client_type, capacity, status, last_heartbeat, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, testProjectID, model.RoleBackend, "test", 5,
		model.SessionStatusOnline, now, now,
	)
	require.NoError(t, err)
}

// seedTestWorker inserts a test worker row.
func seedTestWorker(t *testing.T, s *testStores, sessionID, workerID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_workers (id, session_id, project_id, status, tasks_completed, last_active)
		VALUES (?, ?, ?, ?, ?, ?)`,
		workerID, sessionID, testProjectID, "idle", 0, now,
	)
	require.NoError(t, err)
}

// seedTestWorktree inserts a test worktree row for a task.
func seedTestWorktree(t *testing.T, s *testStores, taskID string) { //nolint:unused
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO worktrees (task_id, project_id, worktree_path, branch_name, base_commit, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		taskID, testProjectID, "/tmp/test-worktrees/"+taskID, "task/"+taskID,
		"abc123", model.WorktreeStatusActive, now,
	)
	require.NoError(t, err)
}

// newTestTask creates a minimal Task with sensible defaults for testing.
func newTestTask(id string) *model.Task {
	return &model.Task{
		ID:                 id,
		ProjectID:          testProjectID,
		FeatureID:          testFeatureID,
		Title:              "Test Task " + id,
		Description:        "Description for " + id,
		Role:               model.RoleBackend,
		Status:             model.TaskStatusPending,
		AllowedDirectories: `["src/"]`,
		Dependencies:       json.RawMessage(`[]`),
		Priority:           model.PriorityNormal,
		CreatedAt:          time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:          time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// mustCreateTask creates a task via store and fatals on error.
func mustCreateTask(t *testing.T, ts store.TaskStore, task *model.Task) {
	t.Helper()
	require.NoError(t, ts.Create(context.Background(), testProjectID, task))
}
