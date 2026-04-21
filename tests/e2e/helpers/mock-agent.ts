import { MaestroApiClient } from './api-client';
import { factory } from './test-data';
import { makeFileChange, gitCommit } from './git-helper';

/**
 * MockAgent simulates an AI agent interacting with Maestro via REST API.
 * It claims tasks, does work in worktrees, and submits results.
 */
export class MockAgent {
  private client: MaestroApiClient;
  private projectId: string;
  private sessionId: string;
  private workerId: string;
  private role: string;
  public currentTask: any = null;

  constructor(client: MaestroApiClient, projectId: string, role: string, sessionId?: string, workerId?: string) {
    this.client = client;
    this.projectId = projectId;
    this.role = role;
    this.sessionId = sessionId ?? `mock-session-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    this.workerId = workerId ?? `mock-worker-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  }

  /** Register session + implicitly connect to Maestro. */
  async connect(): Promise<void> {
    await this.client.registerSession(this.projectId, {
      id: this.sessionId,
      role: this.role,
      capacity: 5,
    });
  }

  /** Get the next available task for this agent's role. */
  async pickupTask(): Promise<any> {
    const resp = await this.client.getNextTask(
      this.projectId,
      this.role,
      this.workerId,
    );
    if (resp.status === 200 && resp.data) {
      this.currentTask = resp.data;
    }
    return resp;
  }

  /** Simulate work: create/modify files in the worktree. */
  async doWork(changes: Array<{ path: string; content: string }>, worktreePath?: string): Promise<void> {
    if (!worktreePath && !this.currentTask) {
      throw new Error('No current task - call pickupTask first');
    }
    // Worktree path is determined by the Maestro server.
    // For testing, we use the task's worktree path if available,
    // or fall back to direct project workspace manipulation.
    const basePath = worktreePath ?? '';
    if (!basePath) {
      // Without a real worktree, we can't do file changes.
      // This is expected in virtual-path tests.
      return;
    }
    for (const change of changes) {
      makeFileChange(basePath, change.path, change.content);
    }
  }

  /** Submit task result, triggering zero-trust validation. */
  async submit(summary: string): Promise<any> {
    if (!this.currentTask) {
      throw new Error('No current task - call pickupTask first');
    }
    return this.client.submitTask(this.projectId, this.currentTask.id, {
      summary,
      session_id: this.sessionId,
    });
  }

  /**
   * Execute a full lifecycle: pickup → work → submit.
   * Returns the submit response.
   */
  async executeFullLifecycle(
    changes: Array<{ path: string; content: string }>,
    summary: string,
    worktreePath?: string,
  ): Promise<any> {
    await this.pickupTask();
    if (!this.currentTask) {
      throw new Error('No task available to claim');
    }
    await this.doWork(changes, worktreePath);
    return this.submit(summary);
  }

  /** Release the worker (removes worker record). */
  async releaseWorker(): Promise<any> {
    return this.client.removeWorker(this.projectId, this.sessionId, this.workerId);
  }

  getSessionId(): string { return this.sessionId; }
  getWorkerId(): string { return this.workerId; }
  getRole(): string { return this.role; }
}

/**
 * MockVerifier simulates a verification agent.
 */
export class MockVerifier {
  private client: MaestroApiClient;
  private projectId: string;
  private sessionId: string;
  private workerId: string;
  public currentTask: any = null;

  constructor(client: MaestroApiClient, projectId: string, sessionId?: string, workerId?: string) {
    this.client = client;
    this.projectId = projectId;
    this.sessionId = sessionId ?? `mock-verifier-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    this.workerId = workerId ?? `verifier-worker-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  }

  /** Register as a verifier session. */
  async connect(): Promise<void> {
    await this.client.registerSession(this.projectId, {
      id: this.sessionId,
      role: 'verifier',
      capacity: 5,
    });
  }

  /** Pick up the next task for verification. */
  async pickupVerification(): Promise<any> {
    const resp = await this.client.getNextVerificationTask(
      this.projectId,
      this.sessionId,
      this.workerId,
    );
    if (resp.status === 200 && resp.data) {
      this.currentTask = resp.data;
    }
    return resp;
  }

  /** Approve (pass) the task verification. */
  async approve(taskId: string): Promise<any> {
    return this.client.verifyTask(this.projectId, taskId, {
      session_id: this.sessionId,
      worker_id: this.workerId,
      passed: true,
    });
  }

  /** Reject (fail) the task verification. */
  async reject(taskId: string, notes?: string): Promise<any> {
    return this.client.verifyTask(this.projectId, taskId, {
      session_id: this.sessionId,
      worker_id: this.workerId,
      passed: false,
      notes: notes ?? 'needs rework',
    });
  }

  /** Merge the task after approval. */
  async merge(taskId: string): Promise<any> {
    return this.client.mergeTask(this.projectId, taskId);
  }

  /**
   * Full verification lifecycle: pickup → approve → merge.
   */
  async executeFullVerification(): Promise<{ verify: any; merge: any }> {
    await this.pickupVerification();
    if (!this.currentTask) {
      throw new Error('No task available for verification');
    }
    const taskId = this.currentTask.id;
    const verifyResp = await this.approve(taskId);
    const mergeResp = await this.merge(taskId);
    return { verify: verifyResp, merge: mergeResp };
  }

  getSessionId(): string { return this.sessionId; }
}
