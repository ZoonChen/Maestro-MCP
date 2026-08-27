package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextServiceRequiredSourcesFailClosed(t *testing.T) {
	t.Run("missing dependency", func(t *testing.T) {
		svc := setupTestEnv(t)
		task := newTestTask("T-context-missing-dependency")
		task.Dependencies = json.RawMessage(`[{"task_id":"T-missing","require_state":"done"}]`)
		mustCreateTask(t, svc.stores.taskStore, task)

		_, err := NewContextService(svc.stores.taskStore, svc.stores.contractStore).
			GetTaskContext(context.Background(), testProjectID, task.ID)
		assertContextErrorCode(t, err, ContextErrorRequiredSourceMissing)
		require.ErrorIs(t, err, store.ErrTaskNotFound)
	})

	t.Run("missing required api contract", func(t *testing.T) {
		svc := setupTestEnv(t)
		task := newTestTask("T-context-missing-contract")
		task.RequiredAPIs = json.RawMessage(`[{"method":"GET","path":"/api/v1/missing"}]`)
		mustCreateTask(t, svc.stores.taskStore, task)

		_, err := NewContextService(svc.stores.taskStore, svc.stores.contractStore).
			GetTaskContext(context.Background(), testProjectID, task.ID)
		assertContextErrorCode(t, err, ContextErrorRequiredSourceMissing)
		require.ErrorIs(t, err, store.ErrContractNotFound)
	})

	for name, requiredAPIs := range map[string]json.RawMessage{
		"null":          json.RawMessage(`null`),
		"not array":     json.RawMessage(`{"method":"GET","path":"/api/v1/users"}`),
		"unknown field": json.RawMessage(`[{"method":"GET","path":"/api/v1/users","optional":true}]`),
		"bad method":    json.RawMessage(`[{"method":"TRACE","path":"/api/v1/users"}]`),
		"bad path":      json.RawMessage(`[{"method":"GET","path":"api/v1/users"}]`),
	} {
		t.Run("invalid required api "+name, func(t *testing.T) {
			svc := setupTestEnv(t)
			task := newTestTask("T-context-invalid-api")
			task.RequiredAPIs = requiredAPIs
			mustCreateTask(t, svc.stores.taskStore, task)

			_, err := NewContextService(svc.stores.taskStore, svc.stores.contractStore).
				GetTaskContext(context.Background(), testProjectID, task.ID)
			assertContextErrorCode(t, err, ContextErrorSourceInvalid)
		})
	}

	t.Run("contract storage error", func(t *testing.T) {
		svc := setupTestEnv(t)
		task := newTestTask("T-context-contract-storage")
		task.RequiredAPIs = json.RawMessage(`[{"method":"GET","path":"/api/v1/users"}]`)
		mustCreateTask(t, svc.stores.taskStore, task)
		_, err := svc.stores.db.ExecContext(context.Background(), `DROP TABLE api_contracts`)
		require.NoError(t, err)

		_, err = NewContextService(svc.stores.taskStore, svc.stores.contractStore).
			GetTaskContext(context.Background(), testProjectID, task.ID)
		assertContextErrorCode(t, err, ContextErrorBuildFailed)
		assert.NotErrorIs(t, err, store.ErrContractNotFound)
	})
}

