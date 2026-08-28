package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimNextTaskCompatibilityCreatesWorkerAndReturnsExistingAssignment(t *testing.T) {
	svc, task, _ := newClaimFaultFixture(t, true, false)
	ctx := context.Background()
	claimed, err := svc.taskSvc.GetNextTask(ctx, testProjectID, "claim-session", model.RoleBackend, "auto-worker")
	require.NoError(t, err)
	assert.Equal(t, task.ID, claimed.ID)

	// A retry from the already-busy Worker deterministically returns its current
	// assignment instead of manufacturing a second Lease.
	again, err := svc.taskSvc.GetNextTask(ctx, testProjectID, "claim-session", model.RoleBackend, "auto-worker")
	require.NoError(t, err)
	assert.Equal(t, claimed.ID, again.ID)
	var active int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_leases WHERE project_id = ? AND task_id = ? AND status = 'active'`,
		testProjectID, task.ID).Scan(&active))
	assert.Equal(t, 1, active)
}

func TestTaskLeaseEntryPointsFailClosedWithoutDatabaseOrLease(t *testing.T) {
	svc := setupTestEnv(t)
	require.NoError(t, svc.stores.db.Close())
	_, err := svc.taskSvc.GetNextTask(context.Background(), testProjectID,
		"session", model.RoleBackend, "worker")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read queue version")

	svc = setupTestEnv(t)
	task := newTestTask("T-no-lease")
	require.ErrorIs(t, svc.taskSvc.ensureWorktreeForClaim(context.Background(), task, "session"), store.ErrLeaseNotFound)
	require.ErrorIs(t, svc.taskSvc.compensateFailedClaim(
		context.Background(), task, "session", "worker", "", fmt.Errorf("workspace allocation failed"),
	), store.ErrLeaseNotFound)
}

func TestClaimNextTaskRejectsInvalidAuthorityAndQueueSnapshots(t *testing.T) {
	t.Run("idempotency key", func(t *testing.T) {
		svc := setupTestEnv(t)
		_, _, err := svc.taskSvc.GetNextTaskWithVersion(context.Background(), testProjectID,
			"session", model.RoleBackend, "worker", "short", 0)
		require.ErrorIs(t, err, store.ErrInvalidParameter)
	})
	t.Run("stale queue version", func(t *testing.T) {
		svc, _, version := newClaimFaultFixture(t, false, true)
		_, _, err := svc.taskSvc.GetNextTaskWithVersion(context.Background(), testProjectID,
			"claim-session", model.RoleBackend, "claim-worker", "claim-stale-version", version+1)
		require.ErrorIs(t, err, store.ErrConcurrentConflict)
	})
	t.Run("missing session", func(t *testing.T) {
		svc := setupTestEnv(t)
		task := newTestTask("T-claim-no-session")
		require.NoError(t, svc.taskSvc.CreateTask(context.Background(), testProjectID, task))
		version := readQueueVersion(t, svc)
		_, _, err := svc.taskSvc.GetNextTaskWithVersion(context.Background(), testProjectID,
			"missing", model.RoleBackend, "worker", "claim-missing-session", version)
		require.ErrorIs(t, err, store.ErrSessionNotFound)
	})
	t.Run("role mismatch", func(t *testing.T) {
		svc, _, version := newClaimFaultFixture(t, false, true)
		_, _, err := svc.taskSvc.GetNextTaskWithVersion(context.Background(), testProjectID,
			"claim-session", model.RoleFrontend, "claim-worker", "claim-role-mismatch", version)
		require.ErrorIs(t, err, store.ErrTaskNotOwned)
	})
	t.Run("capacity full", func(t *testing.T) {
		svc := setupTestEnv(t)
		ctx := context.Background()
		require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
			ID: "full-session", Role: model.RoleBackend, ClientType: "test", Capacity: 1,
		}))
		require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, "full-session", &model.AgentWorker{ID: "occupied"}))
		task := newTestTask("T-capacity-full")
		require.NoError(t, svc.taskSvc.CreateTask(ctx, testProjectID, task))
		_, _, err := svc.taskSvc.GetNextTaskWithVersion(ctx, testProjectID,
			"full-session", model.RoleBackend, "new-worker", "claim-capacity-full", readQueueVersion(t, svc))
		require.ErrorIs(t, err, store.ErrSessionCapacityFull)
	})
	t.Run("worker unavailable", func(t *testing.T) {
		svc, _, version := newClaimFaultFixture(t, false, true)
		_, err := svc.stores.db.Exec(`UPDATE agent_workers SET status = 'lost'
			WHERE project_id = ? AND id = ?`, testProjectID, "claim-worker")
		require.NoError(t, err)
		_, _, err = svc.taskSvc.GetNextTaskWithVersion(context.Background(), testProjectID,
			"claim-session", model.RoleBackend, "claim-worker", "claim-worker-unavailable", version)
		require.ErrorIs(t, err, store.ErrConcurrentConflict)
	})
	t.Run("no available task", func(t *testing.T) {
		svc := setupTestEnv(t)
		ctx := context.Background()
		require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
			ID: "empty-session", Role: model.RoleBackend, ClientType: "test", Capacity: 1,
		}))
		require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, "empty-session", &model.AgentWorker{ID: "empty-worker"}))
		_, _, err := svc.taskSvc.GetNextTaskWithVersion(ctx, testProjectID,
			"empty-session", model.RoleBackend, "empty-worker", "claim-empty-queue-1", readQueueVersion(t, svc))
		require.ErrorIs(t, err, store.ErrNoAvailableTask)
	})
	t.Run("malformed imported dependencies", func(t *testing.T) {
		svc := setupTestEnv(t)
		ctx := context.Background()
		require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
			ID: "bad-json-session", Role: model.RoleBackend, ClientType: "test", Capacity: 1,
		}))
		require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, "bad-json-session", &model.AgentWorker{ID: "bad-json-worker"}))
		task := newTestTask("T-bad-dependencies")
		task.Dependencies = []byte(`{not-json`)
		require.NoError(t, svc.stores.taskStore.Create(ctx, testProjectID, task))
		_, _, err := svc.taskSvc.GetNextTaskWithVersion(ctx, testProjectID,
			"bad-json-session", model.RoleBackend, "bad-json-worker", "claim-bad-json-0001", readQueueVersion(t, svc))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "select queued task")
	})
}

func TestClaimNextTaskRollsBackEveryTransactionalWriteStage(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
	}{
		{"queued transition", `CREATE TRIGGER fail_claim BEFORE UPDATE OF status ON tasks
			WHEN NEW.status = 'leased' BEGIN SELECT RAISE(ABORT, 'FAIL_CLAIM_STAGE'); END`},
		{"first history", `CREATE TRIGGER fail_claim BEFORE INSERT ON state_history
			BEGIN SELECT RAISE(ABORT, 'FAIL_CLAIM_STAGE'); END`},
		{"lease insert", `CREATE TRIGGER fail_claim BEFORE INSERT ON task_leases
			BEGIN SELECT RAISE(ABORT, 'FAIL_CLAIM_STAGE'); END`},
		{"executing transition", `CREATE TRIGGER fail_claim BEFORE UPDATE OF status ON tasks
			WHEN NEW.status = 'executing' BEGIN SELECT RAISE(ABORT, 'FAIL_CLAIM_STAGE'); END`},
		{"worker reservation", `CREATE TRIGGER fail_claim BEFORE UPDATE ON agent_workers
			WHEN NEW.status = 'busy' BEGIN SELECT RAISE(ABORT, 'FAIL_CLAIM_STAGE'); END`},
		{"activity append", `CREATE TRIGGER fail_claim BEFORE INSERT ON activity_log
			BEGIN SELECT RAISE(ABORT, 'FAIL_CLAIM_STAGE'); END`},
		{"idempotency append", `CREATE TRIGGER fail_claim BEFORE INSERT ON idempotency_records
			BEGIN SELECT RAISE(ABORT, 'FAIL_CLAIM_STAGE'); END`},
		{"queue CAS", `CREATE TRIGGER fail_claim BEFORE UPDATE ON project_queue_versions
			BEGIN SELECT RAISE(ABORT, 'FAIL_CLAIM_STAGE'); END`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, task, version := newClaimFaultFixture(t, false, true)
			_, err := svc.stores.db.Exec(tt.trigger)
			require.NoError(t, err)
			_, _, err = svc.taskSvc.GetNextTaskWithVersion(context.Background(), testProjectID,
				"claim-session", model.RoleBackend, "claim-worker", "claim-stage-failure", version)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "FAIL_CLAIM_STAGE")
			assertClaimTransactionRolledBack(t, svc, task.ID)
		})
	}
}

func TestClaimNextTaskJoinsWorkspaceAndCompensationFailures(t *testing.T) {
	svc, task, version := newClaimFaultFixture(t, false, true)
	_, err := svc.stores.db.Exec(`CREATE TRIGGER fail_compensation BEFORE UPDATE ON task_leases
		WHEN NEW.status = 'released' BEGIN SELECT RAISE(ABORT, 'FAIL_COMPENSATION'); END`)
	require.NoError(t, err)
	_, _, err = svc.taskSvc.GetNextTaskWithVersion(context.Background(), testProjectID,
		"claim-session", model.RoleBackend, "claim-worker", "claim-compensation-fails", version)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace allocation")
	assert.Contains(t, err.Error(), "FAIL_COMPENSATION")
	got, getErr := svc.taskSvc.GetTask(context.Background(), testProjectID, task.ID)
	require.NoError(t, getErr)
	assert.Equal(t, model.TaskStatusExecuting, got.Status)
	assert.NotNil(t, got.ActiveLeaseID)
}

func TestFailedWorkspaceAllocationCompensatesEveryClaimResource(t *testing.T) {
	svc, task, version := newClaimFaultFixture(t, false, true)
	ctx := context.Background()
	_, _, err := svc.taskSvc.GetNextTaskWithVersion(ctx, testProjectID,
		"claim-session", model.RoleBackend, "claim-worker", "claim-workspace-failure", version)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace allocation")
	after, getErr := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, getErr)
	assert.Equal(t, model.TaskStatusQueued, after.Status)
	assert.Nil(t, after.ActiveLeaseID)
	var leaseID, leaseStatus string
	var leaseVersion int64
	require.NoError(t, svc.stores.db.QueryRow(`SELECT id, status, version FROM task_leases
		WHERE project_id = ? AND task_id = ?`, testProjectID, task.ID).Scan(&leaseID, &leaseStatus, &leaseVersion))
	assert.Equal(t, model.LeaseStatusReleased, leaseStatus)
	assert.Equal(t, int64(2), leaseVersion)
	worker, getErr := svc.stores.workerStore.GetByID(ctx, testProjectID, "claim-session", "claim-worker")
	require.NoError(t, getErr)
	assert.Equal(t, model.WorkerStatusIdle, worker.Status)
	assert.Nil(t, worker.CurrentTaskID)
	var physicalSessionID string
	require.NoError(t, svc.stores.db.QueryRow(`SELECT id FROM agent_sessions
		WHERE project_id = ? AND COALESCE(external_id, id) = ?`, testProjectID, "claim-session").Scan(&physicalSessionID))
	for _, expected := range []struct {
		typeName, id string
		count        int
	}{
		{"task", task.ID, 3},
		{"lease", leaseID, 1},
		{"worker", physicalSessionID + "/claim-worker", 2},
	} {
		var count int
		require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
			WHERE project_id = ? AND aggregate_type = ? AND aggregate_id = ?`,
			testProjectID, expected.typeName, expected.id).Scan(&count))
		assert.Equal(t, expected.count, count, "%s/%s", expected.typeName, expected.id)
	}
	var active int
	require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM task_leases
		WHERE project_id = ? AND task_id = ? AND status = 'active'`, testProjectID, task.ID).Scan(&active))
	assert.Zero(t, active)
}

func TestEnsureExistingClaimWorkspaceRevalidatesOwnerAndRepository(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	workspace, base := createTestGitRepository(t)
	_, err := svc.stores.db.Exec(`UPDATE projects SET workspace_path = ? WHERE id = ?`, workspace, testProjectID)
	require.NoError(t, err)
	worktreePath, err := createWorktree(ctx, workspace, "T-existing-workspace")
	require.NoError(t, err)
	sessionID, leaseID := "workspace-owner", "workspace-lease"
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: sessionID, Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	task := newTestTask("T-existing-workspace")
	task.Status, task.LeaseEpoch, task.ActiveLeaseID = model.TaskStatusExecuting, 1, &leaseID
	mustCreateTask(t, svc.stores.taskStore, task)
	_, err = svc.stores.worktreeStore.Create(ctx, testProjectID, &model.Worktree{
		TaskID: task.ID, ProjectID: testProjectID, SessionID: &sessionID,
		WorktreePath: worktreePath, BranchName: "task/" + task.ID, BaseCommit: base,
		Status: model.WorktreeStatusActive, Generation: 1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	require.ErrorIs(t, svc.taskSvc.ensureWorktreeForClaim(ctx, task, "wrong-owner"), store.ErrConcurrentConflict)
	require.NoError(t, svc.taskSvc.ensureWorktreeForClaim(ctx, task, sessionID))

	_, err = svc.stores.db.Exec(`UPDATE worktrees SET worktree_path = ? WHERE project_id = ? AND task_id = ?`,
		t.TempDir(), testProjectID, task.ID)
	require.NoError(t, err)
	err = svc.taskSvc.ensureWorktreeForClaim(ctx, task, sessionID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity validation")
}

func TestBlockResolveFreshClaimRebindsExactWorktreeGeneration(t *testing.T) {
	svc, task, _ := newClaimFaultFixture(t, true, true)
	ctx := context.Background()

	first, err := svc.taskSvc.GetNextTask(ctx, testProjectID, "claim-session", model.RoleBackend, "claim-worker")
	require.NoError(t, err)
	require.NotNil(t, first.ActiveLeaseID)
	firstLeaseID := *first.ActiveLeaseID
	firstWorktree, err := svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), firstWorktree.Generation)
	changedPath := filepath.Join(firstWorktree.WorktreePath, "src", "preserved.go")
	require.NoError(t, os.WriteFile(changedPath, []byte("package main\n\nvar preserved = true\n"), 0o600))

	require.NoError(t, svc.taskSvc.ReportBlocker(ctx, testProjectID, task.ID, "claim-session", "waiting for dependency"))
	blocked, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	require.Equal(t, model.TaskStatusBlocked, blocked.Status)
	require.Nil(t, blocked.ActiveLeaseID)
	require.NoError(t, svc.taskSvc.ResolveBlocker(ctx, testProjectID, task.ID, false, "dependency available"))

	second, err := svc.taskSvc.GetNextTask(ctx, testProjectID, "claim-session", model.RoleBackend, "claim-worker")
	require.NoError(t, err)
	require.NotNil(t, second.ActiveLeaseID)
	require.NotEqual(t, firstLeaseID, *second.ActiveLeaseID)
	require.Equal(t, int64(2), second.LeaseEpoch)
	secondWorktree, err := svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	require.Equal(t, firstWorktree.ID, secondWorktree.ID)
	require.Equal(t, firstWorktree.WorktreePath, secondWorktree.WorktreePath)
	require.Equal(t, second.LeaseEpoch, secondWorktree.Generation)
	require.Equal(t, firstWorktree.Version+1, secondWorktree.Version)
	preserved, err := os.ReadFile(changedPath)
	require.NoError(t, err)
	assert.Contains(t, string(preserved), "preserved = true")

	var oldLeaseStatus string
	require.NoError(t, svc.stores.db.QueryRow(`SELECT status FROM task_leases
		WHERE project_id = ? AND task_id = ? AND id = ?`, testProjectID, task.ID, firstLeaseID).Scan(&oldLeaseStatus))
	assert.Equal(t, model.LeaseStatusReleased, oldLeaseStatus)
	var activeLeases int
	require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM task_leases
		WHERE project_id = ? AND task_id = ? AND status = 'active'`, testProjectID, task.ID).Scan(&activeLeases))
	assert.Equal(t, 1, activeLeases)

	var fromStatus, toStatus, actor, reason, causation string
	var fromVersion, toVersion int64
	require.NoError(t, svc.stores.db.QueryRow(`SELECT from_status, to_status, from_version, to_version,
		actor_id, reason, causation_id FROM state_history
		WHERE project_id = ? AND aggregate_type = 'worktree' AND aggregate_id = ?
		ORDER BY id DESC LIMIT 1`, testProjectID, fmt.Sprint(firstWorktree.ID)).Scan(
		&fromStatus, &toStatus, &fromVersion, &toVersion, &actor, &reason, &causation,
	))
	assert.Equal(t, model.WorktreeStatusActive, fromStatus)
	assert.Equal(t, model.WorktreeStatusActive, toStatus)
	assert.Equal(t, firstWorktree.Version, fromVersion)
	assert.Equal(t, firstWorktree.Version+1, toVersion)
	assert.Equal(t, "claim-session", actor)
	assert.Contains(t, reason, "fresh lease generation")
	assert.Equal(t, *second.ActiveLeaseID, causation)
	var physicalSessionID string
	require.NoError(t, svc.stores.db.QueryRow(`SELECT id FROM agent_sessions
		WHERE project_id = ? AND COALESCE(external_id, id) = ?`, testProjectID, "claim-session").Scan(&physicalSessionID))
	for _, expected := range []struct {
		aggregateType, aggregateID string
		count                      int
	}{
		{"task", task.ID, 6},
		{"worker", physicalSessionID + "/claim-worker", 3},
		{"lease", firstLeaseID, 1},
		{"worktree", fmt.Sprint(firstWorktree.ID), 1},
	} {
		var count int
		require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
			WHERE project_id = ? AND aggregate_type = ? AND aggregate_id = ?`,
			testProjectID, expected.aggregateType, expected.aggregateID).Scan(&count))
		assert.Equal(t, expected.count, count, "%s/%s history count", expected.aggregateType, expected.aggregateID)
	}
	var invalidHistory int
	require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND (to_version <> from_version + 1 OR
		       COALESCE(actor_id, '') = '' OR reason = '' OR COALESCE(causation_id, '') = '')`,
		testProjectID).Scan(&invalidHistory))
	assert.Zero(t, invalidHistory)

	// An authority snapshot from the old generation can never become valid
	// again after the active Worktree has been rebound.
	err = svc.taskSvc.ensureWorktreeForClaim(ctx, first, "claim-session")
	require.ErrorIs(t, err, store.ErrConcurrentConflict)
}

