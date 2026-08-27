package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeartbeatTaskRenewsExactLeaseAndReplaysOriginalResult(t *testing.T) {
	svc, lease := newHeartbeatFixture(t)
	ctx := context.Background()
	key := "heartbeat-replay-0001"

	renewed, err := svc.taskSvc.HeartbeatTask(ctx, testProjectID, lease.TaskID,
		"heartbeat-session", "heartbeat-worker", lease.ID, lease.Version, key)
	require.NoError(t, err)
	assert.Equal(t, int64(2), renewed.Version)
	assert.Equal(t, lease.Epoch, renewed.Epoch)
	assert.Equal(t, "heartbeat-session", renewed.SessionID, "physical scoped session key must not leak")
	assert.NotEqual(t, lease.ExpiresAt, renewed.ExpiresAt)

	var (
		storedVersion            int64
		storedExpiry, taskExpiry string
		workerStatus             string
		currentTask              *string
	)
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT version, expires_at
		FROM task_leases WHERE project_id = ? AND id = ?`, testProjectID, lease.ID).
		Scan(&storedVersion, &storedExpiry))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT lease_expires_at
		FROM tasks WHERE project_id = ? AND id = ?`, testProjectID, lease.TaskID).Scan(&taskExpiry))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT status, current_task_id
		FROM agent_workers WHERE project_id = ? AND id = ?`, testProjectID, "heartbeat-worker").
		Scan(&workerStatus, &currentTask))
	assert.Equal(t, int64(2), storedVersion)
	assert.Equal(t, renewed.ExpiresAt, storedExpiry)
	assert.Equal(t, renewed.ExpiresAt, taskExpiry)
	assert.Equal(t, model.WorkerStatusBusy, workerStatus)
	require.NotNil(t, currentTask)
	assert.Equal(t, lease.TaskID, *currentTask)

	// Retry uses the original lease version and must return the exact original
	// result without extending or incrementing the Lease a second time.
	replayed, err := svc.taskSvc.HeartbeatTask(ctx, testProjectID, lease.TaskID,
		"heartbeat-session", "heartbeat-worker", lease.ID, lease.Version, key)
	require.NoError(t, err)
	assert.Equal(t, renewed, replayed)
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT version, expires_at
		FROM task_leases WHERE project_id = ? AND id = ?`, testProjectID, lease.ID).
		Scan(&storedVersion, &storedExpiry))
	assert.Equal(t, int64(2), storedVersion)
	assert.Equal(t, renewed.ExpiresAt, storedExpiry)

	var allowed, activity, idempotency int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log
		WHERE bound_project = ? AND target_task = ? AND action = 'task.heartbeat' AND result = 'ALLOWED'`,
		testProjectID, lease.TaskID).Scan(&allowed))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity_log
		WHERE project_id = ? AND task_id = ? AND action = 'task_heartbeat'`,
		testProjectID, lease.TaskID).Scan(&activity))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM idempotency_records
		WHERE project_id = ? AND operation = 'task.heartbeat'`, testProjectID).Scan(&idempotency))
	assert.Equal(t, 1, allowed)
	assert.Equal(t, 1, activity)
	assert.Equal(t, 1, idempotency)
	var histories, invalidHistories int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND causation_id = ?`, testProjectID, lease.ID).Scan(&histories))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND causation_id = ? AND
		(to_version <> from_version + 1 OR actor_id = '' OR reason = '')`,
		testProjectID, lease.ID).Scan(&invalidHistories))
	assert.Equal(t, 4, histories, "Lease, Task, Worker, and Session heartbeat mutations must be traceable")
	assert.Zero(t, invalidHistories)
}

func TestHeartbeatTaskFailsClosedForStaleExpiredOrForgedAuthority(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(t *testing.T, svc *testServices, lease *model.TaskLease)
		sessionID string
		workerID  string
		leaseID   func(*model.TaskLease) string
		version   int64
		want      error
	}{
		{
			name: "stale lease version", version: 2, want: store.ErrLeaseVersionMismatch,
		},
		{
			name: "expired lease",
			mutate: func(t *testing.T, svc *testServices, lease *model.TaskLease) {
				_, err := svc.stores.db.Exec(`UPDATE task_leases SET expires_at = datetime('now', '-1 second')
					WHERE project_id = ? AND id = ?`, testProjectID, lease.ID)
				require.NoError(t, err)
			},
			want: store.ErrLeaseExpired,
		},
		{
			name: "task epoch changed",
			mutate: func(t *testing.T, svc *testServices, lease *model.TaskLease) {
				_, err := svc.stores.db.Exec(`UPDATE tasks SET lease_epoch = lease_epoch + 1
					WHERE project_id = ? AND id = ?`, testProjectID, lease.TaskID)
				require.NoError(t, err)
			},
			want: store.ErrLeaseVersionMismatch,
		},
		{
			name: "wrong session", sessionID: "forged-session", want: store.ErrSessionNotFound,
		},
		{
			name: "wrong worker", workerID: "forged-worker", want: store.ErrTaskNotOwned,
		},
		{
			name: "wrong lease id", leaseID: func(*model.TaskLease) string { return uuid.NewString() }, want: store.ErrLeaseNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, lease := newHeartbeatFixture(t)
			if tc.mutate != nil {
				tc.mutate(t, svc, lease)
			}
			sessionID := tc.sessionID
			if sessionID == "" {
				sessionID = "heartbeat-session"
			}
			workerID := tc.workerID
			if workerID == "" {
				workerID = "heartbeat-worker"
			}
			leaseID := lease.ID
			if tc.leaseID != nil {
				leaseID = tc.leaseID(lease)
			}
			version := tc.version
			if version == 0 {
				version = lease.Version
			}
			_, err := svc.taskSvc.HeartbeatTask(context.Background(), testProjectID, lease.TaskID,
				sessionID, workerID, leaseID, version, "heartbeat-denied-0001")
			require.ErrorIs(t, err, tc.want)

			var storedVersion int64
			require.NoError(t, svc.stores.db.QueryRow(`SELECT version FROM task_leases
				WHERE project_id = ? AND id = ?`, testProjectID, lease.ID).Scan(&storedVersion))
			assert.Equal(t, int64(1), storedVersion)
			var denied int
			require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM audit_log
				WHERE bound_project = ? AND target_task = ? AND action = 'task.heartbeat' AND result = 'DENIED'`,
				testProjectID, lease.TaskID).Scan(&denied))
			assert.Equal(t, 1, denied)
		})
	}
}

