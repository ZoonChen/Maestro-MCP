import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Feature Management', () => {
  let api: ReturnType<typeof createClient>;
  let projectId: string;

  test.beforeEach(async ({ request }) => {
    api = createClient(request);
    const projectResp = await api.createProject(factory.project());
    expect(projectResp.status).toBe(200);
    projectId = projectResp.data.id;
  });

  test('should create a feature', async () => {
    const data = factory.feature({ title: 'New Feature' });
    const resp = await api.createFeature(projectId, data);

    expect(resp.status).toBe(200);
    expect(resp.data).toBeDefined();
    expect(resp.data.id).toBeDefined();
    expect(resp.data.title).toBe('New Feature');
    expect(resp.data.status).toBe('planning');
  });

  test('should list features', async ({ request }) => {
    const created = await api.createFeature(projectId, factory.feature({ title: 'Listable Feature' }));
    expect(created.status).toBe(200);

    const resp = await api.listFeatures(projectId);

    expect(resp.status).toBe(200);
    expect(resp.data).toBeDefined();
    const titles = resp.data.map((f: any) => f.title);
    expect(titles).toContain('Listable Feature');
  });

  test('should get a feature by id', async () => {
    const data = factory.feature({ title: 'Gettable Feature', description: 'Detailed description' });
    const created = await api.createFeature(projectId, data);
    expect(created.status).toBe(200);

    const resp = await api.getFeature(projectId, created.data.id);

    expect(resp.status).toBe(200);
    expect(resp.data).toBeDefined();
    expect(resp.data.id).toBe(created.data.id);
    expect(resp.data.title).toBe('Gettable Feature');
    expect(resp.data.description).toBe('Detailed description');
    expect(resp.data.status).toBe('planning');
  });

  test('should update a feature', async () => {
    const created = await api.createFeature(projectId, factory.feature({ title: 'Original Title' }));
    expect(created.status).toBe(200);

    const resp = await api.updateFeature(projectId, created.data.id, { title: 'Updated Title' });

    expect(resp.status).toBe(200);
    expect(resp.data.title).toBe('Updated Title');

    // Verify via GET
    const fetched = await api.getFeature(projectId, created.data.id);
    expect(fetched.data.title).toBe('Updated Title');
  });

  test('should auto-transition feature from planning to active when task created', async () => {
    // Create a feature — starts in 'planning'
    const feature = await api.createFeature(projectId, factory.feature({ title: 'Auto Active' }));
    expect(feature.status).toBe(200);
    expect(feature.data.status).toBe('planning');

    // Create a task under this feature
    const taskResp = await api.createTask(
      projectId,
      factory.task({
        feature_id: feature.data.id,
        title: 'First task for feature',
        role: 'backend',
      }),
    );
    expect(taskResp.status).toBe(200);

    // Feature should now be 'active'
    const updated = await api.getFeature(projectId, feature.data.id);
    expect(updated.data.status).toBe('active');
  });

  test('should auto-transition feature to completed when all tasks done', async () => {
    // Create a feature
    const feature = await api.createFeature(projectId, factory.feature({ title: 'Auto Complete' }));
    expect(feature.status).toBe(200);

    // Create a single task
    const taskResp = await api.createTask(
      projectId,
      factory.task({
        feature_id: feature.data.id,
        title: 'Task to complete',
        role: 'backend',
      }),
    );
    expect(taskResp.status).toBe(200);
    const taskId = taskResp.data.id;

    // Register a session and worker
    const session = factory.session({ role: 'backend' });
    const sessionResp = await api.registerSession(projectId, session);
    expect(sessionResp.status).toBe(200);

    const worker = factory.worker();
    const workerResp = await api.registerWorker(projectId, session.id, worker);
    expect(workerResp.status).toBe(200);

    // Claim the task → in_progress
    const claimResp = await api.claimTask(projectId, taskId, {
      session_id: session.id,
      worker_id: worker.id,
    });
    expect(claimResp.status).toBe(200);

    // Submit the task → submitted
    const submitResp = await api.submitTask(projectId, taskId, { summary: 'Done', session_id: session.id });
    expect(submitResp.status).toBe(200);

    // Claim for verification → verifying
    const verifyClaimResp = await api.getNextVerificationTask(projectId, session.id, worker.id);
    expect(verifyClaimResp.status).toBe(200);

    // Verify the task (pass) → verifying → ready_to_merge
    const verifyResp = await api.verifyTask(projectId, taskId, {
      session_id: session.id,
      worker_id: worker.id,
      passed: true,
      notes: 'All good',
    });
    expect(verifyResp.status).toBe(200);

    // Merge the task → done
    const mergeResp = await api.mergeTask(projectId, taskId);
    expect(mergeResp.status).toBe(200);

    // Confirm task is done
    const taskCheck = await api.getTask(projectId, taskId);
    expect(taskCheck.data.status).toBe('done');

    // Feature should now be 'completed'
    const featureCheck = await api.getFeature(projectId, feature.data.id);
    expect(featureCheck.data.status).toBe('completed');
  });

  test('should isolate features between projects', async ({ request }) => {
    // Create a second project
    const project2Resp = await api.createProject(factory.project({ name: 'Second Project' }));
    expect(project2Resp.status).toBe(200);
    const project2Id = project2Resp.data.id;

    // Create features in both projects
    const f1 = await api.createFeature(projectId, factory.feature({ title: 'Project 1 Feature' }));
    const f2 = await api.createFeature(project2Id, factory.feature({ title: 'Project 2 Feature' }));
    expect(f1.status).toBe(200);
    expect(f2.status).toBe(200);

    // Project 1 should only see its own feature
    const list1 = await api.listFeatures(projectId);
    expect(list1.status).toBe(200);
    const titles1 = list1.data.map((f: any) => f.title);
    expect(titles1).toContain('Project 1 Feature');
    expect(titles1).not.toContain('Project 2 Feature');

    // Project 2 should only see its own feature
    const list2 = await api.listFeatures(project2Id);
    expect(list2.status).toBe(200);
    const titles2 = list2.data.map((f: any) => f.title);
    expect(titles2).toContain('Project 2 Feature');
    expect(titles2).not.toContain('Project 1 Feature');
  });
});