func TestResolveBlockerFailsClosedForUnsafeWorktreeOrActiveOldLease(t *testing.T) {
	for _, targetStatus := range []string{
		model.WorktreeStatusSealed,
		model.WorktreeStatusSubmitted,
		model.WorktreeStatusQuarantined,
		model.WorktreeStatusCleanupPending,
	} {
		t.Run(targetStatus, func(t *testing.T) {
			svc, task, _ := newClaimFaultFixture(t, true, true)
			ctx := context.Background()
			_, err := svc.taskSvc.GetNextTask(ctx, testProjectID, "claim-session", model.RoleBackend, "claim-worker")
			require.NoError(t, err)
			require.NoError(t, svc.taskSvc.ReportBlocker(ctx, testProjectID, task.ID, "claim-session", "blocked"))
			worktree, err := svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
			require.NoError(t, err)
			worktreeSvc := NewWorktreeService(svc.stores.worktreeStore, svc.stores.projectStore, svc.stores.db)
			require.NoError(t, worktreeSvc.UpdateWorktreeStatus(ctx, testProjectID, worktree.ID, targetStatus))
			queueBefore := readQueueVersion(t, svc)
			err = svc.taskSvc.ResolveBlocker(ctx, testProjectID, task.ID, false, "retry")
			require.Error(t, err)
			after, getErr := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
			require.NoError(t, getErr)
			assert.Equal(t, model.TaskStatusBlocked, after.Status)
			assert.Equal(t, queueBefore, readQueueVersion(t, svc))
		})
	}

	t.Run("prior generation lease remains active", func(t *testing.T) {
		svc, task, _ := newClaimFaultFixture(t, true, true)
		ctx := context.Background()
		claimed, err := svc.taskSvc.GetNextTask(ctx, testProjectID, "claim-session", model.RoleBackend, "claim-worker")
		require.NoError(t, err)
		require.NoError(t, svc.taskSvc.ReportBlocker(ctx, testProjectID, task.ID, "claim-session", "blocked"))
		_, err = svc.stores.db.Exec(`UPDATE task_leases SET status = 'active', version = version + 1
			WHERE project_id = ? AND task_id = ? AND id = ? AND status = 'released'`,
			testProjectID, task.ID, *claimed.ActiveLeaseID)
		require.NoError(t, err)
		err = svc.taskSvc.ResolveBlocker(ctx, testProjectID, task.ID, false, "retry")
		require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
		after, getErr := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
		require.NoError(t, getErr)
		assert.Equal(t, model.TaskStatusBlocked, after.Status)
	})
}

