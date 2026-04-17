import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { createFullProjectSetup } from '../helpers/test-data';

test.describe('Task Detail API', () => {
  let pid: string;
  let taskId: string;

  test.beforeAll(async ({ request }) => {
    const client = createClient(request);
    const setup = await createFullProjectSetup(client);
    pid = setup.project.id;
    taskId = setup.tasks[0].id;
  });

  test('GET /tasks/:tid/validation returns history', async ({ request }) => {
    const client = createClient(request);
    const res = await client.getValidationHistory(pid, taskId);
    expect(res.status).toBe(200);
    // New task has no validation runs
    expect(Array.isArray(res.data)).toBe(true);
  });

  test('GET /tasks/:tid/result returns 404 when no result submitted', async ({ request }) => {
    const client = createClient(request);
    const res = await client.getTaskResult(pid, taskId);
    // Task has not been submitted yet, so no TaskResult exists.
    expect(res.status).toBe(404);
  });

  test('GET /tasks/:tid/diff handles missing worktree', async ({ request }) => {
    const client = createClient(request);
    const res = await client.getTaskDiff(pid, taskId);
    // May be 404 (no worktree) or 200 (empty diff)
    expect([200, 404]).toContain(res.status);
  });

  test('GET /overview includes task_counts', async ({ request }) => {
    const client = createClient(request);
    const res = await client.getOverview();
    expect(res.status).toBe(200);
    const proj = res.data.projects.find((p: any) => p.id === pid);
    expect(proj).toBeDefined();
    expect(proj.task_counts).toBeDefined();
  });
});
