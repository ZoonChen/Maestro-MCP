import { APIRequestContext } from '@playwright/test';

/** Unified response shape returned by every MaestroApiClient method. */
export interface ApiResponse<T = any> {
  status: number;
  data: T | null;
  error: string | null;
}

export class MaestroApiClient {
  constructor(
    private request: APIRequestContext,
    private baseUrl = '/api/v1',
  ) {}

  // ---------------------------------------------------------------------------
  // Internal helpers
  // ---------------------------------------------------------------------------

  private async send<T = any>(
    method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE',
    path: string,
    body?: unknown,
    params?: Record<string, string | number | boolean | undefined>,
  ): Promise<ApiResponse<T>> {
    const url = `${this.baseUrl}${path}`;
    const queryParams: Record<string, string> = {};
    if (params) {
      for (const [k, v] of Object.entries(params)) {
        if (v !== undefined && v !== null) {
          queryParams[k] = String(v);
        }
      }
    }

    const response = await this.request.fetch(url, {
      method,
      data: body,
      params: queryParams,
      headers: { 'Content-Type': 'application/json' },
    });

    const status = response.status();
    const json = await response.json();

    if (response.ok()) {
      return { status, data: json.data as T, error: null };
    }
    return { status, data: null, error: (json.error as string) ?? response.statusText() };
  }

  private post<T = any>(path: string, body?: unknown): Promise<ApiResponse<T>> {
    return this.send<T>('POST', path, body);
  }

  private get<T = any>(
    path: string,
    params?: Record<string, string | number | boolean | undefined>,
  ): Promise<ApiResponse<T>> {
    return this.send<T>('GET', path, undefined, params);
  }

  private patch<T = any>(path: string, body?: unknown): Promise<ApiResponse<T>> {
    return this.send<T>('PATCH', path, body);
  }

  private put<T = any>(path: string, body?: unknown): Promise<ApiResponse<T>> {
    return this.send<T>('PUT', path, body);
  }

  private delete<T = any>(path: string): Promise<ApiResponse<T>> {
    return this.send<T>('DELETE', path);
  }

  // ===========================================================================
  // Project
  // ===========================================================================

  createProject(data: {
    name: string;
    workspace_path: string;
    description?: string;
    config?: any;
  }): Promise<ApiResponse> {
    return this.post('/projects', data);
  }

  listProjects(includeArchived?: boolean): Promise<ApiResponse> {
    return this.get('/projects', { include_archived: includeArchived });
  }

  getProject(id: string): Promise<ApiResponse> {
    return this.get(`/projects/${id}`);
  }

  updateProject(
    id: string,
    data: { name?: string; description?: string },
  ): Promise<ApiResponse> {
    return this.patch(`/projects/${id}`, data);
  }

  archiveProject(id: string): Promise<ApiResponse> {
    return this.post(`/projects/${id}/archive`);
  }

  restoreProject(id: string): Promise<ApiResponse> {
    return this.post(`/projects/${id}/restore`);
  }

  // ===========================================================================
  // Feature (project-scoped)
  // ===========================================================================

