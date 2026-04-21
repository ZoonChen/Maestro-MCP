import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Task Creation', () => {
  let api: ReturnType<typeof createClient>;
  let projectId: string;
  let featureId: string;

  test.beforeEach(async ({ request }) => {
    api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskCount: 0 });
    projectId = setup.project.id;
    featureId = setup.feature.id;
  });

  test('should create a task with defaults', async () => {
    const resp = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'Default Task',
        role: 'backend',
      }),
    );

    expect(resp.status).toBe(200);
    expect(resp.data).toBeDefined();
    expect(resp.data.id).toBeDefined();
    expect(resp.data.title).toBe('Default Task');
    expect(resp.data.status).toBe('pending');
    expect(resp.data.priority).toBe('normal');
    expect(resp.data.feature_id).toBe(featureId);
    expect(resp.data.role).toBe('backend');
    expect(resp.data.created_at).toBeDefined();
  });

  test('should create a task with all optional fields', async () => {
    const resp = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'Full Task',
        description: 'A task with all fields',
        role: 'backend',
        allowed_directories: '["src/", "pkg/"]',
        forbidden_patterns: '["*.env"]',
        required_apis: '["file_read", "file_write"]',
        dependencies: '[]',
        test_requirements: '{"command":"go test ./..."}',
        priority: 'high',
      }),
    );

    expect(resp.status).toBe(200);
    expect(resp.data.title).toBe('Full Task');
    expect(resp.data.priority).toBe('high');
    expect(resp.data.forbidden_patterns).toEqual(['*.env']);
    expect(resp.data.required_apis).toEqual(['file_read', 'file_write']);
    expect(resp.data.test_requirements).toEqual({command: 'go test ./...'});
  });

  test('should fail without title', async () => {
    const resp = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: '',
        role: 'backend',
      }),
    );

    expect(resp.status).toBe(400);
    expect(resp.error).toBeDefined();
  });

  test('should fail without role', async () => {
    const resp = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'No Role Task',
        role: '',
      }),
    );

    expect(resp.status).toBe(400);
    expect(resp.error).toBeDefined();
  });

  test('should fail with invalid role', async () => {
    const resp = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'Invalid Role Task',
        role: 'invalid_role',
      }),
    );

    expect(resp.status).toBe(400);
    expect(resp.error).toBeDefined();
  });

  test('should fail with non-existent feature_id', async () => {
    const resp = await api.createTask(
      projectId,
      factory.task({
        feature_id: 'non-existent-feature-id',
        title: 'Bad Feature Task',
        role: 'backend',
      }),
    );

    expect(resp.status).toBe(404);
    expect(resp.error).toBeDefined();
  });

  test('should order tasks by priority', async () => {
    // Create a low priority task first
    const lowResp = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'Low Priority Task',
        role: 'backend',
        priority: 'low',
      }),
    );
    expect(lowResp.status).toBe(200);

    // Create an urgent task second
    const urgentResp = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'Urgent Priority Task',
        role: 'backend',
        priority: 'urgent',
      }),
    );
    expect(urgentResp.status).toBe(200);

    // Register session + worker for claiming
    const session = factory.session({ role: 'backend' });
    await api.registerSession(projectId, session);
    const worker = factory.worker();
    await api.registerWorker(projectId, session.id, worker);

    // getNextTask should return the urgent task first
    const nextResp = await api.getNextTask(projectId, 'backend', worker.id);

    expect(nextResp.status).toBe(200);
    expect(nextResp.data).toBeDefined();
    expect(nextResp.data.title).toBe('Urgent Priority Task');
    expect(nextResp.data.priority).toBe('urgent');
  });

  test('should detect circular dependencies', async () => {
    // Step 1: Create T1 with no dependencies
    const t1Resp = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'Task T1',
        role: 'backend',
        dependencies: '[]',
      }),
    );
    expect(t1Resp.status).toBe(200);
    const t1Id = t1Resp.data.id;

    // Step 2: Create T2 depending on T1 — valid chain T2 → T1
    const t2Resp = await api.createTask(
      projectId,
      factory.task({
        feature_id: featureId,
        title: 'Task T2',
        role: 'backend',
        dependencies: JSON.stringify([{ task_id: t1Id, require_state: 'done' }]),
      }),
    );
    expect(t2Resp.status).toBe(200);
    const t2Id = t2Resp.data.id;

    // Step 3: Try to update T1 so that it depends on T2, forming a cycle T1 → T2 → T1.
    // The updateTask API does not expose a dependencies field in its typed signature,
    // so we use the raw PATCH endpoint directly to send the dependency update.
    const cycleResp = await (api as any).send(
      'PATCH',
      `/projects/${projectId}/tasks/${t1Id}`,
      {
        dependencies: JSON.stringify([{ task_id: t2Id, require_state: 'done' }]),
      },
    );

    // The server should reject this with a 422 error due to circular dependency
    expect(cycleResp.status).toBe(422);
    expect(cycleResp.error).toBeDefined();
  });
});