func TestContextServiceReturnsCompleteRequiredContext(t *testing.T) {
	svc := setupTestEnv(t)
	dependency := newTestTask("T-context-dependency")
	dependencyTitle := "dependency fallback title"
	dependency.Title = dependencyTitle
	dependency.Status = model.TaskStatusCancelled
	mustCreateTask(t, svc.stores.taskStore, dependency)
	require.NoError(t, svc.stores.contractStore.Upsert(context.Background(), testProjectID, &model.APIContract{
		Method:     "POST",
		Path:       "/api/v1/orders",
		SourceFile: "openapi.json",
		ParsedAt:   time.Now().UTC().Format(time.RFC3339),
	}))
	task := newTestTask("T-context-complete")
	task.Dependencies = json.RawMessage(`[{"task_id":"T-context-dependency","require_state":"done"}]`)
	task.RequiredAPIs = json.RawMessage(`[{"method":"POST","path":"/api/v1/orders"}]`)
	mustCreateTask(t, svc.stores.taskStore, task)

	result, err := NewContextService(svc.stores.taskStore, svc.stores.contractStore).
		GetTaskContext(context.Background(), testProjectID, task.ID)
	require.NoError(t, err)
	require.Equal(t, task.ID, result.Task.ID)
	assert.Equal(t, dependencyTitle, result.DependencySummaries[dependency.ID])
	require.Len(t, result.APIContracts, 1)
	assert.Equal(t, testProjectID, result.APIContracts[0].ProjectID)
	assert.Equal(t, "POST", result.APIContracts[0].Method)
	assert.Equal(t, "/api/v1/orders", result.APIContracts[0].Path)

	longSummary := strings.Repeat("界", maxDependencySummaryChars+1)
	truncated := truncateSummary(longSummary, maxDependencySummaryChars)
	assert.True(t, strings.HasSuffix(truncated, "[TRUNCATED]"))
	assert.True(t, strings.HasPrefix(truncated, strings.Repeat("界", maxDependencySummaryChars)))
}

