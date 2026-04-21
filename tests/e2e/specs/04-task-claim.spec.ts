import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Task Claim', () => {
  test('should claim next task atomically via getNextTask', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    const resp = await api.getNextTask(projectId, 'backend', 'worker-claim-1');

    expect(resp.status).toBe(200);
    expect(resp.data).toBeTruthy();
    expect(resp.data.id).toBe(taskId);
    expect(resp.data.status).toBe('in_progress');
    expect(resp.data.assigned_session_id).toBeTruthy();
    expect(resp.data.assigned_worker_id).toBe('worker-claim-1');
  });

  test('should claim a specific task by id', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Register session + worker
    const session = factory.session({ role: 'backend' });
    await api.registerSession(projectId, session);
    const worker = factory.worker();
    await api.registerWorker(projectId, session.id, worker);

    const resp = await api.claimTask(projectId, taskId, {
      session_id: session.id,
      worker_id: worker.id,
    });

    expect(resp.status).toBe(200);
    expect(resp.data).toBeTruthy();
    expect(resp.data.id).toBe(taskId);
    expect(resp.data.status).toBe('in_progress');
    expect(resp.data.assigned_session_id).toBe(session.id);
    expect(resp.data.assigned_worker_id).toBe(worker.id);
  });

  test('should fail to claim non-pending task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim the task first
    const claimResp = await api.getNextTask(projectId, 'backend', 'worker-already-claimed');
    expect(claimResp.status).toBe(200);

    // Register session + worker for second claim attempt
    const session = factory.session({ role: 'backend' });
    await api.registerSession(projectId, session);
    const worker = factory.worker();
    await api.registerWorker(projectId, session.id, worker);

    // Try to claim the same task again
    const resp = await api.claimTask(projectId, taskId, {
      session_id: session.id,
      worker_id: worker.id,
    });

    // After B3 atomic claim: claiming an already-claimed task returns 409 Conflict
    // (the Claim method uses WHERE status='pending' and returns ErrConcurrentConflict
    // when the task exists but is no longer pending).
    expect(resp.status).toBe(409);
    expect(resp.error).toBeTruthy();
  });

  test('should return 404 when no tasks available', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;

    // Claim the only task
    await api.getNextTask(projectId, 'backend', 'worker-exhaust-1');

    // No more tasks available for same role
    const resp = await api.getNextTask(projectId, 'backend', 'worker-exhaust-2');

    expect(resp.status).toBe(404);
    expect(resp.error).toBeTruthy();
  });

  test('should not claim task with unmet dependencies', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 0 });
    const projectId = setup.project.id;
    const featureId = setup.feature.id;

    // Create T1 (pending)
    const t1 = await api.createTask(
      projectId,
      factory.task({ feature_id: featureId, title: 'T1', role: 'backend' }),
    );
    expect(t1.status).toBe(200);

    // Create T2 depending on T1 being done
    const t2 = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'T2',
        role: 'backend',
        dependencies: JSON.stringify([{ task_id: t1.data.id, require_state: 'done' }]),
      }),
    );
    expect(t2.status).toBe(200);

    // getNextTask should return T1 (no deps), not T2
    const resp = await api.getNextTask(projectId, 'backend', 'worker-dep-1');
    expect(resp.status).toBe(200);
    expect(resp.data.id).toBe(t1.data.id);
  });

  test('should treat cancelled dependencies as satisfied', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 0 });
    const projectId = setup.project.id;
    const featureId = setup.feature.id;

    // Create T1 and immediately cancel it
    const t1 = await api.createTask(
      projectId,
      factory.task({ feature_id: featureId, title: 'T1-cancel', role: 'backend' }),
    );
    expect(t1.status).toBe(200);

    await api.cancelTask(projectId, t1.data.id, { reason: 'no longer needed' });
    const t1Check = await api.getTask(projectId, t1.data.id);
    expect(t1Check.data.status).toBe('cancelled');

    // Create T2 depending on T1 (done state)
    const t2 = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'T2-after-cancel',
        role: 'backend',
        dependencies: JSON.stringify([{ task_id: t1.data.id, require_state: 'done' }]),
      }),
    );
    expect(t2.status).toBe(200);

    // T2 should be claimable because cancelled dependency is treated as satisfied
    const resp = await api.getNextTask(projectId, 'backend', 'worker-dep-cancel');
    expect(resp.status).toBe(200);
    expect(resp.data.id).toBe(t2.data.id);
  });

  test('should filter tasks by role', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 0 });
    const projectId = setup.project.id;
    const featureId = setup.feature.id;

    // Create a backend task
    const backendTask = await api.createTask(
      projectId,
      factory.task({ feature_id: featureId, title: 'Backend Task', role: 'backend' }),
    );
    expect(backendTask.status).toBe(200);

    // Create a frontend task
    const frontendTask = await api.createTask(
      projectId,
      factory.task({ feature_id: featureId, title: 'Frontend Task', role: 'frontend' }),
    );
    expect(frontendTask.status).toBe(200);

    // GetNextTask with role=frontend should only return the frontend task
    const resp = await api.getNextTask(projectId, 'frontend', 'worker-frontend-1');
    expect(resp.status).toBe(200);
    expect(resp.data.id).toBe(frontendTask.data.id);
    expect(resp.data.role).toBe('frontend');
  });
});