func TestHeartbeatTaskIdempotencyMismatchAndConcurrentCAS(t *testing.T) {
	t.Run("idempotency payload mismatch", func(t *testing.T) {
		svc, lease := newHeartbeatFixture(t)
		ctx := context.Background()
		key := "heartbeat-conflict-0001"
		_, err := svc.taskSvc.HeartbeatTask(ctx, testProjectID, lease.TaskID,
			"heartbeat-session", "heartbeat-worker", lease.ID, 1, key)
		require.NoError(t, err)
		_, err = svc.taskSvc.HeartbeatTask(ctx, testProjectID, lease.TaskID,
			"heartbeat-session", "heartbeat-worker", lease.ID, 2, key)
		require.ErrorIs(t, err, store.ErrIdempotencyConflict)
	})

	t.Run("one lease version has one winner", func(t *testing.T) {
		svc, lease := newHeartbeatFixture(t)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, key := range []string{"heartbeat-concurrent-01", "heartbeat-concurrent-02"} {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				_, err := svc.taskSvc.HeartbeatTask(context.Background(), testProjectID, lease.TaskID,
					"heartbeat-session", "heartbeat-worker", lease.ID, 1, key)
				errs <- err
			}(key)
		}
		wg.Wait()
		close(errs)
		var successes, conflicts int
		for err := range errs {
			if err == nil {
				successes++
			} else if errors.Is(err, store.ErrLeaseVersionMismatch) {
				conflicts++
			} else {
				t.Fatalf("unexpected heartbeat result: %v", err)
			}
		}
		assert.Equal(t, 1, successes)
		assert.Equal(t, 1, conflicts)
	})
}

func TestHeartbeatTaskValidatesWireInputs(t *testing.T) {
	svc, lease := newHeartbeatFixture(t)
	tests := []struct {
		name, leaseID, key string
		version            int64
	}{
		{name: "non uuid lease", leaseID: "lease-not-uuid", key: "heartbeat-input-0001", version: 1},
		{name: "zero version", leaseID: lease.ID, key: "heartbeat-input-0002", version: 0},
		{name: "short key", leaseID: lease.ID, key: "short", version: 1},
		{name: "invalid key characters", leaseID: lease.ID, key: "heartbeat invalid key", version: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.taskSvc.HeartbeatTask(context.Background(), testProjectID, lease.TaskID,
				"heartbeat-session", "heartbeat-worker", tc.leaseID, tc.version, tc.key)
			require.ErrorIs(t, err, store.ErrInvalidParameter)
		})
	}
	_, err := svc.taskSvc.HeartbeatTask(context.Background(), "", lease.TaskID,
		"heartbeat-session", "heartbeat-worker", lease.ID, 1, "heartbeat-input-0003")
	require.ErrorIs(t, err, store.ErrInvalidParameter)
	var unavailable *TaskService
	_, err = unavailable.HeartbeatTask(context.Background(), testProjectID, lease.TaskID,
		"heartbeat-session", "heartbeat-worker", lease.ID, 1, "heartbeat-input-0004")
	require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
}

