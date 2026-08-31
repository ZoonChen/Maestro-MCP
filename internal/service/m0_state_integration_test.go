package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestM0ClaimValidateVerifyStopsBeforeVerifiedMergeFact(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	workspace, _ := createTestGitRepository(t)
	_, err := svc.stores.db.ExecContext(ctx,
		`UPDATE projects SET workspace_path = ? WHERE id = ?`, workspace, testProjectID)
	require.NoError(t, err)

	profile := testCommandProfile(t, "coverage")
	profileDigest, err := profile.Digest()
	require.NoError(t, err)
	registry, err := NewCommandProfileRegistry([]CommandProfile{profile})
	require.NoError(t, err)
	svc.validSvc.testExecConfig = TestExecutionConfig{
		Profiles: registry, PolicyVersion: "3.0.0",
		PolicyDigest: "sha256:" + strings.Repeat("c", 64), AllowHostExecution: true,
	}

	task := newTestTask("T-m0-lifecycle")
	task.TestRequirements = validationRequirementsJSON(t, profile, profileDigest, "coverage.out", 80)
	require.NoError(t, svc.taskSvc.CreateTask(ctx, testProjectID, task))
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: "executor-session", Role: model.RoleBackend, ClientType: "codex", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, "executor-session", &model.AgentWorker{ID: "executor-worker"}))

	claimQueueVersion := readQueueVersion(t, svc)
	claimed, _, err := svc.taskSvc.GetNextTaskWithVersion(
		ctx, testProjectID, "executor-session", model.RoleBackend, "executor-worker",
		"claim-lifecycle-0001", claimQueueVersion,
	)
	require.NoError(t, err)
	require.Equal(t, model.TaskStatusExecuting, claimed.Status)
	require.NotNil(t, claimed.AssignedSessionID)
	assert.Equal(t, "executor-session", *claimed.AssignedSessionID)
	require.NotNil(t, claimed.ActiveLeaseID)
	assert.Equal(t, int64(2), claimed.Version)

	// An identical retry returns the same durable result, while a different
	// payload cannot reuse the key.
	retry, _, err := svc.taskSvc.GetNextTaskWithVersion(
		ctx, testProjectID, "executor-session", model.RoleBackend, "executor-worker",
		"claim-lifecycle-0001", claimQueueVersion,
	)
	require.NoError(t, err)
	assert.Equal(t, claimed.ID, retry.ID)
	_, _, err = svc.taskSvc.GetNextTaskWithVersion(
		ctx, testProjectID, "executor-session", model.RoleFrontend, "executor-worker",
		"claim-lifecycle-0001", claimQueueVersion,
	)
	require.ErrorIs(t, err, store.ErrIdempotencyConflict)
	require.ErrorIs(t, svc.taskSvc.SubmitTaskResult(
		ctx, testProjectID, task.ID, "executor-session", "executor-worker", &model.TaskResult{},
	), store.ErrOperationDisabled)
	stillExecuting, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusExecuting, stillExecuting.Status)
	assert.NotNil(t, stillExecuting.ActiveLeaseID)

	summary := "validated lifecycle"
	require.NoError(t, svc.validSvc.SubmitAndValidate(
		ctx, testProjectID, task.ID, "executor-session", "executor-worker", &summary,
	))
	validated, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusValidating, validated.Status)
	assert.Nil(t, validated.ActiveLeaseID)

	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: "verifier-session", Role: model.RoleVerifier, ClientType: "codex", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, "verifier-session", &model.AgentWorker{ID: "verifier-worker"}))
	verification, err := svc.taskSvc.GetVerificationTask(ctx, testProjectID, "verifier-session", "verifier-worker")
	require.NoError(t, err)
	require.NotNil(t, verification.VerifiedBy)
	assert.Equal(t, "verifier-session", *verification.VerifiedBy)
	// Local Runner validation is diagnostic only. M0 has no runtime CI ingest,
	// so this controlled fixture represents the authenticated merge-gate fact
	// that M2 will append from GitLab Pipeline/Job reconciliation.
	seedMergeGateValidationEvidence(t, svc.stores, task.ID)
	require.NoError(t, svc.taskSvc.SubmitVerification(
		ctx, testProjectID, "verifier-session", "verifier-worker", task.ID, true, "evidence accepted",
	))

	ready, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusReadyForHumanMerge, ready.Status)
	require.ErrorIs(t, svc.taskSvc.MergeTask(ctx, testProjectID, task.ID, "verifier-session"), store.ErrOperationDisabled)
	// The M2 contract opens the canonical edge; the GENERIC version-
	// guarded path stays closed without a merge fact (the M2 webhook
	// ingestion owns the fact-carrying transition).
	require.ErrorIs(t,
		svc.stores.taskStore.UpdateStatusFromVersion(ctx, testProjectID, task.ID, ready.Status, ready.Version, model.TaskStatusDone),
		store.ErrOperationDisabled,
	)
	require.ErrorIs(t, svc.taskSvc.ConfirmMergedFact(
		ctx, testProjectID, task.ID, "gitlab:event:1", strings.Repeat("a", 40),
	), store.ErrOperationDisabled)
	stillReady, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusReadyForHumanMerge, stillReady.Status)
	assert.Nil(t, stillReady.MergedFactID)
	var deniedConfirmations int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log
		 WHERE bound_project = ? AND target_task = ?
		   AND action = 'task.confirm_merged_fact' AND result = 'DENIED'`,
		testProjectID, task.ID,
	).Scan(&deniedConfirmations))
	assert.Equal(t, 1, deniedConfirmations)

	var transitions int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM state_history WHERE project_id = ? AND aggregate_type = 'task' AND aggregate_id = ?`,
		testProjectID, task.ID,
	).Scan(&transitions))
	assert.GreaterOrEqual(t, transitions, 5)
}

