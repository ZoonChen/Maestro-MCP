import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Task Lifecycle', () => {
  test.describe.serial('Happy path: pending -> done', () => {
    // Shared state across serial tests
    let projectId: string;
    let featureId: string;
    let taskId: string;

    test('step 1: create task', async ({ request }) => {
      const api = createClient(request);

      // Create project
      const projectResp = await api.createProject(factory.project());
      expect(projectResp.status).toBe(200);
      expect(projectResp.error).toBeNull();
      expect(projectResp.data).toBeTruthy();
      projectId = projectResp.data.id;

      // Create feature
      const featureResp = await api.createFeature(projectId, factory.feature());
      expect(featureResp.status).toBe(200);
      expect(featureResp.error).toBeNull();
      featureId = featureResp.data.id;

      // Create task
      const taskResp = await api.createTask(
        projectId,
        factory.task({ feature_id: featureId, role: 'backend' }),
      );
      expect(taskResp.status).toBe(200);
      expect(taskResp.error).toBeNull();
      expect(taskResp.data.status).toBe('pending');
      taskId = taskResp.data.id;
    });

    test('step 2: claim task', async ({ request }) => {
      const api = createClient(request);
      const resp = await api.getNextTask(projectId, 'backend', 'worker-lifecycle-1');

      expect(resp.status).toBe(200);
      expect(resp.error).toBeNull();
      expect(resp.data).toBeTruthy();
      expect(resp.data.id).toBe(taskId);
      expect(resp.data.status).toBe('in_progress');
    });

    test('step 3: submit task', async ({ request }) => {
      const api = createClient(request);
      const resp = await api.submitTask(projectId, taskId, {
        summary: 'test summary',
      });

      expect(resp.status).toBe(200);
      expect(resp.error).toBeNull();
      expect(resp.data.status).toBe('submitted');
    });

    test('step 4: claim verification', async ({ request }) => {
      const api = createClient(request);
      const resp = await api.getNextVerificationTask(
        projectId,
        'verifier-lifecycle',
        'verifier-lifecycle-1',
      );

      expect(resp.status).toBe(200);
      expect(resp.error).toBeNull();
      expect(resp.data).toBeTruthy();
      expect(resp.data.id).toBe(taskId);
      expect(resp.data.status).toBe('verifying');
    });

    test('step 5: verify (passed)', async ({ request }) => {
      const api = createClient(request);
      const resp = await api.verifyTask(projectId, taskId, {
        session_id: 'verifier-api',
        worker_id: 'verifier-api',
        passed: true,
      });

      expect(resp.status).toBe(200);
      expect(resp.error).toBeNull();
      expect(resp.data.status).toBe('ready_to_merge');
    });

    test('step 6: merge task', async ({ request }) => {
      const api = createClient(request);
      const resp = await api.mergeTask(projectId, taskId);

      expect(resp.status).toBe(200);
      expect(resp.error).toBeNull();
      expect(resp.data.status).toBe('done');
    });
  });

  test('should reject verification and return to in_progress', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim task
    const claimResp = await api.getNextTask(projectId, 'backend', 'worker-reject-test');
    expect(claimResp.status).toBe(200);
    expect(claimResp.data.id).toBe(taskId);

    // Submit task
    const submitResp = await api.submitTask(projectId, taskId, { summary: 'work done' });
    expect(submitResp.status).toBe(200);
    expect(submitResp.data.status).toBe('submitted');

    // Claim verification
    const verifyClaimResp = await api.getNextVerificationTask(
      projectId,
      'verifier-reject',
      'verifier-reject-1',
    );
    expect(verifyClaimResp.status).toBe(200);
    expect(verifyClaimResp.data.status).toBe('verifying');

    // Reject verification
    const rejectResp = await api.verifyTask(projectId, taskId, {
      session_id: 'verifier-api',
      worker_id: 'verifier-api',
      passed: false,
      notes: 'does not meet requirements',
    });

    expect(rejectResp.status).toBe(200);
    expect(rejectResp.error).toBeNull();
    expect(rejectResp.data.status).toBe('in_progress');
  });

  test('should cancel pending task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Verify task starts as pending
    const getResp = await api.getTask(projectId, taskId);
    expect(getResp.data.status).toBe('pending');

    // Cancel task
    const cancelResp = await api.cancelTask(projectId, taskId, {
      reason: 'no longer needed',
    });
    expect(cancelResp.status).toBe(200);
    expect(cancelResp.error).toBeNull();
    expect(cancelResp.data.status).toBe('cancelled');
  });

  test('should cancel in_progress task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim task to move to in_progress
    const claimResp = await api.getNextTask(projectId, 'backend', 'worker-cancel-ip');
    expect(claimResp.data.id).toBe(taskId);
    expect(claimResp.data.status).toBe('in_progress');

    // Cancel task
    const cancelResp = await api.cancelTask(projectId, taskId, {
      reason: 'abandoned work',
    });
    expect(cancelResp.status).toBe(200);
    expect(cancelResp.error).toBeNull();
    expect(cancelResp.data.status).toBe('cancelled');
  });

  test('should cancel blocked task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim task
    const claimResp = await api.getNextTask(projectId, 'backend', 'worker-cancel-blocked');
    expect(claimResp.data.id).toBe(taskId);

    // Block task
    const blockResp = await api.blockTask(projectId, taskId, {
      reason: 'missing dependency',
    });
    expect(blockResp.status).toBe(200);
    expect(blockResp.data.status).toBe('blocked');

    // Cancel blocked task
    const cancelResp = await api.cancelTask(projectId, taskId, {
      reason: 'dependency permanently unavailable',
    });
    expect(cancelResp.status).toBe(200);
    expect(cancelResp.error).toBeNull();
    expect(cancelResp.data.status).toBe('cancelled');
  });

  test('should record activity log for each transition', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim task
    await api.getNextTask(projectId, 'backend', 'worker-activity-log');

    // Submit task
    await api.submitTask(projectId, taskId, { summary: 'activity log test' });

    // Fetch activity log
    const activityResp = await api.getActivity(projectId);
    expect(activityResp.status).toBe(200);
    expect(activityResp.error).toBeNull();
    expect(activityResp.data).toBeTruthy();

    const logs = activityResp.data;
    const actions = logs.map((entry: any) => entry.action);

    // Verify key transitions are logged
    expect(actions).toContain('created');
    expect(actions).toContain('claimed');
    expect(actions).toContain('submitted');
  });
});