func TestHeartbeatTaskAdditionalAuthorityStatesFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, svc *testServices, lease *model.TaskLease)
		taskID func(*model.TaskLease) string
		want   error
	}{
		{
			name: "offline session",
			mutate: func(t *testing.T, svc *testServices, _ *model.TaskLease) {
				_, err := svc.stores.db.Exec(`UPDATE agent_sessions SET status = 'offline', version = version + 1
					WHERE project_id = ? AND COALESCE(external_id, id) = ?`, testProjectID, "heartbeat-session")
				require.NoError(t, err)
			},
			want: store.ErrTaskNotOwned,
		},
		{
			name: "task no longer executing",
			mutate: func(t *testing.T, svc *testServices, lease *model.TaskLease) {
				_, err := svc.stores.db.Exec(`UPDATE tasks SET status = 'needs_human', version = version + 1
					WHERE project_id = ? AND id = ?`, testProjectID, lease.TaskID)
				require.NoError(t, err)
			},
			want: store.ErrTaskStateInvalid,
		},
		{
			name: "inactive lease",
			mutate: func(t *testing.T, svc *testServices, lease *model.TaskLease) {
				_, err := svc.stores.db.Exec(`UPDATE task_leases SET status = 'released', version = version + 1
					WHERE project_id = ? AND id = ?`, testProjectID, lease.ID)
				require.NoError(t, err)
			},
			want: store.ErrLeaseNotFound,
		},
		{
			name: "worker reservation released",
			mutate: func(t *testing.T, svc *testServices, _ *model.TaskLease) {
				_, err := svc.stores.db.Exec(`UPDATE agent_workers SET status = 'idle', current_task_id = NULL,
					version = version + 1 WHERE project_id = ? AND id = ?`, testProjectID, "heartbeat-worker")
				require.NoError(t, err)
			},
			want: store.ErrTaskNotOwned,
		},
		{
			name: "task missing",
			taskID: func(*model.TaskLease) string {
				return "T-heartbeat-missing"
			},
			want: store.ErrTaskNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, lease := newHeartbeatFixture(t)
			if tc.mutate != nil {
				tc.mutate(t, svc, lease)
			}
			taskID := lease.TaskID
			if tc.taskID != nil {
				taskID = tc.taskID(lease)
			}
			_, err := svc.taskSvc.HeartbeatTask(context.Background(), testProjectID, taskID,
				"heartbeat-session", "heartbeat-worker", lease.ID, 1, "heartbeat-state-0001")
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestHeartbeatTaskRejectsCorruptIdempotencyEvidence(t *testing.T) {
	svc, lease := newHeartbeatFixture(t)
	ctx := context.Background()
	key := "heartbeat-corrupt-0001"
	_, err := svc.taskSvc.HeartbeatTask(ctx, testProjectID, lease.TaskID,
		"heartbeat-session", "heartbeat-worker", lease.ID, 1, key)
	require.NoError(t, err)
	_, err = svc.stores.db.ExecContext(ctx, `UPDATE idempotency_records SET result_ref = '{'
		WHERE project_id = ? AND operation = 'task.heartbeat' AND key = ?`, testProjectID, key)
	require.NoError(t, err)
	_, err = svc.taskSvc.HeartbeatTask(ctx, testProjectID, lease.TaskID,
		"heartbeat-session", "heartbeat-worker", lease.ID, 1, key)
	require.ErrorIs(t, err, store.ErrRecoveryIntegrity)
}

func TestHeartbeatErrorCodesAreStable(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{store.ErrLeaseExpired, "LEASE_EXPIRED"},
		{store.ErrLeaseVersionMismatch, "LEASE_VERSION_MISMATCH"},
		{store.ErrLeaseNotFound, "LEASE_NOT_FOUND"},
		{store.ErrTaskNotOwned, "TASK_NOT_OWNED"},
		{store.ErrIdempotencyConflict, "IDEMPOTENCY_CONFLICT"},
		{store.ErrInvalidParameter, "INVALID_PARAMETER"},
		{errors.New("database unavailable"), "INTERNAL_ERROR"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, heartbeatErrorCode(tc.err))
	}
}

func newHeartbeatFixture(t *testing.T) (*testServices, *model.TaskLease) {
	t.Helper()
	svc := setupTestEnv(t)
	seedTestSession(t, svc.stores, "heartbeat-session")
	task := newTestTask("T-heartbeat")
	seedTaskWithActiveLease(t, svc.stores, task, "heartbeat-session", "heartbeat-worker")
	oldLeaseID := *task.ActiveLeaseID
	leaseID := uuid.NewString()
	expiresAt := time.Now().UTC().Add(15 * time.Second).Format("2006-01-02 15:04:05")
	_, err := svc.stores.db.Exec(`UPDATE task_leases SET id = ?, expires_at = ?
		WHERE project_id = ? AND id = ?`, leaseID, expiresAt, testProjectID, oldLeaseID)
	require.NoError(t, err)
	_, err = svc.stores.db.Exec(`UPDATE tasks SET active_lease_id = ?, lease_expires_at = ?
		WHERE project_id = ? AND id = ?`, leaseID, expiresAt, testProjectID, task.ID)
	require.NoError(t, err)
	return svc, &model.TaskLease{
		ID: leaseID, ProjectID: testProjectID, TaskID: task.ID,
		SessionID: "heartbeat-session", WorkerID: "heartbeat-worker",
		Epoch: 1, Status: model.LeaseStatusActive, Version: 1, ExpiresAt: expiresAt,
	}
}
