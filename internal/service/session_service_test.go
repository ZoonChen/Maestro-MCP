package service

import (
	"context"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// RegisterSession
// ---------------------------------------------------------------------------

func TestRegisterSession_ValidRole(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	roles := []string{
		model.RoleBackend, model.RoleFrontend, model.RoleDevops,
		model.RoleVerifier, model.RoleCoordinator,
	}
	for i, role := range roles {
		sess := &model.AgentSession{
			ID:       "session-" + role,
			Role:     role,
			Capacity: 3,
		}
		err := svc.sessSvc.RegisterSession(ctx, testProjectID, sess)
		require.NoError(t, err, "role %s should be valid", role)

		got, err := svc.stores.sessionStore.GetByID(ctx, testProjectID, "session-"+role)
		require.NoError(t, err)
		assert.Equal(t, role, got.Role)
		assert.Equal(t, "other", got.ClientType, "client_type default")
		assert.Equal(t, 3, got.Capacity)
		assert.Equal(t, model.SessionStatusOnline, got.Status)
		_ = i
	}
}

func TestRegisterSession_InvalidRole(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	sess := &model.AgentSession{
		ID:       "session-badrole",
		Role:     "hacker",
		Capacity: 1,
	}
	err := svc.sessSvc.RegisterSession(ctx, testProjectID, sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid role")
}

func TestRegisterSession_DefaultValues(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	sess := &model.AgentSession{
		ID:   "session-defaults",
		Role: model.RoleBackend,
		// ClientType, Capacity, Status left empty/zero.
	}
	err := svc.sessSvc.RegisterSession(ctx, testProjectID, sess)
	require.NoError(t, err)

	got, err := svc.stores.sessionStore.GetByID(ctx, testProjectID, "session-defaults")
	require.NoError(t, err)
	assert.Equal(t, "other", got.ClientType, "client_type should default to 'other'")
	assert.Equal(t, 1, got.Capacity, "capacity should default to 1")
	assert.Equal(t, model.SessionStatusOnline, got.Status, "status should default to 'online'")
}

func TestEnsureSessionIsIdempotentOnlyForIdenticalIdentity(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	original := &model.AgentSession{
		ID: "session-idempotent", Role: model.RoleBackend, ClientType: "codex", Capacity: 2,
	}
	created, err := svc.sessSvc.EnsureSession(ctx, testProjectID, original)
	require.NoError(t, err)
	require.True(t, created)

	identical := &model.AgentSession{
		ID: original.ID, Role: original.Role, ClientType: original.ClientType, Capacity: original.Capacity,
	}
	created, err = svc.sessSvc.EnsureSession(ctx, testProjectID, identical)
	require.NoError(t, err)
	assert.False(t, created)

	for name, mutate := range map[string]func(*model.AgentSession){
		"role":     func(s *model.AgentSession) { s.Role = model.RoleVerifier },
		"client":   func(s *model.AgentSession) { s.ClientType = "other-client" },
		"capacity": func(s *model.AgentSession) { s.Capacity = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			conflict := &model.AgentSession{
				ID: original.ID, Role: original.Role, ClientType: original.ClientType, Capacity: original.Capacity,
			}
			mutate(conflict)
			created, err := svc.sessSvc.EnsureSession(ctx, testProjectID, conflict)
			assert.False(t, created)
			require.ErrorIs(t, err, store.ErrIdempotencyConflict)
		})
	}

	var denied int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE bound_project = ? AND action = 'session.ensure' AND result = 'DENIED'`,
		testProjectID,
	).Scan(&denied))
	assert.Equal(t, 3, denied)
}

func TestRegisterSessionRejectsCrossProjectAndInvalidCapacity(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	err := svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: "session-scope", ProjectID: "forged-project", Role: model.RoleBackend, Capacity: 1,
	})
	require.ErrorIs(t, err, store.ErrProjectScopeViolation)
	err = svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: "session-capacity", Role: model.RoleBackend, Capacity: 6,
	})
	require.ErrorIs(t, err, store.ErrInvalidParameter)
}

// ---------------------------------------------------------------------------
// DisconnectSession
// ---------------------------------------------------------------------------

func TestDisconnectSession_MarksOffline(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	sess := &model.AgentSession{
		ID:       "session-dc",
		Role:     model.RoleBackend,
		Capacity: 2,
	}
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, sess))

	err := svc.sessSvc.DisconnectSession(ctx, testProjectID, "session-dc")
	require.NoError(t, err)

	got, err := svc.stores.sessionStore.GetByID(ctx, testProjectID, "session-dc")
	require.NoError(t, err)
	assert.Equal(t, model.SessionStatusOffline, got.Status)
}

func TestDisconnectSession_CleansUpWorkers(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	sessID := "session-dcw"
	sess := &model.AgentSession{ID: sessID, Role: model.RoleBackend, Capacity: 5}
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, sess))

	// Create a worker.
	seedTestWorker(t, svc.stores, sessID, "worker-1")

	// Before disconnect, worker exists.
	workers, err := svc.stores.workerStore.ListBySession(ctx, testProjectID, sessID)
	require.NoError(t, err)
	assert.Len(t, workers, 1)

	err = svc.sessSvc.DisconnectSession(ctx, testProjectID, sessID)
	require.NoError(t, err)

	// Worker identity and lease history remain durable; it is marked lost.
	workers, err = svc.stores.workerStore.ListBySession(ctx, testProjectID, sessID)
	require.NoError(t, err)
	require.Len(t, workers, 1)
	assert.Equal(t, model.WorkerStatusLost, workers[0].Status)
}

func TestDisconnectSession_NonexistentSession(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	err := svc.sessSvc.DisconnectSession(ctx, testProjectID, "session-nonexist")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// ForceRelease
// ---------------------------------------------------------------------------

func TestForceRelease_MarksOfflineAndResetsTasks(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	sessID := "session-fr"
	sess := &model.AgentSession{ID: sessID, Role: model.RoleBackend, Capacity: 5}
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, sess))
	seedTestWorker(t, svc.stores, sessID, "worker-fr")

	// Create an in_progress task assigned to this session.
	sid := sessID
	wid := "worker-fr"
	task := newTestTask("T-fr")
	task.Status = model.TaskStatusInProgress
	task.AssignedSessionID = &sid
	task.AssignedWorkerID = &wid
	mustCreateTask(t, svc.stores.taskStore, task)

	// Update the worker's current task.
	require.NoError(t, svc.stores.workerStore.UpdateCurrentTask(ctx, testProjectID, sessID, "worker-fr", "T-fr"))

	err := svc.sessSvc.ForceRelease(ctx, testProjectID, sessID)
	require.NoError(t, err)

	// Session should be offline.
	got, err := svc.stores.sessionStore.GetByID(ctx, testProjectID, sessID)
	require.NoError(t, err)
	assert.Equal(t, model.SessionStatusOffline, got.Status)

	taskGot, err := svc.stores.taskStore.GetByID(ctx, testProjectID, "T-fr")
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusNeedsHuman, taskGot.Status)
	assert.Nil(t, taskGot.AssignedSessionID)
	assert.Nil(t, taskGot.AssignedWorkerID)
	workers, err := svc.stores.workerStore.ListBySession(ctx, testProjectID, sessID)
	require.NoError(t, err)
	require.Len(t, workers, 1)
	assert.Equal(t, model.WorkerStatusLost, workers[0].Status)
}

// ---------------------------------------------------------------------------
// RegisterWorker
// ---------------------------------------------------------------------------

func TestRegisterWorker_WithinCapacity(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	sessID := "session-rw"
	sess := &model.AgentSession{ID: sessID, Role: model.RoleBackend, Capacity: 3}
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, sess))

	for i := range 3 {
		worker := &model.AgentWorker{
			ID:     "worker-rw-" + string(rune('0'+i)),
			Status: "idle",
		}
		err := svc.sessSvc.RegisterWorker(ctx, testProjectID, sessID, worker)
		require.NoError(t, err, "worker %d should be registered within capacity", i)
	}
}

func TestRegisterWorker_ExceedsCapacity(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	sessID := "session-cap"
	sess := &model.AgentSession{ID: sessID, Role: model.RoleBackend, Capacity: 2}
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, sess))

	// Register up to capacity.
	for i := range 2 {
		worker := &model.AgentWorker{
			ID:     "worker-cap-" + string(rune('0'+i)),
			Status: "idle",
		}
		require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, sessID, worker))
	}

	// One more should fail.
	worker := &model.AgentWorker{
		ID:     "worker-cap-extra",
		Status: "idle",
	}
	err := svc.sessSvc.RegisterWorker(ctx, testProjectID, sessID, worker)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "capacity")
}

func TestRegisterWorker_NonexistentSession(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	worker := &model.AgentWorker{ID: "worker-ghost", Status: "idle"}
	err := svc.sessSvc.RegisterWorker(ctx, testProjectID, "session-nonexist", worker)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// FindStaleSessions
// ---------------------------------------------------------------------------

func TestFindStaleSessions(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	// Insert a session with a very old heartbeat (over 1 hour ago).
	sessID := "session-stale"
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, err := svc.stores.db.ExecContext(ctx, `
		INSERT INTO agent_sessions (id, project_id, role, client_type, capacity, status, last_heartbeat, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessID, testProjectID, model.RoleBackend, "test", 1,
		model.SessionStatusOnline, now, now,
	)
	require.NoError(t, err)

	// Manually set heartbeat to 2 hours ago.
	_, err = svc.stores.db.ExecContext(ctx, `
		UPDATE agent_sessions SET last_heartbeat = datetime('now', '-2 hours') WHERE id = ?`,
		sessID,
	)
	require.NoError(t, err)

	// With a very long timeout, no sessions should be stale.
	stale, err := svc.sessSvc.FindStaleSessions(ctx, 86400)
	require.NoError(t, err)
	assert.Empty(t, stale, "no sessions should be stale with 1-day timeout")

	// With a short timeout, the session with old heartbeat should be stale.
	stale, err = svc.sessSvc.FindStaleSessions(ctx, 60)
	require.NoError(t, err)
	assert.NotEmpty(t, stale, "session with 2-hour-old heartbeat should be stale with 60s timeout")
}

func TestCleanupStaleSessionRejectsSnapshotAfterHeartbeat(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: "stale-race", Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, "stale-race", &model.AgentWorker{ID: "stale-worker"}))
	_, err := svc.stores.db.ExecContext(ctx, `UPDATE agent_sessions
		SET last_heartbeat = datetime('now', '-2 hours') WHERE project_id = ? AND id = ?`,
		testProjectID, "stale-race")
	require.NoError(t, err)
	stale, err := svc.sessSvc.FindStaleSessions(ctx, 60)
	require.NoError(t, err)
	require.Len(t, stale, 1)

	require.NoError(t, svc.sessSvc.UpdateHeartbeat(ctx, testProjectID, "stale-race"))
	require.NoError(t, svc.sessSvc.cleanupStaleSessionAt(
		ctx, stale[0], time.Now().UTC().Add(-time.Minute),
	))

	session, err := svc.sessSvc.GetSession(ctx, testProjectID, "stale-race")
	require.NoError(t, err)
	assert.Equal(t, model.SessionStatusOnline, session.Status)
	assert.Equal(t, stale[0].Version+1, session.Version)
	worker, err := svc.stores.workerStore.GetByID(ctx, testProjectID, "stale-race", "stale-worker")
	require.NoError(t, err)
	assert.Equal(t, model.WorkerStatusIdle, worker.Status)
	var sessionKey string
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT id FROM agent_sessions
		WHERE project_id = ? AND external_id = ?`, testProjectID, "stale-race").Scan(&sessionKey))
	var heartbeatHistory, offlineHistory int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND aggregate_type = 'session' AND aggregate_id = ?
		  AND from_status = 'online' AND to_status = 'online'`, testProjectID, sessionKey).Scan(&heartbeatHistory))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND aggregate_type = 'session' AND aggregate_id = ?
		  AND to_status = 'offline'`, testProjectID, sessionKey).Scan(&offlineHistory))
	assert.Equal(t, 1, heartbeatHistory)
	assert.Zero(t, offlineHistory)
}

func TestDisconnectPreservesStableValidatingAndClearsLostWorkerTask(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	sessionID, workerID := "stable-validator-owner", "stable-worker"
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: sessionID, Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, sessionID, &model.AgentWorker{ID: workerID}))
	task := newTestTask("T-stable-validating-disconnect")
	task.Status = model.TaskStatusValidating
	task.AssignedSessionID = &sessionID
	task.AssignedWorkerID = &workerID
	mustCreateTask(t, svc.stores.taskStore, task)
	require.NoError(t, svc.stores.workerStore.UpdateCurrentTask(ctx, testProjectID, sessionID, workerID, task.ID))
	seedTestWorktree(t, svc.stores, task.ID)

	require.NoError(t, svc.sessSvc.DisconnectSession(ctx, testProjectID, sessionID))
	got, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusValidating, got.Status)
	assert.Equal(t, task.Version, got.Version)
	worker, err := svc.stores.workerStore.GetByID(ctx, testProjectID, sessionID, workerID)
	require.NoError(t, err)
	assert.Equal(t, model.WorkerStatusLost, worker.Status)
	assert.Nil(t, worker.CurrentTaskID)
	worktree, err := svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.WorktreeStatusActive, worktree.Status)

	var sessionKey string
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT id FROM agent_sessions
		WHERE project_id = ? AND external_id = ?`, testProjectID, sessionID).Scan(&sessionKey))
	var sessionHistory, workerHistory, taskHistory int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND aggregate_type = 'session' AND aggregate_id = ?`,
		testProjectID, sessionKey).Scan(&sessionHistory))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND aggregate_type = 'worker' AND aggregate_id = ?`,
		testProjectID, sessionKey+"/"+workerID).Scan(&workerHistory))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND aggregate_type = 'task' AND aggregate_id = ?`,
		testProjectID, task.ID).Scan(&taskHistory))
	assert.Equal(t, 1, sessionHistory)
	assert.Equal(t, 1, workerHistory)
	assert.Zero(t, taskHistory)
}

func TestDisconnectSessionRollsBackEveryResourceWhenWorkerHistoryFails(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	sessionID, workerID := "cleanup-atomic", "cleanup-worker"
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: sessionID, Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, sessionID, &model.AgentWorker{ID: workerID}))
	task := newTestTask("T-cleanup-atomic")
	task.Status = model.TaskStatusExecuting
	task.AssignedSessionID = &sessionID
	task.AssignedWorkerID = &workerID
	mustCreateTask(t, svc.stores.taskStore, task)
	require.NoError(t, svc.stores.workerStore.UpdateCurrentTask(ctx, testProjectID, sessionID, workerID, task.ID))
	seedTestWorktree(t, svc.stores, task.ID)
	_, err := svc.stores.db.ExecContext(ctx, `CREATE TRIGGER fail_cleanup_worker_history
		BEFORE INSERT ON state_history WHEN NEW.aggregate_type = 'worker'
		BEGIN SELECT RAISE(ABORT, 'FAIL_CLEANUP_WORKER_HISTORY'); END`)
	require.NoError(t, err)

	err = svc.sessSvc.DisconnectSession(ctx, testProjectID, sessionID)
	require.ErrorContains(t, err, "FAIL_CLEANUP_WORKER_HISTORY")
	session, err := svc.sessSvc.GetSession(ctx, testProjectID, sessionID)
	require.NoError(t, err)
	assert.Equal(t, model.SessionStatusOnline, session.Status)
	got, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusExecuting, got.Status)
	worker, err := svc.stores.workerStore.GetByID(ctx, testProjectID, sessionID, workerID)
	require.NoError(t, err)
	assert.Equal(t, model.WorkerStatusBusy, worker.Status)
	require.NotNil(t, worker.CurrentTaskID)
	worktree, err := svc.stores.worktreeStore.GetByTaskID(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	assert.Equal(t, model.WorktreeStatusActive, worktree.Status)
	var histories int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM state_history WHERE project_id = ?`, testProjectID).Scan(&histories))
	assert.Zero(t, histories)
}

