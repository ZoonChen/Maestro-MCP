import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Blocker and Merge Conflict', () => {
  test('should report blocker', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim task first
    const claimResp = await api.getNextTask(projectId, 'backend', 'worker-block-test');
    expect(claimResp.status).toBe(200);
    expect(claimResp.data.status).toBe('in_progress');

    // Report blocker
    const blockResp = await api.blockTask(projectId, taskId, {
      reason: 'missing dependency',
    });
    expect(blockResp.status).toBe(200);
    expect(blockResp.error).toBeNull();
    expect(blockResp.data.status).toBe('blocked');
    expect(blockResp.data.blocker_reason).toBeTruthy();
    expect(blockResp.data.blocker_reason).toContain('missing dependency');
  });

  test('should resolve blocker with reassign', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim task
    const claimResp = await api.getNextTask(projectId, 'backend', 'worker-block-reassign');
    expect(claimResp.data.status).toBe('in_progress');
    const originalSessionId = claimResp.data.assigned_session_id;

    // Block task
    const blockResp = await api.blockTask(projectId, taskId, {
      reason: 'environment issue',
    });
    expect(blockResp.data.status).toBe('blocked');

    // Resolve blocker with reassign=true
    const resolveResp = await api.resolveBlocker(projectId, taskId, {
      reassign: true,
    });
    expect(resolveResp.status).toBe(200);
    expect(resolveResp.error).toBeNull();
    expect(resolveResp.data.status).toBe('in_progress');
    // assigned_session_id should be preserved when reassign=true
    expect(resolveResp.data.assigned_session_id).toBe(originalSessionId);
  });

  test('should resolve blocker without reassign', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Claim task
    const claimResp = await api.getNextTask(projectId, 'backend', 'worker-block-no-reassign');
    expect(claimResp.data.status).toBe('in_progress');

    // Block task
    const blockResp = await api.blockTask(projectId, taskId, {
      reason: 'blocked by another task',
    });
    expect(blockResp.data.status).toBe('blocked');

    // Resolve blocker with reassign=false
    const resolveResp = await api.resolveBlocker(projectId, taskId, {
      reassign: false,
    });
    expect(resolveResp.status).toBe(200);
    expect(resolveResp.error).toBeNull();
    expect(resolveResp.data.status).toBe('pending');
    // assigned_session_id should be null when reassign=false
    expect(resolveResp.data.assigned_session_id).toBeNull();
  });

  test('should fail to block non-in_progress task', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Task is still pending (not claimed)
    const getResp = await api.getTask(projectId, taskId);
    expect(getResp.data.status).toBe('pending');

    // Attempt to block should fail
    const blockResp = await api.blockTask(projectId, taskId, {
      reason: 'should not work',
    });
    expect(blockResp.status).toBe(409);
    expect(blockResp.error).toBeTruthy();
  });

  // Helper: advance a task through claim → submit → verify → ready_to_merge
  async function advanceToReadyToMerge(api: any, projectId: string, taskId: string) {
    // Claim
    const claimResp = await api.getNextTask(projectId, 'backend', 'worker-mc-' + taskId.slice(-4));
    expect(claimResp.status).toBe(200);

    // Submit (REST API simplified submit: in_progress → submitted)
    const submitResp = await api.submitTask(projectId, taskId, { summary: 'done' });
    expect(submitResp.status).toBe(200);

    // Register verifier session + worker
    const vSession = factory.session({ role: 'verifier' });
    await api.registerSession(projectId, vSession);
    const vWorker = factory.worker();
    await api.registerWorker(projectId, vSession.id, vWorker);

    // Get next verification task (submitted → verifying)
    const verifyClaimResp = await api.getNextVerificationTask(projectId, vSession.id, vWorker.id);
    expect(verifyClaimResp.status).toBe(200);

    // Verify (pass): verifying → ready_to_merge
    const verifyResp = await api.verifyTask(projectId, taskId, {
      session_id: vSession.id,
      worker_id: vWorker.id,
      passed: true,
      notes: 'looks good',
    });
    expect(verifyResp.status).toBe(200);
    expect(verifyResp.data.status).toBe('ready_to_merge');
  }

  // NOTE: merge conflict tests require a real git repo with conflicting changes.
  // Without that, merge succeeds (done) and never enters merge_conflicted state.
  // These tests are skipped in environments without a real git workspace.
  test.skip('should resolve merge conflict with reopen', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Advance to ready_to_merge
    await advanceToReadyToMerge(api, projectId, taskId);

    // Merge — requires real git repo with conflicting changes to produce merge_conflicted.
    const mergeResp = await api.mergeTask(projectId, taskId);
    expect(mergeResp.status).toBe(200);
    expect(mergeResp.data.status).toBe('merge_conflicted');

    // Resolve with reopen
    const resolveResp = await api.resolveMergeConflict(projectId, taskId, {
      action: 'reopen',
      reason: 'fixing merge conflict',
    });

    expect(resolveResp.status).toBe(200);
    expect(resolveResp.error).toBeNull();
    expect(resolveResp.data.status).toBe('in_progress');
  });

  test.skip('should resolve merge conflict with followup', async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    const projectId = setup.project.id;
    const taskId = setup.tasks[0].id;

    // Advance to ready_to_merge
    await advanceToReadyToMerge(api, projectId, taskId);

    // Merge — requires real git repo with conflicting changes to produce merge_conflicted.
    const mergeResp = await api.mergeTask(projectId, taskId);
    expect(mergeResp.status).toBe(200);
    expect(mergeResp.data.status).toBe('merge_conflicted');

    // Resolve with followup — original task stays merge_conflicted, a new task is created
    const resolveResp = await api.resolveMergeConflict(projectId, taskId, {
      action: 'followup',
      reason: 'creating follow-up task',
    });

    expect(resolveResp.status).toBe(200);
    expect(resolveResp.error).toBeNull();
    expect(resolveResp.data.status).toBe('merge_conflicted');
  });
});