func TestM0ExpiredLeaseSurvivesOfflineCleanupThenRecovers(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	seedTestSession(t, svc.stores, "stale-owner")
	task := newTestTask("T-expiring")
	seedTaskWithActiveLease(t, svc.stores, task, "stale-owner", "worker-expiring")
	seedTestWorktree(t, svc.stores, task.ID)

	session, err := svc.sessSvc.GetSession(ctx, testProjectID, "stale-owner")
	require.NoError(t, err)
	require.NoError(t, svc.sessSvc.CleanupStaleSession(ctx, session))
	preserved, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusExecuting, preserved.Status)
	assert.NotNil(t, preserved.ActiveLeaseID)

	_, err = svc.stores.db.ExecContext(ctx,
		`UPDATE task_leases SET expires_at = datetime('now', '-1 second') WHERE project_id = ? AND task_id = ?`,
		testProjectID, task.ID)
	require.NoError(t, err)
	require.NoError(t, svc.sessSvc.RecoverExpiredLeases(ctx))
	recovered, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusNeedsHuman, recovered.Status)
	assert.Nil(t, recovered.ActiveLeaseID)
	worktree, err := svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.WorktreeStatusQuarantined, worktree.Status)
	worker, err := svc.stores.workerStore.GetByID(ctx, testProjectID, "stale-owner", "worker-expiring")
	require.NoError(t, err)
	assert.Equal(t, model.WorkerStatusLost, worker.Status)
	assert.Nil(t, worker.CurrentTaskID)
	var histories, invalidHistories int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ?`, testProjectID).Scan(&histories))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND (to_version <> from_version + 1 OR COALESCE(actor_id, '') = ''
		 OR reason = '' OR COALESCE(causation_id, '') = '')`, testProjectID).Scan(&invalidHistories))
	assert.Equal(t, 5, histories, "session, worker, lease, task and worktree must each have history")
	assert.Zero(t, invalidHistories)
}

func TestM0StartupRecoveryIsAtomicAndFailClosed(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	seedTestSession(t, svc.stores, "restart-owner")
	task := newTestTask("T-restart")
	seedTaskWithActiveLease(t, svc.stores, task, "restart-owner", "worker-restart")
	seedTestWorktree(t, svc.stores, task.ID)
	recovery := NewRecoveryService(svc.stores.db, svc.stores.projectStore)

	_, err := svc.stores.db.ExecContext(ctx, `CREATE TRIGGER test_recovery_failure
		BEFORE UPDATE ON agent_workers
		BEGIN SELECT RAISE(ABORT, 'INJECTED_RECOVERY_FAILURE'); END`)
	require.NoError(t, err)
	err = recovery.Run(ctx)
	require.Error(t, err)
	unchanged, getErr := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, getErr)
	assert.Equal(t, model.TaskStatusExecuting, unchanged.Status)
	assert.NotNil(t, unchanged.ActiveLeaseID)
	session, getErr := svc.sessSvc.GetSession(ctx, testProjectID, "restart-owner")
	require.NoError(t, getErr)
	assert.Equal(t, model.SessionStatusOnline, session.Status)

	_, err = svc.stores.db.ExecContext(ctx, `DROP TRIGGER test_recovery_failure`)
	require.NoError(t, err)
	require.NoError(t, recovery.Run(ctx))
	recovered, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusNeedsHuman, recovered.Status)
	assert.Nil(t, recovered.ActiveLeaseID)
	session, err = svc.sessSvc.GetSession(ctx, testProjectID, "restart-owner")
	require.NoError(t, err)
	assert.Equal(t, model.SessionStatusOffline, session.Status)
	var leaseStatus, workerStatus, worktreeStatus string
	var leaseVersion, workerVersion, worktreeVersion int64
	var workerTask sql.NullString
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT status, version FROM task_leases
		WHERE project_id = ? AND task_id = ?`, testProjectID, task.ID).Scan(&leaseStatus, &leaseVersion))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT status, version, current_task_id FROM agent_workers
		WHERE project_id = ? AND id = ?`, testProjectID, "worker-restart").Scan(&workerStatus, &workerVersion, &workerTask))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT status, version FROM worktrees
		WHERE project_id = ? AND task_id = ?`, testProjectID, task.ID).Scan(&worktreeStatus, &worktreeVersion))
	assert.Equal(t, model.LeaseStatusExpired, leaseStatus)
	assert.Equal(t, model.WorkerStatusLost, workerStatus)
	assert.False(t, workerTask.Valid)
	assert.Equal(t, model.WorktreeStatusQuarantined, worktreeStatus)
	var historyCount int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ?`, testProjectID).Scan(&historyCount))
	assert.Equal(t, 5, historyCount)

	// Recovery is safe to repeat and keeps already reconciled states stable.
	require.NoError(t, recovery.Run(ctx))
	again, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, recovered.Version, again.Version)
	againSession, err := svc.sessSvc.GetSession(ctx, testProjectID, "restart-owner")
	require.NoError(t, err)
	assert.Equal(t, session.Version, againSession.Version)
	var againLeaseVersion, againWorkerVersion, againWorktreeVersion int64
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT version FROM task_leases
		WHERE project_id = ? AND task_id = ?`, testProjectID, task.ID).Scan(&againLeaseVersion))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT version FROM agent_workers
		WHERE project_id = ? AND id = ?`, testProjectID, "worker-restart").Scan(&againWorkerVersion))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT version FROM worktrees
		WHERE project_id = ? AND task_id = ?`, testProjectID, task.ID).Scan(&againWorktreeVersion))
	assert.Equal(t, leaseVersion, againLeaseVersion)
	assert.Equal(t, workerVersion, againWorkerVersion)
	assert.Equal(t, worktreeVersion, againWorktreeVersion)
	var againHistoryCount int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ?`, testProjectID).Scan(&againHistoryCount))
	assert.Equal(t, historyCount, againHistoryCount)
}

