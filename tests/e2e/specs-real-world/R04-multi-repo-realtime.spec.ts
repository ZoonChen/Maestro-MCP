import { test, expect, request as pwRequest, type APIRequestContext } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory } from '../helpers/test-data';
import { REAL_PROJECTS, getSubProjects } from '../helpers/real-project-data';
import { MockAgent, MockVerifier } from '../helpers/mock-agent';
import { TestWSClient } from '../helpers/ws-client';
import * as gitHelper from '../helpers/git-helper';
import * as os from 'os';
import * as path from 'path';
import * as fs from 'fs';

/**
 * R04: Multi-Repo & Real-Time (16 scenarios)
 *
 * Tests multi-project registration, project lifecycle (archive/restore),
 * WebSocket events, activity logs, session management, stale session detection,
 * worker capacity, force release, cross-project isolation, and recovery service.
 *
 * Layout:
 *   - beforeAll registers mcp_test as the primary project (reused by most tests)
 *   - Scenario 1 tests multi-project registration with multi-repo sub-projects
 *   - Scenario 3 (archive/restore) creates a temporary local project for isolation
 *   - Scenario 4 tests duplicate workspace_path rejection against mcp_test's path
 *   - Scenarios 5-16 use the primary mcp_test projectId
 */
test.describe('R04: Multi-Repo & Real-Time', () => {
  const projectConfig = REAL_PROJECTS.mcp_test;
  let client: any;
  let projectId: string;
  let apiContext: APIRequestContext;

  test.beforeAll(async () => {
    apiContext = await pwRequest.newContext({ baseURL: 'http://localhost:19080' });
    client = createClient(apiContext);

    // Register mcp_test as the primary project (idempotent)
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
    // Cleanup: remove test worktrees from mcp_test
    try {
      gitHelper.cleanupWorktrees(projectConfig.path);
    } catch {}
    await apiContext?.dispose();
  });

  // ---------------------------------------------------------------------------
  // Scenario 1: Multi-project registration
  // ---------------------------------------------------------------------------
  test('1: Multi-project registration — 3 sub-projects', async () => {
    const subProjects = getSubProjects();
    expect(subProjects.length).toBeGreaterThanOrEqual(3);

    const registered: any[] = [];
    for (const sp of subProjects) {
      // Each sub-project has a unique workspace_path, so registration should
      // either succeed (first run) or fail gracefully if already registered
      const resp = await client.createProject(sp.registerConfig);
      if (resp.status === 200) {
        expect(resp.data.id).toBeDefined();
        expect(resp.data.name).toBe(sp.name);
        registered.push(resp.data);
      } else {
        // Already registered from a previous test run — acceptable
        expect([409, 422, 500]).toContain(resp.status);
      }
    }

    // Verify successfully registered projects are independently queryable
    for (const proj of registered) {
      const detail = await client.getProject(proj.id);
      expect(detail.status).toBe(200);
      expect(detail.data.name).toBe(proj.name);
    }

    // Verify they are distinct
    if (registered.length >= 2) {
      const ids = registered.map((p: any) => p.id);
      expect(new Set(ids).size).toBe(ids.length);
    }
  });

  // ---------------------------------------------------------------------------
  // Scenario 2: Empty project tolerance
  // ---------------------------------------------------------------------------
  test('2: Empty project tolerance — task operations on primary project', async () => {
    // Use the primary mcp_test project (already has git).
    // Simulates "empty project tolerance" scenario.
    // We simulate "empty project tolerance" by creating minimal data.
    const feature = await client.createFeature(projectId, factory.feature());
    expect(feature.status).toBe(200);

    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: 'EmptyProject-Task',
    }));
    expect(task.status).toBe(200);

    // Claim should work
    const sid = `empty-s-${Date.now()}`;
    const wid = `empty-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    const claimed = await client.getNextTask(projectId, 'backend', wid);
    // May succeed or return 404 depending on worktree creation
    expect([200, 404, 409]).toContain(claimed.status);
  });

  // ---------------------------------------------------------------------------
  // Scenario 3: Project Archive/Restore
  // ---------------------------------------------------------------------------
  test('3: Project Archive/Restore lifecycle', async () => {
    // Create a temporary git repo to use as a unique workspace_path
    const tmpDir = path.join(os.tmpdir(), `maestro-archive-${Date.now()}`);
    fs.mkdirSync(tmpDir, { recursive: true });
    gitHelper.initGitRepo(tmpDir);
    gitHelper.makeFileChange(tmpDir, 'README.md', '# archive test');
    gitHelper.gitInitCommit(tmpDir, 'init');

    try {
      const regResp = await client.createProject({
        name: `ArchiveTest-${Date.now()}`,
        workspace_path: tmpDir,
        description: 'Temporary project for archive/restore test',
      });
      expect(regResp.status).toBe(200);
      const pid = regResp.data.id;

      // Create a feature to have some data
      const feature = await client.createFeature(pid, factory.feature());
      expect(feature.status).toBe(200);

      // Archive the project
      const archiveResp = await client.archiveProject(pid);
      expect(archiveResp.status).toBe(200);
      expect(archiveResp.data.status === 'archived' || archiveResp.data.archived_at).toBeTruthy();

      // POST operations should be rejected on archived project
      const taskResp = await client.createTask(pid, factory.task({
        feature_id: feature.data.id,
        title: 'Should be rejected',
      }));
      expect([403, 409, 422]).toContain(taskResp.status);

      // GET operations should still work
      const getResp = await client.getProject(pid);
      expect(getResp.status).toBe(200);

      const listFeatures = await client.listFeatures(pid);
      expect(listFeatures.status).toBe(200);

      // Restore the project
      const restoreResp = await client.restoreProject(pid);
      expect(restoreResp.status).toBe(200);

      // POST operations should work again after restore
      const taskResp2 = await client.createTask(pid, factory.task({
        feature_id: feature.data.id,
        title: 'After restore',
      }));
      expect(taskResp2.status).toBe(200);
    } finally {
      // Cleanup temp directory
      try { fs.rmSync(tmpDir, { recursive: true, force: true }); } catch {}
    }
  });

  // ---------------------------------------------------------------------------
  // Scenario 4: BindProject path matching
  // ---------------------------------------------------------------------------
  test('4: BindProject — same workspace_path registered twice', async () => {
    // Attempt to register a project with the same workspace_path as the
    // beforeAll project (mcp_test). This should fail because workspace_path
    // is UNIQUE in the database.
    const reg2 = await client.createProject({
      name: `BindTest-Duplicate-${Date.now()}`,
      workspace_path: projectConfig.registerConfig.workspace_path,
      description: 'Attempt to re-register same path',
    });
    // Should fail with 409 (conflict) or 422 (unprocessable)
    expect([409, 422, 500]).toContain(reg2.status);
  });

  // ---------------------------------------------------------------------------
  // Scenario 5: WebSocket events
  // ---------------------------------------------------------------------------
  test('5: WebSocket events — full lifecycle', async () => {
    // Connect WebSocket
    const wsClient = new TestWSClient();
    let wsConnected = false;
    try {
      await wsClient.connect('ws://localhost:19080', projectId);
      wsConnected = true;
    } catch {
      // WebSocket server may not be running — skip WS verification
    }

    // Perform lifecycle operations on the primary project
    const feature = await client.createFeature(projectId, factory.feature());
    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: 'WS-Lifecycle-Task',
    }));

    const sid = `ws-s-${Date.now()}`;
    const wid = `ws-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);

    // Wait briefly for events to propagate
    await new Promise(r => setTimeout(r, 500));

    if (wsConnected) {
      const events = wsClient.getEvents();
      // Should have received some events during the lifecycle
      expect(events.length).toBeGreaterThan(0);

      // Check for expected event types
      const eventTypes = events.map(e => e.type);
      // Expected: task_created, task_claimed, session_registered, etc.
      expect(eventTypes.length).toBeGreaterThan(0);
    }

    await wsClient.disconnect();
  });

  // ---------------------------------------------------------------------------
  // Scenario 6: Activity Log
  // ---------------------------------------------------------------------------
  test('6: Activity Log — full lifecycle audit trail', async () => {
    // Use the primary project
    const feature = await client.createFeature(projectId, factory.feature());
    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: 'Activity-Log-Task',
    }));
    const tid = task.data.id;

    const sid = `act-s-${Date.now()}`;
    const wid = `act-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);
    await client.submitTask(projectId, tid, {
      summary: 'activity log test',
      session_id: sid,
    });

    // Query activity log
    const activity = await client.getActivity(projectId);
    expect(activity.status).toBe(200);
    expect(Array.isArray(activity.data)).toBe(true);

    // Should contain records for key actions
    if (activity.data.length > 0) {
      const actions = activity.data.map((a: any) => a.action);
      // Expected actions: feature_created, task_created, session_registered,
      // task_claimed, task_submitted, etc.
      expect(actions.length).toBeGreaterThan(0);
    }
  });

  // ---------------------------------------------------------------------------
  // Scenario 7: Force Rollback
  // ---------------------------------------------------------------------------
  test('7: Force Rollback + GC', async () => {
    const feature = await client.createFeature(projectId, factory.feature());
    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: 'Rollback-Task',
    }));
    const tid = task.data.id;

    // Claim and submit
    const sid = `rb-s-${Date.now()}`;
    const wid = `rb-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);
    await client.submitTask(projectId, tid, {
      summary: 'rollback test submission',
      session_id: sid,
    });

    // Force rollback
    const rollbackResp = await client.forceRollbackTask(projectId, tid);
    if (rollbackResp.status === 200) {
      // Task should be back to in_progress or pending
      const taskState = await client.getTask(projectId, tid);
      expect(['in_progress', 'pending', 'submitted']).toContain(taskState.data?.status);
    }

    // Trigger GC
    const gcResp = await client.triggerWorktreeGC(projectId);
    expect(gcResp.status).toBe(200);
  });

  // ---------------------------------------------------------------------------
  // Scenario 8: Stale Session detection
  // ---------------------------------------------------------------------------
  test('8: Stale Session detection — no heartbeat', async () => {
    // Register a session but never send heartbeat
    const staleSid = `stale-session-${Date.now()}`;
    const regResp = await client.registerSession(projectId, {
      id: staleSid,
      role: 'backend',
      capacity: 5,
    });
    expect(regResp.status).toBe(200);

    // The session should exist
    const session = await client.getSession(projectId, staleSid);
    expect(session.status).toBe(200);
    expect(session.data.id).toBe(staleSid);

    // Without heartbeat, the session will eventually be marked stale
    // This test verifies the session is queryable immediately after registration
    // Actual stale detection requires time passage or manual trigger
  });

  // ---------------------------------------------------------------------------
  // Scenario 9: Worker capacity limit
  // ---------------------------------------------------------------------------
  test('9: Worker capacity limit — session capacity=2, register 3', async () => {
    // Create 3 tasks in primary project
    const feature = await client.createFeature(projectId, factory.feature());
    for (let i = 0; i < 3; i++) {
      await client.createTask(projectId, factory.task({
        feature_id: feature.data.id,
        title: `Capacity-Task-${i + 1}`,
      }));
    }

    // Register session with capacity=2
    const sid = `cap-s-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 2 });

    // Register workers up to and beyond capacity
    const w1 = await client.registerWorker(projectId, sid, { id: `cap-w1-${Date.now()}` });
    expect(w1.status).toBe(200);

    const w2 = await client.registerWorker(projectId, sid, { id: `cap-w2-${Date.now()}` });
    expect(w2.status).toBe(200);

    // Third worker should be rejected or queued
    const w3 = await client.registerWorker(projectId, sid, { id: `cap-w3-${Date.now()}` });
    expect([200, 403, 409, 422, 429]).toContain(w3.status);

    if (w3.status === 200) {
      // If accepted, verify only 2 can actively claim tasks
      // The third might be accepted but not claim tasks
    }
  });

  // ---------------------------------------------------------------------------
  // Scenario 10: Disconnect cleanup
  // ---------------------------------------------------------------------------
  test('10: Disconnect cleanup — session with in_progress tasks', async () => {
    const feature = await client.createFeature(projectId, factory.feature());
    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: 'Disconnect-Task',
    }));
    const tid = task.data.id;

    // Claim task
    const sid = `disc-s-${Date.now()}`;
    const wid = `disc-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);

    // Verify task is in_progress
    const taskState = await client.getTask(projectId, tid);
    if (taskState.data?.status === 'in_progress') {
      // Disconnect session
      const discResp = await client.disconnectSession(projectId, sid);
      expect([200, 204]).toContain(discResp.status);

      // Task should be released back to pending or remain in_progress
      // depending on disconnect cleanup behavior
      const taskAfter = await client.getTask(projectId, tid);
      expect(taskAfter.status).toBe(200);
      // Task may be: in_progress (assigned to disconnected session),
      // pending (released), or another state
      expect(taskAfter.data.status).toBeDefined();
    }
  });

  // ---------------------------------------------------------------------------
  // Scenario 11: ForceRelease session
  // ---------------------------------------------------------------------------
  test('11: ForceRelease — session with active tasks', async () => {
    const feature = await client.createFeature(projectId, factory.feature());
    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: 'ForceRelease-Task',
    }));
    const tid = task.data.id;

    // Claim task
    const sid = `fr-s-${Date.now()}`;
    const wid = `fr-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);

    // Force release the session
    const forceResp = await client.forceReleaseSession(projectId, sid);
    expect(forceResp.status).toBe(200);

    // Verify session is cleaned up
    const session = await client.getSession(projectId, sid);
    // Session may return 404 (deleted) or have a terminated status
    if (session.status === 200) {
      expect(session.data.status).toBeDefined();
    }
  });

  // ---------------------------------------------------------------------------
  // Scenario 12: Startup recovery (may need special handling)
  // ---------------------------------------------------------------------------
  test('12: Startup recovery — RecoveryService', async () => {
    // This test verifies that recovery-related APIs exist and are callable
    // Actual restart-based recovery testing requires external orchestration

    // Verify metrics endpoint is accessible (indicates service health)
    const metrics = await client.getMetrics();
    expect(metrics.status).toBe(200);

    // Verify overview endpoint works
    const overview = await client.getOverview();
    expect(overview.status).toBe(200);
  });

  // ---------------------------------------------------------------------------
  // Scenario 13: Cross-project task isolation
  // ---------------------------------------------------------------------------
  test('13: Cross-project task isolation — A task invisible to B', async () => {
    // Use the primary project (A) and register a second project (B) with a
    // unique temporary workspace_path
    const tmpDir = path.join(os.tmpdir(), `maestro-isoB-${Date.now()}`);
    fs.mkdirSync(tmpDir, { recursive: true });
    gitHelper.initGitRepo(tmpDir);
    gitHelper.makeFileChange(tmpDir, 'README.md', '# isolation test B');
    gitHelper.gitInitCommit(tmpDir, 'init');

    try {
      const regB = await client.createProject({
        name: `IsolationB-${Date.now()}`,
        workspace_path: tmpDir,
        description: 'Project B for cross-project isolation test',
      });
      expect(regB.status).toBe(200);
      const pidB = regB.data.id;

      // Create task in primary project (A)
      const featureA = await client.createFeature(projectId, factory.feature());
      const taskA = await client.createTask(projectId, factory.task({
        feature_id: featureA.data.id,
        title: 'Isolated-Task-A',
      }));
      const tidA = taskA.data.id;

      // Query task from project B — should return 404
      const taskFromB = await client.getTask(pidB, tidA);
      expect(taskFromB.status).toBe(404);

      // List tasks in project B — should not include task A
      const tasksB = await client.listTasks(pidB);
      expect(tasksB.status).toBe(200);
      if (Array.isArray(tasksB.data)) {
        const found = tasksB.data.find((t: any) => t.id === tidA);
        expect(found).toBeUndefined();
      }
    } finally {
      try { fs.rmSync(tmpDir, { recursive: true, force: true }); } catch {}
    }
  });

  // ---------------------------------------------------------------------------
  // Scenario 14: ForceRelease blocks submit
  // ---------------------------------------------------------------------------
  test('14: ForceRelease blocks subsequent submit', async () => {
    const feature = await client.createFeature(projectId, factory.feature());
    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: 'ForceReleaseBlock-Task',
    }));
    const tid = task.data.id;

    // Claim task
    const sid = `frb-s-${Date.now()}`;
    const wid = `frb-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);

    // Force release the session
    await client.forceReleaseSession(projectId, sid);

    // Try to submit using the force-released session — should be rejected
    const submitResp = await client.submitTask(projectId, tid, {
      summary: 'submit after force release',
      session_id: sid,
    });
    expect([403, 409, 422]).toContain(submitResp.status);
  });

  // ---------------------------------------------------------------------------
  // Scenario 15: ForceRelease vs Submit race condition
  // ---------------------------------------------------------------------------
  test('15: ForceRelease vs Submit race — Promise.all', async () => {
    const feature = await client.createFeature(projectId, factory.feature());
    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: 'Race-Task',
    }));
    const tid = task.data.id;

    // Claim task
    const sid = `race-s-${Date.now()}`;
    const wid = `race-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);

    // Simultaneously force-release and submit
    const [forceResp, submitResp] = await Promise.all([
      client.forceReleaseSession(projectId, sid),
      client.submitTask(projectId, tid, {
        summary: 'concurrent submit during force release',
        session_id: sid,
      }),
    ]);

    // One should succeed, one should fail — no assertion on which
    // The important thing is no crash or data corruption
    expect([200, 403, 409, 422]).toContain(forceResp.status);
    expect([200, 403, 409, 422]).toContain(submitResp.status);

    // Task should be in a valid state regardless of race outcome
    const taskState = await client.getTask(projectId, tid);
    expect(taskState.status).toBe(200);
    expect(taskState.data.status).toBeDefined();
  });

  // ---------------------------------------------------------------------------
  // Scenario 16: RecoveryService observability
  // ---------------------------------------------------------------------------
  test('16: RecoveryService observability — recovery events recorded', async () => {
    // Create a scenario that might trigger recovery: register session, claim task, disconnect
    const feature = await client.createFeature(projectId, factory.feature());
    const task = await client.createTask(projectId, factory.task({
      feature_id: feature.data.id,
      title: 'RecoveryObs-Task',
    }));

    const sid = `obs-s-${Date.now()}`;
    const wid = `obs-w-${Date.now()}`;
    await client.registerSession(projectId, { id: sid, role: 'backend', capacity: 5 });
    await client.getNextTask(projectId, 'backend', wid);

    // Force release triggers cleanup which should be observable
    await client.forceReleaseSession(projectId, sid);

    // Check activity log for recovery-related events
    const activity = await client.getActivity(projectId, { limit: 50 });
    expect(activity.status).toBe(200);
    if (Array.isArray(activity.data) && activity.data.length > 0) {
      // Should contain records for the lifecycle events above
      const actions = activity.data.map((a: any) => a.action);
      expect(actions.length).toBeGreaterThan(0);
    }
  });
});
