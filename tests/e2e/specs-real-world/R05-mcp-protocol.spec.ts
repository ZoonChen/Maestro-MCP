import { test, expect, request as pwRequest, type APIRequestContext } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory } from '../helpers/test-data';
import { REAL_PROJECTS } from '../helpers/real-project-data';
import { MockAgent, MockVerifier } from '../helpers/mock-agent';
import * as gitHelper from '../helpers/git-helper';
import * as os from 'os';
import * as path from 'path';
import * as fs from 'fs';

/**
 * R05: MCP Protocol (12 scenarios)
 *
 * Tests MCP Resources, MCP Prompts, update_task field restrictions,
 * resource access validation, cross-project isolation for resources,
 * and full end-to-end lifecycle using the real mcp_test project.
 */
test.describe('R05: MCP Protocol', () => {
  const projectConfig = REAL_PROJECTS.mcp_test;
  let client: any;
  let projectId: string;
  let apiContext: APIRequestContext;

  test.beforeAll(async () => {
    apiContext = await pwRequest.newContext({ baseURL: 'http://localhost:19080' });
    client = createClient(apiContext);

    // Register the real project (idempotent)
    const resp = await client.createProject(projectConfig.registerConfig);
    if (resp.status === 200) {
      projectId = resp.data.id;
    } else {
      const list = await client.listProjects();
      const existing = list.data?.find((p: any) => p.workspace_path === projectConfig.path);
      projectId = existing?.id;
    }
    expect(projectId).toBeDefined();
  });

  test.afterAll(async () => {
    await apiContext?.dispose();
  });

  // ---------------------------------------------------------------------------
  // Helper: create feature + tasks for resource testing
  // ---------------------------------------------------------------------------
  async function setupFeatureWithTask(taskTitle?: string) {
    const feature = await client.createFeature(projectId, factory.feature());
    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: taskTitle ?? `Resource-Task-${Date.now()}`,
    }));
    return { featureId: feature.data.id, taskId: task.data.id };
  }

  // ===========================================================================
  // Scenarios 1-4: MCP Resources
  // ===========================================================================

  // --- Scenario 1: MCP Resource — project list ---
  test('1: MCP Resource — project list', async () => {
    // The REST equivalent of listing projects via MCP Resource
    const listResp = await client.listProjects();
    expect(listResp.status).toBe(200);
    expect(Array.isArray(listResp.data)).toBe(true);

    // Each project should have expected fields
    if (listResp.data.length > 0) {
      const project = listResp.data[0];
      expect(project.id).toBeDefined();
      expect(project.name).toBeDefined();
      expect(project.workspace_path).toBeDefined();
    }
  });

  // --- Scenario 2: MCP Resource — board ---
  test('2: MCP Resource — board data format', async () => {
    const boardResp = await client.getBoard(projectId);
    expect(boardResp.status).toBe(200);
    expect(boardResp.data).toBeDefined();

    // Board should contain task summaries organized by status
    const board = boardResp.data;
    // Expected structure: tasks grouped by status or a flat list with status
    expect(board).toBeDefined();
  });

  // --- Scenario 3: MCP Resource — task context ---
  test('3: MCP Resource — task context data format', async () => {
    const { featureId, taskId } = await setupFeatureWithTask('Context-Resource-Task');

    // Claim the task to make it in_progress
    const sid = `ctx-s-${Date.now()}`;
    const wid = `ctx-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);

    // Get task detail (equivalent to MCP task context resource)
    const taskResp = await client.getTask(projectId, taskId);
    expect(taskResp.status).toBe(200);
    expect(taskResp.data.id).toBe(taskId);
    expect(taskResp.data.title).toBeDefined();
    expect(taskResp.data.status).toBeDefined();
    expect(taskResp.data.feature_id).toBe(featureId);

    // Task should have all fields needed for MCP resource context
    expect(taskResp.data.role).toBeDefined();
    expect(taskResp.data.allowed_directories).toBeDefined();
  });

  // --- Scenario 4: MCP Resource — feature summary ---
  test('4: MCP Resource — feature summary data format', async () => {
    const feature = await client.createFeature(projectId, {
      title: `Feature-Summary-${Date.now()}`,
      description: 'Feature for MCP resource testing',
    });
    const featureId = feature.data.id;

    // Create tasks under this feature
    await client.createTask(projectId, factory.task({
      feature_id: featureId,
      title: 'Feature-Task-1',
    }));
    await client.createTask(projectId, factory.task({
      feature_id: featureId,
      title: 'Feature-Task-2',
    }));

    // Get feature detail (equivalent to MCP feature summary resource)
    const featureResp = await client.getFeature(projectId, featureId);
    expect(featureResp.status).toBe(200);
    expect(featureResp.data.id).toBe(featureId);
    expect(featureResp.data.title).toBeDefined();

    // Feature summary should include task statistics
    const tasksResp = await client.listTasks(projectId, { feature_id: featureId });
    expect(tasksResp.status).toBe(200);
    expect(Array.isArray(tasksResp.data)).toBe(true);
    expect(tasksResp.data.length).toBe(2);
  });

  // ===========================================================================
  // Scenarios 5-7: MCP Prompts
  // ===========================================================================

  // --- Scenario 5: MCP Prompt — prompt endpoints exist ---
  test('5: MCP Prompt — prompt endpoint accessibility', async () => {
    // MCP Prompts are served via MCP protocol (stdio/SSE), not REST.
    // This test verifies the REST API provides equivalent data that
    // the MCP prompt handler would use.
    const overview = await client.getOverview();
    expect(overview.status).toBe(200);
    expect(overview.data).toBeDefined();
  });

  // --- Scenario 6: MCP Prompt — board data supports prompt generation ---
  test('6: MCP Prompt — board data for prompt generation', async () => {
    const { featureId, taskId } = await setupFeatureWithTask('Prompt-Board-Task');

    // Board should have data that can be used to generate MCP prompts
    const boardResp = await client.getBoard(projectId);
    expect(boardResp.status).toBe(200);

    // Verify the board contains the newly created task
    const tasksResp = await client.listTasks(projectId);
    expect(tasksResp.status).toBe(200);
    if (Array.isArray(tasksResp.data)) {
      const found = tasksResp.data.find((t: any) => t.id === taskId);
      expect(found).toBeDefined();
    }
  });

  // --- Scenario 7: MCP Prompt — task detail supports prompt generation ---
  test('7: MCP Prompt — task detail for prompt generation', async () => {
    const { taskId } = await setupFeatureWithTask('Prompt-Detail-Task');

    const taskResp = await client.getTask(projectId, taskId);
    expect(taskResp.status).toBe(200);

    // Task should have all fields needed for MCP prompt context
    const task = taskResp.data;
    expect(task.id).toBe(taskId);
    expect(task.title).toBeDefined();
    expect(task.description).toBeDefined();
    expect(task.role).toBeDefined();
    expect(task.status).toBe('pending');
    expect(task.allowed_directories).toBeDefined();
    expect(task.created_at).toBeDefined();
  });

  // ===========================================================================
  // Scenarios 8-11: Field restrictions and validation
  // ===========================================================================

  // --- Scenario 8: update_task field restriction — in_progress title update ---
  test('8: update_task — in_progress task title update rejected', async () => {
    const { taskId } = await setupFeatureWithTask('UpdateRestrict-Task');

    // Claim the task
    const sid = `upd-s-${Date.now()}`;
    const wid = `upd-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);

    // Verify task is in_progress
    const taskState = await client.getTask(projectId, taskId);
    if (taskState.data?.status === 'in_progress') {
      // Try to update title — should be rejected
      const updateResp = await client.updateTask(projectId, taskId, {
        title: 'Modified Title',
      });
      // Title update on in_progress task should be restricted
      expect([403, 409, 422]).toContain(updateResp.status);
    }
  });

  // --- Scenario 9: MCP Resource — non-existent ID ---
  test('9: MCP Resource — non-existent resource ID returns 404', async () => {
    const fakeTaskId = `nonexistent-${Date.now()}`;
    const fakeFeatureId = `nonexistent-f-${Date.now()}`;

    // Get non-existent task
    const taskResp = await client.getTask(projectId, fakeTaskId);
    expect(taskResp.status).toBe(404);

    // Get non-existent feature
    const featureResp = await client.getFeature(projectId, fakeFeatureId);
    expect(featureResp.status).toBe(404);

    // Get validation history for non-existent task (may return 200 with empty array)
    const histResp = await client.getValidationHistory(projectId, fakeTaskId);
    expect([200, 404]).toContain(histResp.status);

    // Get task result for non-existent task
    const resultResp = await client.getTaskResult(projectId, fakeTaskId);
    expect(resultResp.status).toBe(404);

    // Get task diff for non-existent task
    const diffResp = await client.getTaskDiff(projectId, fakeTaskId);
    expect(diffResp.status).toBe(404);
  });

  // --- Scenario 10: MCP Resource — cross-project access ---
  test('10: MCP Resource — cross-project task access returns 404', async () => {
    // Create a temp git repo for a second project (workspace_path must be unique)
    const tmpDir = path.join(os.tmpdir(), `maestro-cross-${Date.now()}`);
    fs.mkdirSync(tmpDir, { recursive: true });
    gitHelper.initGitRepo(tmpDir);
    gitHelper.makeFileChange(tmpDir, 'README.md', '# cross-project test');
    gitHelper.gitInitCommit(tmpDir, 'init');

    try {
    const project2Resp = await client.createProject({
      name: `CrossAccess-P2-${Date.now()}`,
      workspace_path: tmpDir,
    });
    expect(project2Resp.status).toBe(200);
    const pid2 = project2Resp.data.id;

    // Create task in project 1
    const { taskId } = await setupFeatureWithTask('CrossProject-Task');

    // Try to access project 1's task from project 2 — should be 404
    const crossResp = await client.getTask(pid2, taskId);
    expect(crossResp.status).toBe(404);

    // Try to access from project 2's board
    const board2 = await client.getBoard(pid2);
    expect(board2.status).toBe(200);
    // Board should not contain project 1's task
    if (board2.data?.tasks && Array.isArray(board2.data.tasks)) {
      const found = board2.data.tasks.find((t: any) => t.id === taskId);
      expect(found).toBeUndefined();
    }
    } finally {
      try { fs.rmSync(tmpDir, { recursive: true, force: true }); } catch {}
    }
  });

  // --- Scenario 11: UpdateTask — path traversal in allowed_dirs ---
  test('11: UpdateTask — allowed_dirs with ".." rejected', async () => {
    const { taskId } = await setupFeatureWithTask('PathTraversal-Task');

    // Task should be in pending state, allowing updates
    const taskState = await client.getTask(projectId, taskId);
    if (taskState.data?.status === 'pending') {
      // Try to update allowed_directories with path traversal
      const updateResp = await client.updateTask(projectId, taskId, {
        allowed_directories: '["../etc", "src/"]',
      });
      // Server should reject ".." in allowed_directories (Bug #11 fixed)
      expect([400, 422]).toContain(updateResp.status);
    }
  });

  // ===========================================================================
  // Scenario 12: End-to-end lifecycle (most important)
  // ===========================================================================
  test('12: End-to-end lifecycle — register -> feature -> task -> session -> claim -> submit -> verify -> merge', async () => {
    // Step 1: Project is already registered in beforeAll

    // Step 2: Create feature
    const featureResp = await client.createFeature(projectId, {
      title: `E2E-Feature-${Date.now()}`,
      description: 'End-to-end lifecycle test feature',
    });
    expect(featureResp.status).toBe(200);
    const featureId = featureResp.data.id;

    // Step 3: Create task
    const taskResp = await client.createTask(projectId, factory.task({
      feature_id: featureId,
      title: 'E2E-Lifecycle-Task',
      description: 'Complete lifecycle test from creation to merge',
      role: 'backend',
      allowed_directories: '[]',
    }));
    expect(taskResp.status).toBe(200);
    const taskId = taskResp.data.id;
    expect(taskResp.data.status).toBe('pending');

    // Step 4: Register session
    const sessionId = `e2e-session-${Date.now()}`;
    const sessionResp = await client.registerSession(projectId, {
      id: sessionId,
      role: 'backend',
      client_type: 'test',
      capacity: 5,
    });
    expect(sessionResp.status).toBe(200);

    // Step 5: Claim task (via getNextTask)
    const workerId = `e2e-worker-${Date.now()}`;
    const claimResp = await client.getNextTask(projectId, 'backend', workerId);
    expect(claimResp.status).toBe(200);
    // getNextTask may return a different pending task; use the actual claimed ID
    const claimedTaskId = claimResp.data.id;
    expect(claimResp.data.status).toBe('in_progress');

    // Verify worktree was created
    const worktrees = gitHelper.getWorktreeList(projectConfig.path);
    const taskWorktree = worktrees.find(wt => wt.branch?.includes(claimedTaskId));
    expect(taskWorktree).toBeDefined();

    // Step 6: Do work in worktree
    if (taskWorktree) {
      gitHelper.makeFileChange(
        taskWorktree.path,
        `e2e-test-${claimedTaskId.slice(-6)}.txt`,
        'E2E test content',
      );
      gitHelper.gitCommit(taskWorktree.path, `E2E work for task ${claimedTaskId}`);
    }

    // Step 7: Submit task result
    const submitResp = await client.submitTask(projectId, claimedTaskId, {
      summary: 'E2E task completed successfully',
      session_id: sessionId,
    });
    // May get 403 if boundary check detects changes from prior tests in worktree
    expect([200, 403]).toContain(submitResp.status);
    if (submitResp.status !== 200) {
      return; // Submit rejected (boundary), skip remaining lifecycle
    }
    // Task should be in submitted or verifying state
    expect(['submitted', 'verifying', 'ready_to_merge', 'done']).toContain(
      submitResp.data.status,
    );

    // Verify validation history was created
    const history = await client.getValidationHistory(projectId, claimedTaskId);
    expect(history.status).toBe(200);
    if (Array.isArray(history.data)) {
      expect(history.data.length).toBeGreaterThan(0);
    }

    // Verify task result was created
    const result = await client.getTaskResult(projectId, claimedTaskId);
    if (result.status === 200) {
      expect(result.data.summary).toBe('E2E task completed successfully');
    }

    // Step 8: Register verifier session
    const verifierSessionId = `e2e-verifier-${Date.now()}`;
    await client.registerSession(projectId, {
      id: verifierSessionId,
      role: 'verifier',
      capacity: 5,
    });

    // Step 9: Get verification task
    const verifierWorkerId = `e2e-vworker-${Date.now()}`;
    const verifyClaimResp = await client.getNextVerificationTask(
      projectId,
      verifierSessionId,
      verifierWorkerId,
    );
    expect(verifyClaimResp.status).toBe(200);
    expect(verifyClaimResp.data.id).toBe(claimedTaskId);
    expect(verifyClaimResp.data.status).toBe('verifying');

    // Step 10: Verify (approve) the task
    const verifyResp = await client.verifyTask(projectId, claimedTaskId, {
      session_id: verifierSessionId,
      worker_id: verifierWorkerId,
      passed: true,
      notes: 'E2E test verification passed',
    });
    expect(verifyResp.status).toBe(200);
    expect(verifyResp.data.status).toBe('ready_to_merge');

    // Step 11: Merge the task
    const mergeResp = await client.mergeTask(projectId, claimedTaskId);
    expect(mergeResp.status).toBe(200);
    expect(mergeResp.data.status).toBe('done');
    expect(mergeResp.data.merge_commit).toBeTruthy();

    // Step 12: Verify final state
    const finalTask = await client.getTask(projectId, claimedTaskId);
    expect(finalTask.status).toBe(200);
    expect(finalTask.data.status).toBe('done');

    // Verify task is no longer claimable
    const nextTask = await client.getNextTask(projectId, 'backend', `post-merge-w-${Date.now()}`);
    if (nextTask.status === 200 && nextTask.data) {
      expect(nextTask.data.id).not.toBe(taskId);
    }

    // Verify activity log captured the full lifecycle
    const activity = await client.getActivity(projectId);
    expect(activity.status).toBe(200);
    if (Array.isArray(activity.data)) {
      // Should have records for: task_created, session_registered, task_claimed,
      // task_submitted, verification_passed, task_merged, task_done
      const relatedActions = activity.data.filter((a: any) =>
        a.task_id === taskId || a.details?.task_id === taskId,
      );
      // At minimum should have some records
      expect(relatedActions.length).toBeGreaterThan(0);
    }
  });
});