func TestReportBlockerRollsBackEveryResourceAndHistoryOnFault(t *testing.T) {
	stages := []struct {
		name, trigger string
	}{
		{"task", `CREATE TRIGGER fail_block BEFORE UPDATE OF status ON tasks
			WHEN NEW.status = 'blocked' BEGIN SELECT RAISE(ABORT, 'FAIL_BLOCK_STAGE'); END`},
		{"lease", `CREATE TRIGGER fail_block BEFORE UPDATE OF status ON task_leases
			WHEN NEW.status = 'released' BEGIN SELECT RAISE(ABORT, 'FAIL_BLOCK_STAGE'); END`},
		{"lease history", `CREATE TRIGGER fail_block BEFORE INSERT ON state_history
			WHEN NEW.aggregate_type = 'lease' BEGIN SELECT RAISE(ABORT, 'FAIL_BLOCK_STAGE'); END`},
		{"worker", `CREATE TRIGGER fail_block BEFORE UPDATE OF status ON agent_workers
			WHEN NEW.status = 'idle' BEGIN SELECT RAISE(ABORT, 'FAIL_BLOCK_STAGE'); END`},
		{"worker history", `CREATE TRIGGER fail_block BEFORE INSERT ON state_history
			WHEN NEW.aggregate_type = 'worker' AND NEW.to_status = 'idle'
			BEGIN SELECT RAISE(ABORT, 'FAIL_BLOCK_STAGE'); END`},
		{"task history", `CREATE TRIGGER fail_block BEFORE INSERT ON state_history
			WHEN NEW.aggregate_type = 'task' AND NEW.to_status = 'blocked'
			BEGIN SELECT RAISE(ABORT, 'FAIL_BLOCK_STAGE'); END`},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			svc, task, _ := newClaimFaultFixture(t, true, true)
			ctx := context.Background()
			claimed, err := svc.taskSvc.GetNextTask(ctx, testProjectID, "claim-session", model.RoleBackend, "claim-worker")
			require.NoError(t, err)
			require.NotNil(t, claimed.ActiveLeaseID)
			var historyBefore int
			require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
				WHERE project_id = ?`, testProjectID).Scan(&historyBefore))
			_, err = svc.stores.db.Exec(stage.trigger)
			require.NoError(t, err)
			err = svc.taskSvc.ReportBlocker(ctx, testProjectID, task.ID, "claim-session", "blocked")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "FAIL_BLOCK_STAGE")

			after, getErr := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
			require.NoError(t, getErr)
			assert.Equal(t, model.TaskStatusExecuting, after.Status)
			require.NotNil(t, after.ActiveLeaseID)
			assert.Equal(t, *claimed.ActiveLeaseID, *after.ActiveLeaseID)
			var leaseStatus string
			var leaseVersion int64
			require.NoError(t, svc.stores.db.QueryRow(`SELECT status, version FROM task_leases
				WHERE project_id = ? AND id = ?`, testProjectID, *claimed.ActiveLeaseID).Scan(&leaseStatus, &leaseVersion))
			assert.Equal(t, model.LeaseStatusActive, leaseStatus)
			assert.Equal(t, int64(1), leaseVersion)
			worker, getErr := svc.stores.workerStore.GetByID(ctx, testProjectID, "claim-session", "claim-worker")
			require.NoError(t, getErr)
			assert.Equal(t, model.WorkerStatusBusy, worker.Status)
			require.NotNil(t, worker.CurrentTaskID)
			assert.Equal(t, task.ID, *worker.CurrentTaskID)
			var historyAfter int
			require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
				WHERE project_id = ?`, testProjectID).Scan(&historyAfter))
			assert.Equal(t, historyBefore, historyAfter)
		})
	}
}

