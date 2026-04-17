import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Update Task', () => {
  test('should update title and description of a pending task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    const resp = await api.updateTask(projectId, taskId, {
      title: 'Updated Title',
      description: 'Updated description',
    });

    expect(resp.status).toBe(200);
    expect(resp.data.title).toBe('Updated Title');

    // Verify via GET
    const getResp = await api.getTask(projectId, taskId);
    expect(getResp.data.title).toBe('Updated Title');
  });

  test('should update priority of a pending task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    const resp = await api.updateTask(projectId, taskId, {
      priority: 'urgent',
    });

    expect(resp.status).toBe(200);
    expect(resp.data.priority).toBe('urgent');
  });

  test('should update allowed_directories of a pending task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    const resp = await api.updateTask(projectId, taskId, {
      allowed_directories: '["src/api/", "src/lib/"]',
    });

    expect(resp.status).toBe(200);
    expect(resp.data.allowed_directories).toBe('["src/api/", "src/lib/"]');
  });

  test('should allow partial update — only changed fields', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;
    const originalTitle = setup.tasks[0].title;

    // Update only description, title should remain unchanged
    const resp = await api.updateTask(projectId, taskId, {
      description: 'Only description changed',
    });

    expect(resp.status).toBe(200);
    expect(resp.data.description).toBe('Only description changed');
    expect(resp.data.title).toBe(originalTitle);
  });

  test('should reject update for non-existent task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;

    const resp = await api.updateTask(projectId, 'T-nonexistent', {
      title: 'Should Fail',
    });

    expect(resp.status).toBe(404);
    expect(resp.error).toBeTruthy();
  });

  test('should reject update for in_progress task restricted fields', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim the task to make it in_progress
    await api.getNextTask(projectId, 'backend', 'worker-update-1');

    // Try to update title — should be silently reset (not error, but title unchanged)
    const resp = await api.updateTask(projectId, taskId, {
      title: 'Try Change Title',
    });

    // Update succeeds (200) but title is reset to original
    expect(resp.status).toBe(200);
    expect(resp.data.title).toBe(setup.tasks[0].title);
  });

  test('should reject update for cancelled task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Cancel the task
    await api.cancelTask(projectId, taskId, { reason: 'test cancel' });

    // Try to update — should be rejected
    const resp = await api.updateTask(projectId, taskId, {
      title: 'Should Fail',
    });

    expect(resp.status).toBe(409);
    expect(resp.error).toBeTruthy();
  });

  test('should allow description update for in_progress task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim the task to make it in_progress
    await api.getNextTask(projectId, 'backend', 'worker-update-2');

    // Description update should succeed
    const resp = await api.updateTask(projectId, taskId, {
      description: 'Updated during work',
    });

    expect(resp.status).toBe(200);
    expect(resp.data.description).toBe('Updated during work');
  });
});
