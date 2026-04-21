import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';

test.describe('Project Management', () => {
  test('should create a project', async ({ request }) => {
    const api = createClient(request);
    const data = factory.project();

    const resp = await api.createProject(data);

    expect(resp.status).toBe(200);
    expect(resp.data).toBeDefined();
    expect(resp.data.id).toBeDefined();
    expect(resp.data.name).toBe(data.name);
    expect(resp.data.workspace_path).toBe(data.workspace_path);
    expect(resp.data.status).toBe('active');
    expect(resp.data.created_at).toBeDefined();
  });

  test('should list projects', async ({ request }) => {
    const api = createClient(request);
    const data = factory.project({ name: 'Listable Project' });
    const created = await api.createProject(data);
    expect(created.status).toBe(200);

    const resp = await api.listProjects();

    expect(resp.status).toBe(200);
    expect(resp.data).toBeDefined();
    const names = resp.data.map((p: any) => p.name);
    expect(names).toContain('Listable Project');
  });

  test('should get a project by id', async ({ request }) => {
    const api = createClient(request);
    const data = factory.project({ name: 'Gettable Project', description: 'Full field check' });
    const created = await api.createProject(data);
    expect(created.status).toBe(200);

    const resp = await api.getProject(created.data.id);

    expect(resp.status).toBe(200);
    expect(resp.data).toBeDefined();
    expect(resp.data.id).toBe(created.data.id);
    expect(resp.data.name).toBe('Gettable Project');
    expect(resp.data.workspace_path).toBe(data.workspace_path);
    expect(resp.data.description).toBe('Full field check');
    expect(resp.data.status).toBe('active');
  });

  test('should update a project', async ({ request }) => {
    const api = createClient(request);
    const created = await api.createProject(factory.project({ name: 'Original Name' }));
    expect(created.status).toBe(200);

    const resp = await api.updateProject(created.data.id, { name: 'Updated Name' });

    expect(resp.status).toBe(200);
    expect(resp.data.name).toBe('Updated Name');

    // Verify via GET
    const fetched = await api.getProject(created.data.id);
    expect(fetched.data.name).toBe('Updated Name');
  });

  test('should archive a project', async ({ request }) => {
    const api = createClient(request);
    const created = await api.createProject(factory.project({ name: 'To Archive' }));
    expect(created.status).toBe(200);

    const resp = await api.archiveProject(created.data.id);

    expect(resp.status).toBe(200);
    expect(resp.data.status).toBe('archived');

    // Verify via GET
    const fetched = await api.getProject(created.data.id);
    expect(fetched.data.status).toBe('archived');
  });

  test('should restore an archived project', async ({ request }) => {
    const api = createClient(request);
    const created = await api.createProject(factory.project({ name: 'To Restore' }));
    expect(created.status).toBe(200);

    await api.archiveProject(created.data.id);
    const fetched = await api.getProject(created.data.id);
    expect(fetched.data.status).toBe('archived');

    const resp = await api.restoreProject(created.data.id);

    expect(resp.status).toBe(200);
    expect(resp.data.status).toBe('active');

    // Verify via GET
    const restored = await api.getProject(created.data.id);
    expect(restored.data.status).toBe('active');
  });

  test('should fail to create project without required fields', async ({ request }) => {
    const api = createClient(request);

    // Missing name
    const resp = await api.createProject({
      name: '',
      workspace_path: '/tmp/missing-name',
    });

    expect(resp.status).toBe(400);
    expect(resp.error).toBeDefined();
  });

  test('should return 404 for non-existent project', async ({ request }) => {
    const api = createClient(request);

    const resp = await api.getProject('non-existent-id');

    expect(resp.status).toBe(404);
    expect(resp.error).toBeDefined();
  });
});