func TestRecoveryServiceFailureBoundariesAndInvariants(t *testing.T) {
	t.Run("nil database", func(t *testing.T) {
		err := NewRecoveryService(nil, nil).Run(context.Background())
		require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
	})
	t.Run("closed database", func(t *testing.T) {
		svc := setupTestEnv(t)
		require.NoError(t, svc.stores.db.Close())
		err := NewRecoveryService(svc.stores.db, nil).Run(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "begin tx")
	})

	stages := []struct {
		name    string
		trigger string
	}{
		{"sessions", `CREATE TRIGGER fail_recovery BEFORE UPDATE ON agent_sessions
			BEGIN SELECT RAISE(ABORT, 'FAIL_RECOVERY_STAGE'); END`},
		{"leases", `CREATE TRIGGER fail_recovery BEFORE UPDATE ON task_leases
			BEGIN SELECT RAISE(ABORT, 'FAIL_RECOVERY_STAGE'); END`},
		{"task", `CREATE TRIGGER fail_recovery BEFORE UPDATE OF status ON tasks
			BEGIN SELECT RAISE(ABORT, 'FAIL_RECOVERY_STAGE'); END`},
		{"history", `CREATE TRIGGER fail_recovery BEFORE INSERT ON state_history
			BEGIN SELECT RAISE(ABORT, 'FAIL_RECOVERY_STAGE'); END`},
		{"worktree", `CREATE TRIGGER fail_recovery BEFORE UPDATE ON worktrees
			BEGIN SELECT RAISE(ABORT, 'FAIL_RECOVERY_STAGE'); END`},
		{"worker", `CREATE TRIGGER fail_recovery BEFORE UPDATE ON agent_workers
			BEGIN SELECT RAISE(ABORT, 'FAIL_RECOVERY_STAGE'); END`},
		{"runtime state", `CREATE TRIGGER fail_recovery BEFORE INSERT ON runtime_state
			BEGIN SELECT RAISE(ABORT, 'FAIL_RECOVERY_STAGE'); END`},
		{"audit", `CREATE TRIGGER fail_recovery BEFORE INSERT ON audit_log
			WHEN NEW.action = 'runtime.recovery' BEGIN SELECT RAISE(ABORT, 'FAIL_RECOVERY_STAGE'); END`},
	}
	for _, tt := range stages {
		t.Run(tt.name, func(t *testing.T) {
			svc, task := newRecoveryFaultFixture(t)
			_, err := svc.stores.db.Exec(tt.trigger)
			require.NoError(t, err)
			err = NewRecoveryService(svc.stores.db, svc.stores.projectStore).Run(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "FAIL_RECOVERY_STAGE")
			assertRecoveryRolledBack(t, svc, task.ID)
		})
	}

	t.Run("dangling active lease reference", func(t *testing.T) {
		svc, task := newRecoveryFaultFixture(t)
		_, err := svc.stores.db.Exec(`UPDATE tasks SET status = 'blocked', version = version + 1
			WHERE project_id = ? AND id = ?`, testProjectID, task.ID)
		require.NoError(t, err)
		err = NewRecoveryService(svc.stores.db, nil).Run(context.Background())
		require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
		assertRecoveryRolledBack(t, svc, task.ID)
	})

	t.Run("active task inserted during recovery", func(t *testing.T) {
		svc, _ := newRecoveryFaultFixture(t)
		_, err := svc.stores.db.Exec(fmt.Sprintf(`CREATE TRIGGER inject_active_task AFTER UPDATE ON agent_workers
			BEGIN INSERT INTO tasks(
			 id, project_id, feature_id, title, description, role, status, allowed_directories,
			 forbidden_patterns, required_apis, dependencies, test_requirements, priority, created_at, updated_at)
			 VALUES ('T-injected-active', %q, %q, 'Injected', 'Injected', 'backend', 'leased', '["src/"]',
			 '[]', '[]', '[]', '{}', 'normal', datetime('now'), datetime('now')); END`,
			testProjectID, testFeatureID))
		require.NoError(t, err)
		err = NewRecoveryService(svc.stores.db, nil).Run(context.Background())
		require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
		var count int
		require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = 'T-injected-active'`).Scan(&count))
		assert.Zero(t, count, "integrity failure must roll back trigger side effects")
	})

	t.Run("foreign key violation", func(t *testing.T) {
		svc := setupTestEnv(t)
		_, err := svc.stores.db.Exec(`PRAGMA foreign_keys = OFF`)
		require.NoError(t, err)
		_, err = svc.stores.db.Exec(`INSERT INTO features(
			id, project_id, title, description, status, created_at, updated_at)
			VALUES ('bad-feature', 'missing-project', 'Bad', 'Bad', 'planning', datetime('now'), datetime('now'))`)
		require.NoError(t, err)
		_, err = svc.stores.db.Exec(`PRAGMA foreign_keys = ON`)
		require.NoError(t, err)
		err = NewRecoveryService(svc.stores.db, nil).Run(context.Background())
		require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
		assert.Contains(t, err.Error(), "foreign key violation")
	})

	t.Run("illegal worktree transition rolls back every recovery write", func(t *testing.T) {
		svc := setupTestEnv(t)
		const sessionID, workerID = "illegal-worktree-session", "illegal-worktree-worker"
		seedTestSession(t, svc.stores, sessionID)
		seedTestWorker(t, svc.stores, sessionID, workerID)
		expiresAt := time.Now().UTC().Add(time.Minute).Format("2006-01-02 15:04:05")
		leaseID := "lease-illegal-worktree"
		task := newTestTask("T-illegal-recovery-worktree")
		task.Status = model.TaskStatusLeased
		sid, wid := sessionID, workerID
		task.AssignedSessionID = &sid
		task.AssignedWorkerID = &wid
		task.Version = 1
		task.LeaseEpoch = 1
		task.ActiveLeaseID = &leaseID
		task.LeaseExpiresAt = &expiresAt
		mustCreateTask(t, svc.stores.taskStore, task)
		_, err := svc.stores.db.Exec(`UPDATE agent_workers SET status = 'busy', current_task_id = ?,
			version = version + 1 WHERE project_id = ? AND session_id = ? AND id = ?`,
			task.ID, testProjectID, sessionID, workerID)
		require.NoError(t, err)
		_, err = svc.stores.db.Exec(`INSERT INTO task_leases(id, project_id, task_id, session_id,
			worker_id, epoch, status, version, expires_at) VALUES (?, ?, ?, ?, ?, 1, 'active', 1, ?)`,
			leaseID, testProjectID, task.ID, sessionID, workerID, expiresAt)
		require.NoError(t, err)
		seedTestWorktree(t, svc.stores, task.ID)
		_, err = svc.stores.db.Exec(`UPDATE worktrees SET status = 'sealed', version = version + 1
			WHERE project_id = ? AND task_id = ?`, testProjectID, task.ID)
		require.NoError(t, err)

		err = NewRecoveryService(svc.stores.db, nil).Run(context.Background())
		require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
		assert.Contains(t, err.Error(), "sealed -> cleanup_pending")
		taskAfter, getErr := svc.taskSvc.GetTask(context.Background(), testProjectID, task.ID)
		require.NoError(t, getErr)
		assert.Equal(t, model.TaskStatusLeased, taskAfter.Status)
		assert.NotNil(t, taskAfter.ActiveLeaseID)
		session, getErr := svc.sessSvc.GetSession(context.Background(), testProjectID, sessionID)
		require.NoError(t, getErr)
		assert.Equal(t, model.SessionStatusOnline, session.Status)
		var activeLeases int
		require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM task_leases
			WHERE project_id = ? AND task_id = ? AND status = 'active'`, testProjectID, task.ID).Scan(&activeLeases))
		assert.Equal(t, 1, activeLeases)
		worktree, getErr := svc.stores.worktreeStore.GetByTaskID(context.Background(), testProjectID, task.ID)
		require.NoError(t, getErr)
		assert.Equal(t, model.WorktreeStatusSealed, worktree.Status)
		var histories int
		require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
			WHERE project_id = ?`, testProjectID).Scan(&histories))
		assert.Zero(t, histories)
	})
}

func TestRecoveryServiceRequeuesLeasedAndPreservesStableValidation(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	leased := newTestTask("T-recovery-leased")
	leased.Status = model.TaskStatusLeased
	mustCreateTask(t, svc.stores.taskStore, leased)
	stable := newTestTask("T-recovery-stable-validation")
	stable.Status = model.TaskStatusValidating
	mustCreateTask(t, svc.stores.taskStore, stable)

	queueBefore := readQueueVersion(t, svc)
	require.NoError(t, NewRecoveryService(svc.stores.db, nil).Run(ctx))
	leasedAfter, err := svc.taskSvc.GetTask(ctx, testProjectID, leased.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusQueued, leasedAfter.Status)
	stableAfter, err := svc.taskSvc.GetTask(ctx, testProjectID, stable.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusValidating, stableAfter.Status)
	assert.Equal(t, queueBefore+1, readQueueVersion(t, svc))
}

func newClaimFaultFixture(t *testing.T, realRepository, createWorker bool) (*testServices, *model.Task, int64) {
	t.Helper()
	svc := setupTestEnv(t)
	ctx := context.Background()
	if realRepository {
		workspace, _ := createTestGitRepository(t)
		_, err := svc.stores.db.Exec(`UPDATE projects SET workspace_path = ? WHERE id = ?`, workspace, testProjectID)
		require.NoError(t, err)
	}
	task := newTestTask("T-claim-fault")
	require.NoError(t, svc.taskSvc.CreateTask(ctx, testProjectID, task))
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: "claim-session", Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	if createWorker {
		require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, "claim-session", &model.AgentWorker{ID: "claim-worker"}))
	}
	return svc, task, readQueueVersion(t, svc)
}

func readQueueVersion(t *testing.T, svc *testServices) int64 {
	t.Helper()
	var version int64
	require.NoError(t, svc.stores.db.QueryRow(`SELECT COALESCE(
		(SELECT version FROM project_queue_versions WHERE project_id = ?), 0)`, testProjectID).Scan(&version))
	return version
}

func assertClaimTransactionRolledBack(t *testing.T, svc *testServices, taskID string) {
	t.Helper()
	task, err := svc.taskSvc.GetTask(context.Background(), testProjectID, taskID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusQueued, task.Status)
	assert.Nil(t, task.ActiveLeaseID)
	worker, err := svc.stores.workerStore.GetByID(context.Background(), testProjectID, "claim-session", "claim-worker")
	require.NoError(t, err)
	assert.Equal(t, model.WorkerStatusIdle, worker.Status)
	assert.Nil(t, worker.CurrentTaskID)
	var leases int
	require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM task_leases
		WHERE project_id = ? AND task_id = ?`, testProjectID, taskID).Scan(&leases))
	assert.Zero(t, leases)
}

func newRecoveryFaultFixture(t *testing.T) (*testServices, *model.Task) {
	t.Helper()
	svc := setupTestEnv(t)
	seedTestSession(t, svc.stores, "recovery-fault-session")
	task := newTestTask("T-recovery-fault")
	seedTaskWithActiveLease(t, svc.stores, task, "recovery-fault-session", "recovery-fault-worker")
	seedTestWorktree(t, svc.stores, task.ID)
	return svc, task
}

func assertRecoveryRolledBack(t *testing.T, svc *testServices, taskID string) {
	t.Helper()
	task, err := svc.taskSvc.GetTask(context.Background(), testProjectID, taskID)
	require.NoError(t, err)
	assert.True(t, task.Status == model.TaskStatusExecuting || task.Status == model.TaskStatusBlocked)
	assert.NotNil(t, task.ActiveLeaseID)
	session, err := svc.sessSvc.GetSession(context.Background(), testProjectID, "recovery-fault-session")
	require.NoError(t, err)
	assert.Equal(t, model.SessionStatusOnline, session.Status)
	var active int
	require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM task_leases
		WHERE project_id = ? AND task_id = ? AND status = 'active'`, testProjectID, taskID).Scan(&active))
	assert.Equal(t, 1, active)
}