func TestM0StartupRecoveryCompletesPendingCancellation(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	seedTestSession(t, svc.stores, "cancel-owner")
	task := newTestTask("T-restart-cancelling")
	seedTaskWithActiveLease(t, svc.stores, task, "cancel-owner", "cancel-worker")
	require.NoError(t, svc.taskSvc.CancelTask(ctx, testProjectID, task.ID, "cancel-owner", "stop"))

	require.NoError(t, NewRecoveryService(svc.stores.db, svc.stores.projectStore).Run(ctx))
	recovered, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusCancelled, recovered.Status)
	assert.Nil(t, recovered.ActiveLeaseID)
}

func TestM0ConcurrentWorkerRegistrationHonorsCapacity(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: "capacity-session", Role: model.RoleBackend, ClientType: "test", Capacity: 2,
	}))

	var successes atomic.Int32
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := svc.sessSvc.RegisterWorker(ctx, testProjectID, "capacity-session", &model.AgentWorker{
				ID: fmt.Sprintf("capacity-worker-%02d", i),
			})
			if err == nil {
				successes.Add(1)
				return
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	assert.Equal(t, int32(2), successes.Load())
	for err := range errs {
		assert.True(t, errors.Is(err, store.ErrSessionCapacityFull), "unexpected registration error: %v", err)
	}
	workers, err := svc.sessSvc.ListWorkers(ctx, testProjectID, "capacity-session")
	require.NoError(t, err)
	assert.Len(t, workers, 2)
}

func TestM0ClaimWorkspaceFailureCompensatesDurableAuthority(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	task := newTestTask("T-claim-compensate")
	require.NoError(t, svc.taskSvc.CreateTask(ctx, testProjectID, task))
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: "compensate-session", Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, "compensate-session", &model.AgentWorker{ID: "compensate-worker"}))

	_, _, err := svc.taskSvc.GetNextTaskWithVersion(
		ctx, testProjectID, "compensate-session", model.RoleBackend, "compensate-worker",
		"claim-compensate-1", readQueueVersion(t, svc),
	)
	require.Error(t, err)
	compensated, getErr := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, getErr)
	assert.Equal(t, model.TaskStatusQueued, compensated.Status)
	assert.Nil(t, compensated.ActiveLeaseID)
	worker, getErr := svc.stores.workerStore.GetByID(ctx, testProjectID, "compensate-session", "compensate-worker")
	require.NoError(t, getErr)
	assert.Equal(t, model.WorkerStatusIdle, worker.Status)
	assert.Nil(t, worker.CurrentTaskID)
	var activeLeases, idempotencyRows int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_leases WHERE project_id = ? AND task_id = ? AND status = 'active'`,
		testProjectID, task.ID).Scan(&activeLeases))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM idempotency_records WHERE project_id = ? AND key = ?`,
		testProjectID, "claim-compensate-1").Scan(&idempotencyRows))
	assert.Zero(t, activeLeases)
	assert.Zero(t, idempotencyRows)
}