func TestContextServiceRevalidatesDependencyStateAfterClaim(t *testing.T) {
	svc := setupTestEnv(t)
	workspace, _ := createTestGitRepository(t)
	_, err := svc.stores.db.ExecContext(context.Background(),
		`UPDATE projects SET workspace_path = ? WHERE id = ?`, workspace, testProjectID)
	require.NoError(t, err)
	dependency := newTestTask("T-context-dependency-toctou")
	dependency.Status = model.TaskStatusValidating
	mustCreateTask(t, svc.stores.taskStore, dependency)
	task := newTestTask("T-context-dependent-claim")
	task.Dependencies = json.RawMessage(`[{"task_id":"T-context-dependency-toctou","require_state":"validating"}]`)
	require.NoError(t, svc.taskSvc.CreateTask(context.Background(), testProjectID, task))
	require.NoError(t, svc.sessSvc.RegisterSession(context.Background(), testProjectID, &model.AgentSession{
		ID: "dependency-session", Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(
		context.Background(), testProjectID, "dependency-session", &model.AgentWorker{ID: "dependency-worker"},
	))
	claimed, err := svc.taskSvc.GetNextTaskWithVersion(
		context.Background(), testProjectID, "dependency-session", model.RoleBackend, "dependency-worker",
		"dependency-claim-0001", readQueueVersion(t, svc),
	)
	require.NoError(t, err)
	require.Equal(t, task.ID, claimed.ID)

	// The scheduler observed validating, but the source becomes failed before
	// the ContextSet is built. Context construction must re-read and reject it.
	require.NoError(t, svc.stores.taskStore.UpdateStatusFromVersion(
		context.Background(), testProjectID, dependency.ID,
		model.TaskStatusValidating, dependency.Version, model.TaskStatusFailed,
	))
	_, err = NewContextService(svc.stores.taskStore, svc.stores.contractStore).
		GetTaskContext(context.Background(), testProjectID, claimed.ID)
	assertContextErrorCode(t, err, ContextErrorRequiredSourceMissing)
	require.ErrorIs(t, err, store.ErrDependencyNotReady)
}

func TestContextClaimCompensationIsAtomicAndCleanupIsExact(t *testing.T) {
	svc, contextService, worktrees, claimed, claimKey := newContextClaimFixture(t, "T-context-compensate")

	_, contextErr := contextService.GetTaskContext(context.Background(), testProjectID, claimed.ID)
	assertContextErrorCode(t, contextErr, ContextErrorRequiredSourceMissing)
	require.NoError(t, svc.taskSvc.CompensateContextFailure(
		context.Background(), claimed, "context-session", "context-worker", claimKey,
		ContextBuildErrorCode(contextErr), true,
	))

	stopped, err := svc.taskSvc.GetTask(context.Background(), testProjectID, claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusNeedsHuman, stopped.Status)
	assert.Nil(t, stopped.ActiveLeaseID)
	assert.Nil(t, stopped.AssignedSessionID)
	assert.Nil(t, stopped.AssignedWorkerID)
	require.NotNil(t, stopped.BlockerReason)
	assert.Contains(t, *stopped.BlockerReason, ContextErrorRequiredSourceMissing)

	var leaseStatus string
	require.NoError(t, svc.stores.db.QueryRowContext(context.Background(), `SELECT status
		FROM task_leases WHERE project_id = ? AND task_id = ?`, testProjectID, claimed.ID).Scan(&leaseStatus))
	assert.Equal(t, model.LeaseStatusReleased, leaseStatus)
	worker, err := svc.stores.workerStore.GetByID(
		context.Background(), testProjectID, "context-session", "context-worker",
	)
	require.NoError(t, err)
	assert.Equal(t, model.WorkerStatusIdle, worker.Status)
	assert.Nil(t, worker.CurrentTaskID)
	worktree, err := svc.stores.worktreeStore.GetByTaskID(context.Background(), testProjectID, claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, model.WorktreeStatusCleanupPending, worktree.Status)

	var idempotencyRows int
	require.NoError(t, svc.stores.db.QueryRowContext(context.Background(), `SELECT COUNT(*)
		FROM idempotency_records WHERE project_id = ? AND operation = 'task.claim' AND key = ?`,
		testProjectID, claimKey).Scan(&idempotencyRows))
	assert.Zero(t, idempotencyRows)
	var historyRows int
	require.NoError(t, svc.stores.db.QueryRowContext(context.Background(), `SELECT COUNT(*)
		FROM state_history WHERE project_id = ? AND causation_id = ?
		  AND aggregate_type IN ('task','lease','worker','worktree')
		  AND reason LIKE 'required context rejected:%'`,
		testProjectID, *claimed.ActiveLeaseID).Scan(&historyRows))
	assert.Equal(t, 4, historyRows)

	worktreePath := worktree.WorktreePath
	require.NoError(t, worktrees.CleanupPendingWorktree(context.Background(), testProjectID, claimed.ID))
	_, err = svc.stores.worktreeStore.GetByTaskID(context.Background(), testProjectID, claimed.ID)
	require.ErrorIs(t, err, store.ErrWorktreeNotFound)
	_, err = os.Stat(worktreePath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestContextClaimCompensationRollsBackEveryResourceOnFault(t *testing.T) {
	svc, contextService, _, claimed, claimKey := newContextClaimFixture(t, "T-context-rollback")
	_, contextErr := contextService.GetTaskContext(context.Background(), testProjectID, claimed.ID)
	assertContextErrorCode(t, contextErr, ContextErrorRequiredSourceMissing)

	_, err := svc.stores.db.ExecContext(context.Background(), `CREATE TRIGGER context_compensation_fault
		BEFORE UPDATE ON worktrees
		BEGIN SELECT RAISE(ABORT, 'INJECTED_CONTEXT_COMPENSATION_FAILURE'); END`)
	require.NoError(t, err)
	err = svc.taskSvc.CompensateContextFailure(
		context.Background(), claimed, "context-session", "context-worker", claimKey,
		ContextBuildErrorCode(contextErr), true,
	)
	require.Error(t, err)

	unchanged, getErr := svc.taskSvc.GetTask(context.Background(), testProjectID, claimed.ID)
	require.NoError(t, getErr)
	assert.Equal(t, model.TaskStatusExecuting, unchanged.Status)
	require.NotNil(t, unchanged.ActiveLeaseID)
	assert.Equal(t, *claimed.ActiveLeaseID, *unchanged.ActiveLeaseID)
	worker, getErr := svc.stores.workerStore.GetByID(
		context.Background(), testProjectID, "context-session", "context-worker",
	)
	require.NoError(t, getErr)
	assert.Equal(t, model.WorkerStatusBusy, worker.Status)
	require.NotNil(t, worker.CurrentTaskID)
	assert.Equal(t, claimed.ID, *worker.CurrentTaskID)
	worktree, getErr := svc.stores.worktreeStore.GetByTaskID(context.Background(), testProjectID, claimed.ID)
	require.NoError(t, getErr)
	assert.Equal(t, model.WorktreeStatusActive, worktree.Status)
	var leaseStatus string
	require.NoError(t, svc.stores.db.QueryRowContext(context.Background(), `SELECT status
		FROM task_leases WHERE project_id = ? AND task_id = ?`, testProjectID, claimed.ID).Scan(&leaseStatus))
	assert.Equal(t, model.LeaseStatusActive, leaseStatus)
	var idempotencyRows int
	require.NoError(t, svc.stores.db.QueryRowContext(context.Background(), `SELECT COUNT(*)
		FROM idempotency_records WHERE project_id = ? AND operation = 'task.claim' AND key = ?`,
		testProjectID, claimKey).Scan(&idempotencyRows))
	assert.Equal(t, 1, idempotencyRows)
}

func TestContextClaimCompensationRejectsOutputOnlyCodes(t *testing.T) {
	for _, code := range []string{
		ContextErrorCompensationFailed,
		ContextErrorCleanupPending,
		"UNKNOWN_CONTEXT_ERROR",
	} {
		t.Run(code, func(t *testing.T) {
			svc, _, _, claimed, claimKey := newContextClaimFixture(t, "T-context-invalid-code")
			err := svc.taskSvc.CompensateContextFailure(
				context.Background(), claimed, "context-session", "context-worker", claimKey,
				code, true,
			)
			require.ErrorIs(t, err, store.ErrInvalidParameter)
			unchanged, getErr := svc.taskSvc.GetTask(context.Background(), testProjectID, claimed.ID)
			require.NoError(t, getErr)
			assert.Equal(t, model.TaskStatusExecuting, unchanged.Status)
			require.NotNil(t, unchanged.ActiveLeaseID)
			var activeLeases int
			require.NoError(t, svc.stores.db.QueryRowContext(context.Background(), `SELECT COUNT(*)
				FROM task_leases WHERE project_id = ? AND task_id = ? AND status = 'active'`,
				testProjectID, claimed.ID).Scan(&activeLeases))
			assert.Equal(t, 1, activeLeases)
		})
	}
}

func newContextClaimFixture(
	t *testing.T,
	taskID string,
) (*testServices, *ContextService, *WorktreeService, *model.Task, string) {
	t.Helper()
	svc := setupTestEnv(t)
	workspace, _ := createTestGitRepository(t)
	_, err := svc.stores.db.ExecContext(context.Background(),
		`UPDATE projects SET workspace_path = ? WHERE id = ?`, workspace, testProjectID)
	require.NoError(t, err)
	task := newTestTask(taskID)
	task.RequiredAPIs = json.RawMessage(`[{"method":"GET","path":"/api/v1/required"}]`)
	require.NoError(t, svc.taskSvc.CreateTask(context.Background(), testProjectID, task))
	require.NoError(t, svc.sessSvc.RegisterSession(context.Background(), testProjectID, &model.AgentSession{
		ID: "context-session", Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(
		context.Background(), testProjectID, "context-session", &model.AgentWorker{ID: "context-worker"},
	))
	claimKey := "context-claim-0001"
	claimed, err := svc.taskSvc.GetNextTaskWithVersion(
		context.Background(), testProjectID, "context-session", model.RoleBackend, "context-worker",
		claimKey, readQueueVersion(t, svc),
	)
	require.NoError(t, err)
	return svc,
		NewContextService(svc.stores.taskStore, svc.stores.contractStore),
		NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db),
		claimed, claimKey
}

func assertContextErrorCode(t *testing.T, err error, expected string) {
	t.Helper()
	require.Error(t, err)
	var contextErr *ContextBuildError
	require.True(t, errors.As(err, &contextErr), "expected ContextBuildError, got %T: %v", err, err)
	assert.Equal(t, expected, contextErr.Code)
}
