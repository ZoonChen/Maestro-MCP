import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Concurrent Operations', () => {
  test('should handle concurrent claim of same task', async ({ request }) => {
    const api = createClient(request);
    const { project, tasks } = await createFullProjectSetup(api, {
      taskCount: 1,
      taskRole: 'backend',
    });
    const pid = project.id;

    // Fire two concurrent getNextTask requests for the same role
    const [r1, r2] = await Promise.all([
      api.getNextTask(pid, 'backend', 'worker-concurrent-1'),
      api.getNextTask(pid, 'backend', 'worker-concurrent-2'),
    ]);

    // Exactly one must succeed (200), the other must fail (404 or 409)
    const successCount = (r1.status === 200 ? 1 : 0) + (r2.status === 200 ? 1 : 0);
    expect(successCount).toBe(1);

    const failStatus = r1.status !== 200 ? r1.status : r2.status;
    expect([404, 409]).toContain(failStatus);
  });

  test('should handle concurrent cancel and claim', async ({ request }) => {
    const api = createClient(request);
    const { project, tasks } = await createFullProjectSetup(api, {
      taskCount: 1,
      taskRole: 'backend',
    });
    const pid = project.id;
    const taskId = tasks[0].id;

    // Fire cancel and getNextTask concurrently
    const [cancelResp, claimResp] = await Promise.all([
      api.cancelTask(pid, taskId, { reason: 'concurrent cancel test' }),
      api.getNextTask(pid, 'backend', 'worker-cancel-1'),
    ]);

    // The task's final state must be either in_progress (claimed) or cancelled
    // pending is NOT acceptable — it means neither operation took effect
    const taskResp = await api.getTask(pid, taskId);
    expect(taskResp.status).toBe(200);
    const finalStatus = taskResp.data!.status;
    expect(['in_progress', 'cancelled']).toContain(finalStatus);
  });

  test('should handle rapid sequential claims', async ({ request }) => {
    const api = createClient(request);
    const { project, tasks } = await createFullProjectSetup(api, {
      taskCount: 3,
      taskRole: 'backend',
    });
    const pid = project.id;

    // Claim 3 tasks sequentially, each with a different worker
    const claimedIds: string[] = [];
    for (let i = 0; i < 3; i++) {
      const resp = await api.getNextTask(pid, 'backend', `worker-seq-${i}`);
      expect(resp.status).toBe(200);
      expect(resp.data).toBeTruthy();
      claimedIds.push(resp.data!.id);
    }

    // All claimed task IDs should be unique
    const uniqueIds = new Set(claimedIds);
    expect(uniqueIds.size).toBe(3);

    // 4th claim should return 404 (no more tasks)
    const resp4 = await api.getNextTask(pid, 'backend', 'worker-seq-3');
    expect(resp4.status).toBe(404);
  });
});
