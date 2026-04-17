import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Verification and Merge', () => {
  let projectId: string;
  let taskId: string;

  test.beforeEach(async ({ request }) => {
    const api = createClient(request);
    const setup = await createFullProjectSetup(api, { taskRole: 'backend' });
    projectId = setup.project.id;
    taskId = setup.tasks[0].id;

    // Drive task to submitted state: claim -> submit
    await api.getNextTask(projectId, 'backend', `worker-vm-${Date.now()}`);
    await api.submitTask(projectId, taskId, { summary: 'ready for verification' });
  });

  test('should claim submitted task for verification', async ({ request }) => {
    const api = createClient(request);

    const resp = await api.getNextVerificationTask(
      projectId,
      `verifier-session-${Date.now()}`,
      `verifier-worker-${Date.now()}`,
    );

    expect(resp.status).toBe(200);
    expect(resp.error).toBeNull();
    expect(resp.data).toBeTruthy();
    expect(resp.data.id).toBe(taskId);
    expect(resp.data.status).toBe('verifying');
  });

  test('should complete verification and merge', async ({ request }) => {
    const api = createClient(request);

    // Claim verification
    const verifyClaimResp = await api.getNextVerificationTask(
      projectId,
      'verifier-merge-session',
      'verifier-merge-worker',
    );
    expect(verifyClaimResp.status).toBe(200);
    expect(verifyClaimResp.data.status).toBe('verifying');

    // Pass verification
    const verifyResp = await api.verifyTask(projectId, taskId, {
      session_id: 'verifier-api',
      worker_id: 'verifier-api',
      passed: true,
    });
    expect(verifyResp.status).toBe(200);
    expect(verifyResp.data.status).toBe('ready_to_merge');

    // Merge
    const mergeResp = await api.mergeTask(projectId, taskId);
    expect(mergeResp.status).toBe(200);
    expect(mergeResp.error).toBeNull();
    expect(mergeResp.data.status).toBe('done');
    // merge_commit may be empty when workspace has no real git repo
    expect(mergeResp.data.merge_commit).toBeDefined();
  });

  test('should reject merge for non-ready_to_merge task', async ({ request }) => {
    const api = createClient(request);

    // Task is in submitted state (not ready_to_merge)
    const mergeResp = await api.mergeTask(projectId, taskId);

    expect(mergeResp.status).toBe(409);
    expect(mergeResp.error).toBeTruthy();
  });

  test('should record merged activity log', async ({ request }) => {
    const api = createClient(request);

    // Complete full flow: claim verification -> verify -> merge
    await api.getNextVerificationTask(
      projectId,
      'verifier-log-session',
      'verifier-log-worker',
    );
    await api.verifyTask(projectId, taskId, {
      session_id: 'verifier-api',
      worker_id: 'verifier-api',
      passed: true,
    });
    await api.mergeTask(projectId, taskId);

    // Check activity log
    const activityResp = await api.getActivity(projectId);
    expect(activityResp.status).toBe(200);
    expect(activityResp.data).toBeTruthy();

    const actions = activityResp.data.map((entry: any) => entry.action);
    expect(actions).toContain('merged');
  });

  test('should handle verification rejection cycle', async ({ request }) => {
    const api = createClient(request);

    // Claim verification
    const claimResp = await api.getNextVerificationTask(
      projectId,
      'verifier-cycle-session',
      'verifier-cycle-worker',
    );
    expect(claimResp.status).toBe(200);
    expect(claimResp.data.status).toBe('verifying');

    // Reject verification
    const rejectResp = await api.verifyTask(projectId, taskId, {
      session_id: 'verifier-api',
      worker_id: 'verifier-api',
      passed: false,
      notes: 'needs rework',
    });
    expect(rejectResp.status).toBe(200);
    expect(rejectResp.data.status).toBe('in_progress');

    // Re-submit task
    const resubmitResp = await api.submitTask(projectId, taskId, {
      summary: 'reworked and ready again',
    });
    expect(resubmitResp.status).toBe(200);
    expect(resubmitResp.data.status).toBe('submitted');

    // Verify task can enter verification again
    const reclaimResp = await api.getNextVerificationTask(
      projectId,
      'verifier-cycle-session-2',
      'verifier-cycle-worker-2',
    );
    expect(reclaimResp.status).toBe(200);
    expect(reclaimResp.data.id).toBe(taskId);
    expect(reclaimResp.data.status).toBe('verifying');
  });
});