func TestHeartbeatAndReleaseWorkerAppendExactStateHistory(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	sessionID, workerID := "history-session", "history-worker"
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: sessionID, Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, sessionID, &model.AgentWorker{ID: workerID}))
	require.NoError(t, svc.sessSvc.UpdateHeartbeat(ctx, testProjectID, sessionID))
	require.NoError(t, svc.sessSvc.ReleaseWorker(ctx, testProjectID, sessionID, workerID))

	session, err := svc.sessSvc.GetSession(ctx, testProjectID, sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), session.Version)
	worker, err := svc.stores.workerStore.GetByID(ctx, testProjectID, sessionID, workerID)
	require.NoError(t, err)
	assert.Equal(t, model.WorkerStatusLost, worker.Status)
	assert.Equal(t, int64(1), worker.Version)
	require.NoError(t, svc.sessSvc.ReleaseWorker(ctx, testProjectID, sessionID, workerID))
	again, err := svc.stores.workerStore.GetByID(ctx, testProjectID, sessionID, workerID)
	require.NoError(t, err)
	assert.Equal(t, worker.Version, again.Version)

	var invalid, total int
	require.NoError(t, svc.stores.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND (to_version <> from_version + 1 OR COALESCE(actor_id, '') = ''
		 OR reason = '' OR COALESCE(causation_id, '') = '')`, testProjectID).Scan(&invalid))
	require.NoError(t, svc.stores.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM state_history WHERE project_id = ?`, testProjectID).Scan(&total))
	assert.Zero(t, invalid)
	assert.Equal(t, 2, total)
}
