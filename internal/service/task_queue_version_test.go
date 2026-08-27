package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueueVersionTracksQueuedMembershipAndOrdering(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	draft := newTestTask("T-queue-draft")
	draft.Status = model.TaskStatusDraft
	require.NoError(t, svc.taskSvc.CreateTask(ctx, testProjectID, draft))
	assert.Equal(t, int64(0), readQueueVersion(t, svc), "draft is not claimable")

	task := newTestTask("T-queue-version")
	require.NoError(t, svc.taskSvc.CreateTask(ctx, testProjectID, task))
	assert.Equal(t, int64(1), readQueueVersion(t, svc), "queued creation changes membership")

	stored, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	stored.Title = "A non-ordering edit"
	require.NoError(t, svc.taskSvc.UpdateTask(ctx, testProjectID, stored))
	assert.Equal(t, int64(1), readQueueVersion(t, svc), "title does not change claim order")

	stored.Priority = model.PriorityHigh
	require.NoError(t, svc.taskSvc.UpdateTask(ctx, testProjectID, stored))
	assert.Equal(t, int64(2), readQueueVersion(t, svc), "priority changes queue order")

	stored.Role = model.RoleFrontend
	require.NoError(t, svc.taskSvc.UpdateTask(ctx, testProjectID, stored))
	assert.Equal(t, int64(3), readQueueVersion(t, svc), "role moves the task to another queue partition")

	stored.Dependencies = []byte(`[{"task_id":"T-not-yet-created","require_state":"done"}]`)
	require.NoError(t, svc.taskSvc.UpdateTask(ctx, testProjectID, stored))
	assert.Equal(t, int64(4), readQueueVersion(t, svc), "dependencies change claim eligibility")

	require.NoError(t, svc.taskSvc.CancelTask(ctx, testProjectID, task.ID, "coordinator", "obsolete"))
	assert.Equal(t, int64(5), readQueueVersion(t, svc), "queued cancellation changes membership")

	blocked := newTestTask("T-blocked-edit")
	blocked.Status = model.TaskStatusBlocked
	mustCreateTask(t, svc.stores.taskStore, blocked)
	blocked.Priority = model.PriorityUrgent
	require.NoError(t, svc.taskSvc.UpdateTask(ctx, testProjectID, blocked))
	assert.Equal(t, int64(5), readQueueVersion(t, svc), "non-queued edits do not change the queue snapshot")
}

