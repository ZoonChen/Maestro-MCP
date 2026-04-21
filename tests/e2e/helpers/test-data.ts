import { MaestroApiClient } from './api-client';
import * as os from 'os';
import * as path from 'path';

// =============================================================================
// Type definitions
// =============================================================================

export interface ProjectData {
  name: string;
  workspace_path: string;
  description?: string;
  config?: any;
}

export interface FeatureData {
  title: string;
  description?: string;
  reference_urls?: string;
  status?: string;
}

export interface TaskData {
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
}

export interface SessionData {
  id: string;
  role: string;
  client_type?: string;
  capacity?: number;
}

export interface WorkerData {
  id: string;
  status?: string;
}

// =============================================================================
// Constants
// =============================================================================

export const ROLES = [
  'backend',
  'frontend',
  'devops',
  'verifier',
  'coordinator',
] as const;

export const PRIORITIES = ['low', 'normal', 'high', 'urgent'] as const;

export const TASK_STATUSES = [
  'pending',
  'in_progress',
  'submitted',
  'verifying',
  'ready_to_merge',
  'merge_conflicted',
  'done',
  'blocked',
  'cancelled',
] as const;

/** Convenience type aliases derived from the const tuples above. */
export type Role = (typeof ROLES)[number];
export type Priority = (typeof PRIORITIES)[number];
export type TaskStatus = (typeof TASK_STATUSES)[number];

// =============================================================================
// Factory
// =============================================================================

/** Unique-ish suffix to avoid collisions when tests run in parallel. */
function suffix(): string {
  return `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

export const factory = {
  project: (overrides?: Partial<ProjectData>): ProjectData => ({
    name: `Test Project ${suffix()}`,
    workspace_path: path.join(os.tmpdir(), `test-project-${suffix()}`),
    description: 'Test project description',
    ...overrides,
  }),

  feature: (overrides?: Partial<FeatureData>): FeatureData => ({
    title: `Feature ${suffix()}`,
    description: 'Test feature description',
    ...overrides,
  }),

  task: (overrides?: Partial<TaskData>): TaskData => ({
    feature_id: '', // caller must supply or rely on createFullProjectSetup
    title: `Task ${suffix()}`,
    description: 'Test task description',
    role: 'backend',
    allowed_directories: '["src/"]',
    ...overrides,
  }),

  session: (overrides?: Partial<SessionData>): SessionData => ({
    id: `session-${suffix()}`,
    role: 'backend',
    client_type: 'test',
    capacity: 5,
    ...overrides,
  }),

  worker: (overrides?: Partial<WorkerData>): WorkerData => ({
    id: `worker-${suffix()}`,
    status: 'idle',
    ...overrides,
  }),
};

// =============================================================================
// Helpers
// =============================================================================

/**
 * Create a full project scaffold in one call:
 *   1. Create a project
 *   2. Create a feature inside that project
 *   3. Create `taskCount` tasks linked to that feature
 *
 * Returns the API responses so callers can assert on them.
 */
export async function createFullProjectSetup(
  client: MaestroApiClient,
  options?: {
    taskCount?: number;
    taskRole?: string;
    priorities?: string[];
  },
): Promise<{ project: any; feature: any; tasks: any[] }> {
  const taskCount = options?.taskCount ?? 1;
  const taskRole = options?.taskRole ?? 'backend';
  const priorities = options?.priorities ?? ['normal'];

  // 1. Project
  const projectResp = await client.createProject(factory.project());
  if (projectResp.error) {
    throw new Error(`Failed to create project: ${projectResp.error}`);
  }
  const project = projectResp.data;

  // 2. Feature
  const featureResp = await client.createFeature(
    project.id,
    factory.feature(),
  );
  if (featureResp.error) {
    throw new Error(`Failed to create feature: ${featureResp.error}`);
  }
  const feature = featureResp.data;

  // 3. Tasks
  const tasks: any[] = [];
  for (let i = 0; i < taskCount; i++) {
    const priority = priorities[i % priorities.length];
    const taskResp = await client.createTask(
      project.id,
      factory.task({
        feature_id: feature.id,
        role: taskRole,
        priority,
      }),
    );
    if (taskResp.error) {
      throw new Error(`Failed to create task ${i}: ${taskResp.error}`);
    }
    tasks.push(taskResp.data);
  }

  return { project, feature, tasks };
}
