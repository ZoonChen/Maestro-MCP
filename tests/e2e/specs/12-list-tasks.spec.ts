import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('List Tasks', () => {
  test('should list all tasks for a project', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 3, taskRole: 'backend' });
    const projectId = setup.project.id;

    const resp = await api.listTasks(projectId);

    expect(resp.status).toBe(200);
    expect(resp.data).toBeTruthy();
    expect(resp.data.length).toBe(3);
  });

  test('should filter tasks by status', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 2, taskRole: 'backend' });
    const projectId = setup.project.id;

    // Claim one task to make it in_progress
    await api.getNextTask(projectId, 'backend', 'worker-filter-1');

    // Filter for pending only
    const pendingResp = await api.listTasks(projectId, { status: 'pending' });
    expect(pendingResp.status).toBe(200);
    expect(pendingResp.data.length).toBe(1);
    expect(pendingResp.data[0].status).toBe('pending');

    // Filter for in_progress only
    const inProgressResp = await api.listTasks(projectId, { status: 'in_progress' });
    expect(inProgressResp.status).toBe(200);
    expect(inProgressResp.data.length).toBe(1);
    expect(inProgressResp.data[0].status).toBe('in_progress');
  });

  test('should filter tasks by role', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 0 });
    const projectId = setup.project.id;
    const featureId = setup.feature.id;

    // Create tasks with different roles
    await api.createTask(projectId, factory.task({ feature_id: featureId, role: 'backend', title: 'Backend Task' }));
    await api.createTask(projectId, factory.task({ feature_id: featureId, role: 'frontend', title: 'Frontend Task' }));
    await api.createTask(projectId, factory.task({ feature_id: featureId, role: 'backend', title: 'Another Backend' }));

    const backendResp = await api.listTasks(projectId, { role: 'backend' });
    expect(backendResp.status).toBe(200);
    expect(backendResp.data.length).toBe(2);

    const frontendResp = await api.listTasks(projectId, { role: 'frontend' });
    expect(frontendResp.status).toBe(200);
    expect(frontendResp.data.length).toBe(1);
    expect(frontendResp.data[0].role).toBe('frontend');
  });

  test('should filter tasks by feature_id', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 0 });
    const projectId = setup.project.id;
    const featureId1 = setup.feature.id;

    // Create a second feature
    const feature2Resp = await api.createFeature(projectId, factory.feature({ title: 'Feature 2' }));
    const featureId2 = feature2Resp.data.id;

    // Create tasks in different features
    await api.createTask(projectId, factory.task({ feature_id: featureId1, title: 'F1 Task' }));
    await api.createTask(projectId, factory.task({ feature_id: featureId2, title: 'F2 Task' }));

    const f1Resp = await api.listTasks(projectId, { feature_id: featureId1 });
    expect(f1Resp.status).toBe(200);
    expect(f1Resp.data.length).toBe(1);
    expect(f1Resp.data[0].feature_id).toBe(featureId1);

    const f2Resp = await api.listTasks(projectId, { feature_id: featureId2 });
    expect(f2Resp.status).toBe(200);
    expect(f2Resp.data.length).toBe(1);
    expect(f2Resp.data[0].feature_id).toBe(featureId2);
  });

  test('should return empty array for project with no tasks', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 0 });
    const projectId = setup.project.id;

    const resp = await api.listTasks(projectId);
    expect(resp.status).toBe(200);
    // Go nil slice serializes to null; treat both as "no tasks"
    expect(resp.data === null || resp.data.length === 0).toBe(true);
  });

  test('should not leak tasks across projects', async ({ request }) => {
    const api = createClient(request);
    const setup1 = await createFullProjectSetup(api, { taskCount: 2 });
    const setup2 = await createFullProjectSetup(api, { taskCount: 1 });

    const resp1 = await api.listTasks(setup1.project.id);
    expect(resp1.data.length).toBe(2);

    const resp2 = await api.listTasks(setup2.project.id);
    expect(resp2.data.length).toBe(1);
  });
});
