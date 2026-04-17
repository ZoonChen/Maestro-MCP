import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { createFullProjectSetup } from '../helpers/test-data';

test.describe('Force Rollback', () => {
  let pid: string;
  let featureId: string;

  test.beforeAll(async ({ request }) => {
    const client = createClient(request);
    const setup = await createFullProjectSetup(client);
    pid = setup.project.id;
    featureId = setup.feature.id;
  });

  test('force rollback in_progress task to pending', async ({ request }) => {
    const client = createClient(request);

    // Create and claim a task
    const task = await client.createTask(pid, {
      feature_id: featureId,
      title: 'Rollback Task',
      description: 'test',
      role: 'backend',
      allowed_directories: '["src/"]',
    });
    expect(task.status).toBe(200);
    const taskId = task.data.id;

    // Claim via getNextTask — the claimed task may differ from taskId
    const claimed = await client.getNextTask(pid, 'backend', 'worker-rb-1');
    expect(claimed.status).toBe(200);
    const claimedId = claimed.data.id;

    // Force rollback the claimed task (in_progress -> pending)
    const res = await client.forceRollbackTask(pid, claimedId);
    expect(res.status).toBe(200);

    // Verify back to pending
    const updated = await client.getTask(pid, claimedId);
    expect(updated.data.status).toBe('pending');
  });

  test('force rollback fails on cancelled task', async ({ request }) => {
    const client = createClient(request);

    const task = await client.createTask(pid, {
      feature_id: featureId,
      title: 'Rollback Fail Task',
      description: 'test',
      role: 'backend',
      allowed_directories: '["src/"]',
    });
    const taskId = task.data.id;

    // Cancel (terminal state)
    await client.cancelTask(pid, taskId, { reason: 'test' });

    const res = await client.forceRollbackTask(pid, taskId);
    expect(res.status).toBe(409);
  });
});