func TestQueueVersionWritesRollbackWithBusinessMutation(t *testing.T) {
	tests := []struct {
		name   string
		action func(t *testing.T, svc *testServices) error
		verify func(t *testing.T, svc *testServices)
	}{
		{
			name: "create audit failure",
			action: func(t *testing.T, svc *testServices) error {
				require.NoError(t, installAuditFailureTrigger(svc, "task.create"))
				return svc.taskSvc.CreateTask(context.Background(), testProjectID, newTestTask("T-create-rollback"))
			},
			verify: func(t *testing.T, svc *testServices) {
				_, err := svc.taskSvc.GetTask(context.Background(), testProjectID, "T-create-rollback")
				require.ErrorIs(t, err, store.ErrTaskNotFound)
				assert.Equal(t, int64(0), readQueueVersion(t, svc))
			},
		},
		{
			name: "update audit failure",
			action: func(t *testing.T, svc *testServices) error {
				task := newTestTask("T-update-rollback")
				require.NoError(t, svc.taskSvc.CreateTask(context.Background(), testProjectID, task))
				require.NoError(t, installAuditFailureTrigger(svc, "task.update"))
				task.Priority = model.PriorityUrgent
				return svc.taskSvc.UpdateTask(context.Background(), testProjectID, task)
			},
			verify: func(t *testing.T, svc *testServices) {
				task, err := svc.taskSvc.GetTask(context.Background(), testProjectID, "T-update-rollback")
				require.NoError(t, err)
				assert.Equal(t, model.PriorityNormal, task.Priority)
				assert.Equal(t, int64(0), task.Version)
				assert.Equal(t, int64(1), readQueueVersion(t, svc))
			},
		},
		{
			name: "cancel audit failure",
			action: func(t *testing.T, svc *testServices) error {
				task := newTestTask("T-cancel-rollback")
				require.NoError(t, svc.taskSvc.CreateTask(context.Background(), testProjectID, task))
				require.NoError(t, installAuditFailureTrigger(svc, "task.cancel"))
				return svc.taskSvc.CancelTask(context.Background(), testProjectID, task.ID, "coordinator", "rollback")
			},
			verify: func(t *testing.T, svc *testServices) {
				task, err := svc.taskSvc.GetTask(context.Background(), testProjectID, "T-cancel-rollback")
				require.NoError(t, err)
				assert.Equal(t, model.TaskStatusQueued, task.Status)
				assert.Equal(t, int64(0), task.Version)
				assert.Equal(t, int64(1), readQueueVersion(t, svc))
				var history int
				require.NoError(t, svc.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
					WHERE project_id = ? AND aggregate_id = ?`, testProjectID, task.ID).Scan(&history))
				assert.Zero(t, history)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := setupTestEnv(t)
			err := tt.action(t, svc)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "FAIL_QUEUE_AUDIT")
			tt.verify(t, svc)
		})
	}
}

func TestQueueVersionConcurrentUpdateCASHasOneEffect(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	task := newTestTask("T-queue-update-cas")
	require.NoError(t, svc.taskSvc.CreateTask(ctx, testProjectID, task))

	first, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	second, err := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, err)
	first.Priority = model.PriorityHigh
	second.Priority = model.PriorityUrgent

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, candidate := range []*model.Task{first, second} {
		wg.Add(1)
		go func(candidate *model.Task) {
			defer wg.Done()
			<-start
			errs <- svc.taskSvc.UpdateTask(ctx, testProjectID, candidate)
		}(candidate)
	}
	close(start)
	wg.Wait()
	close(errs)

	var succeeded, conflicted int
	for err := range errs {
		if err == nil {
			succeeded++
		} else if errors.Is(err, store.ErrConcurrentConflict) {
			conflicted++
		} else {
			t.Fatalf("unexpected update error: %v", err)
		}
	}
	assert.Equal(t, 1, succeeded)
	assert.Equal(t, 1, conflicted)
	assert.Equal(t, int64(2), readQueueVersion(t, svc), "failed CAS must not increment queue version")
}

func TestQueueVersionConcurrentCreatesDoNotLoseIncrements(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	const count = 20
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs <- svc.taskSvc.CreateTask(ctx, testProjectID, newTestTask(fmt.Sprintf("T-queue-create-%02d", i)))
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(count), readQueueVersion(t, svc))
}

func TestQueueVersionRejectsStaleClaimAfterOrderingEdit(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()
	task := newTestTask("T-stale-queue-claim")
	require.NoError(t, svc.taskSvc.CreateTask(ctx, testProjectID, task))
	require.NoError(t, svc.sessSvc.RegisterSession(ctx, testProjectID, &model.AgentSession{
		ID: "stale-claim-session", Role: model.RoleBackend, ClientType: "test", Capacity: 1,
	}))
	require.NoError(t, svc.sessSvc.RegisterWorker(ctx, testProjectID, "stale-claim-session", &model.AgentWorker{ID: "stale-claim-worker"}))

	staleVersion := readQueueVersion(t, svc)
	task.Priority = model.PriorityHigh
	require.NoError(t, svc.taskSvc.UpdateTask(ctx, testProjectID, task))
	_, err := svc.taskSvc.GetNextTaskWithVersion(ctx, testProjectID,
		"stale-claim-session", model.RoleBackend, "stale-claim-worker",
		"stale-queue-claim-0001", staleVersion)
	require.ErrorIs(t, err, store.ErrConcurrentConflict)
	stored, getErr := svc.taskSvc.GetTask(ctx, testProjectID, task.ID)
	require.NoError(t, getErr)
	assert.Equal(t, model.TaskStatusQueued, stored.Status)
	assert.Nil(t, stored.ActiveLeaseID)
}

func installAuditFailureTrigger(svc *testServices, action string) error {
	_, err := svc.stores.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_queue_audit BEFORE INSERT ON audit_log
		WHEN NEW.action = '%s' BEGIN SELECT RAISE(ABORT, 'FAIL_QUEUE_AUDIT'); END`, action))
	return err
}
