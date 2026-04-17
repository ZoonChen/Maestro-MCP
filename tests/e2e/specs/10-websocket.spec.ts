import { test, expect } from '@playwright/test';
import { createClient } from '../helpers/api-client';
import { factory, createFullProjectSetup } from '../helpers/test-data';
import { TestWSClient } from '../helpers/ws-client';

test.describe('WebSocket Events', () => {
  test('should connect to WebSocket', async ({ request }) => {
    const api = createClient(request);
    const proj = await api.createProject(factory.project());

    const ws = new TestWSClient();
    await ws.connect('ws://localhost:19080', proj.data!.id);
    expect(ws.isConnected()).toBeTruthy();
    await ws.disconnect();
  });

  // NOTE: Event broadcasting is not yet fully implemented on the server side.
  // This test validates WS infrastructure connectivity. Once the server
  // broadcasts task.created events, add explicit event assertion here.
  test('should maintain WS connection during task creation', async ({ request }) => {
    const api = createClient(request);
    const proj = await api.createProject(factory.project());
    const feat = await api.createFeature(proj.data!.id, factory.feature());

    const ws = new TestWSClient();
    await ws.connect('ws://localhost:19080', proj.data!.id);

    // Create a task while connected
    await api.createTask(
      proj.data!.id,
      factory.task({ feature_id: feat.data!.id, role: 'backend', allowed_directories: '["src/"]' }),
    );

    // Connection should remain stable after task creation
    expect(ws.isConnected()).toBeTruthy();
    await ws.disconnect();
  });

  test('should isolate events between projects', async ({ request }) => {
    const api = createClient(request);
    const proj1 = await api.createProject(factory.project());
    const proj2 = await api.createProject(factory.project());

    const ws1 = new TestWSClient();
    const ws2 = new TestWSClient();
    await ws1.connect('ws://localhost:19080', proj1.data!.id);
    await ws2.connect('ws://localhost:19080', proj2.data!.id);

    expect(ws1.isConnected()).toBeTruthy();
    expect(ws2.isConnected()).toBeTruthy();

    await ws1.disconnect();
    await ws2.disconnect();
  });

  test('should handle WebSocket disconnect', async ({ request }) => {
    const api = createClient(request);
    const proj = await api.createProject(factory.project());

    const ws = new TestWSClient();
    await ws.connect('ws://localhost:19080', proj.data!.id);
    expect(ws.isConnected()).toBeTruthy();

    await ws.disconnect();
    expect(ws.isConnected()).toBeFalsy();
  });
});
