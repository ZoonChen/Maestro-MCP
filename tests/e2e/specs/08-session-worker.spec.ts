import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Session and Worker Management', () => {
  let projectId: string;

  test.beforeEach(async ({ request }) => {
    const api = createClient(request);
    const resp = await api.createProject(factory.project());
    expect(resp.status).toBe(200);
    projectId = resp.data!.id;
  });

  test('should register a session', async ({ request }) => {
    const api = createClient(request);
    const resp = await api.registerSession(projectId, {
      id: 's-001',
      role: 'backend',
      client_type: 'test',
      capacity: 5,
    });

    expect(resp.status).toBe(200);
    expect(resp.data).toMatchObject({
      id: 's-001',
      status: 'online',
    });
  });

  test('should list sessions', async ({ request }) => {
    const api = createClient(request);
    await api.registerSession(projectId, {
      id: 's-list-001',
      role: 'backend',
      client_type: 'test',
      capacity: 5,
    });

    const resp = await api.listSessions(projectId);
    expect(resp.status).toBe(200);
    expect(Array.isArray(resp.data)).toBeTruthy();
    expect(resp.data!.length).toBeGreaterThanOrEqual(1);
    expect(resp.data!.some((s: any) => s.id === 's-list-001')).toBeTruthy();
  });

  test('should get a session by id', async ({ request }) => {
    const api = createClient(request);
    await api.registerSession(projectId, {
      id: 's-get-001',
      role: 'frontend',
      client_type: 'test',
      capacity: 3,
    });

    const resp = await api.getSession(projectId, 's-get-001');
    expect(resp.status).toBe(200);
    expect(resp.data).toMatchObject({
      id: 's-get-001',
      role: 'frontend',
    });
  });

  test('should update heartbeat', async ({ request }) => {
    const api = createClient(request);
    await api.registerSession(projectId, {
      id: 's-hb-001',
      role: 'backend',
      client_type: 'test',
      capacity: 5,
    });

    const resp = await api.heartbeat(projectId, 's-hb-001');
    expect(resp.status).toBe(200);
    expect(resp.data).toMatchObject({
      id: 's-hb-001',
      status: 'heartbeat_updated',
    });
  });

  test('should disconnect a session', async ({ request }) => {
    const api = createClient(request);
    await api.registerSession(projectId, {
      id: 's-disc-001',
      role: 'backend',
      client_type: 'test',
      capacity: 5,
    });

    const resp = await api.disconnectSession(projectId, 's-disc-001');
    expect(resp.status).toBe(200);
    expect(resp.data).toMatchObject({
      id: 's-disc-001',
      status: 'offline',
    });
  });

  test('should register a worker', async ({ request }) => {
    const api = createClient(request);
    await api.registerSession(projectId, {
      id: 's-wk-001',
      role: 'backend',
      client_type: 'test',
      capacity: 5,
    });

    const resp = await api.registerWorker(projectId, 's-wk-001', {
      id: 'w-001',
      status: 'active',
    });

    expect(resp.status).toBe(200);
    expect(resp.data).toMatchObject({
      id: 'w-001',
    });
  });

  test('should list workers', async ({ request }) => {
    const api = createClient(request);
    await api.registerSession(projectId, {
      id: 's-lw-001',
      role: 'backend',
      client_type: 'test',
      capacity: 5,
    });
    await api.registerWorker(projectId, 's-lw-001', {
      id: 'w-list-001',
      status: 'active',
    });

    const resp = await api.listWorkers(projectId, 's-lw-001');
    expect(resp.status).toBe(200);
    expect(Array.isArray(resp.data)).toBeTruthy();
    expect(resp.data!.length).toBeGreaterThanOrEqual(1);
    expect(resp.data!.some((w: any) => w.id === 'w-list-001')).toBeTruthy();
  });

  test('should remove a worker', async ({ request }) => {
    const api = createClient(request);
    await api.registerSession(projectId, {
      id: 's-rw-001',
      role: 'backend',
      client_type: 'test',
      capacity: 5,
    });
    await api.registerWorker(projectId, 's-rw-001', {
      id: 'w-remove-001',
      status: 'active',
    });

    const resp = await api.removeWorker(projectId, 's-rw-001', 'w-remove-001');
    expect(resp.status).toBe(200);
  });

  test('should fail to register session with invalid role', async ({ request }) => {
    const api = createClient(request);
    const resp = await api.registerSession(projectId, {
      id: 's-002',
      role: 'invalid_role',
      client_type: 'test',
      capacity: 5,
    });

    expect(resp.status).toBe(400);
    expect(resp.error).toBeTruthy();
  });
});
