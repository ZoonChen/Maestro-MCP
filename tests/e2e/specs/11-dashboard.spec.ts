import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Dashboard', () => {
  let projectId: string;

  test.beforeEach(async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 3 });
    projectId = setup.project.id;
  });

  test('should return board data', async ({ request }) => {
    const api = createClient(request);
    const resp = await api.getBoard(projectId);

    expect(resp.status).toBe(200);
    expect(resp.data).toBeTruthy();
    expect(resp.data!.task_counts).toBeDefined();
    expect(resp.data!.total_tasks).toBe(3);
    expect(Array.isArray(resp.data!.features)).toBeTruthy();
    expect(resp.data!.total_features).toBeGreaterThanOrEqual(1);
  });

  test('should return activity log', async ({ request }) => {
    const api = createClient(request);

    // Claim a task to generate activity
    const nextResp = await api.getNextTask(projectId, 'backend', 'worker-activity');
    expect(nextResp.status).toBe(200);

    const resp = await api.getActivity(projectId);
    expect(resp.status).toBe(200);
    expect(Array.isArray(resp.data)).toBeTruthy();
    expect(resp.data!.length).toBeGreaterThanOrEqual(1);

    // Each activity entry should have core fields
    const entry = resp.data![0];
    expect(entry.id).toBeDefined();
    expect(entry.action).toBeDefined();
    expect(entry.created_at).toBeDefined();
  });

  test('should filter activity by limit', async ({ request }) => {
    const api = createClient(request);

    // Generate some activity
    await api.getNextTask(projectId, 'backend', 'worker-limit-1');
    await api.getNextTask(projectId, 'backend', 'worker-limit-2');

    const resp = await api.getActivity(projectId, { limit: 1 });
    expect(resp.status).toBe(200);
    expect(Array.isArray(resp.data)).toBeTruthy();
    expect(resp.data!.length).toBe(1);
  });

  test('should return overview with project summaries', async ({ request }) => {
    const api = createClient(request);
    const resp = await api.getOverview();

    expect(resp.status).toBe(200);
    expect(resp.data).toBeDefined();
    expect(typeof resp.data!.total_projects).toBe('number');
    expect(Array.isArray(resp.data!.projects)).toBeTruthy();
    expect(resp.data!.total_projects).toBeGreaterThanOrEqual(1);

    // Verify project summary structure
    const proj = resp.data!.projects[0];
    expect(proj.id).toBeDefined();
    expect(proj.name).toBeDefined();
    expect(proj.status).toBeDefined();
  });
});