  createFeature(
    pid: string,
    data: {
      title: string;
      description?: string;
      reference_urls?: string;
      status?: string;
    },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/features`, data);
  }

  listFeatures(pid: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/features`);
  }

  getFeature(pid: string, id: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/features/${id}`);
  }

  updateFeature(
    pid: string,
    id: string,
    data: {
      title?: string;
      description?: string;
      reference_urls?: string;
      status?: string;
    },
  ): Promise<ApiResponse> {
    return this.patch(`/projects/${pid}/features/${id}`, data);
  }

  // ===========================================================================
  // Task (project-scoped)
  // ===========================================================================

  createTask(
    pid: string,
    data: {
      feature_id: string;
      title: string;
      description: string;
      role: string;
      allowed_directories: string;
      forbidden_patterns?: string;
      required_apis?: string;
      dependencies?: string;
      test_requirements?: string;
      priority?: string;
    },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/tasks`, data);
  }

  listTasks(
    pid: string,
    filters?: { status?: string; role?: string; feature_id?: string },
  ): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/tasks`, filters);
  }

  getNextTask(
    pid: string,
    role: string,
    workerId?: string,
  ): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/tasks/next`, {
      role,
      worker_id: workerId,
    });
  }

  getNextVerificationTask(
    pid: string,
    sessionId?: string,
    workerId?: string,
  ): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/tasks/next-verification`, {
      session_id: sessionId,
      worker_id: workerId,
    });
  }

  getTask(pid: string, id: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/tasks/${id}`);
  }

  updateTask(
    pid: string,
    id: string,
    data: {
      title?: string;
      description?: string;
      allowed_directories?: string;
      priority?: string;
    },
  ): Promise<ApiResponse> {
    return this.patch(`/projects/${pid}/tasks/${id}`, data);
  }

  claimTask(
    pid: string,
    id: string,
    data: { session_id: string; worker_id: string },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/tasks/${id}/claim`, data);
  }

  submitTask(
    pid: string,
    id: string,
    data?: { summary?: string; session_id?: string },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/tasks/${id}/submit`, data);
  }

  blockTask(
    pid: string,
    id: string,
    data: { reason: string },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/tasks/${id}/block`, data);
  }

  resolveBlocker(
    pid: string,
    id: string,
    data: { reassign?: boolean },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/tasks/${id}/resolve`, data);
  }

  verifyTask(
    pid: string,
    id: string,
    data: {
      session_id: string;
      worker_id: string;
      passed: boolean;
      notes?: string;
    },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/tasks/${id}/verify`, data);
  }

  mergeTask(pid: string, id: string): Promise<ApiResponse> {
    return this.send('POST', `/projects/${pid}/tasks/${id}/merge`);
  }

  resolveMergeConflict(
    pid: string,
    id: string,
    data: { action: string; reason?: string },
  ): Promise<ApiResponse> {
    return this.post(
      `/projects/${pid}/tasks/${id}/resolve-merge-conflict`,
      data,
    );
  }

  cancelTask(
    pid: string,
    id: string,
    data: { reason: string },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/tasks/${id}/cancel`, data);
  }

  // ===========================================================================
  // Session (project-scoped)
  // ===========================================================================

  registerSession(
    pid: string,
    data: {
      id: string;
      role: string;
      client_type?: string;
      capacity?: number;
    },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/sessions`, data);
  }

  listSessions(pid: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/sessions`);
  }

  getSession(pid: string, id: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/sessions/${id}`);
  }

  heartbeat(pid: string, id: string): Promise<ApiResponse> {
    return this.put(`/projects/${pid}/sessions/${id}/heartbeat`);
  }

  disconnectSession(pid: string, id: string): Promise<ApiResponse> {
    return this.delete(`/projects/${pid}/sessions/${id}`);
  }

  // ===========================================================================
  // Worker (nested under session)
  // ===========================================================================

  registerWorker(
    pid: string,
    sid: string,
    data: { id: string; status?: string },
  ): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/sessions/${sid}/workers`, data);
  }

  listWorkers(pid: string, sid: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/sessions/${sid}/workers`);
  }

  removeWorker(
    pid: string,
    sid: string,
    wid: string,
  ): Promise<ApiResponse> {
    return this.delete(`/projects/${pid}/sessions/${sid}/workers/${wid}`);
  }

  // ===========================================================================
  // Board (project-scoped)
  // ===========================================================================

  getBoard(pid: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/board`);
  }

  getActivity(
    pid: string,
    params?: { limit?: number; since?: string },
  ): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/board/activity`, params);
  }

  // ===========================================================================
  // Global
  // ===========================================================================

  getOverview(): Promise<ApiResponse> {
    return this.get('/overview');
  }

  getMetrics(): Promise<ApiResponse> {
    return this.get('/metrics');
  }

  // ===========================================================================
  // Task detail APIs
  // ===========================================================================

  getValidationHistory(pid: string, tid: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/tasks/${tid}/validation`);
  }

  getTaskResult(pid: string, tid: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/tasks/${tid}/result`);
  }

  getTaskDiff(pid: string, tid: string): Promise<ApiResponse> {
    return this.get(`/projects/${pid}/tasks/${tid}/diff`);
  }

  forceRollbackTask(pid: string, tid: string): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/tasks/${tid}/force-rollback`);
  }

  // ===========================================================================
  // Session admin
  // ===========================================================================

  forceReleaseSession(pid: string, sid: string): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/sessions/${sid}/force-release`);
  }

  // ===========================================================================
  // Worktree GC
  // ===========================================================================

  triggerWorktreeGC(pid: string): Promise<ApiResponse> {
    return this.post(`/projects/${pid}/worktrees/gc`);
  }
}

/**
 * Convenience factory — create a MaestroApiClient from a Playwright
 * APIRequestContext (typically obtained via the `request` fixture).
 */
export function createClient(request: APIRequestContext): MaestroApiClient {
  return new MaestroApiClient(request);
}
